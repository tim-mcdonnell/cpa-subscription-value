package estimate

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// FlagPricingChanged marks a cycle whose events span two price snapshots.
const FlagPricingChanged = "pricing_changed"

// Recompute re-derives one (account, meter) from stored readings and events
// with the default options. See RecomputeWith.
func Recompute(st *store.Store, accountID int64, meterKey string, now time.Time) error {
	return RecomputeWith(st, accountID, meterKey, now, DefaultAnalyzeOptions())
}

// RecomputeWith runs Analyze over everything stored for the meter and
// persists it: UpsertCycle + SetReadingCycles per cycle, ReplaceSegments
// (all lags), and one estimate. Meters that are neither estimable (5h, 7d)
// nor given a scope regex are stored and charted only: cycles and reading
// assignments, no segments or estimates. Closed cycles that already hold a final for
// their price hash are frozen and left untouched; a running estimate is
// appended only when it differs from the cycle's latest one, so repeated
// calls on unchanged data leave the tables byte-identical.
func RecomputeWith(st *store.Store, accountID int64, meterKey string, now time.Time, o AnalyzeOptions) error {
	readings, err := loadReadings(st, accountID, meterKey)
	if err != nil || len(readings) == 0 {
		return err
	}
	var window int64
	for _, r := range readings {
		window = max(window, r.WindowSec)
	}
	events, err := loadEvents(st, accountID, readings[0].ObservedAt.Add(-time.Duration(window)*time.Second))
	if err != nil {
		return err
	}
	if o.Series == "" {
		o.Series = fmt.Sprint(accountID)
	}
	existing, err := st.ListCycles(accountID, meterKey, 5000)
	if err != nil {
		return err
	}
	type key struct {
		anchor int64
		regime int
	}
	stored := map[key]store.Cycle{}
	for _, c := range existing {
		stored[key{c.ResetAnchor.Unix(), c.RegimeSeq}] = c
	}
	scoped := meter.ScopeEvents(meterKey, events, o.Scopes)
	estimable := domain.IsEstimable(meterKey) || meter.IsScoped(meterKey, o.Scopes)
	assigned := map[int64]bool{}
	for _, cr := range Analyze(meterKey, readings, events, now, o) {
		var ids []int64
		for _, r := range cr.Cycle.Readings {
			ids = append(ids, r.ID)
			assigned[r.ID] = true
		}
		hash, mixed := priceHash(scoped, cr.Cycle.StartsAt, cr.Cycle.End(now))
		if ex, ok := stored[key{cr.Cycle.ResetAnchor.Unix(), cr.Cycle.RegimeSeq}]; ok && !ex.ClosedAt.IsZero() {
			if fin, ok, err := st.LatestEstimate(ex.ID, KindFinal); err != nil {
				return err
			} else if ok && fin.PriceHash == hash {
				continue
			}
		}
		sc, err := st.UpsertCycle(storeCycle(accountID, cr.Cycle))
		if err != nil {
			return err
		}
		if err := st.SetReadingCycles(ids, &sc.ID); err != nil {
			return err
		}
		if !estimable {
			continue
		}
		if err := st.ReplaceSegments(sc.ID, storeSegments(cr.Segments)); err != nil {
			return err
		}
		e, err := storeEstimate(sc.ID, cr, now, hash, mixed)
		if err != nil {
			return err
		}
		if e.Kind == KindRunning {
			if last, ok, err := st.LatestEstimate(sc.ID, KindRunning); err != nil {
				return err
			} else if ok && sameEstimate(last, e) {
				continue
			}
		}
		if err := st.InsertEstimate(&e); err != nil {
			return err
		}
	}
	var orphans []int64
	for _, r := range readings {
		if !assigned[r.ID] && r.CycleID != nil {
			orphans = append(orphans, r.ID)
		}
	}
	return st.SetReadingCycles(orphans, nil)
}

const pageSize = 5000

// loadReadings pages through the store's capped listing by observed_at.
func loadReadings(st *store.Store, accountID int64, meterKey string) ([]domain.MeterReading, error) {
	var out []domain.MeterReading
	seen := map[int64]bool{}
	var from time.Time
	for {
		page, err := st.ListReadings(store.ReadingQuery{AccountID: accountID, MeterKey: meterKey, From: from, Limit: pageSize})
		if err != nil {
			return nil, err
		}
		added := 0
		for _, r := range page {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
				added++
			}
		}
		if len(page) < pageSize || added == 0 {
			return out, nil
		}
		from = page[len(page)-1].ObservedAt
	}
}

func loadEvents(st *store.Store, accountID int64, from time.Time) ([]domain.UsageEvent, error) {
	var out []domain.UsageEvent
	seen := map[int64]bool{}
	for {
		page, err := st.ListEvents(store.EventQuery{AccountID: accountID, From: from, Limit: pageSize})
		if err != nil {
			return nil, err
		}
		added := 0
		for _, e := range page {
			if !seen[e.ID] {
				seen[e.ID] = true
				e.HeadersJSON = nil
				out = append(out, e)
				added++
			}
		}
		if len(page) < pageSize || added == 0 {
			return out, nil
		}
		from = page[len(page)-1].ObservedAt
	}
}

// priceHash is the most common price hash of the cycle's events and
// whether more than one appears.
func priceHash(evs []domain.UsageEvent, from, to time.Time) (string, bool) {
	counts := map[string]int{}
	for _, e := range evs {
		if !e.ObservedAt.Before(from) && e.ObservedAt.Before(to) {
			counts[e.PriceHash]++
		}
	}
	best, n := "", -1
	for h, c := range counts {
		if c > n || (c == n && h < best) {
			best, n = h, c
		}
	}
	return best, len(counts) > 1
}

func storeCycle(accountID int64, c meter.CycleAssignment) store.Cycle {
	return store.Cycle{
		AccountID: accountID, MeterKey: c.MeterKey, WindowSec: c.WindowSec, ResetAnchor: c.ResetAnchor,
		StartsAt: c.StartsAt, FirstReadingAt: c.FirstReadingAt, ClosedAt: c.ClosedAt, EndReason: c.EndReason,
		RegimeSeq: c.RegimeSeq, Tick: c.Tick, PeakFraction: c.PeakFraction, Flags: c.Flags,
	}
}

// features is a segment's features_json.
type features struct {
	Tokens      map[string]meter.TokenCounts `json:"tokens"`
	FastTokens  int64                        `json:"fast_tokens"`
	LongTokens  int64                        `json:"long_tokens"`
	USDByFamily map[string]float64           `json:"usd_by_family"`
	USDByType   meter.TypeUSD                `json:"usd_by_type"`
	USDFailed   float64                      `json:"usd_failed"`
}

func storeSegments(byLag map[meter.Lag][]meter.Segment) []store.Segment {
	var out []store.Segment
	for _, lag := range meter.Lags {
		for _, s := range byLag[lag] {
			f, _ := json.Marshal(features{s.Tokens, s.FastTokens, s.LongTokens, s.USDByFamily, s.USDByType, s.USDFailed})
			from, to := s.FromReadingID, s.ToReadingID
			out = append(out, store.Segment{
				Lag: string(s.Lag), Seq: s.Seq, FromLevel: s.FromLevel, ToLevel: s.ToLevel, DeltaTicks: s.DeltaTicks,
				FromReadingID: &from, ToReadingID: &to, TStart: s.TStart, TEnd: s.TEnd,
				USD: s.USD, USDCacheWrite1h: s.USDCacheWrite1h, Features: f,
				NEvents: s.NEvents, NFailed: s.NFailed, Flags: s.Flags,
			})
		}
	}
	return out
}

// storedMix is mix_json: the plan's {by_family, by_type} plus a diag
// object carrying the Result fields that have no column of their own.
type storedMix struct {
	Mix
	Diag diag `json:"diag"`
}

type diag struct {
	VHatAll         float64               `json:"v_hat_all"`
	VHatExclFailed  float64               `json:"v_hat_excl_failed"`
	Tick            float64               `json:"tick"`
	Crossings       int                   `json:"crossings"`
	Blocks          int                   `json:"blocks"`
	LagScores       map[meter.Lag]float64 `json:"lag_scores,omitempty"`
	LagStable       bool                  `json:"lag_stable"`
	CoverageGapTick int64                 `json:"coverage_gap_ticks,omitempty"`
	CoverageGapUSD  float64               `json:"coverage_gap_usd,omitempty"`
	SubRegimes      []SubRegime           `json:"sub_regimes,omitempty"`
	Forecast        *Forecast             `json:"forecast,omitempty"`
	ProxiedUSD      float64               `json:"proxied_usd"`
	HeaderCoverage  float64               `json:"header_coverage"`
}

func storeEstimate(cycleID int64, cr CycleResult, now time.Time, hash string, mixedPrices bool) (store.Estimate, error) {
	r := cr.Result
	flags := slices.Clone(r.Flags)
	if mixedPrices {
		flags = append(flags, FlagPricingChanged)
	}
	mix, err := json.Marshal(storedMix{Mix: r.Mix, Diag: diag{
		VHatAll: r.VHatAll, VHatExclFailed: r.VHatExclFailed, Tick: r.Tick, Crossings: r.Crossings, Blocks: r.Blocks,
		LagScores: r.LagScores, LagStable: r.LagStable, CoverageGapTick: r.CoverageGapTick, CoverageGapUSD: r.CoverageGapUSD,
		SubRegimes: r.SubRegimes, Forecast: r.Forecast, ProxiedUSD: cr.ProxiedUSD, HeaderCoverage: cr.HeaderCoverage,
	}})
	if err != nil {
		return store.Estimate{}, err
	}
	return store.Estimate{
		CycleID: cycleID, ComputedAt: now, Kind: r.Kind, Method: r.Method, Lag: string(r.Lag),
		VHat: r.VHat, VHatCacheWrite1h: r.VHatCW1h, CILo: r.CILo, CIHi: r.CIHi, SEQuant: r.SEQuant, SEBoot: r.SEBoot,
		TicksUsed: r.TicksUsed, TicksExcluded: r.TicksExcluded, Runs: r.Runs, UnexplainedFrac: r.UnexplainedFrac,
		Grade: r.Grade, Mix: mix, VBlend: r.VBlend, Flags: flags, PriceHash: hash,
	}, nil
}

// sameEstimate compares everything but id, computed_at and the forecast
// (which moves with now; /summary should recompute it live with
// ForecastCycle), so a running row is appended only when the estimate moved.
func sameEstimate(a, b store.Estimate) bool {
	return a.Method == b.Method && a.Lag == b.Lag && a.VHat == b.VHat && a.CILo == b.CILo && a.CIHi == b.CIHi &&
		a.TicksUsed == b.TicksUsed && a.TicksExcluded == b.TicksExcluded && a.Grade == b.Grade &&
		a.VBlend == b.VBlend && slices.Equal(a.Flags, b.Flags) && a.PriceHash == b.PriceHash &&
		mixSansForecast(a.Mix) == mixSansForecast(b.Mix)
}

func mixSansForecast(raw json.RawMessage) string {
	var m storedMix
	if json.Unmarshal(raw, &m) != nil {
		return string(raw)
	}
	m.Diag.Forecast = nil
	out, _ := json.Marshal(m)
	return string(out)
}
