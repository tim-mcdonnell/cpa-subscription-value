// Package estimate turns lag-attributed segments into V̂ ($ per 100% of a
// meter) with its uncertainty, grade, lag choice and forecast
// (docs/plan.md §Estimator). Primary, ChooseLag, ForecastCycle and Analyze
// are pure; Recompute (run.go) is the only code that touches the store.
package estimate

import (
	"math"
	"slices"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
)

// Options holds the estimator's thresholds; DefaultOptions are the plan's.
type Options struct {
	Phi        float64 // coverage guard: low_usd when $_k < Phi·V̂·Δ_k·tick
	GuardIters int
	Resamples  int
	// Bootstrap units: consecutive used segments merged until they span
	// BlockTicks ticks or BlockDur of time.
	BlockTicks int64
	BlockDur   time.Duration
	SigmaRW    float64 // per-cycle random-walk sd of log V for v_blend
	// Lag choice: argmin of the prequential score once MinLagCrossings are
	// scored, pooled over the last LagHistory cycles of the meter.
	MinLagCrossings int
	LagHistory      int
	// SeriesID seeds the bootstrap (FNV-1a → PCG), so a given series always
	// resamples identically.
	SeriesID string
}

// DefaultOptions returns the plan's values.
func DefaultOptions() Options {
	return Options{
		Phi: 0.2, GuardIters: 3, Resamples: 1000,
		BlockTicks: 5, BlockDur: time.Hour,
		SigmaRW:         0.35,
		MinLagCrossings: 15, LagHistory: 4,
	}
}

// Methods, kinds and grades.
const (
	MethodHeader = "header"
	MethodPoll   = "poll"

	KindRunning = "running"
	KindFinal   = "final"

	GradeInsufficient = "insufficient"
	GradeLow          = "low"
	GradeMedium       = "medium"
	GradeHigh         = "high"
)

// Estimate-level flags (segment and cycle flags live in package meter).
const (
	FlagCoverageGap = meter.FlagCoverageGap
	FlagLevelShift  = "level_shift"
	FlagRegimeSplit = "regime_split"
	FlagLagUnstable = "lag_unstable"
	FlagFewBlocks   = "few_blocks"
)

// excluded are the segment flags that keep a segment out of V̂ (low_usd is
// added by the coverage guard itself). coverage_gap marks segments that
// overlap a poll-detected bypass burst; together with low_usd it makes up
// unexplained_frac.
var excluded = []string{meter.FlagQuotaDrop, meter.FlagRegimeChange, meter.FlagRestartGap, meter.FlagLagIncomplete, meter.FlagCoverageGap}

// Mix is the $ share of the used segments by model family and token type.
type Mix struct {
	ByFamily map[string]float64 `json:"by_family"`
	ByType   map[string]float64 `json:"by_type"`
}

// SubRegime is one side of a within-cycle CUSUM split.
type SubRegime struct {
	FromSeq int     `json:"from_seq"`
	ToSeq   int     `json:"to_seq"`
	VHat    float64 `json:"v_hat"`
	Ticks   int64   `json:"ticks"`
}

// Result is one estimate of a cycle. Its JSON is what /summary, /cycles and
// /series serve; ToStore maps it onto store.Estimate.
type Result struct {
	Kind            string                `json:"kind"`
	Method          string                `json:"method"`
	Lag             meter.Lag             `json:"lag"`
	VHat            float64               `json:"v_hat"`
	VHatCW1h        float64               `json:"v_hat_cw1h"`
	VHatAll         float64               `json:"v_hat_all"`
	VHatExclFailed  float64               `json:"v_hat_excl_failed"`
	CILo            float64               `json:"ci_lo"`
	CIHi            float64               `json:"ci_hi"`
	SEQuant         float64               `json:"se_quant"`
	SEBoot          float64               `json:"se_boot"`
	TicksUsed       int64                 `json:"ticks_used"`
	TicksExcluded   int64                 `json:"ticks_excluded"`
	Tick            float64               `json:"tick"`
	Runs            int                   `json:"runs"`
	Crossings       int                   `json:"crossings"`
	Blocks          int                   `json:"blocks"`
	UnexplainedFrac float64               `json:"unexplained_frac"`
	Grade           string                `json:"grade"`
	Mix             Mix                   `json:"mix_json"`
	VBlend          float64               `json:"v_blend,omitempty"`
	Flags           []string              `json:"flags"`
	LagScores       map[meter.Lag]float64 `json:"lag_scores,omitempty"`
	LagStable       bool                  `json:"lag_stable"`
	CoverageGapTick int64                 `json:"coverage_gap_ticks,omitempty"`
	CoverageGapUSD  float64               `json:"coverage_gap_usd,omitempty"`
	SubRegimes      []SubRegime           `json:"sub_regimes,omitempty"`
	Forecast        *Forecast             `json:"forecast,omitempty"`

	lowUSD map[int]bool // seq → flagged by the coverage guard
	blocks []block
}

// SE is the combined standard error √(SE_q² + SE_b²).
func (r Result) SE() float64 { return math.Hypot(r.SEQuant, r.SEBoot) }

// RelWidth is (ci_hi − ci_lo)/V̂, the grade's width criterion.
func (r Result) RelWidth() float64 {
	if r.VHat == 0 {
		return math.Inf(1)
	}
	return (r.CIHi - r.CILo) / r.VHat
}

func (r *Result) flag(f string) {
	if !slices.Contains(r.Flags, f) {
		r.Flags = append(r.Flags, f)
	}
}

// Primary computes V̂ for one cycle's segments under one lag
// (§Primary estimate, §Variance, §Coverage guard, §Grade).
func Primary(segs []meter.Segment, o Options) Result {
	r := Result{Flags: []string{}, Mix: Mix{ByFamily: map[string]float64{}, ByType: map[string]float64{}}}
	if len(segs) > 0 {
		r.Tick = segs[0].Tick
	}
	var usdAll, dAll float64
	var ticksAll int64
	eligible := make([]bool, len(segs))
	for k, s := range segs {
		usdAll += s.USD
		dAll += s.D()
		ticksAll += s.DeltaTicks
		eligible[k] = !slices.ContainsFunc(excluded, s.Has)
	}
	if dAll > 0 {
		r.VHatAll = usdAll / dAll
	}

	// Coverage guard: flag segments whose $ is under Phi of what V̂ says the
	// movement should have cost, recompute, at most GuardIters times.
	low := map[int]bool{}
	for it := 0; it < o.GuardIters; it++ {
		v := ratio(segs, eligible, low)
		if v == 0 {
			break
		}
		next := map[int]bool{}
		for k, s := range segs {
			if eligible[k] && s.USD < o.Phi*v*s.D() {
				next[k] = true
			}
		}
		same := len(next) == len(low)
		for k := range next {
			same = same && low[k]
		}
		low = next
		if same {
			break
		}
	}
	used := make([]bool, len(segs))
	var usd, cw1h, failed, d float64
	var lowTicks int64
	r.lowUSD = map[int]bool{}
	for k, s := range segs {
		if low[k] {
			r.lowUSD[s.Seq] = true
		}
		if low[k] || s.Has(meter.FlagCoverageGap) {
			lowTicks += s.DeltaTicks
		}
		used[k] = eligible[k] && !low[k]
		if !used[k] {
			continue
		}
		usd += s.USD
		cw1h += s.USDCacheWrite1h
		failed += s.USDFailed
		d += s.D()
		r.TicksUsed += s.DeltaTicks
		r.Crossings++
		if k == 0 || !used[k-1] {
			r.Runs++
		}
		for f, v := range s.USDByFamily {
			r.Mix.ByFamily[f] += v
		}
		r.Mix.ByType["uncached_input"] += s.USDByType.UncachedInput
		r.Mix.ByType["cache_read"] += s.USDByType.CacheRead
		r.Mix.ByType["cache_write"] += s.USDByType.CacheWrite
		r.Mix.ByType["output"] += s.USDByType.Output
	}
	r.TicksExcluded = ticksAll - r.TicksUsed
	if ticksAll > 0 {
		r.UnexplainedFrac = float64(lowTicks) / float64(ticksAll)
	}
	if d == 0 || usd == 0 {
		r.Grade = GradeInsufficient
		return r
	}
	// V̂ = Σ$_k / D, D = Σ Δ_k·tick.
	r.VHat = usd / d
	r.VHatCW1h = cw1h / d
	r.VHatExclFailed = (usd - failed) / d

	// Var_q(D) = R·tick²/6: each run's two endpoints overshoot by U(0, tick)
	// and interior endpoints telescope. By the delta method SE_q/V̂ = SE(D)/D.
	r.SEQuant = r.VHat * r.Tick * math.Sqrt(float64(r.Runs)/6) / d

	r.blocks = makeBlocks(segs, used, o)
	r.Blocks = len(r.blocks)
	if len(r.blocks) < 3 {
		r.flag(FlagFewBlocks)
	}
	r.SEBoot = bootstrapSE(r.blocks, o.Resamples, o.SeriesID)

	// CI95 = V̂·exp(±1.96·√((SE_q/V̂)² + (SE_b/V̂)²)), combined in log space.
	z := 1.96 * math.Hypot(r.SEQuant/r.VHat, r.SEBoot/r.VHat)
	r.CILo, r.CIHi = r.VHat*math.Exp(-z), r.VHat*math.Exp(z)

	r.Grade = Grade(GradeInput{Ticks: r.TicksUsed, Crossings: r.Crossings, RelWidth: r.RelWidth(), Unexplained: r.UnexplainedFrac, LagStable: true})
	return r
}

func ratio(segs []meter.Segment, eligible []bool, low map[int]bool) float64 {
	var usd, d float64
	for k, s := range segs {
		if eligible[k] && !low[k] {
			usd += s.USD
			d += s.D()
		}
	}
	if d == 0 {
		return 0
	}
	return usd / d
}

// GradeInput is everything the grade depends on.
type GradeInput struct {
	Ticks           int64
	Crossings       int
	RelWidth        float64
	Unexplained     float64
	RegimeChange    bool
	PrecisionChange bool
	LagStable       bool
	CapMedium       bool // poll-only method
}

// Grade implements §Grade. The plan's bands leave a hole (≥15 ticks, width
// ≤30%, 0.15 < unexplained ≤ 0.30); anything that is neither medium nor
// high, and not insufficient, is low.
func Grade(g GradeInput) string {
	switch {
	case g.Ticks < 5 || g.Crossings < 3:
		return GradeInsufficient
	case g.RegimeChange:
		return GradeLow
	case !g.CapMedium && g.Ticks >= 30 && g.RelWidth <= 0.15 && g.Unexplained <= 0.05 && !g.PrecisionChange && g.LagStable:
		return GradeHigh
	case g.Ticks >= 15 && g.RelWidth <= 0.30 && g.Unexplained <= 0.15:
		return GradeMedium
	}
	return GradeLow
}
