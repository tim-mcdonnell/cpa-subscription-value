package api

import (
	"net/http"
	"net/url"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/estimate"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// accountSummary is /summary for one account.
type accountSummary struct {
	Account                accountView    `json:"account"`
	CoverageMode           string         `json:"coverage_mode"`
	CapacityDisabledReason string         `json:"capacity_disabled_reason,omitempty"`
	GeneratedAt            time.Time      `json:"generated_at"`
	Meters                 []meterSummary `json:"meters"`
}

// meterSummary is the current cycle of one meter.
type meterSummary struct {
	Meter          string        `json:"meter"`
	Role           string        `json:"role"` // headline | secondary | scoped
	Cycle          *cycleView    `json:"cycle"`
	UsedFraction   *float64      `json:"used_fraction"`
	UsedAt         *time.Time    `json:"used_at"`
	UsedSource     string        `json:"used_source,omitempty"`
	LatestEstimate *estimateView `json:"latest_estimate"`
	Forecast       *forecastView `json:"forecast"`
	SpentUSD       float64       `json:"spent_usd"` // Σ api_usd of (scoped) events in the cycle
	Events         int           `json:"events"`
	Mix            mixView       `json:"mix"`
	Collecting     *collecting   `json:"collecting,omitempty"`
}

// forecastView is estimate.Forecast computed live; pointer fields are
// capacity figures (null without a V̂ or when coverage disables them).
type forecastView struct {
	UNow            float64    `json:"u_now"`
	PTime           float64    `json:"p_time"`
	Status          string     `json:"status"`
	BurnPerHour     float64    `json:"burn_per_hour"`
	PaceWindowH     float64    `json:"pace_window_h"`
	TExhaust        *time.Time `json:"t_exhaust"`
	UEnd            float64    `json:"u_end"`
	ProxiedUSD      float64    `json:"proxied_usd"`
	RemainingUSD    *float64   `json:"remaining_usd"`
	RemainingLo     *float64   `json:"remaining_lo"`
	RemainingHi     *float64   `json:"remaining_hi"`
	ImpliedSpentUSD *float64   `json:"implied_spent_usd"` // u_now·V̂
	USDPerDay       *float64   `json:"usd_per_day"`
	UnusedUSD       *float64   `json:"unused_usd"`
}

func newForecastView(f estimate.Forecast, capacity bool) *forecastView {
	money := func(v float64) *float64 {
		if !capacity {
			return nil
		}
		return &v
	}
	return &forecastView{
		UNow: f.UNow, PTime: f.PTime, Status: f.Status, BurnPerHour: f.BurnPerHour, PaceWindowH: f.PaceWindowH,
		TExhaust: tsPtrP(f.TExhaust), UEnd: f.UEnd, ProxiedUSD: f.ProxiedUSD,
		RemainingUSD: money(f.RemainingUSD), RemainingLo: money(f.RemainingLo), RemainingHi: money(f.RemainingHi),
		ImpliedSpentUSD: money(f.SpentUSD), USDPerDay: money(f.USDPerDay), UnusedUSD: money(f.UnusedUSD),
	}
}

func (r *Router) handleSummary(q url.Values, _ []byte) abi.ManagementResponse {
	now := r.now()
	if q.Get("auth_index") == "" {
		accounts, err := r.d.Store.ListAccounts()
		if err != nil {
			return serverError("list accounts", err)
		}
		out := make([]accountSummary, 0, len(accounts))
		for _, a := range accounts {
			s, err := r.summarize(a, now)
			if err != nil {
				return serverError("summary "+a.AuthIndex, err)
			}
			out = append(out, s)
		}
		return abi.JSONResponse(http.StatusOK, map[string]any{"generated_at": now.UTC(), "accounts": out})
	}
	acct, resp := r.resolveAccount(q.Get("auth_index"), true)
	if resp != nil {
		return *resp
	}
	s, err := r.summarize(acct, now)
	if err != nil {
		return serverError("summary", err)
	}
	return abi.JSONResponse(http.StatusOK, s)
}

func (r *Router) summarize(a domain.Account, now time.Time) (accountSummary, error) {
	av, err := r.accountView(a)
	if err != nil {
		return accountSummary{}, err
	}
	mode := av.CoverageMode
	out := accountSummary{Account: av, CoverageMode: mode, CapacityDisabledReason: capacityDisabledReason(mode),
		GeneratedAt: now.UTC(), Meters: []meterSummary{}}
	capacity := mode == CoverageCPAOnly

	meters := summaryMeters(av.Meters)
	current := map[string]store.Cycle{}
	earliest := time.Time{}
	for _, m := range meters {
		cs, err := r.d.Store.ListCycles(a.ID, m, 1)
		if err != nil {
			return out, err
		}
		if len(cs) == 0 {
			continue
		}
		current[m] = cs[0]
		if earliest.IsZero() || cs[0].StartsAt.Before(earliest) {
			earliest = cs[0].StartsAt
		}
	}
	var events []domain.UsageEvent
	if !earliest.IsZero() {
		if events, err = r.loadEvents(a.ID, earliest, time.Time{}); err != nil {
			return out, err
		}
	}
	for _, m := range meters {
		ms := meterSummary{Meter: m, Role: meterRole(m), Mix: mixView{ByFamily: map[string]float64{}, ByType: map[string]float64{}, Source: "events"}}
		if c, ok := current[m]; ok {
			if err := r.fillMeterSummary(&ms, a, c, events, now, capacity); err != nil {
				return out, err
			}
		} else {
			ms.Collecting = &collecting{TicksNeeded: minTicks, CrossingsNeeded: minCrossings}
		}
		out.Meters = append(out.Meters, ms)
	}
	return out, nil
}

func (r *Router) fillMeterSummary(ms *meterSummary, a domain.Account, c store.Cycle, events []domain.UsageEvent, now time.Time, capacity bool) error {
	cv := newCycleView(c)
	ms.Cycle = &cv
	readings, err := r.cycleReadings(a.ID, c)
	if err != nil {
		return err
	}
	var latest *domain.MeterReading
	for i := range readings {
		if !readings[i].ObservedAt.After(now) {
			latest = &readings[i]
		}
	}
	if latest != nil {
		u := latest.UsedFraction
		ms.UsedFraction, ms.UsedAt, ms.UsedSource = &u, tsPtr(latest.ObservedAt), latest.Source
	}

	scoped := eventsIn(meter.ScopeEvents(c.MeterKey, events, meter.DefaultScopes()), c.StartsAt, cycleEnd(c, now))
	for _, e := range scoped {
		ms.SpentUSD += e.APIUSD
	}
	ms.Events = len(scoped)
	ms.Mix = mixFromEvents(scoped)

	est, hasEst, err := r.d.Store.LatestEstimate(c.ID, "")
	if err != nil {
		return err
	}
	var vr estimate.Result
	if hasEst {
		ev := newEstimateView(est, capacity)
		ms.LatestEstimate = &ev
		ms.Collecting = ev.Collecting
		vr = estimate.Result{VHat: est.VHat, CILo: est.CILo, CIHi: est.CIHi}
	} else {
		ms.Collecting = &collecting{TicksNeeded: minTicks, CrossingsNeeded: minCrossings}
	}
	if c.ClosedAt.IsZero() {
		f := estimate.ForecastCycle(assignment(c, readings, scoped), vr, ms.SpentUSD, now)
		ms.Forecast = newForecastView(f, capacity && vr.VHat > 0)
	}
	return nil
}

// assignment rebuilds the meter.CycleAssignment ForecastCycle needs from the
// stored cycle and its readings; header readings of failed attempts are
// marked so the forecast skips them.
func assignment(c store.Cycle, readings []domain.MeterReading, events []domain.UsageEvent) meter.CycleAssignment {
	failed := map[int64]bool{}
	for _, e := range events {
		if e.Failed {
			failed[e.ID] = true
		}
	}
	ca := meter.CycleAssignment{
		MeterKey: c.MeterKey, WindowSec: c.WindowSec, ResetAnchor: c.ResetAnchor, StartsAt: c.StartsAt,
		FirstReadingAt: c.FirstReadingAt, ClosedAt: c.ClosedAt, RegimeSeq: c.RegimeSeq, Tick: c.Tick,
		PeakFraction: c.PeakFraction, Flags: c.Flags,
	}
	for _, m := range readings {
		ca.Readings = append(ca.Readings, meter.AssignedReading{MeterReading: m, Failed: m.EventID != nil && failed[*m.EventID]})
	}
	return ca
}

func tsPtrP(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return tsPtr(*t)
}
