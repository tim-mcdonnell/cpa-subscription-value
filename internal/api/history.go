package api

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/estimate"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

const (
	defaultCycles = 26
	maxCycles     = 200
	maxPoints     = 2000
)

// excludedSegment lists the segment flags that keep a crossing out of V̂.
var excludedSegment = []string{meter.FlagQuotaDrop, meter.FlagRegimeChange, meter.FlagRestartGap,
	meter.FlagLagIncomplete, meter.FlagCoverageGap, meter.FlagLowUSD}

// cycleItem is one row of /cycles: the week-over-week series.
type cycleItem struct {
	cycleView
	Estimate    *estimateView `json:"estimate"`
	Mix         mixView       `json:"mix"`
	SpentUSD    float64       `json:"spent_usd"`
	Flags       []string      `json:"flags"`
	LevelShift  bool          `json:"level_shift"`
	LowCoverage bool          `json:"low_coverage"`
}

func (r *Router) meterParam(q url.Values) string {
	if m := strings.TrimSpace(q.Get("meter")); m != "" {
		return m
	}
	return domain.MeterSevenDay
}

func (r *Router) handleCycles(q url.Values, _ []byte) abi.ManagementResponse {
	acct, resp := r.resolveAccount(q.Get("auth_index"), true)
	if resp != nil {
		return *resp
	}
	limit, err := parseLimit(q.Get("limit"), defaultCycles, maxCycles)
	if err != nil {
		return *bad("limit: " + err.Error())
	}
	m := r.meterParam(q)
	cycles, err := r.d.Store.ListCycles(acct.ID, m, limit)
	if err != nil {
		return serverError("list cycles", err)
	}
	items := make([]cycleItem, 0, len(cycles))
	for _, c := range cycles {
		it, err := r.cycleItem(c)
		if err != nil {
			return serverError("cycle estimate", err)
		}
		items = append(items, it)
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{
		"auth_index": acct.AuthIndex, "provider": acct.Provider, "meter": m,
		"coverage_mode": r.coverageMode(acct.AuthIndex), "cycles": items,
	})
}

// cycleItem picks the cycle's latest final (latest running while open, or
// any estimate when neither exists).
func (r *Router) cycleItem(c store.Cycle) (cycleItem, error) {
	it := cycleItem{cycleView: newCycleView(c), Mix: mixView{ByFamily: map[string]float64{}, ByType: map[string]float64{}, Source: "estimate"}}
	kind := estimate.KindFinal
	if c.ClosedAt.IsZero() {
		kind = estimate.KindRunning
	}
	e, ok, err := r.d.Store.LatestEstimate(c.ID, kind)
	if err == nil && !ok {
		e, ok, err = r.d.Store.LatestEstimate(c.ID, "")
	}
	if err != nil {
		return it, err
	}
	it.Flags = flagUnion(c.Flags)
	if !ok {
		return it, nil
	}
	ev := newEstimateView(e, true)
	it.Estimate = &ev
	mix := parseMix(e.Mix)
	it.Mix = mixFromEstimate(mix)
	it.SpentUSD = mix.Diag.ProxiedUSD
	it.Flags = flagUnion(c.Flags, e.Flags)
	it.LevelShift = slices.Contains(e.Flags, estimate.FlagLevelShift)
	it.LowCoverage = e.UnexplainedFrac > 0.15 || slices.Contains(e.Flags, estimate.FlagCoverageGap) ||
		e.Method == estimate.MethodPoll
	return it, nil
}

// crossing is a segment endpoint at the chosen lag.
type crossing struct {
	T          int64    `json:"t"` // unix ms
	Level      int64    `json:"level"`
	U          float64  `json:"u"`
	DeltaTicks int64    `json:"delta_ticks"`
	USD        float64  `json:"usd"`
	CumUSD     float64  `json:"cum_usd"`
	Used       bool     `json:"used"`
	Flags      []string `json:"flags"`
}

type estimatePoint struct {
	T     int64   `json:"t"`
	Kind  string  `json:"kind"`
	VHat  float64 `json:"v_hat"`
	CILo  float64 `json:"ci_lo"`
	CIHi  float64 `json:"ci_hi"`
	Grade string  `json:"grade"`
}

// point is [unix ms, value].
type point [2]float64

func (r *Router) handleSeries(q url.Values, _ []byte) abi.ManagementResponse {
	acct, resp := r.resolveAccount(q.Get("auth_index"), true)
	if resp != nil {
		return *resp
	}
	id, resp := parseID("cycle_id", q.Get("cycle_id"))
	if resp != nil {
		return *resp
	}
	c, resp := r.pickCycle(acct, q.Get("meter"), id)
	if resp != nil {
		return *resp
	}
	now := r.now()
	readings, err := r.cycleReadings(acct.ID, c)
	if err != nil {
		return serverError("readings", err)
	}
	var header, polls []point
	for _, m := range readings {
		p := point{float64(m.ObservedAt.UnixMilli()), m.UsedFraction}
		if m.Source == domain.SourcePoll {
			polls = append(polls, p)
		} else {
			header = append(header, p)
		}
	}
	hBudget := maxPoints
	if n := len(header) + len(polls); n > maxPoints {
		hBudget = max(maxPoints*len(header)/n, 1)
	}
	header = downsample(header, hBudget)
	polls = downsample(polls, maxPoints-len(header))

	events, err := r.loadEvents(acct.ID, c.StartsAt, cycleEnd(c, now))
	if err != nil {
		return serverError("events", err)
	}
	var cum []point
	var total float64
	for _, e := range meter.ScopeEvents(c.MeterKey, events, meter.DefaultScopes()) {
		total += e.APIUSD
		cum = append(cum, point{float64(e.ObservedAt.UnixMilli()), total})
	}
	cum = downsample(cum, maxPoints)

	history, err := r.d.Store.ListEstimates(c.ID)
	if err != nil {
		return serverError("estimates", err)
	}
	points := make([]estimatePoint, 0, len(history))
	var latest *store.Estimate
	for i, e := range history {
		points = append(points, estimatePoint{e.ComputedAt.UnixMilli(), e.Kind, e.VHat, e.CILo, e.CIHi, e.Grade})
		latest = &history[i]
	}
	if len(points) > maxPoints {
		points = points[len(points)-maxPoints:]
	}
	lag := string(meter.LagTime)
	var vhat float64
	if latest != nil {
		lag, vhat = latest.Lag, latest.VHat
	}
	segs, err := r.d.Store.ListSegments(c.ID, lag)
	if err != nil {
		return serverError("segments", err)
	}
	crossings := []crossing{}
	var baseline map[string]any
	var cumSeg float64
	for i, s := range segs {
		if i == 0 {
			baseline = map[string]any{"t": s.TStart.UnixMilli(), "level": s.FromLevel, "u": float64(s.FromLevel) * c.Tick}
		}
		cumSeg += s.USD
		crossings = append(crossings, crossing{
			T: s.TEnd.UnixMilli(), Level: s.ToLevel, U: float64(s.ToLevel) * c.Tick, DeltaTicks: s.DeltaTicks,
			USD: s.USD, CumUSD: cumSeg, Used: !slices.ContainsFunc(excludedSegment, func(f string) bool { return slices.Contains(s.Flags, f) }),
			Flags: nonNil(s.Flags),
		})
	}
	var fit map[string]any
	if vhat > 0 && baseline != nil {
		fit = map[string]any{"v_hat": vhat, "usd_per_pct": vhat / 100, "u0": baseline["u"]}
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{
		"auth_index": acct.AuthIndex, "provider": acct.Provider,
		"cycle": newCycleView(c), "lag": lag, "now": now.UTC(),
		"readings":       map[string]any{"header": nonNil(header), "poll": nonNil(polls)},
		"cumulative_usd": nonNil(cum),
		"crossings":      crossings,
		"baseline":       baseline,
		"estimates":      points,
		"fit":            fit,
	})
}

// pickCycle resolves cycle_id (which must belong to the account, and to the
// meter when one is given) or the account's newest cycle of the meter.
func (r *Router) pickCycle(acct domain.Account, meterKey string, id int64) (store.Cycle, *abi.ManagementResponse) {
	meterKey = strings.TrimSpace(meterKey)
	if id > 0 {
		c, ok, err := r.d.Store.GetCycle(id)
		if err != nil {
			resp := serverError("get cycle", err)
			return c, &resp
		}
		if !ok || c.AccountID != acct.ID || (meterKey != "" && c.MeterKey != meterKey) {
			resp := abi.ErrorResponse(http.StatusNotFound, "unknown cycle_id")
			return c, &resp
		}
		return c, nil
	}
	if meterKey == "" {
		meterKey = domain.MeterSevenDay
	}
	cs, err := r.d.Store.ListCycles(acct.ID, meterKey, 1)
	if err != nil {
		resp := serverError("list cycles", err)
		return store.Cycle{}, &resp
	}
	if len(cs) == 0 {
		resp := abi.ErrorResponse(http.StatusNotFound, "no cycles for meter "+meterKey)
		return store.Cycle{}, &resp
	}
	return cs[0], nil
}

// downsample keeps at most n points: consecutive groups collapse to their
// highest value (the envelope of a rising meter or running total), and the
// last point always survives.
func downsample(pts []point, n int) []point {
	if n <= 0 {
		return nil
	}
	if len(pts) <= n {
		return pts
	}
	k := (len(pts) + n - 1) / n
	out := make([]point, 0, n+1)
	for i := 0; i < len(pts); i += k {
		best := pts[i]
		for _, p := range pts[i:min(i+k, len(pts))] {
			if p[1] >= best[1] {
				best = p
			}
		}
		out = append(out, best)
	}
	if last := pts[len(pts)-1]; out[len(out)-1] != last {
		out[len(out)-1] = last
	}
	return out
}
