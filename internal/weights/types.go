// Package weights learns how a subscription meter weights each model family
// and token type relative to its API list price. Ported (MIT) from
// Autsunset/cpa-quota-estimator; see THIRD_PARTY_NOTICES.md.
//
// Model, for segment k of cycle (regime) c:
//
//	Δu_k ≈ exp(s_c) · W_k
//	W_k  = Σ_features tok · refRate(family, type, fast, long) · exp(f_family) · exp(g_type) · exp(h_fast)^fast · exp(h_long)^long
//
// Δu_k = DeltaTicks·Tick is the utilization change as a fraction (1.0 = 100 %).
// refRate is API list USD per token, so W_k is "factor-weighted API USD".
// exp(s_c) is therefore the fraction of the allowance consumed per weighted
// USD, and V_c = exp(−s_c) is the weighted-API-USD worth 100 % of the meter
// (CycleScale.ValuePer100). f_anchor = 0 and g_uncached_input = 0, so V_c is
// denominated in the anchor family's uncached-input prices; with every factor
// at 1.0 it equals the primary estimator's V̂ = Σ$ / Σ(Δticks·tick).
//
// Residuals are in percentage points (100·Δu), the unit the ported Huber
// δ = 0.6, σ_obs = 0.35 and the Fisher-information thresholds were tuned in.
package weights

import (
	"math"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// TokenType is one priced token bucket.
type TokenType string

const (
	UncachedInput TokenType = "uncached_input"
	CacheRead     TokenType = "cache_read"
	CacheWrite    TokenType = "cache_write"
	Output        TokenType = "output"
)

// Feature is the token count of one (family, type, fast, long) cell of a
// segment. Tokens is a raw count (not millions); Family must be a key
// RefRate can price (the event's model_family).
type Feature struct {
	Family string
	Type   TokenType
	Fast   bool
	Long   bool
	Tokens int64
}

// Segment is the spend between two consecutive crossings of one meter under
// one lag. All segments passed to one FitWeights call must belong to a single
// (account, meter): cycles are chained by one random walk in EndAt order.
type Segment struct {
	// CycleKey identifies the reset cycle; RegimeKey a sub-regime inside it
	// (regime_seq). Each distinct (CycleKey, RegimeKey) gets its own scale s_c.
	CycleKey  string
	RegimeKey string
	EndAt     time.Time
	// DeltaTicks·Tick is Δu as a fraction: DeltaTicks is the crossing's level
	// difference in ticks, Tick the fraction per tick (0.01 for whole percent).
	DeltaTicks float64
	Tick       float64
	// Weight is the boundary weight 1/(1+2·boundaryTokens/totalTokens);
	// values ≤ 0 mean unknown and are treated as 1.
	Weight   float64
	Features []Feature
	Lag      string
}

// RateFunc returns the API list price in USD per token. The returned rate
// already includes any fast / long-context multiplier the price table has.
type RateFunc func(family string, t TokenType, fast, long bool) (usdPerTok float64, ok bool)

// Prior is a normal prior on a log factor.
type Prior struct{ Mean, SD float64 }

// ProviderParams replaces every provider-specific constant of the learner.
type ProviderParams struct {
	Provider domain.Provider
	// Anchor is the family whose f is fixed at 0; "" picks the highest
	// reference-$ family among segments that ended within HalfLife of now.
	Anchor  string
	RefRate RateFunc
	// Types lists the token types whose g_t may unlock; uncached_input is the
	// base (g = 0) and ignored here.
	Types     []TokenType
	FastPrior Prior
	LongPrior Prior
	HalfLife  time.Duration // default 21 d
	// RandomWalkSigma is the SD of s_c − s_{c−1} (default 0.35).
	RandomWalkSigma float64
}

// Factor statuses.
const (
	StatusIdentified  = "identified"
	StatusPriorLocked = "prior_locked"
	StatusAnchor      = "anchor"
)

// Factor is one learned multiplier on API price.
type Factor struct {
	Name string `json:"name"`
	// Estimate = exp(param): the meter counts these tokens at Estimate × their API price.
	Estimate float64 `json:"estimate"`
	CILo     float64 `json:"ci_lo"`
	CIHi     float64 `json:"ci_hi"`
	// Status is identified (unlock gates passed, data share ≥ 0.5, not
	// collinear), prior_locked, or anchor.
	Status string `json:"status"`
	// Segments counts eligible segments carrying this factor's tokens.
	Segments int `json:"segments"`
	// ShareSD is the weighted within-cycle SD of this factor's $ share;
	// Fisher the conditional Fisher information for its log factor.
	ShareSD float64 `json:"share_sd"`
	Fisher  float64 `json:"fisher"`
	// DataShare = 1 − posterior var / prior var.
	DataShare float64 `json:"data_share"`
}

// CycleScale is the fitted s_c of one (cycle, regime).
type CycleScale struct {
	CycleKey  string  `json:"cycle_key"`
	RegimeKey string  `json:"regime_key,omitempty"`
	LogScale  float64 `json:"log_scale"` // s_c: ln(fraction of allowance per weighted USD)
	SD        float64 `json:"sd"`        // posterior SD of s_c
	// ValuePer100 = exp(−s_c): weighted-API-USD worth 100 % of the meter.
	ValuePer100 float64 `json:"value_per_100"`
	ValueCILo   float64 `json:"value_ci_lo"`
	ValueCIHi   float64 `json:"value_ci_hi"`
	Segments    int     `json:"segments"`
}

// Fit is one learner run.
type Fit struct {
	Provider domain.Provider
	Anchor   string
	Lag      string
	// Factors keys: "model:<family>", "type:<t>", "fast", "long".
	Factors map[string]Factor
	// Scales in chronological order (first segment EndAt); the last is the latest.
	Scales []CycleScale
	Loss   float64
	// Segments is the input count, Eligible those used; UnknownFamilies are the
	// families whose missing price made a segment ineligible.
	Segments, Eligible int
	UnknownFamilies    []string
	// Backtest maps lag → prequential MAE in percentage points.
	Backtest     map[string]float64
	LagAmbiguous bool
	ComputedAt   time.Time
}

// ModelValuePer100 is V_m = exp(−s_latest − f_m): the API list-price USD of
// family m's usage (at m's own prices, uncached-input basis) that fills 100 %
// of the meter at the latest cycle's scale.
func (f Fit) ModelValuePer100(family string) (usd float64, ok bool) {
	fac, ok := f.Factors["model:"+family]
	if !ok || len(f.Scales) == 0 || fac.Estimate <= 0 {
		return 0, false
	}
	return f.Scales[len(f.Scales)-1].ValuePer100 / fac.Estimate, true
}

// ModelValueCI is the delta-method 95 % interval of V_m, treating s_latest
// and f_m as independent (they come from separate fit stages).
func (f Fit) ModelValueCI(family string) (lo, hi float64, ok bool) {
	v, ok := f.ModelValuePer100(family)
	if !ok {
		return 0, 0, false
	}
	fac := f.Factors["model:"+family]
	sdF := 0.0
	if fac.CILo > 0 && fac.CIHi > 0 {
		sdF = (math.Log(fac.CIHi) - math.Log(fac.CILo)) / (2 * z95)
	}
	sdS := f.Scales[len(f.Scales)-1].SD
	sd := math.Sqrt(sdF*sdF + sdS*sdS)
	return v * math.Exp(-z95*sd), v * math.Exp(z95*sd), true
}

// WeightedUSD is W_k for seg under this fit's factors: Σ tok·refRate·factors.
// ok is false when a feature's family has no reference price.
func (f Fit) WeightedUSD(seg Segment, params ProviderParams) (usd float64, ok bool) {
	mult := func(key string) float64 {
		if fac, ok := f.Factors[key]; ok && fac.Estimate > 0 {
			return fac.Estimate
		}
		return 1
	}
	for _, ft := range seg.Features {
		if ft.Tokens <= 0 {
			continue
		}
		rate, ok := params.RefRate(ft.Family, ft.Type, ft.Fast, ft.Long)
		if !ok {
			return 0, false
		}
		term := float64(ft.Tokens) * rate * mult("model:"+ft.Family) * mult("type:"+string(ft.Type))
		if ft.Fast {
			term *= mult("fast")
		}
		if ft.Long {
			term *= mult("long")
		}
		usd += term
	}
	return usd, true
}
