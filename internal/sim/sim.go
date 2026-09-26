// Package sim generates deterministic synthetic corpora for the estimator
// tests (docs/plan.md §Estimator "Synthetic corpus"): a hidden meter with
// known $-per-100%, realistic agent traffic, and the provider headers CPA
// would hand the plugin. Records go through ingest.Normalize, so events and
// readings have exactly the production shapes.
package sim

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// Lag modes of the hidden meter.
const (
	// LagTime: input $ registers at request start, output $ at completion; a
	// header reflects registrations up to the instant it was sent (TTFT).
	LagTime = "time"
	// LagIdx1: a header reflects every response observed before it, in full.
	LagIdx1 = "idx1"
)

// Unrouted (bypass) usage placement.
const (
	BetaGaps    = "gaps"    // bursts inside ≥1 h silences of proxied traffic
	BetaDiffuse = "diffuse" // interleaved with proxied traffic
)

// Scenario parameterizes one corpus. Zero values take DefaultScenario's.
type Scenario struct {
	Seed     uint64
	Provider domain.Provider
	Start    time.Time
	Cycles   int // weekly cycles

	// V7d is the hidden $ per 100% of the weekly meter at multiplier 1;
	// 0 draws log-uniform in [V7dLo, V7dHi]. V5h = V7d × V5hRatio.
	V7d, V7dLo, V7dHi float64
	V5hRatio          float64

	// FamilyMult and TypeMult are the hidden multipliers of API price.
	FamilyMult map[string]float64
	TypeMult   map[string]float64

	LagMode string

	Beta          float64 // share of meter movement from unrouted usage
	BetaMode      string
	BurstsPerWeek int // gaps mode

	// EarlyResetCycle ≥ 0 resets that cycle's meter 2.5–4 days in; with
	// EarlyResetNewAnchor the provider also moves the anchor.
	EarlyResetCycle     int
	EarlyResetNewAnchor bool

	// ShiftCycle ≥ 0 multiplies the hidden V by ShiftFactor from that cycle on.
	ShiftCycle  int
	ShiftFactor float64

	// PrecisionChange: Claude headers carry 4 dp for the first half of
	// cycle 0, then 2 dp.
	PrecisionChange bool

	PollInterval   time.Duration // ≤ 0 disables polls
	HeaderCoverage float64       // share of responses that carry meter headers
	StaleProb      float64       // share of headers served from a stale replica
	FailureRate    float64
	PeakLo, PeakHi float64 // weekly peak targets
}

// DefaultScenario is the plan's corpus: 4 weekly cycles peaking 60–90%, one
// early reset, one 30% V shift, one precision change (Claude), polls every
// 20 min, 3% failures, no bypass.
func DefaultScenario(seed uint64, p domain.Provider) Scenario {
	sc := Scenario{
		Seed:     seed,
		Provider: p,
		Start:    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seed%24) * time.Hour),
		Cycles:   4,
		V7dLo:    200, V7dHi: 600,
		V5hRatio:        0.15,
		FamilyMult:      map[string]float64{"claude-fable-5": 1.2, "gpt-5.6-sol-mini": 0.8},
		TypeMult:        map[string]float64{TypeUncachedInput: 1, TypeCacheRead: 0.5, TypeCacheWrite: 1, TypeOutput: 1},
		LagMode:         LagTime,
		BetaMode:        BetaGaps,
		BurstsPerWeek:   3,
		EarlyResetCycle: 1,
		ShiftCycle:      2,
		ShiftFactor:     1.3,
		PrecisionChange: p == domain.ProviderClaude,
		PollInterval:    20 * time.Minute,
		HeaderCoverage:  1,
		StaleProb:       0.03,
		FailureRate:     0.03,
		PeakLo:          0.6, PeakHi: 0.9,
	}
	if p == domain.ProviderCodex {
		// Codex requests are ~4× cheaper; a smaller allowance keeps the
		// events-per-tick ratio (and the corpus size) comparable to Claude's.
		sc.V7dLo, sc.V7dHi = 60, 180
	}
	return sc
}

// PeriodTruth is one meter period of the weekly meter: a cycle, or one
// regime of a cycle split by an early reset.
type PeriodTruth struct {
	Cycle, Regime int
	Anchor        time.Time
	Start, End    time.Time
	// VBase is the hidden $ per 100% at multiplier 1; VEff is what the
	// estimator should recover: API $ of proxied usage per 100% of the
	// movement it caused, for this period's actual mix.
	VBase, VEff  float64
	Peak         float64
	ProxiedUSD   float64
	UnroutedFrac float64 // share of this period's movement from bypass
}

// Truth is everything hidden from the estimator.
type Truth struct {
	LagMode           string
	V7d, V5h          float64
	Periods           []PeriodTruth
	EarlyResetAt      time.Time
	ShiftAt           time.Time
	PrecisionChangeAt time.Time
}

// PeriodAt returns the weekly period containing t, if any.
func (t Truth) PeriodAt(at time.Time) (PeriodTruth, bool) {
	for _, p := range t.Periods {
		if !at.Before(p.Start) && at.Before(p.End) {
			return p, true
		}
	}
	return PeriodTruth{}, false
}

// Corpus is one generated dataset for a single account.
type Corpus struct {
	Scenario Scenario
	Records  []domain.UsageRecord
	Events   []domain.UsageEvent
	// Readings holds header readings (all meters) and polls, ordered by id.
	Readings []domain.MeterReading
	End      time.Time
	Truth    Truth

	m *meterModel
}

// ReadingsFor returns one meter's readings.
func (c Corpus) ReadingsFor(meterKey string) []domain.MeterReading {
	var out []domain.MeterReading
	for _, r := range c.Readings {
		if r.MeterKey == meterKey {
			out = append(out, r)
		}
	}
	return out
}

// TrueU7 is the hidden weekly utilization at t.
func (c Corpus) TrueU7(t time.Time) float64 { return c.m.u7(t) }

// Generate builds the corpus for sc. The same Scenario always yields the
// same corpus.
func Generate(sc Scenario) Corpus {
	sc = withDefaults(sc)
	rng := rand.New(rand.NewPCG(sc.Seed, sc.Seed^0x9e3779b97f4a7c15))
	v7 := sc.V7d
	if v7 == 0 {
		v7 = math.Exp(math.Log(sc.V7dLo) + rng.Float64()*(math.Log(sc.V7dHi)-math.Log(sc.V7dLo)))
	}
	g := &gen{sc: sc, rng: rng, v7: v7, v5: v7 * sc.V5hRatio}
	g.buildPeriods()
	g.traffic()
	g.unrouted()
	m := g.buildMeter()
	c := g.emit(m)
	c.Truth = g.truth(m)
	c.m = m
	return c
}

// withDefaults fills unset maps, strings and ranges from DefaultScenario.
// Integer switches (EarlyResetCycle, ShiftCycle: −1 disables) are taken as
// given, so build scenarios from DefaultScenario and edit them.
func withDefaults(sc Scenario) Scenario {
	if sc.Provider == "" {
		sc.Provider = domain.ProviderClaude
	}
	d := DefaultScenario(sc.Seed, sc.Provider)
	if sc.Start.IsZero() {
		sc.Start = d.Start
	}
	if sc.Cycles == 0 {
		sc.Cycles = d.Cycles
	}
	if sc.V7dLo == 0 {
		sc.V7dLo, sc.V7dHi = d.V7dLo, d.V7dHi
	}
	if sc.V5hRatio == 0 {
		sc.V5hRatio = d.V5hRatio
	}
	if sc.FamilyMult == nil {
		sc.FamilyMult = d.FamilyMult
	}
	if sc.TypeMult == nil {
		sc.TypeMult = d.TypeMult
	}
	if sc.LagMode == "" {
		sc.LagMode = d.LagMode
	}
	if sc.BetaMode == "" {
		sc.BetaMode = d.BetaMode
	}
	if sc.BurstsPerWeek == 0 {
		sc.BurstsPerWeek = d.BurstsPerWeek
	}
	if sc.ShiftFactor == 0 {
		sc.ShiftFactor = d.ShiftFactor
	}
	if sc.PeakHi == 0 {
		sc.PeakLo, sc.PeakHi = d.PeakLo, d.PeakHi
	}
	return sc
}

// period is one span of the weekly meter between zero points.
type period struct {
	start, end, anchor time.Time
	cycle, regime      int
	vbase              float64
	peakTarget         float64
}

type gen struct {
	sc     Scenario
	rng    *rand.Rand
	v7, v5 float64

	periods []period
	reqs    []request
	bypass  []registration
	erAt    time.Time
	pcAt    time.Time
	shiftAt time.Time
}

const week = 7 * 24 * time.Hour

func (g *gen) buildPeriods() {
	sc := g.sc
	start := sc.Start
	for c := 0; c < sc.Cycles; c++ {
		vb := g.v7
		if sc.ShiftCycle >= 0 && c >= sc.ShiftCycle {
			vb *= sc.ShiftFactor
			if c == sc.ShiftCycle {
				g.shiftAt = start
			}
		}
		peak := sc.PeakLo + g.rng.Float64()*(sc.PeakHi-sc.PeakLo)
		end := start.Add(week)
		if c != sc.EarlyResetCycle {
			g.periods = append(g.periods, period{start: start, end: end, anchor: end, cycle: c, vbase: vb, peakTarget: peak})
			start = end
			continue
		}
		er := start.Add(time.Duration((2.5 + 1.5*g.rng.Float64()) * float64(24*time.Hour))).Truncate(time.Second)
		g.erAt = er
		g.periods = append(g.periods, period{start: start, end: er, anchor: end, cycle: c, vbase: vb, peakTarget: peak})
		if sc.EarlyResetNewAnchor {
			end = er.Add(week)
		}
		g.periods = append(g.periods, period{start: er, end: end, anchor: end, cycle: c, regime: 1, vbase: vb, peakTarget: peak})
		start = end
	}
	if sc.PrecisionChange {
		g.pcAt = sc.Start.Add(time.Duration((3 + g.rng.Float64()) * float64(24*time.Hour)))
	}
}

func (g *gen) periodAt(t time.Time) *period {
	for i := range g.periods {
		if !t.Before(g.periods[i].start) && t.Before(g.periods[i].end) {
			return &g.periods[i]
		}
	}
	return nil
}

func (g *gen) end() time.Time { return g.periods[len(g.periods)-1].end }
