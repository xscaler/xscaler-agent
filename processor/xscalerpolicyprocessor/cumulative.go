package xscalerpolicyprocessor

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// accumulator holds the cross-batch state behind every stateful merge.
// Receivers deliver sources in separate batches (the prometheus receiver
// and OTLP pushes send one pod per batch), so an output can only be right
// if it remembers the sources it is not seeing in this batch.
//
// For cumulative outputs it turns several cumulative source series into one
// cumulative output series that never decreases. Summing each source's latest value is
// wrong: when one pod restarts its counter drops to zero, the sum drops, and
// rate() reads that as a reset of the whole merged series and reports a
// spike. Instead each source's last value is kept, every new point becomes a
// per-source delta (a drop or a start-time change is a reset, so the delta is
// the new value), and deltas add into a running total with a stable start
// time.
//
// State is bounded: sources not seen within ttl are forgotten, and at most
// maxSources sources are tracked across all outputs. Points of sources past
// that cap are refused (the caller counts them as overflow). A forgotten
// source leaves a tombstone (its start time and last value) for
// tombstoneTTLs * ttl, so if it comes back with the same start time it only
// adds its increase since that value instead of its whole history again.
// An output left without sources is parked for as long: its running total,
// start time and last timestamp are kept, and if any source comes back the
// output resumes the same series instead of restarting below its total.
//
// State is split into shards by output key, each with its own lock, so
// batches of different pipelines touching different outputs do not wait
// for each other; only the source budget is shared (atomically). Sweeps
// are incremental: each batch sweeps at most sweepBudget entries' worth of
// shards that are due, instead of walking all state under one lock.
//
// Level sources (gauges and non-monotonic sums) expire sooner, after
// levelStale, so a pod that is gone stops counting in the combination
// about as soon as Prometheus would stop returning it. Delta outputs keep
// the end of their last window so windows from separate batches never
// overlap; each one takes one slot of the source budget.
type accumulator struct {
	ttl        time.Duration
	levelStale time.Duration
	warmup     time.Duration
	maxSources int
	procStart  atomic.Int64 // unix nanos of the first batch; 0 until then
	sources    atomic.Int64
	// Entries in the bounded side maps, across shards; each is capped at
	// maxSources.
	nTombstones, nBorn, nLayouts atomic.Int64
	// clamped counts input points timestamped beyond now + futureGrace;
	// the processor drains it into a self-metric after each batch.
	clamped   atomic.Int64
	sweepNext atomic.Uint64
	shards    [numShards]accShard
}

// numShards is the number of independently locked state shards.
const numShards = 64

// sweepBudget is roughly how many state entries one batch sweeps at most.
const sweepBudget = 4096

type accShard struct {
	mu         sync.Mutex
	series     map[uint64]*outputSeries
	tombstones map[uint64]tombstone
	// born parks removed cumulative outputs (totals, start time, first
	// creation), so recreating one resumes the same series and does not
	// restart its warm-up hold-back.
	born      map[uint64]parked
	layouts   map[uint64]*layout
	deltas    map[uint64]*deltaSeries
	lastSweep atomic.Int64 // unix nanos
}

// shard picks the shard of an output (or layout) key and locks it.
func (a *accumulator) shard(key uint64) *accShard {
	sh := &a.shards[(key^key>>29^key>>47)%numShards]
	sh.mu.Lock()
	return sh
}

// parked is a removed cumulative output, kept so it can resume.
type parked struct {
	s     *outputSeries // sources is nil while parked
	until time.Time
}

// deltaSeries is the state of one delta output: where its last emitted
// window ended.
type deltaSeries struct {
	lastEnd pcommon.Timestamp
	seen    time.Time
}

// layout records which histogram bucket layout an output uses. A second
// layout under the same output attributes (bounds changed during a rollout)
// stays unaggregated until the first one expires.
type layout struct {
	bounds uint64
	seen   time.Time
}

type tombstone struct {
	start  pcommon.Timestamp
	last   point
	lastTS pcommon.Timestamp
	until  time.Time
}

// tombstoneTTLs is how many ttls a forgotten source's start time is kept.
const tombstoneTTLs = 6

// futureGrace is how far past the collector's clock an output timestamp
// may go. A source whose clock runs ahead would otherwise ratchet the
// output's last timestamp forward and pin it in the future, where Mimir
// rejects it (and every later point, which must not go back in time).
const futureGrace = time.Minute

// tsLimit is the latest timestamp an output may carry at now.
func tsLimit(now time.Time) pcommon.Timestamp {
	return pcommon.NewTimestampFromTime(now.Add(futureGrace))
}

// clampTS caps a point's timestamp at limit, counting it when it does.
func (a *accumulator) clampTS(ts, limit pcommon.Timestamp) pcommon.Timestamp {
	if ts > limit {
		a.clamped.Add(1)
		return limit
	}
	return ts
}

type outputSeries struct {
	// created is when this incarnation of the output appeared; sources
	// joining within the warm-up of it count in full. born is when the
	// output first appeared (kept across incarnations); the output holds
	// back until the warm-up after born is over.
	created  time.Time
	born     time.Time
	level    bool
	start    pcommon.Timestamp
	lastEmit pcommon.Timestamp
	isInt    bool
	i        int64
	d        float64
	hist     *histPoint // running totals; min and max are not kept
	exp      *expPoint
	sources  map[uint64]*sourceState
}

type sourceState struct {
	start  pcommon.Timestamp
	last   point
	lastTS pcommon.Timestamp
	seen   time.Time
}

func newAccumulator(ttl, levelStale, warmup time.Duration, maxSources int) *accumulator {
	if levelStale <= 0 || levelStale > ttl {
		levelStale = ttl
	}
	a := &accumulator{ttl: ttl, levelStale: levelStale, warmup: warmup, maxSources: maxSources}
	for i := range a.shards {
		sh := &a.shards[i]
		sh.series = map[uint64]*outputSeries{}
		sh.tombstones = map[uint64]tombstone{}
		sh.born = map[uint64]parked{}
		sh.layouts = map[uint64]*layout{}
		sh.deltas = map[uint64]*deltaSeries{}
	}
	return a
}

func (a *accumulator) full() bool {
	return a.maxSources > 0 && a.sources.Load() >= int64(a.maxSources)
}

// room reports whether a capped side map counted by n may grow.
func (a *accumulator) room(n *atomic.Int64) bool {
	return a.maxSources <= 0 || n.Load() < int64(a.maxSources)
}

// reserve takes one slot of the source budget.
func (a *accumulator) reserve() bool {
	if a.sources.Add(1) > int64(a.maxSources) && a.maxSources > 0 {
		a.sources.Add(-1)
		return false
	}
	return true
}

// started records when the processor saw its first batch, once.
func (a *accumulator) started(now time.Time) {
	a.procStart.CompareAndSwap(0, now.UnixNano())
}

// newOutput starts an output series, or resumes a parked one (reported by
// the second result): a cumulative output whose sources all expired keeps
// its start time and total, so it continues as the same series. A new
// output's start time is when the processor started for outputs created
// during the start-up warm-up, otherwise its creation time: after a
// collector restart (or once a parked output expired) the output is a new
// series and says so, instead of reusing an older start time with a lower
// value. It never comes after the earliest point (start < timestamp).
func (a *accumulator) newOutput(sh *accShard, key uint64, pts []point, now time.Time, resume bool) (*outputSeries, bool) {
	if b, ok := sh.born[key]; ok && resume {
		delete(sh.born, key)
		a.nBorn.Add(-1)
		s := b.s
		// Past its warm-up (removal takes a TTL, which is at least the
		// warm-up), so joining sources follow the steady-state rules.
		s.created = s.born
		s.sources = map[uint64]*sourceState{}
		return s, true
	}
	base := now
	if ps := a.procStart.Load(); ps != 0 && now.Sub(time.Unix(0, ps)) < a.warmup {
		base = time.Unix(0, ps)
	}
	start := pcommon.NewTimestampFromTime(base)
	for _, p := range pts {
		if p.ts != 0 && p.ts <= start {
			start = p.ts - 1
		}
	}
	return &outputSeries{created: now, born: now, start: start, isInt: true, sources: map[uint64]*sourceState{}}, false
}

// primaryLayout reports whether bounds is the layout output key uses,
// claiming it when the output has none. Claims share the source budget:
// once it is used up no new layout is recorded (the points of a new output
// would be refused anyway), so the map stays bounded by max_sources.
func (a *accumulator) primaryLayout(key, bounds uint64, now time.Time) bool {
	sh := a.shard(key)
	defer sh.mu.Unlock()
	l, ok := sh.layouts[key]
	if !ok {
		if a.full() || !a.room(&a.nLayouts) {
			return true
		}
		sh.layouts[key] = &layout{bounds: bounds, seen: now}
		a.nLayouts.Add(1)
		return true
	}
	if l.bounds != bounds {
		return false
	}
	l.seen = now
	return true
}

// track registers a source not seen before, refusing it past the cap.
func (a *accumulator) track(s *outputSeries, orig uint64) (*sourceState, bool) {
	if !a.reserve() {
		return nil, false
	}
	st := &sourceState{}
	s.sources[orig] = st
	return st, true
}

// emitTS keeps an output's timestamps moving forward. Sources' clocks
// differ, so the newest point of a batch can be older than what the output
// already reported, and Mimir rejects samples that go back in time or
// repeat a timestamp.
func (s *outputSeries) emitTS(ts pcommon.Timestamp) pcommon.Timestamp {
	if ts <= s.lastEmit {
		ts = s.lastEmit + pcommon.Timestamp(time.Millisecond)
	}
	s.lastEmit = ts
	return ts
}

// latest combines the newest point of every live source of the output
// under key: gauges and non-monotonic sums, whose values are levels. A
// batch carrying one pod updates that pod and reports the combination over
// all pods still within the TTL, so the output does not flap between
// per-pod values.
//
// A staleness marker (NoRecordedValue) removes its source at once, and a
// source not seen within levelStale stops counting. When the last source
// goes away through a staleness marker the output emits a staleness marker
// of its own.
func (a *accumulator) latest(key uint64, pts []point, typ pmetric.MetricType, gaugeFn string, now time.Time) (point, bool, int) {
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].ts < pts[j].ts })
	sh := a.shard(key)
	defer sh.mu.Unlock()
	s, ok := sh.series[key]
	if !ok {
		s, _ = a.newOutput(sh, key, pts, now, false)
		s.level = true
	}
	for src, st := range s.sources {
		if now.Sub(st.seen) >= a.levelStale {
			delete(s.sources, src)
			a.sources.Add(-1)
		}
	}
	refused := 0
	updated := false
	limit := tsLimit(now)
	var staleTS pcommon.Timestamp
	for _, p := range pts {
		st, known := s.sources[p.orig]
		if p.stale {
			if known && p.ts > st.lastTS {
				delete(s.sources, p.orig)
				a.sources.Add(-1)
				updated = true
				staleTS = max(staleTS, a.clampTS(p.ts, limit))
			}
			continue
		}
		if !known {
			if st, known = a.track(s, p.orig); !known {
				refused++
				continue
			}
		} else if p.ts <= st.lastTS {
			st.seen = now
			continue
		}
		a.clampTS(p.ts, limit) // counted here, capped below
		st.start, st.last, st.lastTS, st.seen = p.start, p, p.ts, now
		updated = true
	}
	if len(s.sources) == 0 {
		delete(sh.series, key)
		if !ok || !updated {
			return point{}, false, refused
		}
		// Every source of the output is gone: say so, the way the source
		// did, so backends mark the series stale instead of keeping the
		// last value for their lookback window.
		out := point{stale: true, start: s.start, ts: s.emitTS(staleTS)}
		if typ == pmetric.MetricTypeGauge {
			out.start = 0
		}
		return out, true, refused
	}
	sh.series[key] = s
	if !updated {
		return point{}, false, refused
	}
	live := make([]point, 0, len(s.sources))
	ts := staleTS
	for _, st := range s.sources {
		live = append(live, st.last)
		if st.lastTS > ts {
			ts = st.lastTS
		}
	}
	// Map order is random; sort so "last" and ties are deterministic.
	sort.Slice(live, func(i, j int) bool {
		if live[i].ts != live[j].ts {
			return live[i].ts < live[j].ts
		}
		return live[i].orig < live[j].orig
	})
	out := combine(live, typ, gaugeFn)
	out.start, out.ts = s.start, s.emitTS(min(ts, limit))
	if typ == pmetric.MetricTypeGauge {
		out.start = 0
	}
	return out, true, refused
}

// sweep forgets state not seen within its TTL. Each call sweeps the shards
// that are due (last swept at least ttl/10 ago), starting where the last
// call stopped, until about sweepBudget entries were visited, so a batch
// never walks all state.
func (a *accumulator) sweep(now time.Time) {
	if a.ttl <= 0 {
		return
	}
	due := now.Add(-a.ttl / 10).UnixNano()
	work := 0
	start := a.sweepNext.Load()
	for i := uint64(0); i < numShards && work < sweepBudget; i++ {
		idx := (start + i) % numShards
		sh := &a.shards[idx]
		if sh.lastSweep.Load() > due {
			continue
		}
		sh.mu.Lock()
		if sh.lastSweep.Load() <= due {
			work += a.sweepShard(sh, now)
			sh.lastSweep.Store(now.UnixNano())
		}
		sh.mu.Unlock()
		a.sweepNext.Store(idx + 1)
	}
}

// sweepShard forgets the shard's sources not seen within their TTL and
// outputs left without sources, and returns how many entries it visited.
// The caller holds sh.mu.
func (a *accumulator) sweepShard(sh *accShard, now time.Time) int {
	visited := len(sh.tombstones) + len(sh.born) + len(sh.layouts) + len(sh.deltas) + len(sh.series)
	for key, tb := range sh.tombstones {
		if !now.Before(tb.until) {
			delete(sh.tombstones, key)
			a.nTombstones.Add(-1)
		}
	}
	for key, b := range sh.born {
		if !now.Before(b.until) {
			delete(sh.born, key)
			a.nBorn.Add(-1)
		}
	}
	for key, l := range sh.layouts {
		if now.Sub(l.seen) >= a.ttl {
			delete(sh.layouts, key)
			a.nLayouts.Add(-1)
		}
	}
	for key, d := range sh.deltas {
		if now.Sub(d.seen) >= a.ttl {
			delete(sh.deltas, key)
			a.sources.Add(-1)
		}
	}
	for key, s := range sh.series {
		ttl := a.ttl
		if s.level {
			ttl = a.levelStale
		}
		visited += len(s.sources)
		var lastSeen time.Time
		for src, st := range s.sources {
			if now.Sub(st.seen) >= ttl {
				if st.seen.After(lastSeen) {
					lastSeen = st.seen
				}
				delete(s.sources, src)
				a.sources.Add(-1)
				// Tombstones share the source budget so they stay bounded.
				if tk := mix(key, src); !s.level && a.room(&a.nTombstones) {
					if _, dup := sh.tombstones[tk]; !dup {
						a.nTombstones.Add(1)
					}
					last := st.last
					last.attrs = pcommon.Map{}
					sh.tombstones[tk] = tombstone{start: st.start, last: last, lastTS: st.lastTS, until: now.Add(tombstoneTTLs * a.ttl)}
				}
			}
		}
		if len(s.sources) == 0 {
			delete(sh.series, key)
			// Parked for tombstoneTTLs * ttl after its last point, so a
			// late sweep does not keep it longer.
			until := lastSeen.Add(tombstoneTTLs * a.ttl)
			if !s.level && now.Before(until) && a.room(&a.nBorn) {
				if _, dup := sh.born[key]; !dup {
					a.nBorn.Add(1)
				}
				s.sources = nil
				sh.born[key] = parked{s: s, until: until}
			}
		}
	}
	return visited
}

// deltaWindow adds the group's delta points into one point for the delta
// output under key. Sources arrive in separate batches with their own
// windows, which may overlap or lag what the output already reported; so
// each emitted window starts where the previous one ended and ends at the
// newest point (or just after the previous end), keeping the output's
// windows contiguous, non-overlapping and moving forward. Totals stay
// exact; a late point is reported in the next window.
func (a *accumulator) deltaWindow(key uint64, pts []point, typ pmetric.MetricType, now time.Time) (point, bool, int) {
	live := pts[:0:0]
	for _, p := range pts {
		if !p.stale {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return point{}, false, 0
	}
	sh := a.shard(key)
	defer sh.mu.Unlock()
	d, ok := sh.deltas[key]
	if !ok {
		if !a.reserve() {
			return point{}, false, len(live)
		}
		d = &deltaSeries{}
		sh.deltas[key] = d
	}
	d.seen = now
	out := combine(live, typ, "sum")
	start, ts := span(live)
	limit := tsLimit(now)
	for _, p := range live {
		a.clampTS(p.ts, limit)
	}
	ts = min(ts, limit)
	if d.lastEnd != 0 {
		start = d.lastEnd
		if ts <= d.lastEnd {
			ts = d.lastEnd + pcommon.Timestamp(time.Millisecond)
		}
	} else if start == 0 || start >= ts {
		start = ts - pcommon.Timestamp(time.Millisecond)
	}
	d.lastEnd = ts
	out.start, out.ts = start, ts
	return out, true, 0
}

// accumulate folds the group's points into the output series under key and
// returns the cumulative point to emit, whether there is one, and how many
// points were refused because the source cap was reached.
func (a *accumulator) accumulate(key uint64, pts []point, now time.Time) (point, bool, int) {
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].ts < pts[j].ts })

	sh := a.shard(key)
	defer sh.mu.Unlock()
	s, ok := sh.series[key]
	resumed := false
	if !ok {
		s, resumed = a.newOutput(sh, key, pts, now, true)
	}

	refused := 0
	limit := tsLimit(now)
	var ts pcommon.Timestamp
	for _, p := range pts {
		if p.stale {
			// A staleness marker carries no value. Reading it as 0 would
			// look like a reset and count the source's whole value again
			// on its next scrape. The source stays as it was and expires
			// through the TTL if it does not come back.
			continue
		}
		st, known := s.sources[p.orig]
		var delta point
		switch {
		case !known:
			if st, known = a.track(s, p.orig); !known {
				refused++
				continue
			}
			tb, returning := sh.tombstones[mix(key, p.orig)]
			if returning {
				delete(sh.tombstones, mix(key, p.orig))
				a.nTombstones.Add(-1)
			}
			switch {
			case p.baseline:
				// A cardinality overflow source that had its own series
				// before: its history was already reported there.
				delta = zeroLike(p)
			case (!ok && !resumed) || now.Sub(s.created) < a.warmup:
				// The output is new: every source that shows up during the
				// warm-up counts in full. After a collector restart the
				// pods arrive over several batches; baselining the later
				// ones would make the new series report less than the old
				// one did.
				delta = p
			case returning:
				// A forgotten source is back. Same start: it was counted
				// up to its last value, so only the increase since then is
				// new (nothing for a repeated point). New start, or a lower
				// value: it restarted while away, and all of its value is
				// new.
				prev := &sourceState{start: tb.start, last: tb.last, lastTS: tb.lastTS}
				switch {
				case p.start != 0 && tb.start != 0 && p.start != tb.start:
					delta = p
				case p.ts <= tb.lastTS:
					// Already counted: resume from the tombstone.
					st.start, st.last, st.lastTS, st.seen = tb.start, tb.last, tb.lastTS, now
					continue
				case isReset(prev, p):
					delta = p
				default:
					delta = subtract(p, tb.last)
				}
			case p.start != 0 && p.start > s.start:
				// Started after the output was created: all of its value
				// happened inside the output's lifetime.
				delta = p
			default:
				// Existed before this collector saw it (or reports no start
				// time): a baseline only, or its whole history would land
				// as a spike now.
				delta = zeroLike(p)
			}
		case p.ts <= st.lastTS:
			// Duplicate or out-of-order point: already counted.
			st.seen = now
			continue
		case isReset(st, p):
			delta = p
		default:
			delta = subtract(p, st.last)
		}
		st.start, st.last, st.lastTS, st.seen = p.start, p, p.ts, now
		s.add(delta)
		ts = max(ts, a.clampTS(p.ts, limit))
	}
	if len(s.sources) == 0 {
		return point{}, false, refused
	}
	sh.series[key] = s
	if ts == 0 {
		// Every point was a duplicate; nothing new to report.
		return point{}, false, refused
	}
	if now.Sub(s.born) < a.warmup {
		// A new output holds back until its warm-up is over. After a
		// collector restart the pods arrive over several batches; emitting
		// the first pod's value alone would make the counter fall below
		// what the previous process reported, and rate() would count the
		// recovery as a phantom increase. A short gap is harmless.
		return point{}, false, refused
	}
	out := point{start: s.start, ts: s.emitTS(ts), isInt: s.isInt, i: s.i, d: s.d}
	if s.hist != nil {
		h := *s.hist
		h.buckets = append([]uint64(nil), s.hist.buckets...)
		out.hist = &h
	}
	if s.exp != nil {
		out.exp = s.exp.clone()
		out.exp.hasMin, out.exp.hasMax = false, false
	}
	return out, true, refused
}

func isReset(st *sourceState, p point) bool {
	if p.start != 0 && st.start != 0 && p.start != st.start {
		return true
	}
	if p.exp != nil {
		_, ok := subExp(p.exp, st.last.exp)
		return !ok
	}
	if p.hist != nil {
		prev := st.last.hist
		if prev == nil || p.hist.count < prev.count || len(p.hist.buckets) != len(prev.buckets) {
			return true
		}
		for i := range p.hist.buckets {
			if p.hist.buckets[i] < prev.buckets[i] {
				return true
			}
		}
		return false
	}
	return p.float() < st.last.float()
}

func zeroLike(p point) point {
	z := point{isInt: p.isInt}
	if p.hist != nil {
		z.hist = &histPoint{buckets: make([]uint64, len(p.hist.buckets)), bounds: p.hist.bounds, hasSum: p.hist.hasSum}
	}
	if p.exp != nil {
		z.exp = &expPoint{scale: p.exp.scale, zeroThreshold: p.exp.zeroThreshold, hasSum: p.exp.hasSum}
	}
	return z
}

func subtract(cur, prev point) point {
	if cur.exp != nil {
		d, _ := subExp(cur.exp, prev.exp) // isReset ruled out a failure
		return point{exp: d}
	}
	if cur.hist != nil {
		h := &histPoint{
			count:   cur.hist.count - prev.hist.count,
			sum:     cur.hist.sum - prev.hist.sum,
			hasSum:  cur.hist.hasSum && prev.hist.hasSum,
			bounds:  cur.hist.bounds,
			buckets: make([]uint64, len(cur.hist.buckets)),
		}
		for i := range h.buckets {
			h.buckets[i] = cur.hist.buckets[i] - prev.hist.buckets[i]
		}
		return point{hist: h}
	}
	if cur.isInt && prev.isInt {
		return point{isInt: true, i: cur.i - prev.i}
	}
	return point{d: cur.float() - prev.float()}
}

func (s *outputSeries) add(delta point) {
	if delta.exp != nil {
		s.exp = addExp(s.exp, delta.exp)
		return
	}
	if delta.hist != nil {
		if s.hist == nil {
			s.hist = &histPoint{bounds: delta.hist.bounds, buckets: make([]uint64, len(delta.hist.buckets)), hasSum: true}
		}
		s.hist.count += delta.hist.count
		s.hist.sum += delta.hist.sum
		s.hist.hasSum = s.hist.hasSum && delta.hist.hasSum
		for i := range s.hist.buckets {
			if i < len(delta.hist.buckets) {
				s.hist.buckets[i] += delta.hist.buckets[i]
			}
		}
		return
	}
	if s.isInt && delta.isInt {
		s.i += delta.i
		return
	}
	if s.isInt {
		s.d = float64(s.i)
		s.isInt = false
	}
	s.d += delta.float()
}
