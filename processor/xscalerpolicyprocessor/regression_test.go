package xscalerpolicyprocessor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var staleFlags = pmetric.DefaultDataPointFlags.WithNoRecordedValue(true)

// batchOf builds one batch with one resource per pod; add fills the
// metric of each pod.
func batchOf(pods []string, name string, add func(pod string, m pmetric.Metric)) pmetric.Metrics {
	md := pmetric.NewMetrics()
	for _, pod := range pods {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("k8s.pod.name", pod)
		m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName(name)
		add(pod, m)
	}
	return md
}

func process(t *testing.T, p *policyProcessor, md pmetric.Metrics) []pmetric.Metric {
	t.Helper()
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	var all []pmetric.Metric
	for _, ms := range collectMetrics(out) {
		all = append(all, ms...)
	}
	return all
}

// counterPoint sends cumulative counter points (one per pod); a negative
// value sends a staleness marker instead.
func counterBatch(ts int64, vals map[string]float64) pmetric.Metrics {
	pods := make([]string, 0, len(vals))
	for pod := range vals {
		pods = append(pods, pod)
	}
	return batchOf(pods, "requests_total", func(pod string, m pmetric.Metric) {
		s := m.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		dp := s.DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.Timestamp(ts))
		if v := vals[pod]; v < 0 {
			dp.SetFlags(staleFlags)
		} else {
			dp.SetDoubleValue(v)
		}
	})
}

func onlySum(t *testing.T, ms []pmetric.Metric) (float64, bool) {
	t.Helper()
	if len(ms) == 0 {
		return 0, false
	}
	require.Len(t, ms, 1)
	require.Equal(t, 1, ms[0].Sum().DataPoints().Len())
	return ms[0].Sum().DataPoints().At(0).DoubleValue(), true
}

// Finding 1: a staleness marker (failed scrape) must not read as a reset to
// 0, or the next real point counts the pod's whole value again.
func TestStaleMarkerCounterNotReset(t *testing.T) {
	p, clock := aggProcessor(t, nil)
	v, _ := onlySum(t, process(t, p, counterBatch(10, map[string]float64{"a": 1000, "b": 500})))
	require.Equal(t, 1500.0, v)
	*clock = clock.Add(15 * time.Second)
	v, _ = onlySum(t, process(t, p, counterBatch(20, map[string]float64{"a": 1010})))
	require.Equal(t, 1510.0, v)
	*clock = clock.Add(15 * time.Second)
	_, ok := onlySum(t, process(t, p, counterBatch(30, map[string]float64{"a": -1})))
	assert.False(t, ok, "a staleness marker alone reports nothing new")
	*clock = clock.Add(15 * time.Second)
	v, _ = onlySum(t, process(t, p, counterBatch(40, map[string]float64{"a": 1020})))
	assert.Equal(t, 1520.0, v, "recovery adds only the real increase")
	// A stale marker for a source never seen is ignored and not tracked.
	_, ok = onlySum(t, process(t, p, counterBatch(41, map[string]float64{"zz": -1})))
	assert.False(t, ok)
	assert.Equal(t, 2, p.acc.nSources())
}

func histBatch(ts int64, pods map[string][]uint64, stale bool) pmetric.Metrics {
	names := make([]string, 0, len(pods))
	for pod := range pods {
		names = append(names, pod)
	}
	return batchOf(names, "requests_total", func(pod string, m pmetric.Metric) {
		h := m.SetEmptyHistogram()
		h.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		dp := h.DataPoints().AppendEmpty()
		dp.SetStartTimestamp(1)
		dp.SetTimestamp(pcommon.Timestamp(ts))
		b := pods[pod]
		if stale {
			// The prometheus receiver sends zeroed buckets and may leave
			// the bounds out.
			dp.SetFlags(staleFlags)
			dp.BucketCounts().FromRaw(make([]uint64, len(b)))
			return
		}
		dp.ExplicitBounds().FromRaw([]float64{1})
		var n uint64
		for _, c := range b {
			n += c
		}
		dp.SetCount(n)
		dp.SetSum(float64(n))
		dp.BucketCounts().FromRaw(b)
	})
}

func onlyHist(t *testing.T, ms []pmetric.Metric) (pmetric.HistogramDataPoint, bool) {
	t.Helper()
	if len(ms) == 0 {
		return pmetric.HistogramDataPoint{}, false
	}
	require.Len(t, ms, 1)
	require.Equal(t, 1, ms[0].Histogram().DataPoints().Len())
	return ms[0].Histogram().DataPoints().At(0), true
}

func TestStaleMarkerHistogramNotReset(t *testing.T) {
	p, clock := aggProcessor(t, nil)
	dp, _ := onlyHist(t, process(t, p, histBatch(10, map[string][]uint64{"a": {1000, 0}}, false)))
	require.EqualValues(t, 1000, dp.Count())
	*clock = clock.Add(15 * time.Second)
	_, ok := onlyHist(t, process(t, p, histBatch(20, map[string][]uint64{"a": {0, 0}}, true)))
	assert.False(t, ok)
	*clock = clock.Add(15 * time.Second)
	dp, _ = onlyHist(t, process(t, p, histBatch(30, map[string][]uint64{"a": {1005, 0}}, false)))
	assert.EqualValues(t, 1005, dp.Count())
	assert.Equal(t, []uint64{1005, 0}, dp.BucketCounts().AsRaw())
	// The stale point (no bounds) did not stay behind as its own series.
	assert.Equal(t, 1, p.acc.nLayout())
}

func TestStaleMarkerGaugeRemovesSource(t *testing.T) {
	p, clock := levelProcessor(t, "avg")
	sendLevel(t, p, "mem", false, "a", 10, 0)
	sendLevel(t, p, "mem", false, "b", 11, 100)
	*clock = clock.Add(15 * time.Second)
	md := batchOf([]string{"a"}, "mem", func(_ string, m pmetric.Metric) {
		dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(20)
		dp.SetFlags(staleFlags)
	})
	ms := process(t, p, md)
	require.Len(t, ms, 1)
	got := ms[0].Gauge().DataPoints().At(0)
	assert.Equal(t, 100.0, got.DoubleValue(), "pod a is gone and no longer drags the average")
	assert.False(t, got.Flags().NoRecordedValue())
	v, _ := sendLevel(t, p, "mem", false, "b", 30, 100)
	assert.Equal(t, 100.0, v.DoubleValue())

	// The last source going stale makes the output stale too.
	md = batchOf([]string{"b"}, "mem", func(_ string, m pmetric.Metric) {
		dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(40)
		dp.SetFlags(staleFlags)
	})
	ms = process(t, p, md)
	require.Len(t, ms, 1)
	assert.True(t, ms[0].Gauge().DataPoints().At(0).Flags().NoRecordedValue())
	assert.Zero(t, p.acc.nSeries())
	assert.Equal(t, 0, p.acc.nSources())
}

func TestStaleMarkerNonMonotonicSum(t *testing.T) {
	p, clock := levelProcessor(t, "")
	sendLevel(t, p, "inflight", true, "a", 10, 3)
	v, _ := sendLevel(t, p, "inflight", true, "b", 11, 4)
	require.Equal(t, 7.0, v.DoubleValue())
	*clock = clock.Add(15 * time.Second)
	md := batchOf([]string{"a"}, "inflight", func(_ string, m pmetric.Metric) {
		s := m.SetEmptySum()
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		dp := s.DataPoints().AppendEmpty()
		dp.SetTimestamp(20)
		dp.SetFlags(staleFlags)
	})
	ms := process(t, p, md)
	require.Len(t, ms, 1)
	assert.Equal(t, 4.0, ms[0].Sum().DataPoints().At(0).DoubleValue())
}

func TestStaleMarkerDeltaIgnored(t *testing.T) {
	p, _ := aggProcessor(t, nil)
	dp, ok := sendDeltaPoint(t, p, "a", 100, 200, 5, false)
	require.True(t, ok)
	require.EqualValues(t, 5, dp.IntValue())
	_, ok = sendDeltaPoint(t, p, "a", 200, 300, 0, true)
	assert.False(t, ok)
	dp, _ = sendDeltaPoint(t, p, "a", 300, 400, 2, false)
	assert.EqualValues(t, 2, dp.IntValue())
}

func TestStaleMarkerExponentialHistogram(t *testing.T) {
	p, clock := aggProcessor(t, nil)
	send := func(ts int64, e expSpec, stale bool) (pmetric.ExponentialHistogramDataPoint, bool) {
		md := batchOf([]string{"a"}, "requests_total", func(_ string, m pmetric.Metric) {
			dp := addExpPoint(m, pmetric.AggregationTemporalityCumulative, 1, ts, e)
			if stale {
				dp.SetFlags(staleFlags)
			}
		})
		ms := process(t, p, md)
		if len(ms) == 0 {
			return pmetric.ExponentialHistogramDataPoint{}, false
		}
		return ms[0].ExponentialHistogram().DataPoints().At(0), true
	}
	dp, _ := send(10, expSpec{scale: 0, counts: []uint64{10}}, false)
	require.EqualValues(t, 10, dp.Count())
	*clock = clock.Add(15 * time.Second)
	_, ok := send(20, expSpec{}, true)
	assert.False(t, ok)
	*clock = clock.Add(15 * time.Second)
	dp, _ = send(30, expSpec{scale: 0, counts: []uint64{12}}, false)
	assert.EqualValues(t, 12, dp.Count())
}

func TestStaleMarkerDoesNotClaimCardinalitySlot(t *testing.T) {
	cfg := baseConfig()
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "c", MaxSeries: 1}}
	p := newTestProcessor(t, cfg)
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("c")
	dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.Attributes().PutStr("id", "gone")
	dp.SetTimestamp(10)
	dp.SetFlags(staleFlags)
	process(t, p, md)
	assert.Zero(t, p.seenLen("c"))
}

// Finding 2: a series admitted in one window and overflowed in the next
// had its whole history added to the overflow total.
func TestCardinalityStickyAdmissions(t *testing.T) {
	cfg := baseConfig()
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "c", MaxSeries: 2}}
	p := newTestProcessor(t, cfg)
	clock := time.Unix(10_000, 0)
	p.now = func() time.Time { return clock }
	vals := map[string]int64{"X": 5, "B": 5, "C": 5}
	starts := map[string]int64{"X": 1, "B": 1, "C": 1}
	send := func(order ...string) (overflow int64, own map[string]int64) {
		md := overflowBatch(clock, order, vals, starts)
		return readOverflow(process(t, p, md))
	}
	var o int64
	for i := 0; i < 12; i++ {
		send("X", "B", "C")
		clock = clock.Add(5 * time.Minute)
	}
	// Window 2: new pod A shows up first in its batch.
	starts["A"] = int64(pcommon.NewTimestampFromTime(clock)) - 1
	vals["A"] = 0
	var own map[string]int64
	for i := 0; i < 12; i++ {
		vals["A"] += 100
		o, own = send("X", "A", "B", "C")
		clock = clock.Add(5 * time.Minute)
	}
	assert.NotContains(t, own, "A", "X and B keep their slots across the window boundary")
	prev := o
	// Window 3: A still overflows; the overflow grows by A's real increase.
	vals["A"]++
	o, own = send("X", "B", "A", "C")
	assert.Equal(t, int64(1), o-prev)
	assert.Contains(t, own, "X")
	assert.Contains(t, own, "B")
}

func TestCardinalityWindowFlipLimitOne(t *testing.T) {
	cfg := baseConfig()
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "c", MaxSeries: 1}}
	p := newTestProcessor(t, cfg)
	clock := time.Unix(10_000, 0)
	p.now = func() time.Time { return clock }
	vals := map[string]int64{"X": 10, "B": 50}
	starts := map[string]int64{"X": 1, "B": 1}
	send := func(order ...string) int64 {
		o, _ := readOverflow(process(t, p, overflowBatch(clock, order, vals, starts)))
		return o
	}
	send("X", "B")
	clock = clock.Add(31 * time.Minute)
	starts["A"] = int64(pcommon.NewTimestampFromTime(clock.Add(-time.Minute)))
	vals["A"], vals["X"], vals["B"] = 1, 11, 51
	send("X", "B", "A")
	clock = clock.Add(30 * time.Minute)
	vals["A"], vals["X"], vals["B"] = 2, 12, 52
	send("A", "X", "B")
	clock = clock.Add(59 * time.Minute)
	vals["A"], vals["X"], vals["B"] = 1000, 13, 53
	prev := send("A", "X", "B")
	clock = clock.Add(2 * time.Minute)
	vals["A"], vals["X"], vals["B"] = 1001, 14, 54
	o := send("X", "A", "B")
	assert.Equal(t, int64(2), o-prev, "real increase across B and A")
}

// A series released after a whole idle window that comes back into the
// overflow only sets a baseline: its history went out on its own series.
// Round 3: released series were remembered for one window only, so one
// idle for longer added its whole history to the overflow; and a series
// active in the overflow could claim a freed slot, so its history counted
// in both. Released series are now remembered for several windows, and
// overflow series stay in the overflow while active.
func TestCardinalityReturningSeriesBaselined(t *testing.T) {
	cfg := baseConfig()
	cfg.AggregationState.TTL = 3 * time.Hour // overflow state outlives the hourly sends
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "c", MaxSeries: 2}}
	p := newTestProcessor(t, cfg)
	clock := time.Unix(10_000, 0)
	p.now = func() time.Time { return clock }
	t0 := clock
	vals := map[string]int64{"Y": 1, "Z": 1, "Q": 1, "W": 1}
	starts := map[string]int64{"Y": 1, "Z": 1, "Q": 1, "W": 1}
	send := func(order ...string) (int64, map[string]int64) {
		vals["Q"]++
		return readOverflow(process(t, p, overflowBatch(clock, order, vals, starts)))
	}
	send("Y", "Z", "Q") // Q overflows, creating the overflow output
	clock = t0.Add(time.Hour)
	send("Y", "Q")
	clock = t0.Add(2 * time.Hour) // Z released
	starts["X"] = int64(pcommon.NewTimestampFromTime(clock)) - 1e9
	vals["X"] = 900
	_, own := send("Y", "Q", "X")
	require.NotContains(t, own, "Q", "Q is active in the overflow: it does not take Z's slot")
	require.Contains(t, own, "X", "X, started after the overflow output, takes it")
	clock = t0.Add(3 * time.Hour) // X idle from here, released at 4h
	send("Y", "Q")
	var prev int64
	for _, h := range []time.Duration{4 * time.Hour, 5 * time.Hour, 6 * time.Hour} {
		clock = t0.Add(h)
		prev, own = send("Y", "Q", "W")
	}
	require.Contains(t, own, "W", "W took X's slot")
	clock = clock.Add(time.Minute) // X back after three windows away
	vals["X"] = 1000
	o, own := send("Y", "Q", "W", "X")
	require.NotContains(t, own, "X")
	assert.Equal(t, prev+1, o, "only Q's +1: X's history is not added to the overflow")
	clock = clock.Add(time.Minute)
	vals["X"] = 1005
	o2, _ := send("Y", "Q", "W", "X")
	assert.Equal(t, o+1+5, o2)
}

func overflowBatch(clock time.Time, order []string, vals, starts map[string]int64) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "x")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("c")
	s := m.SetEmptySum()
	s.SetIsMonotonic(true)
	s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	for _, id := range order {
		dp := s.DataPoints().AppendEmpty()
		dp.Attributes().PutStr("id", id)
		dp.SetIntValue(vals[id])
		dp.SetStartTimestamp(pcommon.Timestamp(starts[id]))
		dp.SetTimestamp(pcommon.NewTimestampFromTime(clock))
	}
	return md
}

func readOverflow(ms []pmetric.Metric) (overflow int64, own map[string]int64) {
	own = map[string]int64{}
	for _, m := range ms {
		dps := m.Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			if _, ok := dps.At(i).Attributes().Get(overflowAttribute); ok {
				overflow = dps.At(i).IntValue()
			} else if id, ok := dps.At(i).Attributes().Get("id"); ok {
				own[id.Str()] = dps.At(i).IntValue()
			}
		}
	}
	return overflow, own
}

// Finding 3: a TTL shorter than the scrape interval forgot every source
// between scrapes and, with the warm-up restarting each time, never
// emitted. Validate rejects it, and the warm-up no longer restarts.
func TestShortTTLRejected(t *testing.T) {
	for _, tc := range []struct {
		ttl, warmup time.Duration
		ok          bool
	}{
		{10 * time.Second, 0, false},
		{59 * time.Second, 0, false},
		{time.Minute, 0, true},
		{90 * time.Second, 2 * time.Minute, false},
		{2 * time.Minute, 2 * time.Minute, true},
		{0, 10 * time.Minute, true},  // default ttl 10m
		{0, 11 * time.Minute, false}, // longer than the default ttl
	} {
		cfg := createDefaultConfig()
		cfg.AggregationState.TTL, cfg.AggregationState.Warmup = tc.ttl, tc.warmup
		err := cfg.Validate()
		assert.Equal(t, tc.ok, err == nil, "ttl=%s warmup=%s err=%v", tc.ttl, tc.warmup, err)
	}
}

func TestWarmupDoesNotRestartWhenSourcesExpireBetweenScrapes(t *testing.T) {
	p, clock := aggProcessor(t, func(c *Config) {
		c.AggregationState.TTL = time.Minute
		c.AggregationState.Warmup = time.Minute
	})
	emitted := 0
	var last int64
	for i := 0; i < 20; i++ { // scrapes every 90s, longer than the TTL
		ts := int64(i+1) * 90e9
		if dp, ok := sendCounter(t, p, podPoint{"a", 1, ts, int64(i * 10)}, podPoint{"b", 1, ts, int64(i * 10)}); ok {
			emitted++
			assert.GreaterOrEqual(t, dp.IntValue(), last)
			last = dp.IntValue()
		}
		*clock = clock.Add(90 * time.Second)
	}
	assert.GreaterOrEqual(t, emitted, 18)
}

// Finding 4: delta points of one output arriving in separate batches must
// produce contiguous windows that move forward.
func sendDeltaPoint(t *testing.T, p *policyProcessor, pod string, start, ts, v int64, stale bool) (pmetric.NumberDataPoint, bool) {
	t.Helper()
	md := batchOf([]string{pod}, "requests_total", func(_ string, m pmetric.Metric) {
		s := m.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
		dp := s.DataPoints().AppendEmpty()
		dp.SetIntValue(v)
		dp.SetStartTimestamp(pcommon.Timestamp(start))
		dp.SetTimestamp(pcommon.Timestamp(ts))
		if stale {
			dp.SetFlags(staleFlags)
		}
	})
	ms := process(t, p, md)
	if len(ms) == 0 {
		return pmetric.NumberDataPoint{}, false
	}
	return ms[0].Sum().DataPoints().At(0), true
}

func TestDeltaAcrossBatchesNonOverlapping(t *testing.T) {
	p, clock := aggProcessor(t, nil)
	type in struct {
		pod       string
		start, ts int64
		v         int64
	}
	var total int64
	var prevEnd pcommon.Timestamp
	for i, q := range []in{{"a", 100, 200, 5}, {"b", 100, 200, 7}, {"c", 90, 190, 3}, {"a", 200, 300, 1}, {"b", 200, 290, 2}} {
		dp, ok := sendDeltaPoint(t, p, q.pod, q.start, q.ts, q.v, false)
		require.True(t, ok)
		total += dp.IntValue()
		require.Less(t, dp.StartTimestamp(), dp.Timestamp())
		if i > 0 {
			assert.Equal(t, prevEnd, dp.StartTimestamp(), "window %d starts where the last ended", i)
		} else {
			assert.Equal(t, pcommon.Timestamp(100), dp.StartTimestamp())
		}
		prevEnd = dp.Timestamp()
		*clock = clock.Add(time.Second)
	}
	assert.Equal(t, int64(18), total)
	// State is bounded by the TTL.
	*clock = clock.Add(11 * time.Minute)
	p.acc.sweepAll(*clock)
	assert.Zero(t, p.acc.nDeltas())
	assert.Equal(t, 0, p.acc.nSources())
}

func TestDeltaOutputsRespectMaxSources(t *testing.T) {
	p, _ := aggProcessor(t, func(c *Config) { c.AggregationState.MaxSources = 1 })
	md := pmetric.NewMetrics()
	for i := 0; i < 2; i++ {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("k8s.pod.name", "a")
		m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("requests_total")
		s := m.SetEmptySum()
		s.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
		dp := s.DataPoints().AppendEmpty()
		dp.Attributes().PutStr("route", fmt.Sprint(i))
		dp.SetIntValue(1)
		dp.SetTimestamp(10)
	}
	ms := process(t, p, md)
	require.Len(t, ms, 1)
	assert.Equal(t, 1, ms[0].Sum().DataPoints().Len(), "the second delta output is refused")
}

// Finding 5: overflow state of two scopes with the same metric name must
// not be shared.
func TestOverflowSeparatePerScope(t *testing.T) {
	cfg := baseConfig()
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "c", MaxSeries: 1}}
	p := newTestProcessor(t, cfg)
	send := func(scope string, ts int64, id string, v int64) int64 {
		md := pmetric.NewMetrics()
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("service.name", "x")
		sm := rm.ScopeMetrics().AppendEmpty()
		sm.Scope().SetName(scope)
		m := sm.Metrics().AppendEmpty()
		m.SetName("c")
		s := m.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		dp := s.DataPoints().AppendEmpty()
		dp.Attributes().PutStr("id", id)
		dp.SetIntValue(v)
		dp.SetStartTimestamp(1)
		dp.SetTimestamp(pcommon.Timestamp(ts))
		o, _ := readOverflow(process(t, p, md))
		return o
	}
	send("s1", 10, "admitted", 1)
	assert.Equal(t, int64(100), send("s1", 20, "x1", 100))
	assert.Equal(t, int64(7), send("s2", 21, "y1", 7))
}

// Finding 6: gauge sources stop counting after gauge_staleness, not ttl.
func TestGaugeStaleness(t *testing.T) {
	p, clock := levelProcessor(t, "avg")
	sendLevel(t, p, "mem", false, "a", 10, 100)
	sendLevel(t, p, "mem", false, "b", 11, 0)
	*clock = clock.Add(4 * time.Minute)
	v, _ := sendLevel(t, p, "mem", false, "b", 20, 0)
	assert.Equal(t, 50.0, v.DoubleValue(), "a still counts within 5m")
	*clock = clock.Add(time.Minute)
	v, _ = sendLevel(t, p, "mem", false, "b", 30, 0)
	assert.Equal(t, 0.0, v.DoubleValue(), "a stopped counting after 5m")

	cfg := baseConfig()
	cfg.AggregationState.GaugeStaleness = 2 * time.Minute
	cfg.Aggregations = []AggregationRule{{Metric: "mem", DropAttributes: []string{"k8s.pod.name"}}}
	q := newTestProcessor(t, cfg)
	c2 := time.Unix(1000, 0)
	q.now = func() time.Time { return c2 }
	sendLevel(t, q, "mem", false, "a", 10, 100)
	c2 = c2.Add(2 * time.Minute)
	v, _ = sendLevel(t, q, "mem", false, "b", 20, 0)
	assert.Equal(t, 0.0, v.DoubleValue())

	bad := createDefaultConfig()
	bad.AggregationState.GaugeStaleness = 30 * time.Second
	assert.Error(t, bad.Validate())
	bad.AggregationState.GaugeStaleness = -time.Second
	assert.Error(t, bad.Validate())
}

// Finding 7: exponential histograms merge; summaries are counted.
type expSpec struct {
	scale     int32
	offset    int32
	counts    []uint64
	zeroCount uint64
	zeroThr   float64
}

func addExpPoint(m pmetric.Metric, temp pmetric.AggregationTemporality, start, ts int64, e expSpec) pmetric.ExponentialHistogramDataPoint {
	var h pmetric.ExponentialHistogram
	if m.Type() == pmetric.MetricTypeExponentialHistogram {
		h = m.ExponentialHistogram()
	} else {
		h = m.SetEmptyExponentialHistogram()
		h.SetAggregationTemporality(temp)
	}
	dp := h.DataPoints().AppendEmpty()
	dp.SetStartTimestamp(pcommon.Timestamp(start))
	dp.SetTimestamp(pcommon.Timestamp(ts))
	dp.SetScale(e.scale)
	dp.SetZeroCount(e.zeroCount)
	dp.SetZeroThreshold(e.zeroThr)
	dp.Positive().SetOffset(e.offset)
	dp.Positive().BucketCounts().FromRaw(e.counts)
	n := e.zeroCount
	for _, c := range e.counts {
		n += c
	}
	dp.SetCount(n)
	dp.SetSum(float64(n))
	return dp
}

func TestExponentialHistogramCumulativeMerge(t *testing.T) {
	p, clock := aggProcessor(t, nil)
	send := func(ts int64, specs map[string]expSpec) pmetric.ExponentialHistogramDataPoint {
		pods := []string{}
		for pod := range specs {
			pods = append(pods, pod)
		}
		md := batchOf(pods, "requests_total", func(pod string, m pmetric.Metric) {
			addExpPoint(m, pmetric.AggregationTemporalityCumulative, 1, ts, specs[pod])
		})
		ms := process(t, p, md)
		require.Len(t, ms, 1)
		require.Equal(t, 1, ms[0].ExponentialHistogram().DataPoints().Len())
		return ms[0].ExponentialHistogram().DataPoints().At(0)
	}
	// a at scale 1 (buckets 0,1), b at scale 0 (bucket 0): merged at scale 0.
	dp := send(10, map[string]expSpec{"a": {scale: 1, counts: []uint64{1, 2}}, "b": {scale: 0, counts: []uint64{3}}})
	assert.Equal(t, int32(0), dp.Scale())
	assert.EqualValues(t, 6, dp.Count())
	assert.Equal(t, []uint64{6}, dp.Positive().BucketCounts().AsRaw())
	*clock = clock.Add(15 * time.Second)
	dp = send(20, map[string]expSpec{"a": {scale: 1, counts: []uint64{2, 2}}, "b": {scale: 0, counts: []uint64{4}}})
	assert.EqualValues(t, 8, dp.Count())
	// a restarts (counts drop): the delta is its new value.
	*clock = clock.Add(15 * time.Second)
	dp = send(30, map[string]expSpec{"a": {scale: 1, counts: []uint64{1, 0}}, "b": {scale: 0, counts: []uint64{4}}})
	assert.EqualValues(t, 9, dp.Count())
	assert.Equal(t, []uint64{9}, dp.Positive().BucketCounts().AsRaw())
	assert.False(t, dp.HasMin())
	// a downscales on its own (as SDKs do): not a reset.
	*clock = clock.Add(15 * time.Second)
	dp = send(40, map[string]expSpec{"a": {scale: 0, counts: []uint64{3}}, "b": {scale: 0, counts: []uint64{4}}})
	assert.EqualValues(t, 11, dp.Count())
	// Widening the zero bucket of a moves its low buckets into the zero
	// count, so the merged output widens too.
	*clock = clock.Add(15 * time.Second)
	dp = send(50, map[string]expSpec{"a": {scale: 0, counts: []uint64{0, 3}, zeroCount: 3, zeroThr: 2}, "b": {scale: 0, counts: []uint64{4}}})
	assert.EqualValues(t, 14, dp.Count())
	assert.Equal(t, 2.0, dp.ZeroThreshold())
	assert.EqualValues(t, 14, dp.ZeroCount()+sumU(dp.Positive().BucketCounts().AsRaw()))
}

func sumU(xs []uint64) uint64 {
	var n uint64
	for _, x := range xs {
		n += x
	}
	return n
}

func TestExponentialHistogramDeltaMerge(t *testing.T) {
	p, _ := aggProcessor(t, nil)
	md := batchOf([]string{"a", "b"}, "requests_total", func(pod string, m pmetric.Metric) {
		if pod == "a" {
			addExpPoint(m, pmetric.AggregationTemporalityDelta, 100, 200, expSpec{scale: 2, offset: 4, counts: []uint64{1, 1, 1, 1}})
		} else {
			addExpPoint(m, pmetric.AggregationTemporalityDelta, 100, 200, expSpec{scale: 1, offset: -2, counts: []uint64{5}})
		}
	})
	ms := process(t, p, md)
	require.Len(t, ms, 1)
	dp := ms[0].ExponentialHistogram().DataPoints().At(0)
	assert.Equal(t, int32(1), dp.Scale())
	assert.EqualValues(t, 9, dp.Count())
	// a: indexes 4..7 at scale 2 -> 2,2,3,3 at scale 1; b: -2.
	assert.Equal(t, int32(-2), dp.Positive().Offset())
	assert.Equal(t, []uint64{5, 0, 0, 0, 2, 2}, dp.Positive().BucketCounts().AsRaw())
}

func TestExponentialHistogramFitDownscales(t *testing.T) {
	a := &expPoint{scale: 4, pos: expBuckets{offset: 0, counts: []uint64{1}}, count: 1}
	b := &expPoint{scale: 4, pos: expBuckets{offset: 1000, counts: []uint64{1}}, count: 1}
	m := addExp(a, b)
	assert.LessOrEqual(t, m.pos.span(), maxExpBuckets)
	assert.Less(t, m.scale, int32(4))
	assert.Equal(t, uint64(2), sumU(m.pos.counts))
}

func newTelemetryProcessor(t *testing.T, cfg *Config) (*policyProcessor, *componenttest.Telemetry) {
	t.Helper()
	require.NoError(t, cfg.Validate())
	tel := componenttest.NewTelemetry()
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	set := processortest.NewNopSettings(componentType)
	set.TelemetrySettings = tel.NewTelemetrySettings()
	p, err := newPolicyProcessor(set, cfg)
	require.NoError(t, err)
	return p, tel
}

func counterValue(t *testing.T, tel *componenttest.Telemetry, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	m, err := tel.GetMetric(name)
	require.NoError(t, err)
	want := attribute.NewSet(append(attrs, attribute.String("policy_version", ""))...)
	for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	return 0
}

func summaryBatch(ids ...string) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("k8s.pod.name", "a")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("rpc_latency")
	dps := m.SetEmptySummary().DataPoints()
	for _, id := range ids {
		dp := dps.AppendEmpty()
		dp.Attributes().PutStr("id", id)
		dp.SetCount(1)
		dp.SetTimestamp(10)
	}
	return md
}

func TestSummariesUnderAggregationCounted(t *testing.T) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "rpc_latency", DropAttributes: []string{"k8s.pod.name"}}}
	p, tel := newTelemetryProcessor(t, cfg)
	md := pmetric.NewMetrics()
	for _, pod := range []string{"a", "b"} {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("k8s.pod.name", pod)
		m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("rpc_latency")
		m.SetEmptySummary().DataPoints().AppendEmpty().SetCount(1)
	}
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	assert.Equal(t, 2, out.DataPointCount(), "summaries pass through unaggregated")
	_, ok := out.ResourceMetrics().At(0).Resource().Attributes().Get("k8s.pod.name")
	assert.True(t, ok)
	assert.Equal(t, int64(2), counterValue(t, tel, "xscaler_policy_unaggregated_data_points", attribute.String("reason", "summary")))
}

// removeOverLimit: summaries past the limit are dropped and counted;
// exponential histograms past the limit fold into the overflow series.
func TestCardinalityOverLimitSummaryAndExponential(t *testing.T) {
	cfg := baseConfig()
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "*", MaxSeries: 2}}
	p, tel := newTelemetryProcessor(t, cfg)

	out, err := p.processMetrics(context.Background(), summaryBatch("1", "2", "3", "4", "5"))
	require.NoError(t, err)
	require.Equal(t, 2, out.DataPointCount())
	assert.Equal(t, int64(3), counterValue(t, tel, "xscaler_policy_overflow_data_points"))

	// Every point over the limit: the metric is pruned entirely.
	out, err = p.processMetrics(context.Background(), summaryBatch("6", "7"))
	require.NoError(t, err)
	assert.Equal(t, 0, out.ResourceMetrics().Len())
	assert.Equal(t, int64(5), counterValue(t, tel, "xscaler_policy_overflow_data_points"))

	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("lat")
	for i := 0; i < 5; i++ {
		dp := addExpPoint(m, pmetric.AggregationTemporalityCumulative, 1, 10, expSpec{counts: []uint64{uint64(i + 1)}})
		dp.Attributes().PutStr("id", fmt.Sprint(i))
	}
	out, err = p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	dps := collectMetrics(out)["lat"][0].ExponentialHistogram().DataPoints()
	require.Equal(t, 3, dps.Len(), "two admitted plus one overflow series")
	ov := dps.At(2)
	_, isOverflow := ov.Attributes().Get(overflowAttribute)
	require.True(t, isOverflow)
	assert.EqualValues(t, 3+4+5, ov.Count())
	assert.Equal(t, int64(8), counterValue(t, tel, "xscaler_policy_overflow_data_points"))
}

func TestLayoutsBoundedByMaxSources(t *testing.T) {
	p, clock := aggProcessor(t, func(c *Config) { c.AggregationState.MaxSources = 2 })
	send := func(prefix string) {
		md := pmetric.NewMetrics()
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("k8s.pod.name", "a")
		m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("requests_total")
		h := m.SetEmptyHistogram()
		h.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		for i := 0; i < 5; i++ {
			dp := h.DataPoints().AppendEmpty()
			dp.Attributes().PutStr("route", fmt.Sprint(prefix, i))
			dp.SetTimestamp(10)
			dp.ExplicitBounds().FromRaw([]float64{1})
			dp.BucketCounts().FromRaw([]uint64{1, 0})
			dp.SetCount(1)
		}
		process(t, p, md)
	}
	send("a")
	assert.LessOrEqual(t, p.acc.nLayout(), 2)
	*clock = clock.Add(time.Second)
	send("b") // every source refused: no new layouts
	assert.LessOrEqual(t, p.acc.nLayout(), 2)
	assert.Equal(t, 2, p.acc.nSources())
}

// Coverage: tombstones and born records expire in sweep.
func TestSweepExpiresTombstones(t *testing.T) {
	p, clock := aggProcessor(t, nil)
	sendCounter(t, p, podPoint{"a", 1, 10, 5})
	*clock = clock.Add(11 * time.Minute)
	sendCounter(t, p, podPoint{"b", 1, 20, 1}) // sweeps a away
	require.Equal(t, 1, p.acc.nTomb())
	*clock = clock.Add(11 * time.Minute)
	p.acc.sweepAll(*clock) // b expires, its output is removed
	require.NotZero(t, p.acc.nBornRecs())
	*clock = clock.Add(tombstoneTTLs * 10 * time.Minute)
	p.acc.sweepAll(*clock)
	assert.Zero(t, p.acc.nTomb())
	assert.Zero(t, p.acc.nBornRecs())
}

// Coverage: a histogram whose count stays the same while one bucket drops
// was reset.
func TestHistogramResetBucketDropSameCount(t *testing.T) {
	p, clock := aggProcessor(t, nil)
	dp, _ := onlyHist(t, process(t, p, histBatch(10, map[string][]uint64{"a": {5, 5}}, false)))
	require.EqualValues(t, 10, dp.Count())
	*clock = clock.Add(15 * time.Second)
	dp, _ = onlyHist(t, process(t, p, histBatch(20, map[string][]uint64{"a": {6, 4}}, false)))
	assert.EqualValues(t, 20, dp.Count(), "reset: the new value counts in full")
	assert.Equal(t, []uint64{11, 9}, dp.BucketCounts().AsRaw())
}

// Coverage: traces get service identity.
func TestProcessTracesServiceIdentity(t *testing.T) {
	cfg := baseConfig()
	cfg.ServiceIdentity.Enabled = true
	p := newTestProcessor(t, cfg)
	td := ptrace.NewTraces()
	attrs := td.ResourceSpans().AppendEmpty().Resource().Attributes()
	attrs.PutStr("k8s.deployment.name", "checkout")
	attrs.PutStr("k8s.namespace.name", "shop")
	out, err := p.processTraces(context.Background(), td)
	require.NoError(t, err)
	got := out.ResourceSpans().At(0).Resource().Attributes()
	name, _ := got.Get("service.name")
	ns, _ := got.Get("service.namespace")
	assert.Equal(t, "checkout", name.Str())
	assert.Equal(t, "shop", ns.Str())
}

// Round 3: an output removed after all its sources went silent for longer
// than the TTL was recreated as a new series, and the first pod back (pods
// arrive in separate batches) was emitted alone under a new start time:
// downstream saw a reset, then a phantom increase of one pod's history.
// The output must resume its series: same start time, never decreasing,
// returning pods adding only their increase.
func TestOutputResumesAfterAllSourcesExpire(t *testing.T) {
	for _, tc := range []struct {
		name   string
		aStart int64 // A's start time after the gap
		aValue int64 // A's value after the gap
		want   []int64
	}{
		{"same start", 1, 1080, []int64{2150, 2160}},
		{"real reset", 2, 5, []int64{2145, 2155}}, // A restarted: its 5 is new
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, clock := aggProcessor(t, func(c *Config) {
				c.AggregationState.TTL = time.Minute
				c.AggregationState.Warmup = time.Minute
			})
			var vals []int64
			var starts []pcommon.Timestamp
			send := func(pod string, start, v int64) {
				ts := clock.UnixNano()
				if dp, ok := sendCounter(t, p, podPoint{pod, start, ts, v}); ok {
					vals = append(vals, dp.IntValue())
					starts = append(starts, dp.StartTimestamp())
				}
				*clock = clock.Add(time.Second)
			}
			for i := int64(0); i < 8; i++ { // one pod per batch, every 15s
				send("a", 1, 1000+10*i)
				send("b", 1, 1000+10*i)
				*clock = clock.Add(13 * time.Second)
			}
			require.Equal(t, int64(2140), vals[len(vals)-1])
			n := len(vals)

			*clock = clock.Add(3 * time.Minute) // both pods silent > ttl
			send("a", tc.aStart, tc.aValue)
			send("b", 1, 1080)
			assert.Equal(t, tc.want, vals[n:])
			for i := 1; i < len(vals); i++ {
				assert.GreaterOrEqual(t, vals[i], vals[i-1], "never decreases")
				assert.Equal(t, starts[0], starts[i], "the same series")
			}
		})
	}
}

// Round 3: one source with a clock running ahead ratcheted the output's
// last timestamp forward and pinned the output in the future, beyond what
// Mimir accepts. Output timestamps are capped at now + futureGrace.
func TestFutureSkewedSourceDoesNotPinOutput(t *testing.T) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "requests_total", DropAttributes: []string{"k8s.pod.name"}}}
	p, tel := newTelemetryProcessor(t, cfg)
	clock := time.Unix(10_000, 0)
	p.now = func() time.Time { return clock }
	at := func(d time.Duration) int64 { return clock.Add(d).UnixNano() }
	limit := func() pcommon.Timestamp { return pcommon.NewTimestampFromTime(clock.Add(futureGrace)) }

	// Cumulative: b's clock is an hour ahead.
	dp, ok := sendCounter(t, p, podPoint{"a", 1, at(0), 1}, podPoint{"b", 1, at(time.Hour), 1})
	require.True(t, ok)
	assert.Equal(t, limit(), dp.Timestamp())
	for i := 0; i < 8; i++ { // b goes on skewed; a reports on time
		clock = clock.Add(15 * time.Second)
		sendCounter(t, p, podPoint{"b", 1, at(time.Hour), int64(i + 2)})
		dp, ok = sendCounter(t, p, podPoint{"a", 1, at(0), int64(i + 2)})
		require.True(t, ok)
		// The skewed batch took the limit; this one moves 1ms past it.
		assert.LessOrEqual(t, dp.Timestamp(), limit()+pcommon.Timestamp(time.Millisecond))
	}

	// Delta: one skewed window does not push later windows into the future.
	d, ok := sendDeltaPoint(t, p, "b", at(time.Hour), at(time.Hour+time.Second), 3, false)
	require.True(t, ok)
	assert.Equal(t, limit(), d.Timestamp())
	assert.Less(t, d.StartTimestamp(), d.Timestamp())
	clock = clock.Add(2 * time.Minute)
	d, ok = sendDeltaPoint(t, p, "a", at(-time.Second), at(0), 2, false)
	require.True(t, ok)
	assert.Equal(t, pcommon.Timestamp(at(0)), d.Timestamp(), "back on the collector's clock")

	assert.Equal(t, int64(10), counterValue(t, tel, "xscaler_policy_future_timestamps_clamped"))
}
