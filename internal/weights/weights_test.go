package weights

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/pricing"
)

const (
	opus   = "claude-opus-5"
	sonnet = "claude-sonnet-5"
	tick   = 0.01
	week   = 7 * 24 * time.Hour
)

var (
	t0        = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	testTable = pricing.Table{
		{Provider: domain.ProviderClaude, Family: opus}:   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10, FastMult: 2},
		{Provider: domain.ProviderClaude, Family: sonnet}: {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite5m: 2.5, CacheWrite1h: 4},
	}
	claudeParams = DefaultParams(domain.ProviderClaude, testTable)
)

// genCfg describes a synthetic meter: hidden V per cycle, true multipliers
// of API price, and how the token mix varies.
type genCfg struct {
	seed      uint64
	perCycle  int
	V         []float64 // $ per 100 % for each cycle
	model     map[string]float64
	typ       map[TokenType]float64
	constant  bool      // every segment has the same mix
	cacheBase []float64 // per-cycle mean cache-read share of context
	outBase   []float64 // per-cycle mean output/context ratio
	noise     float64   // Gaussian SD of observed Δ, in ticks
}

func (c genCfg) mult(fam string, t TokenType) float64 {
	m := 1.0
	if v, ok := c.model[fam]; ok {
		m *= v
	}
	if v, ok := c.typ[t]; ok {
		m *= v
	}
	return m
}

// generate builds len(V)·perCycle segments. Each segment's true Δu is its
// factor-weighted API $ divided by V; the observed DeltaTicks adds noise.
func generate(cfg genCfg) ([]Segment, time.Time) {
	rng := rand.New(rand.NewPCG(cfg.seed, 7))
	u := func(lo, hi float64) float64 { return lo + (hi-lo)*rng.Float64() }
	var segs []Segment
	for c, V := range cfg.V {
		for k := 0; k < cfg.perCycle; k++ {
			pSonnet, cr, cw, out := 0.3, 0.7, 0.05, 0.03
			if cfg.cacheBase != nil {
				cr = cfg.cacheBase[c]
			}
			if cfg.outBase != nil {
				out = cfg.outBase[c]
			}
			if !cfg.constant {
				pSonnet = u(0.05, 0.65)
				cr = math.Max(0.05, math.Min(0.95, u(cr-0.35, cr+0.35)))
				cw = u(0.01, 0.04)
				out = u(out-0.004, out+0.004)
			}
			ratios := map[TokenType]float64{UncachedInput: 1 - cr - cw, CacheRead: cr, CacheWrite: cw, Output: out}
			shares := map[string]float64{opus: 1 - pSonnet, sonnet: pSonnet}
			// Weighted $ per context token, then size the segment to ~4–12 ticks.
			var w1 float64
			for _, fam := range []string{opus, sonnet} {
				for _, typ := range []TokenType{UncachedInput, CacheRead, CacheWrite, Output} {
					rate, _ := claudeParams.RefRate(fam, typ, false, false)
					w1 += shares[fam] * ratios[typ] * rate * cfg.mult(fam, typ)
				}
			}
			context := u(4, 12) * tick * V / w1
			seg := Segment{CycleKey: fmt.Sprintf("c%d", c), Tick: tick, Lag: "time",
				EndAt: t0.Add(time.Duration(c)*week + time.Duration(k+1)*week/time.Duration(cfg.perCycle+1))}
			var w float64
			for _, fam := range []string{opus, sonnet} {
				for _, typ := range []TokenType{UncachedInput, CacheRead, CacheWrite, Output} {
					tok := int64(math.Round(context * shares[fam] * ratios[typ]))
					rate, _ := claudeParams.RefRate(fam, typ, false, false)
					w += float64(tok) * rate * cfg.mult(fam, typ)
					seg.Features = append(seg.Features, Feature{Family: fam, Type: typ, Tokens: tok})
				}
			}
			seg.DeltaTicks = w/V/tick + cfg.noise*rng.NormFloat64()
			segs = append(segs, seg)
		}
	}
	return segs, t0.Add(time.Duration(len(cfg.V)) * week)
}

func baseCfg() genCfg {
	return genCfg{seed: 1, perCycle: 40, V: []float64{300, 300, 300},
		model: map[string]float64{sonnet: 1.3}, typ: map[TokenType]float64{CacheRead: 0.5},
		cacheBase: []float64{0.45, 0.6, 0.7}, outBase: []float64{0.008, 0.012, 0.016}, noise: 0.3}
}

func mustFit(t *testing.T, segs []Segment, now time.Time) Fit {
	t.Helper()
	fit, err := FitWeights(segs, claudeParams, now)
	if err != nil {
		t.Fatalf("FitWeights: %v", err)
	}
	return fit
}

func truthOf(cfg genCfg, name string) float64 {
	switch name {
	case "model:" + sonnet:
		return cfg.mult(sonnet, "")
	case "type:" + string(CacheRead), "type:" + string(CacheWrite), "type:" + string(Output):
		return cfg.mult("", TokenType(name[len("type:"):]))
	}
	return 1
}

func checkScales(t *testing.T, fit Fit, V []float64, tol float64) {
	t.Helper()
	if len(fit.Scales) != len(V) {
		t.Fatalf("got %d scales, want %d", len(fit.Scales), len(V))
	}
	for i, s := range fit.Scales {
		rel := s.ValuePer100/V[i] - 1
		t.Logf("cycle %s: V̂=%.2f (CI %.2f–%.2f) truth %.0f, err %+.2f%%", s.CycleKey, s.ValuePer100, s.ValueCILo, s.ValueCIHi, V[i], 100*rel)
		if math.Abs(rel) > tol {
			t.Errorf("cycle %s: V̂=%.2f, want %.0f ±%.0f%%", s.CycleKey, s.ValuePer100, V[i], 100*tol)
		}
	}
}

// checkFactors requires every identified factor's CI to cover its truth.
func checkFactors(t *testing.T, fit Fit, cfg genCfg) {
	t.Helper()
	for name, f := range fit.Factors {
		truth := truthOf(cfg, name)
		t.Logf("%-24s %-12s est %.3f CI %.3f–%.3f truth %.2f shareSD %.3f fisher %.1f", name, f.Status, f.Estimate, f.CILo, f.CIHi, truth, f.ShareSD, f.Fisher)
		if f.Status == StatusIdentified && (truth < f.CILo || truth > f.CIHi) {
			t.Errorf("%s: CI %.3f–%.3f misses truth %.2f", name, f.CILo, f.CIHi, truth)
		}
	}
}

func TestRecoversValueAndFactors(t *testing.T) {
	cfg := baseCfg()
	segs, now := generate(cfg)
	fit := mustFit(t, segs, now)
	if fit.Anchor != opus {
		t.Fatalf("anchor %q, want %q", fit.Anchor, opus)
	}
	checkScales(t, fit, cfg.V, 0.03)
	checkFactors(t, fit, cfg)
	for _, name := range []string{"model:" + sonnet, "type:cache_read"} {
		if fit.Factors[name].Status != StatusIdentified {
			t.Errorf("%s: status %s, want identified", name, fit.Factors[name].Status)
		}
	}
	vOpus, _ := fit.ModelValuePer100(opus)
	vSonnet, ok := fit.ModelValuePer100(sonnet)
	lo, hi, _ := fit.ModelValueCI(sonnet)
	t.Logf("V_opus=%.2f V_sonnet=%.2f (CI %.2f–%.2f, truth %.2f)", vOpus, vSonnet, lo, hi, 300/1.3)
	if !ok || math.Abs(vOpus/300-1) > 0.03 || lo > 300/1.3 || hi < 300/1.3 {
		t.Errorf("per-model values: opus %.2f, sonnet %.2f CI %.2f–%.2f", vOpus, vSonnet, lo, hi)
	}
}

func TestConstantMixStaysPriorLocked(t *testing.T) {
	for _, noise := range []float64{0, 0.3} {
		cfg := baseCfg()
		cfg.constant, cfg.model, cfg.typ, cfg.cacheBase, cfg.outBase, cfg.noise = true, nil, nil, nil, nil, noise
		segs, now := generate(cfg)
		fit := mustFit(t, segs, now)
		tol := 0.03
		if noise == 0 {
			tol = 0.005
		}
		checkScales(t, fit, cfg.V, tol)
		for name, f := range fit.Factors {
			if f.Status == StatusIdentified || f.Estimate != 1 {
				t.Errorf("noise %.1f: %s = %s %.4f, want locked at 1", noise, name, f.Status, f.Estimate)
			}
		}
	}
}

func TestTooFewSegments(t *testing.T) {
	segs, now := generate(baseCfg())
	fit, err := FitWeights(segs[:4], claudeParams, now)
	if err == nil {
		t.Fatal("want error for 4 segments")
	}
	if fit.Eligible != 4 {
		t.Errorf("eligible %d, want 4", fit.Eligible)
	}
}

func TestHuberRobustToOutliers(t *testing.T) {
	cfg := baseCfg()
	segs, now := generate(cfg)
	clean := mustFit(t, segs, now)
	dirty := append([]Segment(nil), segs...)
	for i := 0; i < len(dirty); i += 20 { // 5 %
		dirty[i].DeltaTicks *= 10
	}
	fit := mustFit(t, dirty, now)
	for i, s := range fit.Scales {
		rel := s.ValuePer100/clean.Scales[i].ValuePer100 - 1
		t.Logf("cycle %s: clean %.2f, with outliers %.2f (%+.2f%%)", s.CycleKey, clean.Scales[i].ValuePer100, s.ValuePer100, 100*rel)
		if math.Abs(rel) >= 0.05 {
			t.Errorf("cycle %s moved %.1f%%", s.CycleKey, 100*rel)
		}
	}
}

// shiftFeatures mimics a wrong lag: each crossing gets the previous
// crossing's tokens; the first crossing of each cycle has none and is dropped.
func shiftFeatures(segs []Segment) []Segment {
	var out []Segment
	for i := 1; i < len(segs); i++ {
		if segs[i].CycleKey != segs[i-1].CycleKey {
			continue
		}
		s := segs[i]
		s.Features, s.Lag = segs[i-1].Features, "1"
		out = append(out, s)
	}
	return out
}

func TestBacktestPicksConsistentLag(t *testing.T) {
	segs, now := generate(baseCfg())
	best, mae, ambiguous := Backtest(map[string][]Segment{"time": segs, "1": shiftFeatures(segs)}, claudeParams, now)
	t.Logf("best %q mae %v ambiguous %v", best, mae, ambiguous)
	if best != "time" || ambiguous || !(mae["1"] > mae["time"]) {
		t.Errorf("best %q mae %v ambiguous %v", best, mae, ambiguous)
	}
}

func TestValueShiftDoesNotLeakIntoFactors(t *testing.T) {
	cfg := baseCfg()
	cfg.V = []float64{300, 300, 390}
	segs, now := generate(cfg)
	fit := mustFit(t, segs, now)
	checkScales(t, fit, cfg.V, 0.03)
	checkFactors(t, fit, cfg)
	for _, name := range []string{"model:" + sonnet, "type:cache_read"} {
		f := fit.Factors[name]
		if truth := truthOf(cfg, name); truth < f.CILo || truth > f.CIHi {
			t.Errorf("%s: CI %.3f–%.3f misses %.2f", name, f.CILo, f.CIHi, truth)
		}
	}
}

func TestUnknownFamilyIneligible(t *testing.T) {
	segs, now := generate(baseCfg())
	for i := 0; i < 10; i++ {
		s := segs[i*10]
		s.Features = append([]Feature{{Family: "mystery-1", Type: Output, Tokens: 1000}}, s.Features...)
		segs = append(segs, s)
	}
	fit := mustFit(t, segs, now)
	if fit.Segments != 130 || fit.Eligible != 120 || !reflect.DeepEqual(fit.UnknownFamilies, []string{"mystery-1"}) {
		t.Errorf("segments %d eligible %d unknown %v", fit.Segments, fit.Eligible, fit.UnknownFamilies)
	}
	if _, ok := fit.Factors["model:mystery-1"]; ok {
		t.Error("unknown family got a factor")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	segs, now := generate(baseCfg())
	fit := mustFit(t, segs, now)
	fit.Backtest, fit.LagAmbiguous = map[string]float64{"time": 0.41, "1": 1.7}, true
	w := fit.ToStore(7, domain.MeterSevenDay, "abc123")
	if w.AccountID != 7 || w.MeterKey != "7d" || w.PriceHash != "abc123" || w.AnchorFamily != opus || w.Lag != "time" {
		t.Fatalf("row %+v", w)
	}
	var factors map[string]Factor
	if err := json.Unmarshal(w.Factors, &factors); err != nil || factors["model:"+sonnet].Status != StatusIdentified {
		t.Fatalf("factors_json %s: %v", w.Factors, err)
	}
	back, err := FromStore(w)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, fit) {
		t.Errorf("round trip differs:\n got %+v\nwant %+v", back, fit)
	}
}

func TestOnlineScaleTracksValue(t *testing.T) {
	cfg := baseCfg()
	cfg.model, cfg.typ = nil, nil
	segs, _ := generate(cfg)
	var s OnlineScale
	for _, seg := range segs[:40] {
		usd, _ := Fit{}.WeightedUSD(seg, claudeParams)
		s.Update(seg.DeltaTicks, seg.Tick, usd)
	}
	if rel := s.ValuePer100()/300 - 1; math.Abs(rel) > 0.03 {
		t.Errorf("online V %.2f, want 300 ±3%%", s.ValuePer100())
	}
	sd := s.SD()
	s.Carry(0.35)
	if want := math.Sqrt(sd*sd + 0.35*0.35); math.Abs(s.SD()-want) > 1e-12 {
		t.Errorf("carry SD %.4f, want %.4f", s.SD(), want)
	}
}

func TestDefaultParams(t *testing.T) {
	r, ok := claudeParams.RefRate(opus, Output, true, false)
	if !ok || math.Abs(r-50e-6) > 1e-15 {
		t.Errorf("opus fast output rate %g, want 5e-5", r)
	}
	if _, ok := claudeParams.RefRate("mystery-1", Output, false, false); ok {
		t.Error("unknown family priced")
	}
	codex := DefaultParams(domain.ProviderCodex, testTable)
	if !reflect.DeepEqual(codex.Types, []TokenType{CacheRead, Output}) ||
		!reflect.DeepEqual(claudeParams.Types, []TokenType{CacheRead, CacheWrite, Output}) ||
		claudeParams.LongPrior != (Prior{0, 0.3}) || claudeParams.FastPrior != (Prior{0, 0.5}) {
		t.Errorf("types codex %v claude %v", codex.Types, claudeParams.Types)
	}
}
