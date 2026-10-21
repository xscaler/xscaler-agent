package xscalerpolicyprocessor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor/processortest"
)

func newTestProcessor(t *testing.T, cfg *Config) *policyProcessor {
	t.Helper()
	require.NoError(t, cfg.Validate())
	p, err := newPolicyProcessor(processortest.NewNopSettings(componentType), cfg)
	require.NoError(t, err)
	return p
}

// baseConfig turns the warm-up off so tests of the merge logic see every
// output immediately; the warm-up has tests of its own.
func baseConfig() *Config {
	cfg := createDefaultConfig()
	cfg.CollectorIdentity.Value = "node-a"
	cfg.AggregationState.Warmup = 0
	return cfg
}

// addSum appends a cumulative monotonic int sum point for one pod.
func addSum(md pmetric.Metrics, res map[string]string, name string, attrs map[string]string, v int64, ts int64) {
	rm := md.ResourceMetrics().AppendEmpty()
	for k, val := range res {
		rm.Resource().Attributes().PutStr(k, val)
	}
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName(name)
	s := m.SetEmptySum()
	s.SetIsMonotonic(true)
	s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	dp := s.DataPoints().AppendEmpty()
	for k, val := range attrs {
		dp.Attributes().PutStr(k, val)
	}
	dp.SetIntValue(v)
	dp.SetStartTimestamp(pcommon.Timestamp(1))
	dp.SetTimestamp(pcommon.Timestamp(ts))
}

func collectMetrics(md pmetric.Metrics) map[string][]pmetric.Metric {
	out := map[string][]pmetric.Metric{}
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		sms := rms.At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				out[ms.At(k).Name()] = append(out[ms.At(k).Name()], ms.At(k))
			}
		}
	}
	return out
}

func TestValidate(t *testing.T) {
	cfg := createDefaultConfig()
	cfg.Drops = []DropRule{{Metric: "regex:("}}
	cfg.Aggregations = []AggregationRule{
		{Metric: "x", DropAttributes: nil},
		{Metric: "y", DropAttributes: []string{defaultIdentityAttribute}},
		{Metric: "z", DropAttributes: []string{"a"}, GaugeFunction: "median"},
	}
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "m", MaxSeries: -1}}
	err := cfg.Validate()
	require.Error(t, err)
	for _, want := range []string{"drops[0]", "aggregations[0]", "aggregations[1]", "aggregations[2]", "cardinality.limits[0]"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NoError(t, createDefaultConfig().Validate())
}

func TestConfigUnmarshal(t *testing.T) {
	cm := confmap.NewFromStringMap(map[string]any{
		"policy_version": "v7",
		"drops":          []any{map[string]any{"metric": "k8s_pod_*"}},
		"aggregations": []any{map[string]any{
			"metric":          "http_requests_total",
			"drop_attributes": []any{"k8s.pod.name"},
		}},
		"cardinality": map[string]any{
			"default_max_series": 500,
			"window":             "30m",
			"limits":             []any{map[string]any{"metric": "regex:db_.*", "max_series": 50}},
		},
		"service_identity": map[string]any{"enabled": true},
	})
	cfg := createDefaultConfig()
	require.NoError(t, cm.Unmarshal(cfg))
	require.NoError(t, cfg.Validate())
	assert.Equal(t, "v7", cfg.PolicyVersion)
	assert.Equal(t, 30*time.Minute, cfg.Cardinality.Window)
	assert.Equal(t, 50, cfg.Cardinality.Limits[0].MaxSeries)
	assert.True(t, cfg.ServiceIdentity.Enabled)
	assert.Equal(t, defaultSources, cfg.ServiceIdentity.Sources, "defaults survive a partial section")
}

func TestMatcher(t *testing.T) {
	re, err := compileMatcher("k8s_pod_*")
	require.NoError(t, err)
	assert.True(t, re.MatchString("k8s_pod_cpu_usage"))
	assert.False(t, re.MatchString("xk8s_pod_cpu"))
	re, err = compileMatcher("regex:a|b")
	require.NoError(t, err)
	assert.True(t, re.MatchString("b"))
	assert.False(t, re.MatchString("ab"))
	re, err = compileMatcher("a.b")
	require.NoError(t, err)
	assert.False(t, re.MatchString("axb"), "dots in globs are literal")
}

func TestDrops(t *testing.T) {
	cfg := baseConfig()
	cfg.Drops = []DropRule{{Metric: "k8s_pod_*"}}
	p := newTestProcessor(t, cfg)

	md := pmetric.NewMetrics()
	addSum(md, map[string]string{"service.name": "a"}, "k8s_pod_cpu", nil, 1, 10)
	addSum(md, map[string]string{"service.name": "a"}, "http_requests_total", nil, 1, 10)
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	got := collectMetrics(out)
	assert.NotContains(t, got, "k8s_pod_cpu")
	assert.Contains(t, got, "http_requests_total")
	assert.Equal(t, 1, out.ResourceMetrics().Len(), "empty resources are pruned")
}

func TestServiceIdentity(t *testing.T) {
	cfg := baseConfig()
	cfg.ServiceIdentity.Enabled = true
	p := newTestProcessor(t, cfg)

	cases := []struct {
		name  string
		res   map[string]string
		want  string
		wantN string
	}{
		{"placeholder replaced by deployment", map[string]string{"service.name": "k8s-node", "k8s.deployment.name": "payments", "k8s.namespace.name": "prod"}, "payments", "prod"},
		{"missing replaced by statefulset", map[string]string{"k8s.statefulset.name": "db"}, "db", ""},
		{"sdk default replaced", map[string]string{"service.name": "unknown_service:java", "k8s.daemonset.name": "agent"}, "agent", ""},
		{"real name kept", map[string]string{"service.name": "checkout", "k8s.deployment.name": "checkout-v2"}, "checkout", ""},
		{"recommended label wins", map[string]string{"app.kubernetes.io/name": "api", "k8s.deployment.name": "api-v3"}, "api", ""},
		{"host keeps placeholder", map[string]string{"service.name": "k8s-node"}, "k8s-node", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ld := plog.NewLogs()
			attrs := ld.ResourceLogs().AppendEmpty().Resource().Attributes()
			for k, v := range tc.res {
				attrs.PutStr(k, v)
			}
			_, err := p.processLogs(context.Background(), ld)
			require.NoError(t, err)
			v, _ := attrs.Get("service.name")
			assert.Equal(t, tc.want, v.AsString())
			if tc.wantN != "" {
				ns, _ := attrs.Get("service.namespace")
				assert.Equal(t, tc.wantN, ns.AsString())
			}
		})
	}
}

func TestServiceIdentityDisabledByDefault(t *testing.T) {
	p := newTestProcessor(t, baseConfig())
	ld := plog.NewLogs()
	attrs := ld.ResourceLogs().AppendEmpty().Resource().Attributes()
	attrs.PutStr("service.name", "k8s-node")
	attrs.PutStr("k8s.deployment.name", "payments")
	_, err := p.processLogs(context.Background(), ld)
	require.NoError(t, err)
	v, _ := attrs.Get("service.name")
	assert.Equal(t, "k8s-node", v.AsString())
}

func TestAggregationAcrossPods(t *testing.T) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "http_requests_total", DropAttributes: []string{"k8s.pod.name", "pod"}}}
	p := newTestProcessor(t, cfg)

	md := pmetric.NewMetrics()
	svc := func(pod string) map[string]string {
		return map[string]string{"service.name": "payments", "k8s.pod.name": pod}
	}
	addSum(md, svc("p1"), "http_requests_total", map[string]string{"code": "200", "pod": "p1"}, 10, 100)
	addSum(md, svc("p2"), "http_requests_total", map[string]string{"code": "200", "pod": "p2"}, 5, 105)
	addSum(md, svc("p2"), "http_requests_total", map[string]string{"code": "500", "pod": "p2"}, 1, 105)
	// Earlier point of p1's series in the same batch: must not be added on top.
	addSum(md, svc("p1"), "http_requests_total", map[string]string{"code": "200", "pod": "p1"}, 7, 90)
	addSum(md, svc("p1"), "untouched_total", map[string]string{"pod": "p1"}, 3, 100)

	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	got := collectMetrics(out)

	require.Len(t, got["http_requests_total"], 1, "both pods merge into one resource")
	dps := got["http_requests_total"][0].Sum().DataPoints()
	require.Equal(t, 2, dps.Len())
	byCode := map[string]pmetric.NumberDataPoint{}
	for i := 0; i < dps.Len(); i++ {
		code, _ := dps.At(i).Attributes().Get("code")
		byCode[code.AsString()] = dps.At(i)
	}
	assert.Equal(t, int64(15), byCode["200"].IntValue(), "latest p1 (10) + p2 (5)")
	assert.Equal(t, int64(1), byCode["500"].IntValue())
	assert.Equal(t, pcommon.Timestamp(105), byCode["200"].Timestamp())
	id, ok := byCode["200"].Attributes().Get(defaultIdentityAttribute)
	require.True(t, ok, "collector identity keeps two collectors' series apart")
	assert.Equal(t, "node-a", id.AsString())
	_, hasPod := byCode["200"].Attributes().Get("pod")
	assert.False(t, hasPod)

	m := got["http_requests_total"][0]
	assert.True(t, m.Sum().IsMonotonic())
	assert.Equal(t, pmetric.AggregationTemporalityCumulative, m.Sum().AggregationTemporality())

	require.Len(t, got["untouched_total"], 1)
	_, stillHasPod := got["untouched_total"][0].Sum().DataPoints().At(0).Attributes().Get("pod")
	assert.True(t, stillHasPod, "metrics no rule matches pass through")
}

func TestAggregationGaugeFunctions(t *testing.T) {
	for fn, want := range map[string]float64{"": 3, "avg": 3, "sum": 6, "min": 2, "max": 4, "last": 4} {
		t.Run("fn="+fn, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Aggregations = []AggregationRule{{Metric: "mem", DropAttributes: []string{"pod"}, GaugeFunction: fn}}
			p := newTestProcessor(t, cfg)
			md := pmetric.NewMetrics()
			ms := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
			m := ms.AppendEmpty()
			m.SetName("mem")
			g := m.SetEmptyGauge()
			for i, v := range []float64{2, 4} {
				dp := g.DataPoints().AppendEmpty()
				dp.Attributes().PutStr("pod", []string{"a", "b"}[i])
				dp.SetDoubleValue(v)
				dp.SetTimestamp(pcommon.Timestamp(10 + i))
			}
			out, err := p.processMetrics(context.Background(), md)
			require.NoError(t, err)
			got := collectMetrics(out)["mem"][0].Gauge().DataPoints()
			require.Equal(t, 1, got.Len())
			assert.InDelta(t, want, got.At(0).DoubleValue(), 1e-9)
		})
	}
}

func TestAggregationHistogram(t *testing.T) {
	cfg := baseConfig()
	cfg.Aggregations = []AggregationRule{{Metric: "latency", DropAttributes: []string{"pod"}}}
	p := newTestProcessor(t, cfg)
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("latency")
	h := m.SetEmptyHistogram()
	h.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	add := func(pod string, bounds []float64, buckets []uint64, sum float64, minV, maxV float64) {
		dp := h.DataPoints().AppendEmpty()
		dp.Attributes().PutStr("pod", pod)
		dp.ExplicitBounds().FromRaw(bounds)
		dp.BucketCounts().FromRaw(buckets)
		var c uint64
		for _, b := range buckets {
			c += b
		}
		dp.SetCount(c)
		dp.SetSum(sum)
		dp.SetMin(minV)
		dp.SetMax(maxV)
		dp.SetTimestamp(10)
	}
	add("a", []float64{1, 5}, []uint64{1, 2, 3}, 20, 0.5, 9)
	add("b", []float64{1, 5}, []uint64{4, 0, 1}, 8, 0.1, 6)
	add("c", []float64{2}, []uint64{1, 1}, 3, 1, 2) // a second layout

	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	got := collectMetrics(out)["latency"]
	require.Len(t, got, 2, "the merged metric plus c left in place")
	var merged, passthrough pmetric.HistogramDataPoint
	for _, m := range got {
		dps := m.Histogram().DataPoints()
		require.Equal(t, 1, dps.Len(), "never two series with the same attributes")
		if dps.At(0).ExplicitBounds().Len() == 2 {
			merged = dps.At(0)
		} else {
			passthrough = dps.At(0)
		}
	}
	assert.Equal(t, []uint64{5, 2, 4}, merged.BucketCounts().AsRaw())
	assert.Equal(t, uint64(11), merged.Count())
	assert.InDelta(t, 28.0, merged.Sum(), 1e-9)
	assert.InDelta(t, 0.1, merged.Min(), 1e-9, "delta histograms keep min")
	assert.InDelta(t, 9.0, merged.Max(), 1e-9)
	pod, ok := passthrough.Attributes().Get("pod")
	require.True(t, ok, "the second layout stays unaggregated, attributes intact")
	assert.Equal(t, "c", pod.AsString())
	_, stamped := passthrough.Attributes().Get(defaultIdentityAttribute)
	assert.False(t, stamped)
}

func TestCardinalityOverflow(t *testing.T) {
	cfg := baseConfig()
	cfg.Cardinality.Limits = []CardinalityLimit{{Metric: "req", MaxSeries: 2}}
	p := newTestProcessor(t, cfg)
	clock := time.Unix(0, 0)
	p.now = func() time.Time { return clock }

	batch := func(users ...string) pmetric.Metric {
		md := pmetric.NewMetrics()
		m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("req")
		s := m.SetEmptySum()
		s.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
		for _, u := range users {
			dp := s.DataPoints().AppendEmpty()
			dp.Attributes().PutStr("user", u)
			dp.SetIntValue(1)
			dp.SetTimestamp(10)
		}
		out, err := p.processMetrics(context.Background(), md)
		require.NoError(t, err)
		return collectMetrics(out)["req"][0]
	}

	m := batch("u1", "u2", "u3", "u4")
	dps := m.Sum().DataPoints()
	require.Equal(t, 3, dps.Len(), "two admitted series plus one overflow series")
	var overflow int64
	for i := 0; i < dps.Len(); i++ {
		if v, ok := dps.At(i).Attributes().Get(overflowAttribute); ok && v.AsString() == "true" {
			overflow = dps.At(i).IntValue()
			_, hasUser := dps.At(i).Attributes().Get("user")
			assert.False(t, hasUser)
		}
	}
	assert.Equal(t, int64(2), overflow, "u3 and u4 fold into the overflow series")

	// Known series stay admitted in the next batch.
	m = batch("u1", "u5")
	assert.Equal(t, 2, m.Sum().DataPoints().Len())
	_, ok := m.Sum().DataPoints().At(0).Attributes().Get("user")
	assert.True(t, ok)

	hasOverflow := func(m pmetric.Metric) bool {
		for i := 0; i < m.Sum().DataPoints().Len(); i++ {
			if _, ok := m.Sum().DataPoints().At(i).Attributes().Get(overflowAttribute); ok {
				return true
			}
		}
		return false
	}
	// Admissions are sticky: u1 and u2 reported during the window that just
	// ended, so they keep their slots and new series still overflow.
	clock = clock.Add(2 * time.Hour)
	m = batch("u7", "u8")
	assert.True(t, hasOverflow(m))

	// After a whole window without u1 and u2 their slots are free again.
	clock = clock.Add(time.Hour)
	m = batch("u7", "u8")
	assert.False(t, hasOverflow(m))
}

func TestCardinalityKeepsDuplicatePointsOfAdmittedSeries(t *testing.T) {
	cfg := baseConfig()
	cfg.Cardinality.DefaultMaxSeries = 1
	p := newTestProcessor(t, cfg)
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("g")
	g := m.SetEmptyGauge()
	for i, u := range []string{"a", "a", "b"} {
		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr("user", u)
		dp.SetDoubleValue(float64(i))
		dp.SetTimestamp(pcommon.Timestamp(10 + i))
	}
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	dps := collectMetrics(out)["g"][0].Gauge().DataPoints()
	assert.Equal(t, 3, dps.Len(), "both points of the admitted series stay, plus overflow")
}

func TestFactoryEndToEnd(t *testing.T) {
	f := NewFactory()
	cfg := f.CreateDefaultConfig().(*Config)
	cfg.Drops = []DropRule{{Metric: "drop_me"}}
	sink := new(consumertest.MetricsSink)
	mp, err := f.CreateMetrics(context.Background(), processortest.NewNopSettings(componentType), cfg, sink)
	require.NoError(t, err)
	require.NoError(t, mp.Start(context.Background(), componenttest.NewNopHost()))
	md := pmetric.NewMetrics()
	addSum(md, nil, "drop_me", nil, 1, 1)
	addSum(md, nil, "keep_me", nil, 1, 1)
	require.NoError(t, mp.ConsumeMetrics(context.Background(), md))
	require.NoError(t, mp.Shutdown(context.Background()))
	require.Len(t, sink.AllMetrics(), 1)
	got := collectMetrics(sink.AllMetrics()[0])
	assert.Contains(t, got, "keep_me")
	assert.NotContains(t, got, "drop_me")

	lp, err := f.CreateLogs(context.Background(), processortest.NewNopSettings(componentType), cfg, new(consumertest.LogsSink))
	require.NoError(t, err)
	assert.NotNil(t, lp)
	tp, err := f.CreateTraces(context.Background(), processortest.NewNopSettings(componentType), cfg, new(consumertest.TracesSink))
	require.NoError(t, err)
	assert.NotNil(t, tp)
}
