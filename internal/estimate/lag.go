package estimate

import (
	"math"
	"slices"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
)

// LagChoice is the lag selected for a meter and the evidence behind it.
type LagChoice struct {
	Lag meter.Lag `json:"lag"`
	// Scores is S(L) = median_k |û_k − Δ_k·tick| in ticks (lower is better).
	Scores    map[meter.Lag]float64 `json:"scores"`
	Crossings int                   `json:"crossings"`
	Stable    bool                  `json:"stable"`
	Fallback  bool                  `json:"fallback"`
}

// stableMargin: the winner must beat the runner-up's score by this share
// to count as stable. The plan defines lag_unstable by disagreement with the
// weight learner's backtest (Phase 4); until then a near tie is the signal.
const stableMargin = 0.05

// ChooseLag picks the lag by the prequential score (§Lag attribution).
// history holds each cycle's segments by lag, oldest first (the current
// cycle last); the walk restarts per cycle because V differs between cycles,
// and errors are pooled across them because the lag is a property of the
// (account, meter), not of one week. With fewer than minCrossings scored
// crossings it falls back to prev, else to the time lag.
func ChooseLag(history []map[meter.Lag][]meter.Segment, prev meter.Lag, minCrossings int) LagChoice {
	c := LagChoice{Scores: map[meter.Lag]float64{}}
	n := math.MaxInt
	for _, lag := range meter.Lags {
		var errs []float64
		for _, h := range history {
			errs = append(errs, prequentialErrors(h[lag])...)
		}
		n = min(n, len(errs))
		if len(errs) > 0 {
			c.Scores[lag] = median(errs)
		}
	}
	if n == math.MaxInt {
		n = 0
	}
	c.Crossings = n
	if n < minCrossings || len(c.Scores) == 0 {
		c.Lag, c.Fallback = prev, true
		if c.Lag == "" {
			c.Lag = meter.LagTime
		}
		return c
	}
	ranked := slices.Clone(meter.Lags)
	slices.SortStableFunc(ranked, func(a, b meter.Lag) int {
		sa, oka := c.Scores[a]
		sb, okb := c.Scores[b]
		switch {
		case !oka && !okb:
			return 0
		case !oka:
			return 1
		case !okb:
			return -1
		case sa < sb:
			return -1
		case sa > sb:
			return 1
		}
		return 0
	})
	c.Lag = ranked[0]
	if s2, ok := c.Scores[ranked[1]]; ok {
		c.Stable = c.Scores[c.Lag] <= (1-stableMargin)*s2
	}
	return c
}

// prequentialErrors walks one cycle's segments in order, predicting each
// segment's movement from the V̂ of the segments before it:
// û_k = $_k / V̂_{k−1}, error |û_k − Δ_k·tick| in ticks. Scoring starts once
// 5 ticks have accumulated so the first noisy V̂ does not dominate.
func prequentialErrors(segs []meter.Segment) []float64 {
	var out []float64
	var usd, d float64
	for _, s := range segs {
		if slices.ContainsFunc(excluded, s.Has) || s.Tick == 0 {
			continue
		}
		if usd > 0 && d >= 5*s.Tick {
			v := usd / d
			out = append(out, math.Abs(s.USD/v-s.D())/s.Tick)
		}
		usd += s.USD
		d += s.D()
	}
	return out
}
