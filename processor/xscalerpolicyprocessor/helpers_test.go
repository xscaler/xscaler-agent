package xscalerpolicyprocessor

import "time"

// Test helpers that read sharded state.

func (a *accumulator) count(f func(*accShard) int) int {
	n := 0
	for i := range a.shards {
		sh := &a.shards[i]
		sh.mu.Lock()
		n += f(sh)
		sh.mu.Unlock()
	}
	return n
}

func (a *accumulator) nSeries() int {
	return a.count(func(sh *accShard) int { return len(sh.series) })
}

func (a *accumulator) nTomb() int {
	return a.count(func(sh *accShard) int { return len(sh.tombstones) })
}

func (a *accumulator) nBornRecs() int {
	return a.count(func(sh *accShard) int { return len(sh.born) })
}

func (a *accumulator) nLayout() int {
	return a.count(func(sh *accShard) int { return len(sh.layouts) })
}

func (a *accumulator) nDeltas() int {
	return a.count(func(sh *accShard) int { return len(sh.deltas) })
}

func (a *accumulator) nSources() int { return int(a.sources.Load()) }

// sweepAll sweeps every shard regardless of budget and schedule.
func (a *accumulator) sweepAll(now time.Time) {
	for i := range a.shards {
		sh := &a.shards[i]
		sh.mu.Lock()
		a.sweepShard(sh, now)
		sh.mu.Unlock()
	}
}

func (p *policyProcessor) seenLen(name string) int {
	cs := p.cardShard(name)
	defer cs.mu.Unlock()
	return len(cs.seen[name])
}
