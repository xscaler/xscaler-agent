package xscalerpolicyprocessor

import (
	"math"

	"go.opentelemetry.io/collector/pdata/pmetric"
)

// maxExpBuckets caps the buckets per side of a merged exponential
// histogram. Merging sources whose ranges barely overlap would otherwise
// grow the arrays without bound; past the cap the merge drops to a coarser
// scale. 160 is the OpenTelemetry SDK default.
const maxExpBuckets = 160

// expPoint is an exponential histogram data point lifted out of pdata.
type expPoint struct {
	scale          int32
	zeroThreshold  float64
	zeroCount      uint64
	count          uint64
	sum            float64
	hasSum         bool
	min, max       float64
	hasMin, hasMax bool
	pos, neg       expBuckets
}

// expBuckets is one side of an exponential histogram: counts[i] belongs to
// bucket index offset+i.
type expBuckets struct {
	offset int32
	counts []uint64
}

func (b expBuckets) clone() expBuckets {
	return expBuckets{offset: b.offset, counts: append([]uint64(nil), b.counts...)}
}

// downscale maps the buckets by scale steps coarser: index i becomes i>>by.
func (b expBuckets) downscale(by int32) expBuckets {
	if by <= 0 || len(b.counts) == 0 {
		return b.clone()
	}
	first := b.offset >> by
	last := (b.offset + int32(len(b.counts)) - 1) >> by
	out := expBuckets{offset: first, counts: make([]uint64, last-first+1)}
	for i, c := range b.counts {
		out.counts[((b.offset+int32(i))>>by)-first] += c
	}
	return out
}

// addBuckets adds two bucket arrays of the same scale.
func addBuckets(a, b expBuckets) expBuckets {
	if len(a.counts) == 0 {
		return b.clone()
	}
	if len(b.counts) == 0 {
		return a.clone()
	}
	first := min(a.offset, b.offset)
	last := max(a.offset+int32(len(a.counts)), b.offset+int32(len(b.counts)))
	out := expBuckets{offset: first, counts: make([]uint64, last-first)}
	for i, c := range a.counts {
		out.counts[a.offset+int32(i)-first] += c
	}
	for i, c := range b.counts {
		out.counts[b.offset+int32(i)-first] += c
	}
	return out
}

// subBuckets returns cur - prev for arrays of the same scale, or false when
// any bucket of cur is below prev (a reset).
func subBuckets(cur, prev expBuckets) (expBuckets, bool) {
	out := cur.clone()
	for i, c := range prev.counts {
		if c == 0 {
			continue
		}
		j := prev.offset + int32(i) - cur.offset
		if j < 0 || int(j) >= len(cur.counts) || cur.counts[j] < c {
			return expBuckets{}, false
		}
		out.counts[j] -= c
	}
	return out, true
}

// span is the number of bucket indexes between the first and last non-zero
// bucket.
func (b expBuckets) span() int {
	first, last := -1, -1
	for i, c := range b.counts {
		if c != 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return 0
	}
	return last - first + 1
}

func (e *expPoint) clone() *expPoint {
	c := *e
	c.pos, c.neg = e.pos.clone(), e.neg.clone()
	return &c
}

// downscaleTo lowers e's scale to scale (never raises it).
func (e *expPoint) downscaleTo(scale int32) {
	if scale >= e.scale {
		return
	}
	by := e.scale - scale
	e.pos, e.neg = e.pos.downscale(by), e.neg.downscale(by)
	e.scale = scale
}

// widenZero raises the zero threshold to t, moving every bucket whose upper
// boundary is at or below t into the zero bucket.
func (e *expPoint) widenZero(t float64) {
	if t <= e.zeroThreshold {
		return
	}
	e.zeroThreshold = t
	factor := math.Exp2(-float64(e.scale))
	move := func(b *expBuckets) {
		n := 0
		for n < len(b.counts) && math.Exp2(float64(b.offset+int32(n)+1)*factor) <= t {
			e.zeroCount += b.counts[n]
			n++
		}
		b.counts = b.counts[n:]
		b.offset += int32(n)
	}
	move(&e.pos)
	move(&e.neg)
}

// fit drops to coarser scales until both sides fit maxExpBuckets.
func (e *expPoint) fit() {
	for e.pos.span() > maxExpBuckets || e.neg.span() > maxExpBuckets {
		e.downscaleTo(e.scale - 1)
	}
}

// alignExp returns copies of a and b at a common scale and zero threshold.
func alignExp(a, b *expPoint) (*expPoint, *expPoint) {
	a, b = a.clone(), b.clone()
	s := min(a.scale, b.scale)
	a.downscaleTo(s)
	b.downscaleTo(s)
	t := max(a.zeroThreshold, b.zeroThreshold)
	a.widenZero(t)
	b.widenZero(t)
	return a, b
}

// addExp merges two exponential histograms, downscaling to the coarser
// scale and widening to the larger zero threshold.
func addExp(a, b *expPoint) *expPoint {
	if a == nil {
		return b.clone()
	}
	a, b = alignExp(a, b)
	out := &expPoint{
		scale:         a.scale,
		zeroThreshold: a.zeroThreshold,
		zeroCount:     a.zeroCount + b.zeroCount,
		count:         a.count + b.count,
		sum:           a.sum + b.sum,
		hasSum:        a.hasSum && b.hasSum,
		hasMin:        a.hasMin && b.hasMin,
		hasMax:        a.hasMax && b.hasMax,
		min:           min(a.min, b.min),
		max:           max(a.max, b.max),
		pos:           addBuckets(a.pos, b.pos),
		neg:           addBuckets(a.neg, b.neg),
	}
	out.fit()
	return out
}

// subExp returns cur - prev of one cumulative source, or false when cur
// does not continue prev (a reset). A cumulative exponential histogram
// only ever downscales, so a finer scale on cur is a reset too.
func subExp(cur, prev *expPoint) (*expPoint, bool) {
	if prev == nil || cur.scale > prev.scale || cur.count < prev.count {
		return nil, false
	}
	c, p := alignExp(cur, prev)
	if c.zeroCount < p.zeroCount {
		return nil, false
	}
	pos, ok := subBuckets(c.pos, p.pos)
	if !ok {
		return nil, false
	}
	neg, ok := subBuckets(c.neg, p.neg)
	if !ok {
		return nil, false
	}
	return &expPoint{
		scale:         c.scale,
		zeroThreshold: c.zeroThreshold,
		zeroCount:     c.zeroCount - p.zeroCount,
		count:         c.count - p.count,
		sum:           c.sum - p.sum,
		hasSum:        c.hasSum && p.hasSum,
		pos:           pos,
		neg:           neg,
	}, true
}

func sumExp(pts []point) *expPoint {
	var out *expPoint
	for _, p := range pts {
		out = addExp(out, p.exp)
	}
	return out
}

func liftExp(dp pmetric.ExponentialHistogramDataPoint) *expPoint {
	return &expPoint{
		scale:         dp.Scale(),
		zeroThreshold: dp.ZeroThreshold(),
		zeroCount:     dp.ZeroCount(),
		count:         dp.Count(),
		sum:           dp.Sum(),
		hasSum:        dp.HasSum(),
		min:           dp.Min(),
		max:           dp.Max(),
		hasMin:        dp.HasMin(),
		hasMax:        dp.HasMax(),
		pos:           expBuckets{offset: dp.Positive().Offset(), counts: dp.Positive().BucketCounts().AsRaw()},
		neg:           expBuckets{offset: dp.Negative().Offset(), counts: dp.Negative().BucketCounts().AsRaw()},
	}
}

func writeExp(dp pmetric.ExponentialHistogramDataPoint, e *expPoint) {
	dp.SetScale(e.scale)
	dp.SetZeroThreshold(e.zeroThreshold)
	dp.SetZeroCount(e.zeroCount)
	dp.SetCount(e.count)
	if e.hasSum {
		dp.SetSum(e.sum)
	}
	if e.hasMin {
		dp.SetMin(e.min)
	}
	if e.hasMax {
		dp.SetMax(e.max)
	}
	dp.Positive().SetOffset(e.pos.offset)
	dp.Positive().BucketCounts().FromRaw(e.pos.counts)
	dp.Negative().SetOffset(e.neg.offset)
	dp.Negative().BucketCounts().FromRaw(e.neg.counts)
}
