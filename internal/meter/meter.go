// Package meter turns one account's meter readings into cycles, crossings
// and lag-attributed spend segments (docs/plan.md §Estimator). Everything
// here is a pure function over slices; persistence lives in internal/estimate.
package meter

import (
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// Lag names how a crossing's reading relates to the events that moved it.
type Lag string

const (
	// LagIdx0..2: the reading of event i reflects events with index ≤ i−L
	// (events ordered by observed_at).
	LagIdx0 Lag = "idx0"
	LagIdx1 Lag = "idx1"
	LagIdx2 Lag = "idx2"
	// LagTime: input-side $ registers at requested_at, output $ at
	// requested_at+latency; a reading at t reflects registrations ≤ t.
	LagTime Lag = "time"
)

// Lags lists every candidate in a fixed order; ties resolve to the earlier one.
var Lags = []Lag{LagTime, LagIdx1, LagIdx0, LagIdx2}

func (l Lag) index() int {
	switch l {
	case LagIdx0:
		return 0
	case LagIdx1:
		return 1
	case LagIdx2:
		return 2
	}
	return -1
}

// Segment and cycle flags.
const (
	FlagQuotaDrop       = "quota_drop"
	FlagLongGap         = "long_gap"
	FlagRestartGap      = "restart_gap"
	FlagRegimeChange    = "regime_change"
	FlagLagIncomplete   = "lag_incomplete"
	FlagLowUSD          = "low_usd"
	FlagPrecisionChange = "precision_change"
	FlagFailedTokens    = "failed_tokens"
	// FlagCoverageGap: the segment overlaps a poll-detected rise with no
	// proxied activity (set by the estimator).
	FlagCoverageGap = "coverage_gap"
)

// Cycle end reasons.
const (
	EndReset      = "reset"
	EndEarlyReset = "early_reset"
	EndRegime     = "regime"
	EndExpired    = "expired"
)

// Interval is a half-open time range [From, To).
type Interval struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

func (iv Interval) overlaps(from, to time.Time) bool {
	return from.Before(iv.To) && iv.From.Before(to)
}

// DefaultScopes maps feature-scoped meters to the model families that feed
// them. Meters absent from the table are fed by every event.
func DefaultScopes() map[string]*regexp.Regexp {
	return map[string]*regexp.Regexp{
		domain.MeterSevenDayOI: regexp.MustCompile(`(?i)fable`),
	}
}

// ScopeEvents keeps the events that move meterKey under scopes.
func ScopeEvents(meterKey string, evs []domain.UsageEvent, scopes map[string]*regexp.Regexp) []domain.UsageEvent {
	re, ok := scopes[meterKey]
	if !ok {
		return evs
	}
	out := make([]domain.UsageEvent, 0, len(evs))
	for _, e := range evs {
		if re.MatchString(e.ModelFamily) {
			out = append(out, e)
		}
	}
	return out
}

// IsScoped reports whether meterKey is fed by a subset of events.
func IsScoped(meterKey string, scopes map[string]*regexp.Regexp) bool {
	_, ok := scopes[meterKey]
	return ok
}

// Precision bounds used by Tick. Providers never report coarser than whole
// percent (2 dp as a fraction) and the plan floors the tick at 1e-4 (4 dp).
const (
	minDP     = 2
	maxDP     = 4
	coarseRun = 10
)

// Tick is the quantum implied by the readings' precision (callers pass one
// source: a cycle's header readings, or its polls for the poll-only
// method), and whether the precision changed within them.
//
// Plan: tick = 10^-min(precision_dp), floor 1e-4. Refinement: upstream trims
// trailing zeros ("0.5" for 0.50, "0.0"), so a single short spelling is not
// evidence of a coarser grid. Each reading's dp is clamped to [2, 4]; a
// coarser precision d only counts when ≥10 consecutive readings all have
// dp ≤ d, which trimming alone makes vanishingly unlikely.
func Tick(readings []domain.MeterReading) (tick float64, precisionChange bool) {
	var dps []int
	for _, r := range readings {
		dps = append(dps, min(max(r.PrecisionDP, minDP), maxDP))
	}
	if len(dps) == 0 {
		return math.Pow10(-minDP), false
	}
	hi := slices.Max(dps)
	d := hi
	for cand := minDP; cand < hi; cand++ {
		if longestRun(dps, cand) >= coarseRun {
			d = cand
			break
		}
	}
	return math.Pow10(-d), d < hi
}

func longestRun(dps []int, atMost int) int {
	best, cur := 0, 0
	for _, d := range dps {
		if d <= atMost {
			cur++
			best = max(best, cur)
		} else {
			cur = 0
		}
	}
	return best
}

// Level is floor(u/tick) with a guard against float error at exact multiples.
func Level(u, tick float64) int64 {
	return int64(math.Floor(u/tick + 1e-9))
}

// SortEvents orders events by (observed_at, id); every index lag assumes it.
func SortEvents(evs []domain.UsageEvent) []domain.UsageEvent {
	out := slices.Clone(evs)
	slices.SortStableFunc(out, func(a, b domain.UsageEvent) int {
		if c := a.ObservedAt.Compare(b.ObservedAt); c != 0 {
			return c
		}
		return cmpInt(a.ID, b.ID)
	})
	return out
}

func sortReadings(rs []domain.MeterReading) []domain.MeterReading {
	out := slices.Clone(rs)
	slices.SortStableFunc(out, func(a, b domain.MeterReading) int {
		if c := a.ObservedAt.Compare(b.ObservedAt); c != 0 {
			return c
		}
		return cmpInt(a.ID, b.ID)
	})
	return out
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func addFlag(flags []string, f string) []string {
	if slices.Contains(flags, f) {
		return flags
	}
	return append(flags, f)
}
