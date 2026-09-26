package weights

import (
	"encoding/json"
	"fmt"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// storedBacktest is backtest_json. It also carries the Fit fields the
// weight_fits table has no column for, so FromStore(ToStore(f)) == f.
type storedBacktest struct {
	Provider        domain.Provider    `json:"provider"`
	MAE             map[string]float64 `json:"mae,omitempty"`
	LagAmbiguous    bool               `json:"lag_ambiguous"`
	Segments        int                `json:"segments"`
	Eligible        int                `json:"eligible"`
	UnknownFamilies []string           `json:"unknown_families,omitempty"`
}

// ToStore serializes the fit into a weight_fits row: factors_json is the
// Factors map, scales_json the Scales list, backtest_json a storedBacktest.
func (f Fit) ToStore(accountID int64, meterKey, priceHash string) store.WeightFit {
	return store.WeightFit{
		AccountID:    accountID,
		MeterKey:     meterKey,
		ComputedAt:   f.ComputedAt,
		Lag:          f.Lag,
		AnchorFamily: f.Anchor,
		Factors:      mustJSON(f.Factors),
		Scales:       mustJSON(f.Scales),
		Loss:         f.Loss,
		Backtest: mustJSON(storedBacktest{Provider: f.Provider, MAE: f.Backtest, LagAmbiguous: f.LagAmbiguous,
			Segments: f.Segments, Eligible: f.Eligible, UnknownFamilies: f.UnknownFamilies}),
		PriceHash: priceHash,
	}
}

// FromStore is the inverse of ToStore.
func FromStore(w store.WeightFit) (Fit, error) {
	f := Fit{Lag: w.Lag, Anchor: w.AnchorFamily, Loss: w.Loss, ComputedAt: w.ComputedAt}
	if err := unmarshalIf(w.Factors, &f.Factors); err != nil {
		return Fit{}, fmt.Errorf("weights: factors_json: %w", err)
	}
	if err := unmarshalIf(w.Scales, &f.Scales); err != nil {
		return Fit{}, fmt.Errorf("weights: scales_json: %w", err)
	}
	var bt storedBacktest
	if err := unmarshalIf(w.Backtest, &bt); err != nil {
		return Fit{}, fmt.Errorf("weights: backtest_json: %w", err)
	}
	f.Provider, f.Backtest, f.LagAmbiguous = bt.Provider, bt.MAE, bt.LagAmbiguous
	f.Segments, f.Eligible, f.UnknownFamilies = bt.Segments, bt.Eligible, bt.UnknownFamilies
	return f, nil
}

func unmarshalIf(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// mustJSON marshals values FitWeights guarantees finite; a failure is a bug.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("weights: marshal %T: %v", v, err))
	}
	return b
}
