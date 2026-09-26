package api

import (
	"encoding/json"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/estimate"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// Grade thresholds for "insufficient" (estimate.Grade): the UI shows
// progress toward them while a cycle is still collecting.
const (
	minTicks     = 5
	minCrossings = 3
)

// Coverage modes (settings key coverage_<auth_index>). Only cpa_only lets
// V̂ stand for the allowance: with traffic outside CPA, proxied $ per % is a
// floor, not a capacity.
const (
	CoverageCPAOnly = "cpa_only"
	CoverageMixed   = "mixed"
	CoverageUnknown = "unknown"
)

var coverageModes = []string{CoverageCPAOnly, CoverageMixed, CoverageUnknown}

func coverageKey(authIndex string) string { return "coverage_" + authIndex }

// coverageMode defaults to cpa_only: the plan assumes near-zero unrouted usage.
func (r *Router) coverageMode(authIndex string) string {
	var m string
	if ok, err := r.d.Store.GetSetting(coverageKey(authIndex), &m); err == nil && ok && slices.Contains(coverageModes, m) {
		return m
	}
	return CoverageCPAOnly
}

func capacityDisabledReason(mode string) string {
	switch mode {
	case CoverageMixed:
		return "coverage is mixed: some of this account's usage bypasses CPA, so proxied $ per % understates the allowance"
	case CoverageUnknown:
		return "coverage is unknown: confirm that all of this account's usage flows through CPA to enable capacity figures"
	}
	return ""
}

// meterRank orders meters headline first: 7d, 5h, 7d_oi, then the rest.
func meterRank(k string) int {
	switch k {
	case domain.MeterSevenDay:
		return 0
	case domain.MeterFiveHour:
		return 1
	case domain.MeterSevenDayOI:
		return 2
	}
	return 3
}

func sortMeters(keys []string) []string {
	out := slices.Clone(keys)
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := meterRank(out[i]), meterRank(out[j]); a != b {
			return a < b
		}
		return out[i] < out[j]
	})
	return out
}

// summaryMeters are the meters /summary and /recompute cover: 7d, 5h and
// the Fable-scoped 7d_oi when the account reports it.
func summaryMeters(keys []string) []string {
	var out []string
	for _, k := range sortMeters(keys) {
		if meterRank(k) < 3 {
			out = append(out, k)
		}
	}
	return out
}

func meterRole(k string) string {
	switch k {
	case domain.MeterSevenDay:
		return "headline"
	case domain.MeterFiveHour:
		return "secondary"
	}
	return "scoped"
}

// cycleView is a store.Cycle in the API's vocabulary.
type cycleView struct {
	ID           int64      `json:"cycle_id"`
	MeterKey     string     `json:"meter"`
	WindowSec    int64      `json:"window_s"`
	StartsAt     time.Time  `json:"starts_at"`
	ResetAt      time.Time  `json:"reset_at"`
	ClosedAt     *time.Time `json:"closed_at"`
	EndReason    string     `json:"end_reason,omitempty"`
	RegimeSeq    int        `json:"regime_seq"`
	Tick         float64    `json:"tick"`
	PeakFraction float64    `json:"peak_fraction"`
	Open         bool       `json:"open"`
	Flags        []string   `json:"cycle_flags"`
}

func newCycleView(c store.Cycle) cycleView {
	return cycleView{
		ID: c.ID, MeterKey: c.MeterKey, WindowSec: c.WindowSec, StartsAt: c.StartsAt.UTC(), ResetAt: c.ResetAnchor.UTC(),
		ClosedAt: tsPtr(c.ClosedAt), EndReason: c.EndReason, RegimeSeq: c.RegimeSeq, Tick: c.Tick,
		PeakFraction: c.PeakFraction, Open: c.ClosedAt.IsZero(), Flags: nonNil(c.Flags),
	}
}

func cycleEnd(c store.Cycle, now time.Time) time.Time {
	if c.ClosedAt.IsZero() {
		return now
	}
	return c.ClosedAt
}

// storedMix mirrors estimate's mix_json: {by_family, by_type, diag}.
type storedMix struct {
	ByFamily map[string]float64 `json:"by_family"`
	ByType   map[string]float64 `json:"by_type"`
	Diag     struct {
		VHatAll         float64              `json:"v_hat_all"`
		VHatExclFailed  float64              `json:"v_hat_excl_failed"`
		Tick            float64              `json:"tick"`
		Crossings       int                  `json:"crossings"`
		Blocks          int                  `json:"blocks"`
		LagScores       map[string]float64   `json:"lag_scores"`
		LagStable       bool                 `json:"lag_stable"`
		CoverageGapTick int64                `json:"coverage_gap_ticks"`
		CoverageGapUSD  float64              `json:"coverage_gap_usd"`
		SubRegimes      []estimate.SubRegime `json:"sub_regimes"`
		ProxiedUSD      float64              `json:"proxied_usd"`
		HeaderCoverage  float64              `json:"header_coverage"`
	} `json:"diag"`
}

func parseMix(raw json.RawMessage) storedMix {
	var m storedMix
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

// mixView is $ shares (each map sums to 1, or is empty).
type mixView struct {
	ByFamily map[string]float64 `json:"by_family"`
	ByType   map[string]float64 `json:"by_type"`
	USD      float64            `json:"usd"`
	Source   string             `json:"source"` // estimate (used segments) | events (all proxied $)
}

func shares(m map[string]float64) (map[string]float64, float64) {
	out := map[string]float64{}
	var sum float64
	for _, v := range m {
		sum += v
	}
	if sum <= 0 {
		return out, 0
	}
	for k, v := range m {
		if v > 0 {
			out[k] = v / sum
		}
	}
	return out, sum
}

func mixFromEstimate(m storedMix) mixView {
	fam, usd := shares(m.ByFamily)
	typ, _ := shares(m.ByType)
	return mixView{ByFamily: fam, ByType: typ, USD: usd, Source: "estimate"}
}

func mixFromEvents(evs []domain.UsageEvent) mixView {
	fam, typ := map[string]float64{}, map[string]float64{}
	for _, e := range evs {
		f := e.ModelFamily
		if f == "" {
			f = "unknown"
		}
		fam[f] += e.APIUSD
		s := meter.RatioSplit(e)
		typ["uncached_input"] += s.UncachedInput
		typ["cache_read"] += s.CacheRead
		typ["cache_write"] += s.CacheWrite
		typ["output"] += s.Output
	}
	fs, usd := shares(fam)
	ts, _ := shares(typ)
	return mixView{ByFamily: fs, ByType: ts, USD: usd, Source: "events"}
}

// collecting is progress toward a gradeable estimate.
type collecting struct {
	TicksUsed       int64 `json:"ticks_used"`
	TicksNeeded     int64 `json:"ticks_needed"`
	Crossings       int   `json:"crossings"`
	CrossingsNeeded int   `json:"crossings_needed"`
}

// estimateView is one stored estimate. Pointer fields are the capacity
// figures, null when the account's coverage mode disables them.
type estimateView struct {
	ID              int64                `json:"id"`
	Kind            string               `json:"kind"`
	Method          string               `json:"method"`
	Lag             string               `json:"lag"`
	ComputedAt      time.Time            `json:"computed_at"`
	VHat            *float64             `json:"v_hat"`
	VHatCW1h        *float64             `json:"v_hat_cw1h"`
	CILo            *float64             `json:"ci_lo"`
	CIHi            *float64             `json:"ci_hi"`
	VBlend          *float64             `json:"v_blend"`
	VHatAll         *float64             `json:"v_hat_all"`
	SEQuant         float64              `json:"se_quant"`
	SEBoot          float64              `json:"se_boot"`
	Grade           string               `json:"grade"`
	TicksUsed       int64                `json:"ticks_used"`
	TicksExcluded   int64                `json:"ticks_excluded"`
	Crossings       int                  `json:"crossings"`
	Tick            float64              `json:"tick"`
	UnexplainedFrac float64              `json:"unexplained_frac"`
	CoverageGapUSD  *float64             `json:"coverage_gap_usd"`
	HeaderCoverage  float64              `json:"header_coverage"`
	LagStable       bool                 `json:"lag_stable"`
	SubRegimes      []estimate.SubRegime `json:"sub_regimes,omitempty"`
	Flags           []string             `json:"flags"`
	PriceHash       string               `json:"price_hash"`
	Collecting      *collecting          `json:"collecting,omitempty"`
}

func newEstimateView(e store.Estimate, capacity bool) estimateView {
	m := parseMix(e.Mix)
	money := func(v float64) *float64 {
		if !capacity || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		return &v
	}
	v := estimateView{
		ID: e.ID, Kind: e.Kind, Method: e.Method, Lag: e.Lag, ComputedAt: e.ComputedAt.UTC(),
		VHat: money(e.VHat), VHatCW1h: money(e.VHatCacheWrite1h), CILo: money(e.CILo), CIHi: money(e.CIHi),
		VBlend: money(e.VBlend), VHatAll: money(m.Diag.VHatAll), SEQuant: e.SEQuant, SEBoot: e.SEBoot,
		Grade: e.Grade, TicksUsed: e.TicksUsed, TicksExcluded: e.TicksExcluded, Crossings: m.Diag.Crossings,
		Tick: m.Diag.Tick, UnexplainedFrac: e.UnexplainedFrac, CoverageGapUSD: money(m.Diag.CoverageGapUSD),
		HeaderCoverage: m.Diag.HeaderCoverage, LagStable: m.Diag.LagStable, SubRegimes: m.Diag.SubRegimes,
		Flags: nonNil(e.Flags), PriceHash: e.PriceHash,
	}
	if e.Grade == estimate.GradeInsufficient {
		v.Collecting = &collecting{TicksUsed: e.TicksUsed, TicksNeeded: minTicks, Crossings: m.Diag.Crossings, CrossingsNeeded: minCrossings}
	}
	return v
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func flagUnion(lists ...[]string) []string {
	out := []string{}
	for _, l := range lists {
		for _, f := range l {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	sort.Strings(out)
	return out
}
