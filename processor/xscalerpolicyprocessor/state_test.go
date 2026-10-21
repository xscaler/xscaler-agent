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

// sendLevel sends one pod's gauge (or non-monotonic sum) point as its own
// batch, the way the prometheus receiver and OTLP pushes deliver them.
func sendLevel(t *testing.T, p *policyProcessor, name string, sum bool, pod string, ts int64, v float64) (pmetric.NumberDataPoint, bool) {
	t.Helper()
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("k8s.pod.name", pod)
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName(name)
	var dps pmetric.NumberDataPointSlice
	if sum {
		s := m.SetEmptySum()
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		dps = s.DataPoints()
	} else {
		dps = m.SetEmptyGauge().DataPoints()
	}
	dp := dps.AppendEmpty()
	dp.SetDoubleValue(v)
	dp.SetTimestamp(pcommon.Timestamp(ts))
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	got := collectMetrics(out)[name]
	if len(got) == 0 {
		return pmetric.NumberDataPoint{}, false
	}
	require.Len(t, got, 1)
	var res pmetric.NumberDataPointSlice
	if sum {
		res = got[0].Sum().DataPoints()
	} else {
		res = got[0].Gauge().DataPoints()
	}
	require.Equal(t, 1, res.Len())
	return res.At(0), true
}

func levelProcessor(t *testing.T, gaugeFn string) (*policyProcessor, *time.Time) {
	t.Helper()
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{
		{Metric: "mem", DropAttributes: []string{"k8s.pod.name"}, GaugeFunction: gaugeFn},
		{Metric: "inflight", DropAttributes: []string{"k8s.pod.name"}},
	}
	p := newTestProcessor(t, cfg)
	clock := time.Unix(1_000, 0)
	p.now = func() time.Time { return clock }
	return p, &clock
}

func TestGaugeMergeAcrossBatches(t *testing.T) {
	p, clock := levelProcessor(t, "avg")
	dp, _ := sendLevel(t, p, "mem", false, "a", 100, 2)
	assert.InDelta(t, 2.0, dp.DoubleValue(), 1e-9)
	first := dp.Timestamp()

	// b arrives alone in the next batch, with an older timestamp.
	dp, _ = sendLevel(t, p, "mem", false, "b", 90, 4)
	assert.InDelta(t, 3.0, dp.DoubleValue(), 1e-9, "average over both live pods, not just this batch")
	assert.Greater(t, dp.Timestamp(), first, "timestamps keep moving forward")

	dp, _ = sendLevel(t, p, "mem", false, "a", 200, 6)
	assert.InDelta(t, 5.0, dp.DoubleValue(), 1e-9)

	// A resent or older point of a changes nothing and emits nothing.
	_, ok := sendLevel(t, p, "mem", false, "a", 150, 100)
	assert.False(t, ok)

	// b stops reporting; after the TTL only a counts.
	*clock = clock.Add(6 * time.Minute)
	sendLevel(t, p, "mem", false, "a", 300, 6)
	*clock = clock.Add(6 * time.Minute)
	dp, _ = sendLevel(t, p, "mem", false, "a", 400, 8)
	assert.InDelta(t, 8.0, dp.DoubleValue(), 1e-9, "b expired")
	assert.Equal(t, pcommon.Timestamp(0), dp.StartTimestamp(), "gauges carry no start time")
}

func TestGaugeFunctionsAcrossBatches(t *testing.T) {
	for fn, want := range map[string]float64{"sum": 9, "min": 2, "max": 7, "last": 7} {
		t.Run(fn, func(t *testing.T) {
			p, _ := levelProcessor(t, fn)
			sendLevel(t, p, "mem", false, "a", 100, 2)
			dp, _ := sendLevel(t, p, "mem", false, "b", 200, 7)
			assert.InDelta(t, want, dp.DoubleValue(), 1e-9)
		})
	}
}

func TestNonMonotonicSumAcrossBatches(t *testing.T) {
	p, _ := levelProcessor(t, "")
	dp, _ := sendLevel(t, p, "inflight", true, "a", 100, 5)
	start := dp.StartTimestamp()
	assert.InDelta(t, 5.0, dp.DoubleValue(), 1e-9)
	dp, _ = sendLevel(t, p, "inflight", true, "b", 110, 2)
	assert.InDelta(t, 7.0, dp.DoubleValue(), 1e-9, "sum of every live pod's latest level")
	dp, _ = sendLevel(t, p, "inflight", true, "a", 120, 1)
	assert.InDelta(t, 3.0, dp.DoubleValue(), 1e-9, "levels may go down")
	assert.Equal(t, start, dp.StartTimestamp(), "stable start time")
}

func TestLevelMergeRespectsMaxSources(t *testing.T) {
	p, _ := levelProcessor(t, "sum")
	p.acc.maxSources = 1
	sendLevel(t, p, "mem", false, "a", 100, 2)
	_, ok := sendLevel(t, p, "mem", false, "b", 110, 5)
	assert.False(t, ok, "b is refused, so nothing new to report")
	assert.Equal(t, 1, p.acc.nSources())
}

// TestCollectorRestartNoDecrease reproduces the end-to-end failure: two
// pods, the merged counter at 150 then 160, a collector restart, and p1
// arriving alone first. The old process emitted 90 (p1 alone), so the
// counter fell, resets() was 1 and increase() counted a phantom +90.
func TestCollectorRestartNoDecrease(t *testing.T) {
	warm := func(c *Config) { c.AggregationState.Warmup = 2 * time.Minute }
	var emitted []int64
	var starts []pcommon.Timestamp
	record := func(dp pmetric.NumberDataPoint, ok bool) {
		if ok {
			emitted = append(emitted, dp.IntValue())
			starts = append(starts, dp.StartTimestamp())
		}
	}
	sec := func(s int64) int64 { return s * 1_000_000_000 }

	// The process before the restart, long past its warm-up.
	before, clock := aggProcessor(t, warm)
	*clock = time.Unix(100, 0)
	sendCounter(t, before, podPoint{"p1", sec(10), sec(100), 0}, podPoint{"p2", sec(10), sec(100), 0})
	*clock = time.Unix(400, 0)
	record(sendCounter(t, before, podPoint{"p1", sec(10), sec(400), 70}, podPoint{"p2", sec(10), sec(400), 80}))
	*clock = time.Unix(430, 0)
	record(sendCounter(t, before, podPoint{"p1", sec(10), sec(430), 75}, podPoint{"p2", sec(10), sec(430), 85}))
	require.Equal(t, []int64{150, 160}, emitted)

	// Collector restart: a fresh process, p1 arrives first and alone.
	after, clock := aggProcessor(t, warm)
	*clock = time.Unix(460, 0)
	record(sendCounter(t, after, podPoint{"p1", sec(10), sec(460), 90}))
	*clock = time.Unix(470, 0)
	record(sendCounter(t, after, podPoint{"p2", sec(10), sec(470), 90}))
	assert.Equal(t, []int64{150, 160}, emitted, "nothing is emitted during the warm-up: a gap, not a drop")

	// Warm-up over: the full value comes out, then normal accumulation.
	*clock = time.Unix(590, 0)
	record(sendCounter(t, after, podPoint{"p1", sec(10), sec(590), 95}))
	*clock = time.Unix(600, 0)
	record(sendCounter(t, after, podPoint{"p2", sec(10), sec(600), 100}))
	require.Equal(t, []int64{150, 160, 185, 195}, emitted)

	resets := 0
	for i := 1; i < len(emitted); i++ {
		if emitted[i] < emitted[i-1] {
			resets++
		}
	}
	assert.Zero(t, resets, "the merged counter never decreases across the restart")
	procStart := pcommon.NewTimestampFromTime(time.Unix(460, 0))
	assert.LessOrEqual(t, starts[2], procStart, "start time is the new process's start,")
	assert.Greater(t, starts[2], procStart-pcommon.Timestamp(time.Millisecond), "kept just before p1's first point, which came at that instant")
	assert.Greater(t, starts[2], starts[0], "a new series: later start than before the restart")
	assert.Equal(t, starts[2], starts[3])
}

func TestWarmupHoldsBackOnlyNewOutputs(t *testing.T) {
	p, clock := aggProcessor(t, func(c *Config) { c.AggregationState.Warmup = 2 * time.Minute })
	_, ok := sendCounter(t, p, podPoint{"p1", 10, 100, 5})
	assert.False(t, ok)
	*clock = clock.Add(3 * time.Minute)
	dp, ok := sendCounter(t, p, podPoint{"p1", 10, 200, 6})
	require.True(t, ok)
	assert.Equal(t, int64(6), dp.IntValue())

	// After the warm-up an old pod only sets a baseline.
	dp, _ = sendCounter(t, p, podPoint{"p1", 10, 300, 7}, podPoint{"old", 5, 300, 1000})
	assert.Equal(t, int64(7), dp.IntValue())

	first := dp.StartTimestamp()
	// An output recreated later (every source expired) resumes its series:
	// it does not hold back again (otherwise an output whose sources expire
	// between scrapes would never emit), keeps its start time and total,
	// and joining sources follow the steady-state rules.
	*clock = clock.Add(11 * time.Minute)
	dp, ok = sendCounter(t, p, podPoint{"p9", 10, 900, 1})
	require.True(t, ok, "the recreated output keeps its first warm-up")
	assert.Equal(t, int64(7), dp.IntValue(), "p9 predates the output: a baseline")
	assert.Equal(t, first, dp.StartTimestamp())
	*clock = clock.Add(time.Minute)
	dp, ok = sendCounter(t, p, podPoint{"p1", 10, 1000, 10})
	require.True(t, ok)
	assert.Equal(t, int64(10), dp.IntValue(), "p1 is back with its start: only its increase")

	// Gauges are levels, not totals: they are not held back.
	lp, _ := levelProcessor(t, "avg")
	lp.acc.warmup = 2 * time.Minute
	_, ok = sendLevel(t, lp, "mem", false, "a", 100, 2)
	assert.True(t, ok)
}

func TestWarmupDisabled(t *testing.T) {
	p, clock := aggProcessor(t, nil) // baseConfig: warm-up off
	*clock = time.Unix(0, 5_000)
	dp, ok := sendCounter(t, p, podPoint{"p1", 10, 5_000, 1000})
	require.True(t, ok, "no warm-up: emitted at once")
	assert.Equal(t, int64(1000), dp.IntValue())
	dp, _ = sendCounter(t, p, podPoint{"p2", 10, 6_000, 1000})
	assert.Equal(t, int64(1000), dp.IntValue(), "without a warm-up a pod older than the output only sets a baseline")
}

func TestHistogramLayoutRolloutAcrossBatches(t *testing.T) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "latency", DropAttributes: []string{"k8s.pod.name"}}}
	p := newTestProcessor(t, cfg)
	clock := time.Unix(1_000, 0)
	p.now = func() time.Time { return clock }

	send := func(pod string, bounds []float64, buckets []uint64, ts int64) []pmetric.HistogramDataPoint {
		md := pmetric.NewMetrics()
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("k8s.pod.name", pod)
		m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("latency")
		h := m.SetEmptyHistogram()
		h.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		dp := h.DataPoints().AppendEmpty()
		dp.ExplicitBounds().FromRaw(bounds)
		dp.BucketCounts().FromRaw(buckets)
		dp.SetCount(buckets[0] + buckets[1])
		dp.SetStartTimestamp(1)
		dp.SetTimestamp(pcommon.Timestamp(ts))
		out, err := p.processMetrics(context.Background(), md)
		require.NoError(t, err)
		var res []pmetric.HistogramDataPoint
		rms := out.ResourceMetrics()
		for i := 0; i < rms.Len(); i++ {
			ms := rms.At(i).ScopeMetrics().At(0).Metrics()
			for j := 0; j < ms.Len(); j++ {
				dps := ms.At(j).Histogram().DataPoints()
				for k := 0; k < dps.Len(); k++ {
					res = append(res, dps.At(k))
				}
			}
		}
		return res
	}

	got := send("a", []float64{1}, []uint64{1, 1}, 100)
	require.Len(t, got, 1)
	_, aggregated := got[0].Attributes().Get(defaultIdentityAttribute)
	assert.True(t, aggregated)

	// b runs the new version with different bounds.
	got = send("b", []float64{2}, []uint64{3, 0}, 110)
	require.Len(t, got, 1)
	_, aggregated = got[0].Attributes().Get(defaultIdentityAttribute)
	assert.False(t, aggregated, "second layout passes through instead of duplicating the series")
	assert.Equal(t, 1, got[0].ExplicitBounds().Len())
	assert.InDelta(t, 2.0, got[0].ExplicitBounds().At(0), 1e-9)

	// Once a's layout expires, the new layout becomes the aggregated one.
	clock = clock.Add(11 * time.Minute)
	got = send("b", []float64{2}, []uint64{4, 0}, 200)
	require.Len(t, got, 1)
	_, aggregated = got[0].Attributes().Get(defaultIdentityAttribute)
	assert.True(t, aggregated)
}

func TestValidateAggregationState(t *testing.T) {
	cfg := createDefaultConfig()
	cfg.AggregationState = AggregationStateConfig{TTL: -1, Warmup: -1, MaxSources: -1}
	err := cfg.Validate()
	require.Error(t, err)
	for _, want := range []string{"aggregation_state.ttl", "aggregation_state.warmup", "aggregation_state.max_sources"} {
		assert.Contains(t, err.Error(), want)
	}
	d := createDefaultConfig().AggregationState
	assert.Equal(t, AggregationStateConfig{TTL: 10 * time.Minute, Warmup: 2 * time.Minute, MaxSources: 100000}, d)
}
