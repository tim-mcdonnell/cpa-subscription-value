package weights

import (
	"math"
	"sort"
	"time"
)

const (
	backtestMinSegments = 25   // minimum training set
	backtestTrainFrac   = 0.40 // initial training share
	backtestRefitEvery  = 25   // refit shared factors every N test segments
	ambiguousRatio      = 1.10 // runner-up MAE within 10 % → ambiguous
)

// crossing identifies a crossing independent of lag attribution.
type crossing struct {
	cycle, regime string
	end           int64
	ticks         float64
}

func crossingOf(s Segment) crossing {
	return crossing{s.CycleKey, s.RegimeKey, s.EndAt.UnixNano(), s.DeltaTicks}
}

func sortChronological(segs []Segment) {
	sort.SliceStable(segs, func(i, j int) bool {
		a, b := segs[i], segs[j]
		if !a.EndAt.Equal(b.EndAt) {
			return a.EndAt.Before(b.EndAt)
		}
		if a.CycleKey != b.CycleKey {
			return a.CycleKey < b.CycleKey
		}
		return a.RegimeKey < b.RegimeKey
	})
}

// Backtest scores each lag's segments prequentially and returns the lag with
// the lowest MAE (percentage points). Lags are compared on the crossings that
// are eligible under every lag, so a lag cannot win by dropping hard
// segments. Each lag trains on the first max(25, 40 %) segments, then
// predicts every later crossing from the online cycle scale before folding
// it in, refitting the factors every 25 segments. ambiguous is true when the
// runner-up is within 10 % of the best. bestLag is "" when no lag has ≥ 26
// common segments.
func Backtest(segmentsByLag map[string][]Segment, params ProviderParams, now time.Time) (bestLag string, mae map[string]float64, ambiguous bool) {
	params = withDefaults(params)
	mae = map[string]float64{}
	if params.RefRate == nil {
		return "", mae, false
	}
	lags := make([]string, 0, len(segmentsByLag))
	for lag := range segmentsByLag {
		lags = append(lags, lag)
	}
	sort.Strings(lags)
	common := map[crossing]int{}
	for _, lag := range lags {
		seen := map[crossing]bool{}
		for _, s := range segmentsByLag[lag] {
			if _, _, ok := prepare(s, params.RefRate); ok {
				seen[crossingOf(s)] = true
			}
		}
		for c := range seen {
			common[c]++
		}
	}
	best, second := math.Inf(1), math.Inf(1)
	for _, lag := range lags {
		var eligible []Segment
		for _, s := range segmentsByLag[lag] {
			if common[crossingOf(s)] == len(lags) {
				if _, _, ok := prepare(s, params.RefRate); ok {
					eligible = append(eligible, s)
				}
			}
		}
		sortChronological(eligible)
		score, n := prequential(eligible, params)
		if n == 0 {
			continue
		}
		mae[lag] = score
		switch {
		case score < best:
			second, best, bestLag = best, score, lag
		case score < second:
			second = score
		}
	}
	if !math.IsInf(second, 1) && best > 0 {
		ambiguous = second/best < ambiguousRatio
	}
	return bestLag, mae, ambiguous
}

// prequential returns the mean |predicted − observed| in percentage points
// over the test segments and how many were scored.
func prequential(eligible []Segment, params ProviderParams) (float64, int) {
	if len(eligible) <= backtestMinSegments {
		return 0, 0
	}
	start := max(backtestMinSegments, int(float64(len(eligible))*backtestTrainFrac))
	fit, err := FitWeights(eligible[:start], params, eligible[start-1].EndAt)
	if err != nil {
		return 0, 0
	}
	tracker := newScaleTracker(fit, params.RandomWalkSigma)
	var absErr float64
	var n int
	for i := start; i < len(eligible); i++ {
		if i > start && (i-start)%backtestRefitEvery == 0 {
			if refit, err := FitWeights(eligible[:i], params, eligible[i-1].EndAt); err == nil {
				fit = refit
				tracker = newScaleTracker(fit, params.RandomWalkSigma)
			}
		}
		seg := eligible[i]
		usd, ok := fit.WeightedUSD(seg, params)
		if !ok || usd <= 0 {
			continue
		}
		state := tracker.forSegment(seg)
		if state.Precision > 0 {
			pred := 100 * math.Exp(state.LogScale) * usd
			absErr += math.Abs(pred - 100*seg.DeltaTicks*seg.Tick)
			n++
		}
		state.UpdateWeighted(seg.DeltaTicks, seg.Tick, usd, fitWeight(seg, seg.EndAt, params.HalfLife))
	}
	if n == 0 {
		return 0, 0
	}
	return absErr / float64(n), n
}
