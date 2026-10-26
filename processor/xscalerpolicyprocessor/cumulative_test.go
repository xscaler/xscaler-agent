package xscalerpolicyprocessor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

type podPoint struct {
	pod   string
	start int64
	ts    int64
	v     int64
}

// aggProcessor merges requests_total across pods with a controllable clock.
func aggProcessor(t *testing.T, mutate func(*Config)) (*policyProcessor, *time.Time) {
	t.Helper()
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "requests_total", DropAttributes: []string{"k8s.pod.name"}}}
	if mutate != nil {
		mutate(cfg)
	}
	p := newTestProcessor(t, cfg)
	clock := time.Unix(1_000, 0)
	p.now = func() time.Time { return clock }
	return p, &clock
}

// sendCounter sends one batch of cumulative monotonic points and returns the
// merged point, if any.
func sendCounter(t *testing.T, p *policyProcessor, pts ...podPoint) (pmetric.NumberDataPoint, bool) {
	t.Helper()
	md := pmetric.NewMetrics()
	for _, pp := range pts {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("service.name", "payments")
		rm.Resource().Attributes().PutStr("k8s.pod.name", pp.pod)
		m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("requests_total")
		s := m.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		dp := s.DataPoints().AppendEmpty()
		dp.SetIntValue(pp.v)
		dp.SetStartTimestamp(pcommon.Timestamp(pp.start))
		dp.SetTimestamp(pcommon.Timestamp(pp.ts))
	}
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	got := collectMetrics(out)["requests_total"]
	if len(got) == 0 {
		return pmetric.NumberDataPoint{}, false
	}
	require.Len(t, got, 1)
	require.Equal(t, 1, got[0].Sum().DataPoints().Len())
	return got[0].Sum().DataPoints().At(0), true
}

func TestCumulativeMergeSurvivesPodRestart(t *testing.T) {
	p, _ := aggProcessor(t, nil)
	steps := []struct {
		pts  []podPoint
		want int64
	}{
		{[]podPoint{{"p1", 10, 100, 100}, {"p2", 20, 100, 50}}, 150},
		{[]podPoint{{"p1", 10, 200, 130}, {"p2", 20, 200, 80}}, 210},
		// p2 restarts: new start time, counter back near zero.
		{[]podPoint{{"p1", 10, 300, 160}, {"p2", 250, 300, 5}}, 245},
		{[]podPoint{{"p1", 10, 400, 190}, {"p2", 250, 400, 25}}, 295},
		// p1 resets without a start-time change (value drop only).
		{[]podPoint{{"p1", 10, 500, 3}, {"p2", 250, 500, 40}}, 313},
	}
	var prev int64
	var start pcommon.Timestamp
	for i, step := range steps {
		dp, ok := sendCounter(t, p, step.pts...)
		require.True(t, ok, "step %d", i)
		assert.Equal(t, step.want, dp.IntValue(), "step %d", i)
		assert.GreaterOrEqual(t, dp.IntValue(), prev, "merged counter must never decrease (step %d)", i)
		prev = dp.IntValue()
		if i == 0 {
			start = dp.StartTimestamp()
			assert.Equal(t, pcommon.Timestamp(99), start, "starts at creation, kept just before its first point")
		}
		assert.Equal(t, start, dp.StartTimestamp(), "start time stays stable across source resets (step %d)", i)
	}
}

func TestCumulativeMergeLateJoiners(t *testing.T) {
	p, clock := aggProcessor(t, nil)
	dp, _ := sendCounter(t, p, podPoint{"p1", 10, 100, 100})
	require.Equal(t, int64(100), dp.IntValue())

	// A pod started after the merged series: all of its count happened
	// inside the series' lifetime.
	dp, _ = sendCounter(t, p, podPoint{"p1", 10, 200, 110}, podPoint{"p2", 150, 200, 7})
	assert.Equal(t, int64(117), dp.IntValue())

	// After the warm-up, a pod that started before the series (this
	// collector just began seeing it) only sets a baseline; its history
	// must not land as a spike.
	*clock = clock.Add(3 * time.Minute)
	dp, _ = sendCounter(t, p, podPoint{"p1", 10, 300, 120}, podPoint{"p2", 150, 300, 9}, podPoint{"old", 5, 300, 1_000_000})
	assert.Equal(t, int64(129), dp.IntValue())
	dp, _ = sendCounter(t, p, podPoint{"p1", 10, 400, 120}, podPoint{"p2", 150, 400, 9}, podPoint{"old", 5, 400, 1_000_004})
	assert.Equal(t, int64(133), dp.IntValue(), "after the baseline it counts normally")
}

func TestCumulativeMergeIgnoresDuplicatesAndOutOfOrder(t *testing.T) {
	p, _ := aggProcessor(t, nil)
	dp, _ := sendCounter(t, p, podPoint{"p1", 10, 100, 100}, podPoint{"p1", 10, 200, 120})
	assert.Equal(t, int64(120), dp.IntValue(), "both points of one source in a batch count once, in order")

	// Resent and older points add nothing.
	_, ok := sendCounter(t, p, podPoint{"p1", 10, 200, 120}, podPoint{"p1", 10, 150, 110})
	assert.False(t, ok, "nothing new to report, so no point is emitted")

	dp, _ = sendCounter(t, p, podPoint{"p1", 10, 300, 125})
	assert.Equal(t, int64(125), dp.IntValue())
}

func TestCumulativeMergeSourceExpiry(t *testing.T) {
	p, clock := aggProcessor(t, func(c *Config) { c.AggregationState.TTL = 10 * time.Minute })
	sendCounter(t, p, podPoint{"p1", 10, 100, 100}, podPoint{"p2", 10, 100, 50})
	require.Equal(t, 2, p.acc.nSources())

	// p2 goes away; p1 keeps reporting past the TTL.
	*clock = clock.Add(6 * time.Minute)
	sendCounter(t, p, podPoint{"p1", 10, 200, 110})
	*clock = clock.Add(6 * time.Minute)
	dp, _ := sendCounter(t, p, podPoint{"p1", 10, 300, 120})
	assert.Equal(t, 1, p.acc.nSources(), "p2 is forgotten after the TTL")
	assert.Equal(t, int64(170), dp.IntValue(), "its contribution stays in the total")

	// p2 comes back with its old start time: only its increase since its
	// last counted value (50 -> 60) is added, no spike.
	dp, _ = sendCounter(t, p, podPoint{"p1", 10, 400, 130}, podPoint{"p2", 10, 400, 60})
	assert.Equal(t, int64(190), dp.IntValue())

	// p2 goes away again and comes back restarted (new start time): all of
	// its value is new.
	*clock = clock.Add(6 * time.Minute)
	sendCounter(t, p, podPoint{"p1", 10, 500, 140})
	*clock = clock.Add(6 * time.Minute)
	sendCounter(t, p, podPoint{"p1", 10, 600, 140})
	require.Equal(t, 1, p.acc.nSources(), "p2 forgotten again")
	dp, _ = sendCounter(t, p, podPoint{"p1", 10, 700, 140}, podPoint{"p2", 650, 700, 3})
	assert.Equal(t, int64(203), dp.IntValue(), "200 + p2's 3 since its restart")
	first := dp.StartTimestamp()

	// Once every source has expired the output is parked: a source coming
	// back within 6 TTLs resumes the same series and total.
	*clock = clock.Add(11 * time.Minute)
	dp, _ = sendCounter(t, p, podPoint{"p3", 900, 1000, 4})
	assert.Equal(t, int64(207), dp.IntValue(), "p3 started after the output: counted in full")
	assert.Equal(t, first, dp.StartTimestamp(), "the same series")
	assert.Equal(t, 1, p.acc.nSeries())

	// Parked longer than that, the next point starts a new series with a
	// new start time.
	*clock = clock.Add(71 * time.Minute)
	dp, _ = sendCounter(t, p, podPoint{"p4", 1900, 2000, 4})
	assert.Equal(t, int64(4), dp.IntValue())
	assert.Equal(t, pcommon.Timestamp(1999), dp.StartTimestamp(), "a new series says so with a new start time")
	assert.Equal(t, 1, p.acc.nSeries())
}

func TestCumulativeMergeMaxSources(t *testing.T) {
	p, _ := aggProcessor(t, func(c *Config) { c.AggregationState.MaxSources = 2 })
	dp, _ := sendCounter(t, p, podPoint{"p1", 10, 100, 1}, podPoint{"p2", 10, 100, 2}, podPoint{"p3", 10, 100, 4})
	assert.Equal(t, int64(3), dp.IntValue(), "the third source is refused, not merged without state")
	assert.Equal(t, 2, p.acc.nSources())
}

func TestCumulativeMergeHistogramBucketReset(t *testing.T) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "latency", DropAttributes: []string{"k8s.pod.name"}}}
	p := newTestProcessor(t, cfg)

	send := func(pts ...struct {
		pod     string
		start   int64
		ts      int64
		buckets []uint64
		sum     float64
	}) pmetric.HistogramDataPoint {
		md := pmetric.NewMetrics()
		for _, pp := range pts {
			rm := md.ResourceMetrics().AppendEmpty()
			rm.Resource().Attributes().PutStr("k8s.pod.name", pp.pod)
			m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
			m.SetName("latency")
			h := m.SetEmptyHistogram()
			h.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
			dp := h.DataPoints().AppendEmpty()
			dp.ExplicitBounds().FromRaw([]float64{1, 5})
			dp.BucketCounts().FromRaw(pp.buckets)
			var c uint64
			for _, b := range pp.buckets {
				c += b
			}
			dp.SetCount(c)
			dp.SetSum(pp.sum)
			dp.SetMin(0.1)
			dp.SetStartTimestamp(pcommon.Timestamp(pp.start))
			dp.SetTimestamp(pcommon.Timestamp(pp.ts))
		}
		out, err := p.processMetrics(context.Background(), md)
		require.NoError(t, err)
		dps := collectMetrics(out)["latency"][0].Histogram().DataPoints()
		require.Equal(t, 1, dps.Len())
		return dps.At(0)
	}
	type hp = struct {
		pod     string
		start   int64
		ts      int64
		buckets []uint64
		sum     float64
	}

	dp := send(hp{"a", 10, 100, []uint64{1, 2, 3}, 20}, hp{"b", 10, 100, []uint64{4, 0, 1}, 8})
	assert.Equal(t, []uint64{5, 2, 4}, dp.BucketCounts().AsRaw())

	dp = send(hp{"a", 10, 200, []uint64{2, 3, 3}, 25}, hp{"b", 10, 200, []uint64{4, 1, 1}, 9})
	assert.Equal(t, []uint64{6, 4, 4}, dp.BucketCounts().AsRaw())

	// b restarts: its buckets drop below their last values.
	dp = send(hp{"a", 10, 300, []uint64{2, 4, 3}, 27}, hp{"b", 10, 300, []uint64{1, 0, 0}, 0.5})
	assert.Equal(t, []uint64{7, 5, 4}, dp.BucketCounts().AsRaw(), "b's reset adds its new counts, nothing goes backwards")
	assert.Equal(t, uint64(16), dp.Count())
	assert.True(t, dp.HasSum())
	assert.InDelta(t, 36.5, dp.Sum(), 1e-9, "28, +6 (a +5, b +1), +2.5 (a +2, b reset to 0.5)")
	assert.False(t, dp.HasMin(), "min and max cannot be kept across cumulative merges")
	assert.Equal(t, pcommon.Timestamp(99), dp.StartTimestamp())
}

func TestDeltaMergeAddsEveryPoint(t *testing.T) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "req", DropAttributes: []string{"pod"}}}
	p := newTestProcessor(t, cfg)
	md := pmetric.NewMetrics()
	s := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	s.SetName("req")
	sum := s.SetEmptySum()
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	sum.SetIsMonotonic(true)
	for i, pod := range []string{"a", "a", "b"} {
		dp := sum.DataPoints().AppendEmpty()
		dp.Attributes().PutStr("pod", pod)
		dp.SetIntValue(int64(i + 1))
		dp.SetTimestamp(pcommon.Timestamp(10 + i))
	}
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	dps := collectMetrics(out)["req"][0].Sum().DataPoints()
	require.Equal(t, 1, dps.Len())
	assert.Equal(t, int64(6), dps.At(0).IntValue(), "two delta points of one series both count")
	assert.Zero(t, p.acc.nSeries(), "delta merges keep no state")
}

func TestNonMonotonicCumulativeUsesLatest(t *testing.T) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "inflight", DropAttributes: []string{"pod"}}}
	p := newTestProcessor(t, cfg)
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("inflight")
	sum := m.SetEmptySum()
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	for i, pt := range []struct {
		pod string
		v   int64
	}{{"a", 5}, {"a", 3}, {"b", 2}} {
		dp := sum.DataPoints().AppendEmpty()
		dp.Attributes().PutStr("pod", pt.pod)
		dp.SetIntValue(pt.v)
		dp.SetTimestamp(pcommon.Timestamp(10 + i))
	}
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	assert.Equal(t, int64(5), collectMetrics(out)["inflight"][0].Sum().DataPoints().At(0).IntValue(), "latest a (3) + b (2): levels, not totals")
}

func TestCardinalityOverflowCumulativeStaysMonotonic(t *testing.T) {
	cfg := baseConfig()
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "hits_total", MaxSeries: 1}}
	p := newTestProcessor(t, cfg)
	ts := int64(100)
	send := func(vals map[string][2]int64) int64 { // user -> {start, value}
		ts += 100
		md := pmetric.NewMetrics()
		m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("hits_total")
		s := m.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		for _, u := range []string{"u1", "u2", "u3"} {
			v, ok := vals[u]
			if !ok {
				continue
			}
			dp := s.DataPoints().AppendEmpty()
			dp.Attributes().PutStr("user", u)
			dp.SetStartTimestamp(pcommon.Timestamp(v[0]))
			dp.SetIntValue(v[1])
			dp.SetTimestamp(pcommon.Timestamp(ts))
		}
		out, err := p.processMetrics(context.Background(), md)
		require.NoError(t, err)
		dps := collectMetrics(out)["hits_total"][0].Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			if _, ok := dps.At(i).Attributes().Get(overflowAttribute); ok {
				return dps.At(i).IntValue()
			}
		}
		t.Fatal("no overflow series")
		return 0
	}
	assert.Equal(t, int64(30), send(map[string][2]int64{"u1": {1, 5}, "u2": {1, 10}, "u3": {1, 20}}))
	// u3 restarts inside the overflow series.
	assert.Equal(t, int64(33), send(map[string][2]int64{"u1": {1, 6}, "u2": {1, 12}, "u3": {50, 1}}))
}

func TestCumulativeMergeTimestampsMoveForward(t *testing.T) {
	p, _ := aggProcessor(t, nil)
	dp, _ := sendCounter(t, p, podPoint{"p1", 10, 1_000_000_000, 1}, podPoint{"p2", 10, 2_000_000_000, 1})
	first := dp.Timestamp()
	// p1's clock lags: its next point is older than what was emitted.
	dp, _ = sendCounter(t, p, podPoint{"p1", 10, 1_500_000_000, 2})
	assert.Equal(t, int64(3), dp.IntValue())
	assert.Greater(t, dp.Timestamp(), first, "output timestamps never go back or repeat")
}
