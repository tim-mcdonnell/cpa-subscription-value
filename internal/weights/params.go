package weights

import (
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/pricing"
)

const (
	defaultHalfLife        = 21 * 24 * time.Hour
	defaultRandomWalkSigma = 0.35
	factorPriorSD          = 0.5 // N(0, 0.5) on model and type log factors
)

// DefaultParams returns the per-provider learner settings. RefRate prices a
// feature from table.Lookup: cache writes at the 5-minute rate, the long
// multiplier on input-side buckets and the fast multiplier on every bucket
// (as pricing.Table.Cost does), so h_fast / h_long are learned relative to
// those premiums and their priors are centred on 0.
func DefaultParams(p domain.Provider, table pricing.Table) ProviderParams {
	params := ProviderParams{
		Provider:        p,
		RefRate:         tableRate(p, table),
		FastPrior:       Prior{0, 0.5},
		LongPrior:       Prior{0, 0.3},
		HalfLife:        defaultHalfLife,
		RandomWalkSigma: defaultRandomWalkSigma,
	}
	switch p {
	case domain.ProviderCodex:
		params.Types = []TokenType{CacheRead, Output}
	default:
		params.Types = []TokenType{CacheRead, CacheWrite, Output}
	}
	return params
}

func tableRate(p domain.Provider, table pricing.Table) RateFunc {
	return func(family string, t TokenType, fast, long bool) (float64, bool) {
		r, _, ok := table.Lookup(p, family)
		if !ok {
			return 0, false
		}
		var perM float64
		inputSide := true
		switch t {
		case UncachedInput:
			perM = r.Input
		case CacheRead:
			perM = r.CacheRead
		case CacheWrite:
			perM = r.CacheWrite5m
		case Output:
			perM, inputSide = r.Output, false
		default:
			return 0, false
		}
		if long && inputSide && r.LongCtxMult > 1 {
			perM *= r.LongCtxMult
		}
		if fast && r.FastMult > 1 {
			perM *= r.FastMult
		}
		return perM / 1e6, true
	}
}

func withDefaults(params ProviderParams) ProviderParams {
	if params.HalfLife <= 0 {
		params.HalfLife = defaultHalfLife
	}
	if params.RandomWalkSigma <= 0 {
		params.RandomWalkSigma = defaultRandomWalkSigma
	}
	if params.FastPrior.SD <= 0 {
		params.FastPrior.SD = factorPriorSD
	}
	if params.LongPrior.SD <= 0 {
		params.LongPrior.SD = factorPriorSD
	}
	return params
}
