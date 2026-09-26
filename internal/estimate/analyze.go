package estimate

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
)

// AnalyzeOptions configures the whole per-meter pipeline.
type AnalyzeOptions struct {
	Cycle       meter.Options
	Estimate    Options
	Scopes      map[string]*regexp.Regexp
	Split       meter.SplitFunc
	RestartGaps []meter.Interval
	// Series prefixes every bootstrap seed (e.g. the account's auth index).
	Series string
	// PollOnlyBelow: header coverage under this share switches a cycle to
	// the poll-only method.
	PollOnlyBelow float64
}

// DefaultAnalyzeOptions are the plan's settings.
func DefaultAnalyzeOptions() AnalyzeOptions {
	return AnalyzeOptions{
		Cycle:         meter.DefaultOptions(),
		Estimate:      DefaultOptions(),
		Scopes:        meter.DefaultScopes(),
		PollOnlyBelow: 0.5,
	}
}

// CycleResult is one cycle with everything derived from it.
type CycleResult struct {
	Cycle meter.CycleAssignment `json:"cycle"`
	// Segments holds every lag's segments; the chosen lag's carry low_usd.
	Segments       map[meter.Lag][]meter.Segment `json:"segments"`
	LagChoice      LagChoice                     `json:"lag_choice"`
	Result         Result                        `json:"result"`
	ProxiedUSD     float64                       `json:"proxied_usd"`
	HeaderCoverage float64                       `json:"header_coverage"`
}

// Analyze runs cycle assignment, crossings, segments for every lag, lag
// choice and the primary estimate for one meter of one account. readings
// and events must already be filtered to the account; readings to meterKey.
// Closed cycles get kind final, the open one running; v_blend and
// level_shift chain each cycle to the previous gradeable final.
func Analyze(meterKey string, readings []domain.MeterReading, events []domain.UsageEvent, now time.Time, o AnalyzeOptions) []CycleResult {
	scoped := meter.ScopeEvents(meterKey, events, o.Scopes)
	ix := meter.NewEventIndex(scoped, o.Split)
	cycles := meter.AssignCycles(readings, scoped, now, o.Cycle)
	segOpts := meter.DefaultSegmentOptions(meter.IsScoped(meterKey, o.Scopes), now)
	segOpts.RestartGaps = o.RestartGaps

	var out []CycleResult
	var history []map[meter.Lag][]meter.Segment
	var prevLag meter.Lag
	var prevFinal *Result
	finalsSince := 0
	for _, c := range cycles {
		cr := CycleResult{Cycle: c, Segments: map[meter.Lag][]meter.Segment{}}
		window := ix.Window(c.StartsAt, c.End(now))
		cr.HeaderCoverage = headerCoverage(c, window)
		for _, e := range window {
			cr.ProxiedUSD += e.APIUSD
		}
		polls := sourceReadings(c, domain.SourcePoll)
		ptick := pollTick(polls)
		gapTicks, gaps := CoverageGaps(polls, ix, ptick)

		method := MethodHeader
		if cr.HeaderCoverage < o.PollOnlyBelow && len(polls) > 0 {
			method = MethodPoll
		}
		eo := o.Estimate
		var res Result
		if method == MethodPoll {
			res, cr.Segments[meter.LagTime] = PollOnly(c, ix, segOpts, eo, seriesID(o.Series, meterKey, c))
			cr.LagChoice = LagChoice{Lag: meter.LagTime, Fallback: true}
		} else {
			xs := meter.Crossings(c.Readings, c.Tick, domain.SourceHeader)
			for _, lag := range meter.Lags {
				cr.Segments[lag] = meter.BuildSegments(c, xs, ix, lag, segOpts)
				markGaps(cr.Segments[lag], gaps)
			}
			history = append(history, cr.Segments)
			if len(history) > eo.LagHistory {
				history = history[1:]
			}
			cr.LagChoice = ChooseLag(history, prevLag, eo.MinLagCrossings)
			prevLag = cr.LagChoice.Lag
			eo.SeriesID = seriesID(o.Series, meterKey, c) + "|" + string(cr.LagChoice.Lag)
			res = Primary(cr.Segments[cr.LagChoice.Lag], eo)
			res.Method = MethodHeader
			markLowUSD(cr.Segments[cr.LagChoice.Lag], res.lowUSD)
		}
		res.Lag = cr.LagChoice.Lag
		res.LagScores = cr.LagChoice.Scores
		res.LagStable = method == MethodHeader && cr.LagChoice.Stable
		if method == MethodHeader && !cr.LagChoice.Stable {
			res.flag(FlagLagUnstable)
		}
		for _, f := range c.Flags {
			res.flag(f)
		}
		if res.VHat > 0 {
			if res.CoverageGapTick = gapTicks; gapTicks > 0 {
				res.flag(FlagCoverageGap)
				res.CoverageGapUSD = float64(gapTicks) * ptick * res.VHat
			}
			if res.SubRegimes = subRegimes(res.blocks); res.SubRegimes != nil {
				res.flag(FlagRegimeSplit)
			}
		}
		res.Grade = Grade(GradeInput{
			Ticks: res.TicksUsed, Crossings: res.Crossings, RelWidth: res.RelWidth(), Unexplained: res.UnexplainedFrac,
			RegimeChange:    slices.Contains(c.Flags, meter.FlagRegimeChange),
			PrecisionChange: slices.Contains(c.Flags, meter.FlagPrecisionChange),
			LagStable:       res.LagStable, CapMedium: method == MethodPoll,
		})

		res.Kind = KindRunning
		if c.Closed() {
			res.Kind = KindFinal
		}
		if prevFinal != nil && res.VHat > 0 {
			if res.Kind == KindFinal && LevelShift(*prevFinal, res) {
				res.flag(FlagLevelShift)
			}
			if res.Kind == KindRunning {
				res.VBlend = Blend(res, *prevFinal, finalsSince+1, eo.SigmaRW)
			}
		}
		if res.Kind == KindRunning {
			f := ForecastCycle(c, res, cr.ProxiedUSD, now)
			res.Forecast = &f
		}
		if res.Kind == KindFinal && res.Grade != GradeInsufficient {
			r := res
			prevFinal, finalsSince = &r, 0
		} else if res.Kind == KindFinal {
			finalsSince++
		}
		cr.Result = res
		out = append(out, cr)
	}
	return out
}

func seriesID(prefix, meterKey string, c meter.CycleAssignment) string {
	return fmt.Sprintf("%s|%s|%d|%d", prefix, meterKey, c.ResetAnchor.Unix(), c.RegimeSeq)
}

func markLowUSD(segs []meter.Segment, low map[int]bool) {
	for i := range segs {
		if low[segs[i].Seq] {
			segs[i].Flags = append(segs[i].Flags, meter.FlagLowUSD)
		}
	}
}

// headerCoverage is the share of the cycle's successful events whose
// response carried a reading of this meter.
func headerCoverage(c meter.CycleAssignment, window []domain.UsageEvent) float64 {
	seen := map[int64]bool{}
	for _, r := range c.Readings {
		if r.Source == domain.SourceHeader && r.EventID != nil {
			seen[*r.EventID] = true
		}
	}
	var n, hit int
	for _, e := range window {
		if e.Failed {
			continue
		}
		n++
		if seen[e.ID] {
			hit++
		}
	}
	if n == 0 {
		return 1
	}
	return float64(hit) / float64(n)
}

func sourceReadings(c meter.CycleAssignment, source string) []meter.AssignedReading {
	var out []meter.AssignedReading
	for _, r := range c.Readings {
		if r.Source == source && !r.QuotaDrop {
			out = append(out, r)
		}
	}
	return out
}

func pollTick(polls []meter.AssignedReading) float64 {
	rs := make([]domain.MeterReading, len(polls))
	for i, p := range polls {
		rs[i] = p.MeterReading
	}
	t, _ := meter.Tick(rs)
	return t
}

// PollOnly is the §Poll-only method: crossings over the cycle's polls,
// time-lag segments between them, the same ratio of sums (Var_q = R·tick²/6
// with the polls' tick). The caller caps the grade at medium.
func PollOnly(c meter.CycleAssignment, ix *meter.EventIndex, so meter.SegmentOptions, o Options, series string) (Result, []meter.Segment) {
	polls := sourceReadings(c, domain.SourcePoll)
	c.Tick = pollTick(polls)
	xs := meter.Crossings(polls, c.Tick, domain.SourcePoll)
	segs := meter.BuildSegments(c, xs, ix, meter.LagTime, so)
	o.SeriesID = series + "|poll"
	r := Primary(segs, o)
	r.Method = MethodPoll
	markLowUSD(segs, r.lowUSD)
	return r, segs
}

// gapMargin: activity this close before the first poll of a pair still
// counts, so a provider that books spend a little late cannot fake a gap.
const gapMargin = 5 * time.Minute

// CoverageGaps is the poll detector of unrouted usage: consecutive polls
// whose level rose while no proxied request registered any spend between
// them (or within gapMargin before). It returns the unexplained ticks and
// the (poll, poll] spans where they happened.
func CoverageGaps(polls []meter.AssignedReading, ix *meter.EventIndex, tick float64) (int64, []meter.Interval) {
	var ticks int64
	var gaps []meter.Interval
	m := int64(-1)
	var last time.Time
	for _, p := range polls {
		lvl := meter.Level(p.UsedFraction, tick)
		if m >= 0 && lvl > m && !ix.Active(last.Add(-gapMargin), p.ObservedAt) {
			ticks += lvl - m
			gaps = append(gaps, meter.Interval{From: last, To: p.ObservedAt})
		}
		if lvl > m {
			m = lvl
		}
		last = p.ObservedAt
	}
	return ticks, gaps
}

// markGaps flags (for exclusion) every segment overlapping a coverage gap.
// Refinement over the plan, which names the poll detector but only uses the
// ratio guard for exclusion: a burst of a few ticks inside a silence often
// shares its segment with up to a tick of legitimate spend and passes the
// φ = 0.2 ratio test; the polls locate it directly.
func markGaps(segs []meter.Segment, gaps []meter.Interval) {
	for i := range segs {
		for _, g := range gaps {
			if segs[i].TStart.Before(g.To) && g.From.Before(segs[i].TEnd) {
				segs[i].Flags = append(segs[i].Flags, meter.FlagCoverageGap)
				break
			}
		}
	}
}
