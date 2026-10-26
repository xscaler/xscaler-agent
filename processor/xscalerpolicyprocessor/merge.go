package xscalerpolicyprocessor

import (
	"hash/fnv"
	"math"
	"sort"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// point is one data point lifted out of pdata so it can be merged with
// others. orig identifies the series it came from: a batch may carry several
// points of one series, and only the latest of those may count, or a counter
// would be summed with its own earlier value.
type point struct {
	orig      uint64
	attrs     pcommon.Map
	start, ts pcommon.Timestamp
	isInt     bool
	i         int64
	d         float64
	hist      *histPoint
	exp       *expPoint
	// stale marks a point flagged NoRecordedValue: the prometheus receiver
	// sends one when a scrape fails or a target disappears. It carries no
	// value (histogram buckets are zeroed) and must never count as one.
	stale bool
	// baseline makes a source that joins a cumulative output count from
	// its current value instead of in full: set for cardinality overflow
	// points of series that were admitted before, whose history was
	// already emitted on their own series.
	baseline bool
}

type histPoint struct {
	count          uint64
	sum            float64
	hasSum         bool
	min, max       float64
	hasMin, hasMax bool
	bounds         []float64
	buckets        []uint64
}

func (p point) float() float64 {
	if p.isInt {
		return float64(p.i)
	}
	return p.d
}

// group collects the points that share one output series.
type group struct {
	attrs pcommon.Map
	all   []point
}

func (g *group) add(p point) {
	g.all = append(g.all, p)
}

// mergeMode is how points of one output series combine.
type mergeMode int

const (
	// modeLatest combines each live source's latest point across batches:
	// gauges and non-monotonic cumulative sums, whose values are levels,
	// not totals.
	modeLatest mergeMode = iota
	// modeDelta adds every point: delta sums and delta histograms.
	modeDelta
	// modeCumulative accumulates per-source deltas into a running total:
	// cumulative monotonic sums and cumulative histograms.
	modeCumulative
)

func modeOf(m pmetric.Metric) mergeMode {
	switch m.Type() {
	case pmetric.MetricTypeSum:
		switch {
		case m.Sum().AggregationTemporality() == pmetric.AggregationTemporalityDelta:
			return modeDelta
		case m.Sum().IsMonotonic():
			return modeCumulative
		}
	case pmetric.MetricTypeHistogram:
		if m.Histogram().AggregationTemporality() == pmetric.AggregationTemporalityDelta {
			return modeDelta
		}
		return modeCumulative
	case pmetric.MetricTypeExponentialHistogram:
		if m.ExponentialHistogram().AggregationTemporality() == pmetric.AggregationTemporalityDelta {
			return modeDelta
		}
		return modeCumulative
	}
	return modeLatest
}

// mergedMetric is one output metric: its metadata plus grouped points.
type mergedMetric struct {
	proto   pmetric.Metric
	mode    mergeMode
	gaugeFn string
	groups  map[uint64]*group
	order   []uint64
}

func newMergedMetric(src pmetric.Metric, gaugeFn string) *mergedMetric {
	proto := pmetric.NewMetric()
	proto.SetName(src.Name())
	proto.SetDescription(src.Description())
	proto.SetUnit(src.Unit())
	switch src.Type() {
	case pmetric.MetricTypeSum:
		s := proto.SetEmptySum()
		s.SetAggregationTemporality(src.Sum().AggregationTemporality())
		s.SetIsMonotonic(src.Sum().IsMonotonic())
	case pmetric.MetricTypeGauge:
		proto.SetEmptyGauge()
	case pmetric.MetricTypeHistogram:
		proto.SetEmptyHistogram().SetAggregationTemporality(src.Histogram().AggregationTemporality())
	case pmetric.MetricTypeExponentialHistogram:
		proto.SetEmptyExponentialHistogram().SetAggregationTemporality(src.ExponentialHistogram().AggregationTemporality())
	}
	if gaugeFn == "" {
		gaugeFn = "avg"
	}
	return &mergedMetric{proto: proto, mode: modeOf(src), gaugeFn: gaugeFn, groups: map[uint64]*group{}}
}

func (m *mergedMetric) add(p point) {
	key := hashMap(p.attrs)
	if p.hist != nil {
		key = mix(key, hashFloats(p.hist.bounds))
	}
	g, ok := m.groups[key]
	if !ok {
		g = &group{attrs: p.attrs}
		m.groups[key] = g
		m.order = append(m.order, key)
	}
	g.add(p)
}

// mergeable reports whether points of this metric type can be merged.
// Summaries cannot: quantiles of different sources do not combine.
func mergeable(m pmetric.Metric) bool {
	switch m.Type() {
	case pmetric.MetricTypeSum, pmetric.MetricTypeGauge, pmetric.MetricTypeHistogram, pmetric.MetricTypeExponentialHistogram:
		return true
	}
	return false
}

// eachPoint calls fn with every data point of a mergeable metric, lifted.
// attrs is a fresh copy the caller may modify.
func eachPoint(m pmetric.Metric, resHash uint64, fn func(point)) {
	number := func(dps pmetric.NumberDataPointSlice) {
		for i := 0; i < dps.Len(); i++ {
			dp := dps.At(i)
			attrs := pcommon.NewMap()
			dp.Attributes().CopyTo(attrs)
			p := point{orig: mix(resHash, hashMap(dp.Attributes())), attrs: attrs, start: dp.StartTimestamp(), ts: dp.Timestamp(), stale: dp.Flags().NoRecordedValue()}
			if dp.ValueType() == pmetric.NumberDataPointValueTypeInt {
				p.isInt, p.i = true, dp.IntValue()
			} else {
				p.d = dp.DoubleValue()
			}
			fn(p)
		}
	}
	switch m.Type() {
	case pmetric.MetricTypeSum:
		number(m.Sum().DataPoints())
	case pmetric.MetricTypeGauge:
		number(m.Gauge().DataPoints())
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			dp := dps.At(i)
			attrs := pcommon.NewMap()
			dp.Attributes().CopyTo(attrs)
			h := &histPoint{
				count:   dp.Count(),
				sum:     dp.Sum(),
				hasSum:  dp.HasSum(),
				min:     dp.Min(),
				max:     dp.Max(),
				hasMin:  dp.HasMin(),
				hasMax:  dp.HasMax(),
				bounds:  dp.ExplicitBounds().AsRaw(),
				buckets: dp.BucketCounts().AsRaw(),
			}
			fn(point{orig: mix(resHash, hashMap(dp.Attributes())), attrs: attrs, start: dp.StartTimestamp(), ts: dp.Timestamp(), hist: h, stale: dp.Flags().NoRecordedValue()})
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			dp := dps.At(i)
			attrs := pcommon.NewMap()
			dp.Attributes().CopyTo(attrs)
			fn(point{orig: mix(resHash, hashMap(dp.Attributes())), attrs: attrs, start: dp.StartTimestamp(), ts: dp.Timestamp(), exp: liftExp(dp), stale: dp.Flags().NoRecordedValue()})
		}
	}
}

// writeTo appends the merged points to dst, which must have the same type as
// the metric the points came from. Cumulative outputs go through acc under
// mix(baseKey, group key), so baseKey must identify the output resource,
// scope and metric. It returns how many points acc refused.
func (m *mergedMetric) writeTo(dst pmetric.Metric, acc *accumulator, baseKey uint64, now time.Time) int {
	refused := 0
	for _, key := range m.order {
		g := m.groups[key]
		var out point
		switch m.mode {
		case modeCumulative:
			p, ok, r := acc.accumulate(mix(baseKey, key), g.all, now)
			refused += r
			if !ok {
				continue
			}
			out = p
		case modeDelta:
			p, ok, r := acc.deltaWindow(mix(baseKey, key), g.all, dst.Type(), now)
			refused += r
			if !ok {
				continue
			}
			out = p
		default:
			p, ok, r := acc.latest(mix(baseKey, key), g.all, dst.Type(), m.gaugeFn, now)
			refused += r
			if !ok {
				continue
			}
			out = p
		}
		switch dst.Type() {
		case pmetric.MetricTypeSum:
			dp := dst.Sum().DataPoints().AppendEmpty()
			g.attrs.CopyTo(dp.Attributes())
			dp.SetStartTimestamp(out.start)
			dp.SetTimestamp(out.ts)
			setNumber(dp, out)
			if out.stale {
				dp.SetFlags(pmetric.DefaultDataPointFlags.WithNoRecordedValue(true))
			}
		case pmetric.MetricTypeGauge:
			dp := dst.Gauge().DataPoints().AppendEmpty()
			g.attrs.CopyTo(dp.Attributes())
			dp.SetStartTimestamp(out.start)
			dp.SetTimestamp(out.ts)
			setNumber(dp, out)
			if out.stale {
				dp.SetFlags(pmetric.DefaultDataPointFlags.WithNoRecordedValue(true))
			}
		case pmetric.MetricTypeHistogram:
			dp := dst.Histogram().DataPoints().AppendEmpty()
			g.attrs.CopyTo(dp.Attributes())
			dp.SetStartTimestamp(out.start)
			dp.SetTimestamp(out.ts)
			writeHistogram(dp, out.hist)
		case pmetric.MetricTypeExponentialHistogram:
			dp := dst.ExponentialHistogram().DataPoints().AppendEmpty()
			g.attrs.CopyTo(dp.Attributes())
			dp.SetStartTimestamp(out.start)
			dp.SetTimestamp(out.ts)
			writeExp(dp, out.exp)
		}
	}
	return refused
}

// combine folds points without state: sums for sums, the gauge function for
// gauges, bucket-wise addition for histograms.
func combine(pts []point, typ pmetric.MetricType, gaugeFn string) point {
	switch typ {
	case pmetric.MetricTypeGauge:
		return gaugePoints(pts, gaugeFn)
	case pmetric.MetricTypeHistogram:
		return point{hist: sumHistograms(pts)}
	case pmetric.MetricTypeExponentialHistogram:
		return point{exp: sumExp(pts)}
	}
	return sumPoints(pts)
}

func span(pts []point) (pcommon.Timestamp, pcommon.Timestamp) {
	var start, ts pcommon.Timestamp
	for _, p := range pts {
		if p.start != 0 && (start == 0 || p.start < start) {
			start = p.start
		}
		if p.ts > ts {
			ts = p.ts
		}
	}
	return start, ts
}

func setNumber(dp pmetric.NumberDataPoint, p point) {
	if p.isInt {
		dp.SetIntValue(p.i)
	} else {
		dp.SetDoubleValue(p.d)
	}
}

func allInt(pts []point) bool {
	for _, p := range pts {
		if !p.isInt {
			return false
		}
	}
	return true
}

func sumPoints(pts []point) point {
	if allInt(pts) {
		var s int64
		for _, p := range pts {
			s += p.i
		}
		return point{isInt: true, i: s}
	}
	var s float64
	for _, p := range pts {
		s += p.float()
	}
	return point{d: s}
}

func gaugePoints(pts []point, fn string) point {
	switch fn {
	case "sum":
		return sumPoints(pts)
	case "last":
		last := pts[0]
		for _, p := range pts[1:] {
			if p.ts >= last.ts {
				last = p
			}
		}
		return point{isInt: last.isInt, i: last.i, d: last.d}
	case "min", "max":
		best := pts[0]
		for _, p := range pts[1:] {
			if (fn == "min" && p.float() < best.float()) || (fn == "max" && p.float() > best.float()) {
				best = p
			}
		}
		return point{isInt: best.isInt, i: best.i, d: best.d}
	default: // avg
		var s float64
		for _, p := range pts {
			s += p.float()
		}
		return point{d: s / float64(len(pts))}
	}
}

// sumHistograms adds histograms with identical bounds bucket by bucket.
// Min and max survive only when every input has them.
func sumHistograms(pts []point) *histPoint {
	first := pts[0].hist
	out := &histPoint{bounds: first.bounds, buckets: make([]uint64, len(first.buckets)), hasSum: true, hasMin: true, hasMax: true, min: math.Inf(1), max: math.Inf(-1)}
	for _, p := range pts {
		h := p.hist
		out.count += h.count
		out.sum += h.sum
		out.hasSum = out.hasSum && h.hasSum
		out.hasMin = out.hasMin && h.hasMin
		out.hasMax = out.hasMax && h.hasMax
		if h.hasMin && h.min < out.min {
			out.min = h.min
		}
		if h.hasMax && h.max > out.max {
			out.max = h.max
		}
		for i := range out.buckets {
			if i < len(h.buckets) {
				out.buckets[i] += h.buckets[i]
			}
		}
	}
	return out
}

func writeHistogram(dp pmetric.HistogramDataPoint, h *histPoint) {
	dp.SetCount(h.count)
	if h.hasSum {
		dp.SetSum(h.sum)
	}
	if h.hasMin {
		dp.SetMin(h.min)
	}
	if h.hasMax {
		dp.SetMax(h.max)
	}
	dp.ExplicitBounds().FromRaw(h.bounds)
	dp.BucketCounts().FromRaw(h.buckets)
}

// hashMap hashes attributes independent of insertion order.
func hashMap(m pcommon.Map) uint64 {
	keys := make([]string, 0, m.Len())
	m.Range(func(k string, _ pcommon.Value) bool {
		keys = append(keys, k)
		return true
	})
	sort.Strings(keys)
	h := fnv.New64a()
	for _, k := range keys {
		v, _ := m.Get(k)
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(v.AsString()))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

func hashStrings(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

func hashFloats(fs []float64) uint64 {
	h := fnv.New64a()
	var b [8]byte
	for _, f := range fs {
		u := math.Float64bits(f)
		for i := range b {
			b[i] = byte(u >> (8 * i))
		}
		_, _ = h.Write(b[:])
	}
	return h.Sum64()
}

func mix(a, b uint64) uint64 {
	return a*1099511628211 ^ b
}
