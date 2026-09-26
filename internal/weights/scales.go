package weights

import "math"

// OnlineScale tracks one cycle's log scale s_c between refits: a Gaussian
// state (LogScale, Precision) updated by a robust (Huber) Newton step per
// crossing and widened by the random walk when a new cycle starts. The zero
// value is uninitialized and seeds itself from its first observation with
// the first-cycle prior SD 2.5.
type OnlineScale struct {
	LogScale  float64 `json:"log_scale"`
	Precision float64 `json:"precision"`
}

// NewOnlineScale starts from a fitted cycle scale.
func NewOnlineScale(c CycleScale) OnlineScale {
	prec := 1e-3
	if c.SD > 0 {
		prec = 1 / (c.SD * c.SD)
	}
	return OnlineScale{LogScale: c.LogScale, Precision: prec}
}

// ValuePer100 is exp(−s): weighted-API-USD worth 100 % of the meter.
func (s OnlineScale) ValuePer100() float64 { return math.Exp(-s.LogScale) }

// SD is the posterior SD of the log scale; +Inf when uninitialized.
func (s OnlineScale) SD() float64 {
	if s.Precision <= 0 {
		return math.Inf(1)
	}
	return 1 / math.Sqrt(s.Precision)
}

// Predict is the expected Δu in ticks for a segment worth predictedUSD.
func (s OnlineScale) Predict(tick, predictedUSD float64) float64 {
	if tick <= 0 {
		return 0
	}
	return math.Exp(s.LogScale) * predictedUSD / tick
}

// Carry moves the state to the next cycle: variance += σ_rw².
func (s *OnlineScale) Carry(sigmaRW float64) {
	if sigmaRW <= 0 {
		sigmaRW = defaultRandomWalkSigma
	}
	if s.Precision > 0 {
		s.Precision = 1 / (1/s.Precision + sigmaRW*sigmaRW)
	}
}

// Update folds in one crossing: deltaTicks·tick observed, predictedUSD the
// segment's weighted USD (Fit.WeightedUSD).
func (s *OnlineScale) Update(deltaTicks, tick, predictedUSD float64) {
	s.UpdateWeighted(deltaTicks, tick, predictedUSD, 1)
}

// UpdateWeighted is Update with a fit weight (boundary weight × decay).
// Up to 12 clamped Newton steps on
// ½·P·(x−x₀)² + w·Huber(pct − 100·eˣ·usd)/σ², then P += w_robust·base².
func (s *OnlineScale) UpdateWeighted(deltaTicks, tick, predictedUSD, weight float64) {
	obs := 100 * deltaTicks * tick
	if predictedUSD <= 0 || tick <= 0 || math.IsNaN(obs) {
		return
	}
	if s.Precision <= 0 {
		if obs <= 0 {
			return
		}
		s.LogScale = math.Log(obs / (100 * predictedUSD))
		s.Precision = 1 / (firstScaleSD * firstScaleSD)
	}
	robust := func(x float64) (base, residual, w float64) {
		base = 100 * math.Exp(x) * predictedUSD
		residual = obs - base
		w = weight / (obsSD * obsSD)
		if a := math.Abs(residual); a > huberDelta {
			w *= huberDelta / a
		}
		return
	}
	prior, prec := s.LogScale, s.Precision
	x := prior
	for iter := 0; iter < 12; iter++ {
		base, residual, w := robust(x)
		step := (w*base*residual - prec*(x-prior)) / (prec + w*base*base)
		step = math.Max(-1, math.Min(1, step))
		x += step
		if math.Abs(step) < 1e-7 {
			break
		}
	}
	base, _, w := robust(x)
	s.LogScale = x
	s.Precision = prec + w*base*base
}

// scaleTracker holds one OnlineScale per (cycle, regime), carrying the
// latest state forward when a new one appears.
type scaleTracker struct {
	byKey map[string]*OnlineScale
	last  *OnlineScale
	sigma float64
}

func newScaleTracker(fit Fit, sigma float64) *scaleTracker {
	t := &scaleTracker{byKey: map[string]*OnlineScale{}, sigma: sigma}
	for _, c := range fit.Scales {
		s := NewOnlineScale(c)
		t.byKey[scaleKey(Segment{CycleKey: c.CycleKey, RegimeKey: c.RegimeKey})] = &s
		t.last = &s
	}
	return t
}

func (t *scaleTracker) forSegment(seg Segment) *OnlineScale {
	key := scaleKey(seg)
	if s := t.byKey[key]; s != nil {
		return s
	}
	s := &OnlineScale{}
	if t.last != nil {
		*s = *t.last
		s.Carry(t.sigma)
	}
	t.byKey[key], t.last = s, s
	return s
}
