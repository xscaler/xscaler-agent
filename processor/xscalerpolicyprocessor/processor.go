package xscalerpolicyprocessor

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const scopeName = "github.com/xscaler/xscaler-agent/processor/xscalerpolicyprocessor"

type aggRule struct {
	match   *regexp.Regexp
	drop    map[string]struct{}
	gaugeFn string
}

type limitRule struct {
	match *regexp.Regexp
	max   int
}

type policyProcessor struct {
	cfg        *Config
	drops      []*regexp.Regexp
	aggs       []aggRule
	limits     []limitRule
	identKey   string
	identValue string
	now        func() time.Time

	// State is sharded with a lock per shard (cardinality by metric name,
	// the accumulator by output), so concurrent pipelines only wait for
	// each other when they touch the same shard.
	card [numShards]cardShard
	acc  *accumulator

	metricsDropped metric.Int64Counter
	pointsMerged   metric.Int64Counter
	pointsOverflow metric.Int64Counter
	identitiesSet  metric.Int64Counter
	unaggregated   metric.Int64Counter
	clampedTS      metric.Int64Counter
	telemetryAttrs metric.MeasurementOption
	summaryAttrs   metric.MeasurementOption
	layoutAttrs    metric.MeasurementOption
}

func newPolicyProcessor(set processor.Settings, cfg *Config) (*policyProcessor, error) {
	p := &policyProcessor{cfg: cfg, now: time.Now}
	for i := range p.card {
		p.card[i].seen = map[string]map[uint64]time.Time{}
		p.card[i].retired = map[string]map[uint64]time.Time{}
		p.card[i].overflowed = map[string]map[uint64]time.Time{}
	}
	for _, d := range cfg.Drops {
		re, err := compileMatcher(d.Metric)
		if err != nil {
			return nil, err
		}
		p.drops = append(p.drops, re)
	}
	for _, a := range cfg.Aggregations {
		re, err := compileMatcher(a.Metric)
		if err != nil {
			return nil, err
		}
		drop := map[string]struct{}{}
		for _, k := range a.DropAttributes {
			drop[k] = struct{}{}
		}
		p.aggs = append(p.aggs, aggRule{match: re, drop: drop, gaugeFn: a.GaugeFunction})
	}
	for _, l := range cfg.Cardinality.Limits {
		re, err := compileMatcher(l.Metric)
		if err != nil {
			return nil, err
		}
		p.limits = append(p.limits, limitRule{match: re, max: l.MaxSeries})
	}
	p.identKey, p.identValue = cfg.identity()
	maxSources := cfg.AggregationState.MaxSources
	if maxSources == 0 {
		maxSources = defaultMaxSources
	}
	gaugeStale := cfg.AggregationState.GaugeStaleness
	if gaugeStale == 0 {
		gaugeStale = defaultGaugeStaleness
	}
	p.acc = newAccumulator(cfg.AggregationState.effectiveTTL(), gaugeStale, cfg.AggregationState.Warmup, maxSources)

	meter := set.MeterProvider.Meter(scopeName)
	var err error
	if p.metricsDropped, err = meter.Int64Counter("xscaler_policy_dropped_data_points",
		metric.WithDescription("Data points removed by drop rules")); err != nil {
		return nil, err
	}
	if p.pointsMerged, err = meter.Int64Counter("xscaler_policy_aggregated_data_points",
		metric.WithDescription("Data points folded into a coarser series by aggregation rules")); err != nil {
		return nil, err
	}
	if p.pointsOverflow, err = meter.Int64Counter("xscaler_policy_overflow_data_points",
		metric.WithDescription("Data points moved to the overflow series by cardinality limits")); err != nil {
		return nil, err
	}
	if p.identitiesSet, err = meter.Int64Counter("xscaler_policy_service_identity_set",
		metric.WithDescription("Resources whose service.name was derived from workload attributes")); err != nil {
		return nil, err
	}
	if p.unaggregated, err = meter.Int64Counter("xscaler_policy_unaggregated_data_points",
		metric.WithDescription("Data points an aggregation rule matched but could not merge, left as they were (reason: summary, bucket_layout)")); err != nil {
		return nil, err
	}
	if p.clampedTS, err = meter.Int64Counter("xscaler_policy_future_timestamps_clamped",
		metric.WithDescription("Merged data points timestamped more than 1m ahead of the collector's clock; the output timestamp was capped")); err != nil {
		return nil, err
	}
	version := attribute.String("policy_version", cfg.PolicyVersion)
	p.telemetryAttrs = metric.WithAttributes(version)
	p.summaryAttrs = metric.WithAttributes(version, attribute.String("reason", "summary"))
	p.layoutAttrs = metric.WithAttributes(version, attribute.String("reason", "bucket_layout"))
	return p, nil
}

func (p *policyProcessor) processLogs(ctx context.Context, ld plog.Logs) (plog.Logs, error) {
	rls := ld.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		p.applyIdentity(ctx, rls.At(i).Resource().Attributes())
	}
	return ld, nil
}

func (p *policyProcessor) processTraces(ctx context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		p.applyIdentity(ctx, rss.At(i).Resource().Attributes())
	}
	return td, nil
}

func (p *policyProcessor) processMetrics(ctx context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		p.applyIdentity(ctx, rms.At(i).Resource().Attributes())
	}
	p.applyDrops(ctx, md)
	if len(p.aggs) == 0 && p.cfg.Cardinality.DefaultMaxSeries == 0 && len(p.limits) == 0 {
		return md, nil
	}
	now := p.now()
	p.acc.started(now)
	p.acc.sweep(now)
	p.applyAggregations(ctx, md, now)
	p.applyCardinality(ctx, md, now)
	if n := p.acc.clamped.Swap(0); n > 0 {
		p.clampedTS.Add(ctx, n, p.telemetryAttrs)
	}
	return md, nil
}

// applyIdentity sets service.name from the first non-empty source attribute
// when the current value is missing or a placeholder.
func (p *policyProcessor) applyIdentity(ctx context.Context, attrs pcommon.Map) {
	si := p.cfg.ServiceIdentity
	if !si.Enabled {
		return
	}
	if si.SetNamespace {
		if _, ok := attrs.Get("service.namespace"); !ok {
			if ns, ok := attrs.Get("k8s.namespace.name"); ok && ns.AsString() != "" {
				attrs.PutStr("service.namespace", ns.AsString())
			}
		}
	}
	if cur, ok := attrs.Get("service.name"); ok && !isPlaceholder(cur.AsString(), si.Placeholders) {
		return
	}
	for _, key := range si.Sources {
		if v, ok := attrs.Get(key); ok && v.AsString() != "" {
			attrs.PutStr("service.name", v.AsString())
			p.identitiesSet.Add(ctx, 1, p.telemetryAttrs)
			return
		}
	}
}

func isPlaceholder(name string, placeholders []string) bool {
	if name == "" {
		return true
	}
	for _, ph := range placeholders {
		if prefix, ok := strings.CutSuffix(ph, "*"); ok {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if name == ph {
			return true
		}
	}
	return false
}

func matchAny(res []*regexp.Regexp, name string) bool {
	for _, re := range res {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

func (p *policyProcessor) applyDrops(ctx context.Context, md pmetric.Metrics) {
	if len(p.drops) == 0 {
		return
	}
	var dropped int64
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		sms := rms.At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			sms.At(j).Metrics().RemoveIf(func(m pmetric.Metric) bool {
				if matchAny(p.drops, m.Name()) {
					dropped += int64(dataPointCount(m))
					return true
				}
				return false
			})
		}
	}
	pruneEmpty(md)
	if dropped > 0 {
		p.metricsDropped.Add(ctx, dropped, p.telemetryAttrs)
	}
}

func (p *policyProcessor) aggRuleFor(name string) *aggRule {
	for i := range p.aggs {
		if p.aggs[i].match.MatchString(name) {
			return &p.aggs[i]
		}
	}
	return nil
}

type aggResource struct {
	attrs  pcommon.Map
	schema string
	scopes map[uint64]*aggScope
	order  []uint64
}

type aggScope struct {
	scope   pcommon.InstrumentationScope
	schema  string
	metrics map[uint64]*mergedMetric
	order   []uint64
}

// applyAggregations moves every metric an aggregation rule matches out of
// its resource, strips the rule's attributes from resource and points, stamps
// the collector identity and merges points that now describe the same series.
// Resources that become identical (two pods of one deployment) merge too.
func (p *policyProcessor) applyAggregations(ctx context.Context, md pmetric.Metrics, now time.Time) {
	if len(p.aggs) == 0 {
		return
	}
	out := map[uint64]*aggResource{}
	var outOrder []uint64
	var merged, summaries, layoutKept int64

	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		resHash := hashMap(rm.Resource().Attributes())
		sms := rm.ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			sm := sms.At(j)
			sm.Metrics().RemoveIf(func(m pmetric.Metric) bool {
				rule := p.aggRuleFor(m.Name())
				if rule == nil {
					return false
				}
				if !mergeable(m) {
					// Summaries: quantiles of different sources cannot be
					// combined, so they pass through unaggregated.
					summaries += int64(dataPointCount(m))
					return false
				}
				newRes := pcommon.NewMap()
				rm.Resource().Attributes().CopyTo(newRes)
				newRes.RemoveIf(func(k string, _ pcommon.Value) bool { _, ok := rule.drop[k]; return ok })
				rk := hashMap(newRes)
				ar, ok := out[rk]
				if !ok {
					ar = &aggResource{attrs: newRes, schema: rm.SchemaUrl(), scopes: map[uint64]*aggScope{}}
					out[rk] = ar
					outOrder = append(outOrder, rk)
				}
				sk := hashStrings(sm.Scope().Name(), sm.Scope().Version())
				as, ok := ar.scopes[sk]
				if !ok {
					sc := pcommon.NewInstrumentationScope()
					sc.SetName(sm.Scope().Name())
					sc.SetVersion(sm.Scope().Version())
					as = &aggScope{scope: sc, schema: sm.SchemaUrl(), metrics: map[uint64]*mergedMetric{}}
					ar.scopes[sk] = as
					ar.order = append(ar.order, sk)
				}
				mk := metricKey(m)
				mm, ok := as.metrics[mk]
				if !ok {
					mm = newMergedMetric(m, rule.gaugeFn)
					as.metrics[mk] = mm
					as.order = append(as.order, mk)
				}
				// A histogram point whose bucket layout differs from the one
				// its output already uses (bounds changed mid-rollout) stays
				// in place unaggregated; merging it would emit a second
				// series with the same attributes.
				var keep []bool
				kept := 0
				eachPoint(m, resHash, func(pt point) {
					pt.attrs.RemoveIf(func(k string, _ pcommon.Value) bool { _, ok := rule.drop[k]; return ok })
					pt.attrs.PutStr(p.identKey, p.identValue)
					// Staleness markers fold in whatever their bounds: they
					// carry no value and must not stay behind on their own.
					if pt.hist != nil && !pt.stale && !p.acc.primaryLayout(mix(rk, mix(sk, mix(mk, hashMap(pt.attrs)))), hashFloats(pt.hist.bounds), now) {
						keep = append(keep, true)
						kept++
						return
					}
					keep = append(keep, false)
					mm.add(pt)
					merged++
				})
				layoutKept += int64(kept)
				if kept == 0 {
					return true
				}
				removeByIndex(m, invert(keep))
				return false
			})
		}
	}
	if summaries > 0 {
		p.unaggregated.Add(ctx, summaries, p.summaryAttrs)
	}
	if layoutKept > 0 {
		p.unaggregated.Add(ctx, layoutKept, p.layoutAttrs)
	}
	if len(outOrder) == 0 {
		return
	}
	pruneEmpty(md)

	var written, refused int64
	for _, rk := range outOrder {
		ar := out[rk]
		rm := md.ResourceMetrics().AppendEmpty()
		rm.SetSchemaUrl(ar.schema)
		ar.attrs.CopyTo(rm.Resource().Attributes())
		for _, sk := range ar.order {
			as := ar.scopes[sk]
			sm := rm.ScopeMetrics().AppendEmpty()
			sm.SetSchemaUrl(as.schema)
			as.scope.CopyTo(sm.Scope())
			for _, mk := range as.order {
				mm := as.metrics[mk]
				dst := sm.Metrics().AppendEmpty()
				mm.proto.CopyTo(dst)
				refused += int64(mm.writeTo(dst, p.acc, mix(rk, mix(sk, mk)), now))
				written += int64(dataPointCount(dst))
			}
		}
	}
	pruneEmpty(md)
	if folded := merged - written - refused; folded > 0 {
		p.pointsMerged.Add(ctx, folded, p.telemetryAttrs)
	}
	if refused > 0 {
		p.pointsOverflow.Add(ctx, refused, p.telemetryAttrs)
	}
}

func metricKey(m pmetric.Metric) uint64 {
	temporality := ""
	switch m.Type() {
	case pmetric.MetricTypeSum:
		temporality = m.Sum().AggregationTemporality().String()
	case pmetric.MetricTypeHistogram:
		temporality = m.Histogram().AggregationTemporality().String()
	case pmetric.MetricTypeExponentialHistogram:
		temporality = m.ExponentialHistogram().AggregationTemporality().String()
	}
	return hashStrings(m.Name(), m.Type().String(), m.Unit(), temporality)
}

func (p *policyProcessor) limitFor(name string) int {
	for _, l := range p.limits {
		if l.match.MatchString(name) {
			return l.max
		}
	}
	return p.cfg.Cardinality.DefaultMaxSeries
}

// applyCardinality keeps at most the configured number of distinct series
// per metric. Points of series past the limit lose their attributes and
// fold into one overflow series per resource and scope, marked
// otel_metric_overflow="true", so totals stay correct while series stop
// growing.
//
// Admissions are sticky: a series keeps its slot while it keeps reporting,
// and gives it back only after a whole window without points. A tumbling
// window that forgot every series would let a series admitted in one
// window overflow in the next (or the reverse), adding its whole history
// to the overflow total.
func (p *policyProcessor) applyCardinality(ctx context.Context, md pmetric.Metrics, now time.Time) {
	if p.cfg.Cardinality.DefaultMaxSeries == 0 && len(p.limits) == 0 {
		return
	}
	window := p.cfg.Cardinality.Window
	if window == 0 {
		window = defaultWindow
	}
	var overflowed int64
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		resHash := hashMap(rm.Resource().Attributes())
		sms := rm.ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			scopeHash := hashStrings(sms.At(j).Scope().Name(), sms.At(j).Scope().Version())
			sms.At(j).Metrics().RemoveIf(func(m pmetric.Metric) bool {
				limit := p.limitFor(m.Name())
				if limit == 0 {
					return false
				}
				cs := p.cardShard(m.Name())
				defer cs.mu.Unlock()
				if cs.windowStart.IsZero() {
					cs.windowStart = now
				} else if now.Sub(cs.windowStart) >= window {
					cs.roll(now, window, p.limitFor)
				}
				seen := cs.seen[m.Name()]
				if seen == nil {
					seen = map[uint64]time.Time{}
					cs.seen[m.Name()] = seen
				}
				retired := cs.retired[m.Name()]
				inOverflow := cs.overflowed[m.Name()]
				if inOverflow == nil {
					inOverflow = map[uint64]time.Time{}
					cs.overflowed[m.Name()] = inOverflow
				}
				var lastKey uint64
				// admit reports whether the series may keep its own
				// points. A staleness marker never claims a free slot,
				// and neither does a series active in the overflow: its
				// history is in the overflow total already.
				admit := func(attrs pcommon.Map, stale bool) bool {
					key := mix(resHash, mix(scopeHash, hashMap(attrs)))
					lastKey = key
					if _, ok := seen[key]; ok {
						seen[key] = now
						return true
					}
					if stale {
						return false
					}
					if _, ok := inOverflow[key]; ok {
						inOverflow[key] = now
						return false
					}
					if len(seen) < limit {
						seen[key] = now
						return true
					}
					if len(inOverflow) < limit {
						inOverflow[key] = now
					}
					return false
				}
				if !mergeable(m) {
					// No safe way to fold these; drop the excess instead.
					n := removeOverLimit(m, admit)
					overflowed += int64(n)
					return dataPointCount(m) == 0
				}
				// Admitted points stay untouched; only points past the limit
				// are lifted out and folded into the overflow series.
				var over int64
				var excess []bool
				mm := newMergedMetric(m, "sum")
				baseKey := mix(resHash, mix(scopeHash, metricKey(m)))
				eachPoint(m, resHash, func(pt point) {
					if admit(pt.attrs, pt.stale) {
						excess = append(excess, false)
						return
					}
					excess = append(excess, true)
					over++
					if _, ok := retired[lastKey]; ok {
						// Admitted before: its history went out on its
						// own series already.
						pt.baseline = true
					}
					pt.attrs = pcommon.NewMap()
					pt.attrs.PutStr(overflowAttribute, "true")
					pt.attrs.PutStr(p.identKey, p.identValue)
					// One overflow series per output: a second bucket layout
					// would duplicate it, so its points are dropped.
					if pt.hist != nil && !pt.stale && !p.acc.primaryLayout(mix(baseKey, hashMap(pt.attrs)), hashFloats(pt.hist.bounds), now) {
						return
					}
					mm.add(pt)
				})
				if over == 0 {
					return false
				}
				overflowed += over
				removeByIndex(m, excess)
				// Overflow series are cumulative merges too; their state is
				// keyed apart from aggregation outputs by the overflow attrs.
				mm.writeTo(m, p.acc, baseKey, now)
				return dataPointCount(m) == 0
			})
		}
	}
	pruneEmpty(md)
	if overflowed > 0 {
		p.pointsOverflow.Add(ctx, overflowed, p.telemetryAttrs)
	}
}

// cardShard is the cardinality state of the metrics whose names hash to
// it, per metric: admitted series and when each was last seen, released
// series and until when they are remembered, and series in the overflow
// and when each was last seen there. Each map holds at most the metric's
// series limit.
type cardShard struct {
	mu          sync.Mutex
	windowStart time.Time
	seen        map[string]map[uint64]time.Time
	retired     map[string]map[uint64]time.Time
	overflowed  map[string]map[uint64]time.Time
}

// retiredWindows is how many windows a released series is remembered.
const retiredWindows = 6

// cardShard returns the locked cardinality shard of a metric.
func (p *policyProcessor) cardShard(name string) *cardShard {
	cs := &p.card[hashStrings(name)%numShards]
	cs.mu.Lock()
	return cs
}

// roll releases the slots of series not seen during the window that just
// ended. A released series is remembered for retiredWindows windows, so an
// overflow point of one of them is known to have been admitted before (its
// history went out on its own series). Overflow series not seen during the
// window are forgotten too, so they may claim a free slot again. The
// caller holds cs.mu.
func (cs *cardShard) roll(now time.Time, window time.Duration, limitFor func(string) int) {
	for name, retired := range cs.retired {
		for key, until := range retired {
			if !now.Before(until) {
				delete(retired, key)
			}
		}
		if len(retired) == 0 {
			delete(cs.retired, name)
		}
	}
	for name, seen := range cs.seen {
		limit := limitFor(name)
		for key, last := range seen {
			if last.Before(cs.windowStart) {
				delete(seen, key)
				retired := cs.retired[name]
				if retired == nil {
					retired = map[uint64]time.Time{}
					cs.retired[name] = retired
				}
				if _, ok := retired[key]; ok || len(retired) < limit {
					retired[key] = now.Add(retiredWindows * window)
				}
			}
		}
		if len(seen) == 0 {
			delete(cs.seen, name)
		}
	}
	for name, over := range cs.overflowed {
		for key, last := range over {
			if last.Before(cs.windowStart) {
				delete(over, key)
			}
		}
		if len(over) == 0 {
			delete(cs.overflowed, name)
		}
	}
	cs.windowStart = now
}

// removeOverLimit drops summary points of series past the limit: summaries
// cannot be folded into an overflow series.
func removeOverLimit(m pmetric.Metric, admit func(pcommon.Map, bool) bool) int {
	n := 0
	if m.Type() == pmetric.MetricTypeSummary {
		m.Summary().DataPoints().RemoveIf(func(dp pmetric.SummaryDataPoint) bool {
			if admit(dp.Attributes(), dp.Flags().NoRecordedValue()) {
				return false
			}
			n++
			return true
		})
	}
	return n
}

func invert(b []bool) []bool {
	out := make([]bool, len(b))
	for i, v := range b {
		out[i] = !v
	}
	return out
}

// removeByIndex removes the data points whose index is marked in remove.
// RemoveIf visits points in order, so a running index lines up with the
// order eachPoint produced.
func removeByIndex(m pmetric.Metric, remove []bool) {
	i := 0
	next := func() bool {
		r := i < len(remove) && remove[i]
		i++
		return r
	}
	switch m.Type() {
	case pmetric.MetricTypeSum:
		m.Sum().DataPoints().RemoveIf(func(pmetric.NumberDataPoint) bool { return next() })
	case pmetric.MetricTypeGauge:
		m.Gauge().DataPoints().RemoveIf(func(pmetric.NumberDataPoint) bool { return next() })
	case pmetric.MetricTypeHistogram:
		m.Histogram().DataPoints().RemoveIf(func(pmetric.HistogramDataPoint) bool { return next() })
	case pmetric.MetricTypeExponentialHistogram:
		m.ExponentialHistogram().DataPoints().RemoveIf(func(pmetric.ExponentialHistogramDataPoint) bool { return next() })
	}
}

func dataPointCount(m pmetric.Metric) int {
	switch m.Type() {
	case pmetric.MetricTypeSum:
		return m.Sum().DataPoints().Len()
	case pmetric.MetricTypeGauge:
		return m.Gauge().DataPoints().Len()
	case pmetric.MetricTypeHistogram:
		return m.Histogram().DataPoints().Len()
	case pmetric.MetricTypeExponentialHistogram:
		return m.ExponentialHistogram().DataPoints().Len()
	case pmetric.MetricTypeSummary:
		return m.Summary().DataPoints().Len()
	}
	return 0
}

// pruneEmpty removes metrics left without data points (a cumulative merge
// with nothing new to report emits none), then empty scopes and resources.
func pruneEmpty(md pmetric.Metrics) {
	md.ResourceMetrics().RemoveIf(func(rm pmetric.ResourceMetrics) bool {
		rm.ScopeMetrics().RemoveIf(func(sm pmetric.ScopeMetrics) bool {
			sm.Metrics().RemoveIf(func(m pmetric.Metric) bool { return dataPointCount(m) == 0 })
			return sm.Metrics().Len() == 0
		})
		return rm.ScopeMetrics().Len() == 0
	})
}
