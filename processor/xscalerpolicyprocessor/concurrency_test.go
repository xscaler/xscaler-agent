package xscalerpolicyprocessor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// Pipelines call the processor concurrently. Each goroutine sends its own
// service's counters (aggregated across pods) and capped series; every
// output must stay exact and monotonic.
func TestConcurrentPipelines(t *testing.T) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "requests_total", DropAttributes: []string{"k8s.pod.name"}}}
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "capped", MaxSeries: 3}}
	p := newTestProcessor(t, cfg)
	var mu sync.Mutex
	clock := time.Unix(1000, 0)
	p.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	const workers, rounds = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			svc := fmt.Sprintf("svc-%d", w)
			var last int64
			for r := 1; r <= rounds; r++ {
				md := pmetric.NewMetrics()
				for pod := 0; pod < 2; pod++ {
					rm := md.ResourceMetrics().AppendEmpty()
					rm.Resource().Attributes().PutStr("service.name", svc)
					rm.Resource().Attributes().PutStr("k8s.pod.name", fmt.Sprint(pod))
					ms := rm.ScopeMetrics().AppendEmpty().Metrics()
					m := ms.AppendEmpty()
					m.SetName("requests_total")
					s := m.SetEmptySum()
					s.SetIsMonotonic(true)
					s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
					dp := s.DataPoints().AppendEmpty()
					dp.SetStartTimestamp(1)
					dp.SetTimestamp(pcommon.Timestamp(r * 1000))
					dp.SetIntValue(int64(r))
					c := ms.AppendEmpty()
					c.SetName("capped")
					cs := c.SetEmptySum()
					cs.SetIsMonotonic(true)
					cs.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
					for k := 0; k < 5; k++ {
						cp := cs.DataPoints().AppendEmpty()
						cp.Attributes().PutStr("k", fmt.Sprint(k))
						cp.SetStartTimestamp(1)
						cp.SetTimestamp(pcommon.Timestamp(r * 1000))
						cp.SetIntValue(int64(r))
					}
				}
				out, err := p.processMetrics(context.Background(), md)
				if !assert.NoError(t, err) {
					return
				}
				for _, m := range collectMetrics(out)["requests_total"] {
					v := m.Sum().DataPoints().At(0).IntValue()
					assert.GreaterOrEqual(t, v, last)
					last = v
				}
			}
			assert.Equal(t, int64(2*rounds), last, svc)
		}(w)
	}
	wg.Wait()
}

// A sweep visits a bounded amount of state per batch; repeated batches
// still reach every shard.
func TestSweepIsIncremental(t *testing.T) {
	a := newAccumulator(10*time.Minute, 5*time.Minute, 0, 0)
	now := time.Unix(1000, 0)
	for i := 0; i < 4*sweepBudget; i++ {
		key := uint64(i) * 0x9E3779B97F4A7C15
		a.accumulate(key, []point{{orig: 1, ts: 10, isInt: true, i: 1}}, now)
	}
	require.Equal(t, 4*sweepBudget, a.nSources())
	now = now.Add(11 * time.Minute)
	a.sweep(now)
	left := a.nSources()
	assert.Greater(t, left, 0, "one call does not walk all state")
	assert.Less(t, left, 4*sweepBudget, "but it makes progress")
	for i := 0; i < numShards && a.nSources() > 0; i++ {
		a.sweep(now)
	}
	assert.Equal(t, 0, a.nSources())
	assert.Zero(t, a.nSeries())
}
