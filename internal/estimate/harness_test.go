package estimate

import (
	"math"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/sim"
)

// The corpus tests generate many synthetic accounts, analyze each one's
// weekly meter and keep only a compact summary, so hundreds of corpora fit
// in memory and run in parallel.

type cycleRun struct {
	Period       sim.PeriodTruth
	Matched      bool
	Anchor       time.Time
	Regime       int
	EndReason    string
	Result       Result
	Lag          LagChoice
	OwnCrossings int
	TotalTicks   int64
	Polls        int
	VByLag       map[meter.Lag]float64
}

type seedRun struct {
	Seed     uint64
	Provider domain.Provider
	LagMode  string
	Truth    sim.Truth
	Cycles   []cycleRun

	ResetDetected        bool
	ReadingsBeforeDetect int
	SegmentSpansReset    bool

	Crossings          int
	CrossingViolations int
	NonMonotoneRaw     int
}

func seedCount(full, short int) int {
	if testing.Short() {
		return short
	}
	return full
}

// runSeeds analyzes scenarios for seeds 1..n in parallel, ordered by seed.
func runSeeds(n int, mk func(seed uint64) sim.Scenario) []seedRun {
	out := make([]seedRun, n)
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = runCorpus(mk(uint64(i + 1)))
		}()
	}
	wg.Wait()
	return out
}

func runCorpus(sc sim.Scenario) seedRun {
	c := sim.Generate(sc)
	sr := seedRun{Seed: sc.Seed, Provider: sc.Provider, LagMode: sc.LagMode, Truth: c.Truth}
	res := Analyze(domain.MeterSevenDay, c.ReadingsFor(domain.MeterSevenDay), c.Events, c.End.Add(time.Hour), DefaultAnalyzeOptions())
	quick := DefaultOptions()
	quick.Resamples = 0
	for _, cr := range res {
		run := cycleRun{
			Anchor: cr.Cycle.ResetAnchor, Regime: cr.Cycle.RegimeSeq, EndReason: cr.Cycle.EndReason,
			Result: cr.Result, Lag: cr.LagChoice, VByLag: map[meter.Lag]float64{},
		}
		run.Result.blocks, run.Result.lowUSD = nil, nil
		if p, ok := c.Truth.PeriodAt(cr.Cycle.StartsAt.Add(time.Minute)); ok && absDur(p.Anchor.Sub(cr.Cycle.ResetAnchor)) <= 2*time.Minute {
			run.Period, run.Matched = p, true
		}
		for _, r := range cr.Cycle.Readings {
			if r.Source == domain.SourcePoll {
				run.Polls++
			}
		}
		for _, lag := range meter.Lags {
			segs := cr.Segments[lag]
			if lag == cr.LagChoice.Lag {
				run.OwnCrossings = len(segs)
				for _, s := range segs {
					run.TotalTicks += s.DeltaTicks
				}
			}
			if cr.Result.Method == MethodHeader {
				run.VByLag[lag] = Primary(segs, quick).VHat
			}
		}
		sr.Cycles = append(sr.Cycles, run)
		checkCrossings(&sr, c, cr)
	}
	checkEarlyReset(&sr, c, res)
	return sr
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// checkCrossings: levels strictly increase and never claim more usage than
// the hidden meter had at that instant, even though raw readings go
// backwards under concurrency and stale replicas.
func checkCrossings(sr *seedRun, c sim.Corpus, cr CycleResult) {
	xs := meter.Crossings(cr.Cycle.Readings, cr.Cycle.Tick, domain.SourceHeader)
	for i, x := range xs {
		sr.Crossings++
		if (i > 0 && x.Level <= xs[i-1].Level) || float64(x.Level)*cr.Cycle.Tick > c.TrueU7(x.At)+1e-9 {
			sr.CrossingViolations++
		}
	}
	hi := -1.0
	for _, r := range cr.Cycle.Readings {
		if r.Source != domain.SourceHeader || r.Failed {
			continue
		}
		if r.UsedFraction < hi {
			sr.NonMonotoneRaw++
		}
		hi = math.Max(hi, r.UsedFraction)
	}
}

// checkEarlyReset finds the cycle closed as early_reset at or after the
// hidden reset, counts successful header readings between the reset and
// the detected boundary, and checks no segment of any lag straddles it.
func checkEarlyReset(sr *seedRun, c sim.Corpus, res []CycleResult) {
	er := c.Truth.EarlyResetAt
	if er.IsZero() {
		return
	}
	var det time.Time
	for _, cr := range res {
		if cr.Cycle.EndReason == meter.EndEarlyReset && !cr.Cycle.ClosedAt.Before(er) && cr.Cycle.ClosedAt.Before(er.Add(24*time.Hour)) {
			det = cr.Cycle.ClosedAt
			sr.ResetDetected = true
			break
		}
	}
	if !sr.ResetDetected {
		return
	}
	failed := map[int64]bool{}
	for _, e := range c.Events {
		if e.Failed {
			failed[e.ID] = true
		}
	}
	for _, r := range c.ReadingsFor(domain.MeterSevenDay) {
		if r.Source == domain.SourceHeader && !failed[*r.EventID] && r.ObservedAt.After(er) && r.ObservedAt.Before(det) {
			sr.ReadingsBeforeDetect++
		}
	}
	for _, cr := range res {
		for _, segs := range cr.Segments {
			if slices.ContainsFunc(segs, func(s meter.Segment) bool { return s.TStart.Before(er) && s.TEnd.After(er) }) {
				sr.SegmentSpansReset = true
			}
		}
	}
}

// qualifying yields matched, final, header-method cycles with ≥ minTicks.
func qualifying(runs []seedRun, minTicks int64) []cycleRun {
	var out []cycleRun
	for _, sr := range runs {
		for _, cr := range sr.Cycles {
			if cr.Matched && cr.Result.Kind == KindFinal && cr.Result.Method == MethodHeader && cr.TotalTicks >= minTicks {
				out = append(out, cr)
			}
		}
	}
	return out
}

func relErr(v, truth float64) float64 { return v/truth - 1 }

type summary struct{ n, mean, sd, maxAbs float64 }

func summarize(xs []float64) summary {
	s := summary{n: float64(len(xs))}
	for _, x := range xs {
		s.mean += x
		s.maxAbs = math.Max(s.maxAbs, math.Abs(x))
	}
	if len(xs) > 0 {
		s.mean /= s.n
	}
	s.sd = stddev(xs)
	return s
}
