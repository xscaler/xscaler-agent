package xscalerpolicyprocessor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"
)

// benchBatch builds one scrape of pods pods, each with a counter, a gauge
// and a histogram carrying series label values.
func benchBatch(pods, series int, ts int64) pmetric.Metrics {
	md := pmetric.NewMetrics()
	for p := 0; p < pods; p++ {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("service.name", "payments")
		rm.Resource().Attributes().PutStr("k8s.pod.name", fmt.Sprintf("pod-%d", p))
		ms := rm.ScopeMetrics().AppendEmpty().Metrics()
		c := ms.AppendEmpty()
		c.SetName("requests_total")
		s := c.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		g := ms.AppendEmpty()
		g.SetName("mem")
		gd := g.SetEmptyGauge()
		h := ms.AppendEmpty()
		h.SetName("latency")
		hd := h.SetEmptyHistogram()
		hd.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		for i := 0; i < series; i++ {
			route := fmt.Sprintf("/r%d", i)
			dp := s.DataPoints().AppendEmpty()
			dp.Attributes().PutStr("route", route)
			dp.SetStartTimestamp(1)
			dp.SetTimestamp(pcommon.Timestamp(ts))
			dp.SetIntValue(ts / 1e9)
			gp := gd.DataPoints().AppendEmpty()
			gp.Attributes().PutStr("route", route)
			gp.SetTimestamp(pcommon.Timestamp(ts))
			gp.SetDoubleValue(float64(i))
			hp := hd.DataPoints().AppendEmpty()
			hp.Attributes().PutStr("route", route)
			hp.SetStartTimestamp(1)
			hp.SetTimestamp(pcommon.Timestamp(ts))
			n := uint64(ts / 1e9)
			hp.SetCount(3 * n)
			hp.SetSum(float64(n))
			hp.ExplicitBounds().FromRaw([]float64{0.1, 1})
			hp.BucketCounts().FromRaw([]uint64{n, n, n})
		}
	}
	return md
}

func runBench(b *testing.B, cfg *Config) {
	p, err := newPolicyProcessor(processortestSettings(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	clock := time.Unix(1000, 0)
	p.now = func() time.Time { return clock }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		clock = clock.Add(15 * time.Second)
		md := benchBatch(10, 20, clock.UnixNano())
		b.StartTimer()
		if _, err := p.processMetrics(context.Background(), md); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAggregation(b *testing.B) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "*", DropAttributes: []string{"k8s.pod.name"}}}
	runBench(b, cfg)
}

func BenchmarkCardinalityOverflow(b *testing.B) {
	cfg := baseConfig()
	cfg.Cardinality.DefaultMaxSeries = 50
	runBench(b, cfg)
}

func processortestSettings() processor.Settings { return processortest.NewNopSettings(componentType) }

// BenchmarkAggregationParallel runs batches from several pipelines at once;
// each goroutine sends its own service's pods.
func BenchmarkAggregationParallel(b *testing.B) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "*", DropAttributes: []string{"k8s.pod.name"}}}
	p, err := newPolicyProcessor(processortestSettings(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	var mu sync.Mutex
	clock := time.Unix(1000, 0)
	p.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Millisecond)
		return clock
	}
	var id atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		svc := fmt.Sprintf("svc-%d", id.Add(1))
		ts := int64(1e12)
		for pb.Next() {
			ts += 15e9
			md := benchBatch(10, 20, ts)
			rms := md.ResourceMetrics()
			for i := 0; i < rms.Len(); i++ {
				rms.At(i).Resource().Attributes().PutStr("service.name", svc)
			}
			if _, err := p.processMetrics(context.Background(), md); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkSweepLargeState processes a small batch while 60k sources are
// tracked and a sweep is due on every batch.
func BenchmarkSweepLargeState(b *testing.B) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "*", DropAttributes: []string{"k8s.pod.name"}}}
	cfg.AggregationState.MaxSources = 200000
	p, err := newPolicyProcessor(processortestSettings(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	clock := time.Unix(1000, 0)
	p.now = func() time.Time { return clock }
	for s := 0; s < 100; s++ { // 100 services x 10 pods x 20 routes x 3 metrics = 60k sources
		md := benchBatch(10, 20, 1e12)
		rms := md.ResourceMetrics()
		for i := 0; i < rms.Len(); i++ {
			rms.At(i).Resource().Attributes().PutStr("service.name", fmt.Sprintf("svc-%d", s))
		}
		if _, err := p.processMetrics(context.Background(), md); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		clock = clock.Add(time.Minute) // sweep due every batch
		md := benchBatch(1, 1, 1e12+int64(i+1)*15e9)
		b.StartTimer()
		if _, err := p.processMetrics(context.Background(), md); err != nil {
			b.Fatal(err)
		}
	}
}
