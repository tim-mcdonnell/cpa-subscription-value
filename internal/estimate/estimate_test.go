package estimate

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
)

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// segs builds contiguous 10-minute segments at tick 0.01 from (Δ, $) pairs.
func segs(pairs ...[2]float64) []meter.Segment {
	var out []meter.Segment
	for i, p := range pairs {
		out = append(out, meter.Segment{
			Lag: meter.LagTime, Seq: i, DeltaTicks: int64(p[0]), Tick: 0.01, USD: p[1], USDCacheWrite1h: 1.5 * p[1],
			TStart: t0.Add(time.Duration(i) * 10 * time.Minute), TEnd: t0.Add(time.Duration(i+1) * 10 * time.Minute),
			USDByFamily: map[string]float64{"claude-opus-5": p[1]}, USDByType: meter.TypeUSD{Output: p[1]}, Flags: []string{},
		})
	}
	return out
}

func repeat(n int, p [2]float64) [][2]float64 {
	out := make([][2]float64, n)
	for i := range out {
		out[i] = p
	}
	return out
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestPrimaryQuantizationOnly(t *testing.T) {
	// One run of 60 one-tick segments at $10 each: V̂ = $600 / 0.60 = $1000,
	// SE_q/V̂ = tick·√(R/6)/D = 0.01·√(1/6)/0.6 = 0.680% (the plan's figure),
	// and identical blocks give SE_b = 0.
	r := Primary(segs(repeat(60, [2]float64{1, 10})...), DefaultOptions())
	if !near(r.VHat, 1000, 1e-9) || r.Runs != 1 || r.TicksUsed != 60 || r.Crossings != 60 {
		t.Fatalf("V̂ %v runs %d ticks %d crossings %d", r.VHat, r.Runs, r.TicksUsed, r.Crossings)
	}
	if !near(r.SEQuant/r.VHat, 0.01*math.Sqrt(1.0/6)/0.6, 1e-12) || r.SEBoot != 0 {
		t.Fatalf("SE_q/V̂ = %v, SE_b = %v", r.SEQuant/r.VHat, r.SEBoot)
	}
	z := 1.96 * r.SEQuant / r.VHat
	if !near(r.CILo, 1000*math.Exp(-z), 1e-9) || !near(r.CIHi, 1000*math.Exp(z), 1e-9) {
		t.Fatalf("CI [%v, %v]", r.CILo, r.CIHi)
	}
	if !near(r.VHatCW1h, 1500, 1e-9) || r.Mix.ByFamily["claude-opus-5"] != 600 || r.Mix.ByType["output"] != 600 {
		t.Fatalf("cw1h %v mix %+v", r.VHatCW1h, r.Mix)
	}
	if r.Blocks != 12 { // 5 ticks per block
		t.Fatalf("blocks = %d", r.Blocks)
	}
}

func TestPrimaryCoverageGuardAndExclusions(t *testing.T) {
	pairs := repeat(30, [2]float64{1, 10})
	pairs = append(pairs, [2]float64{5, 1}) // bypass burst: 5 ticks, $1
	pairs = append(pairs, repeat(30, [2]float64{1, 10})...)
	ss := segs(pairs...)
	ss[10].Flags = []string{meter.FlagQuotaDrop}
	r := Primary(ss, DefaultOptions())
	if !r.lowUSD[30] || len(r.lowUSD) != 1 {
		t.Fatalf("low_usd = %v", r.lowUSD)
	}
	if !near(r.VHat, 1000, 1e-9) || r.Runs != 3 || r.TicksUsed != 59 || r.TicksExcluded != 6 {
		t.Fatalf("V̂ %v runs %d used %d excluded %d", r.VHat, r.Runs, r.TicksUsed, r.TicksExcluded)
	}
	if !near(r.UnexplainedFrac, 5.0/65, 1e-12) {
		t.Fatalf("unexplained = %v", r.UnexplainedFrac)
	}
	if want := (600.0 + 1) / 0.65; !near(r.VHatAll, want, 1e-9) {
		t.Fatalf("V̂_all = %v, want %v", r.VHatAll, want)
	}
}

func TestPrimaryInsufficient(t *testing.T) {
	if r := Primary(nil, DefaultOptions()); r.Grade != GradeInsufficient || r.VHat != 0 {
		t.Fatalf("empty: %+v", r)
	}
	if r := Primary(segs([2]float64{1, 10}, [2]float64{1, 12}), DefaultOptions()); r.Grade != GradeInsufficient {
		t.Fatalf("2 crossings graded %s", r.Grade)
	}
}

func TestPrimaryRepriceScalesConsistently(t *testing.T) {
	base := segs(append(repeat(20, [2]float64{1, 9}), repeat(20, [2]float64{1, 11})...)...)
	o := DefaultOptions()
	o.SeriesID = "reprice"
	a := Primary(base, o)
	for i := range base {
		base[i].USD *= 1.1
	}
	b := Primary(base, o)
	if !near(b.VHat, 1.1*a.VHat, 1e-9) || !near(b.CILo, 1.1*a.CILo, 1e-9) || !near(b.CIHi, 1.1*a.CIHi, 1e-9) || a.Grade != b.Grade {
		t.Fatalf("reprice ×1.1: %v→%v, CI [%v,%v]→[%v,%v]", a.VHat, b.VHat, a.CILo, a.CIHi, b.CILo, b.CIHi)
	}
}

func TestBootstrapIsSeededBySeries(t *testing.T) {
	var pairs [][2]float64
	for i := range 80 {
		pairs = append(pairs, [2]float64{1, 8 + float64(i%7)})
	}
	o := DefaultOptions()
	o.SeriesID = "acct|7d|1|0|time"
	a, b := Primary(segs(pairs...), o), Primary(segs(pairs...), o)
	o.SeriesID = "acct|7d|2|0|time"
	c := Primary(segs(pairs...), o)
	if a.SEBoot != b.SEBoot || a.SEBoot == 0 || a.SEBoot == c.SEBoot {
		t.Fatalf("SE_b %v %v %v", a.SEBoot, b.SEBoot, c.SEBoot)
	}
}

func TestGradeThresholds(t *testing.T) {
	high := GradeInput{Ticks: 40, Crossings: 40, RelWidth: 0.12, Unexplained: 0.03, LagStable: true}
	with := func(f func(*GradeInput)) GradeInput { g := high; f(&g); return g }
	tests := []struct {
		name string
		in   GradeInput
		want string
	}{
		{"high", high, GradeHigh},
		{"<5 ticks", with(func(g *GradeInput) { g.Ticks = 4 }), GradeInsufficient},
		{"<3 crossings", with(func(g *GradeInput) { g.Crossings = 2 }), GradeInsufficient},
		{"5–14 ticks", with(func(g *GradeInput) { g.Ticks = 14 }), GradeLow},
		{"15 ticks", with(func(g *GradeInput) { g.Ticks = 15 }), GradeMedium},
		{"29 ticks", with(func(g *GradeInput) { g.Ticks = 29 }), GradeMedium},
		{"width 0.16", with(func(g *GradeInput) { g.RelWidth = 0.16 }), GradeMedium},
		{"width 0.31", with(func(g *GradeInput) { g.RelWidth = 0.31 }), GradeLow},
		{"unexplained 0.06", with(func(g *GradeInput) { g.Unexplained = 0.06 }), GradeMedium},
		{"unexplained 0.20 (between bands)", with(func(g *GradeInput) { g.Unexplained = 0.20 }), GradeLow},
		{"unexplained 0.31", with(func(g *GradeInput) { g.Unexplained = 0.31 }), GradeLow},
		{"regime change", with(func(g *GradeInput) { g.RegimeChange = true }), GradeLow},
		{"precision change", with(func(g *GradeInput) { g.PrecisionChange = true }), GradeMedium},
		{"lag unstable", with(func(g *GradeInput) { g.LagStable = false }), GradeMedium},
		{"poll-only cap", with(func(g *GradeInput) { g.CapMedium = true }), GradeMedium},
	}
	for _, tc := range tests {
		if got := Grade(tc.in); got != tc.want {
			t.Errorf("%s: Grade = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestForecastArithmetic(t *testing.T) {
	week := 7 * 24 * time.Hour
	h := time.Hour
	v := Result{VHat: 1000, CILo: 900, CIHi: 1100}
	type rd struct {
		at time.Duration
		u  float64
	}
	tests := []struct {
		name     string
		now      time.Duration
		rs       []rd
		status   string
		pTime    float64
		burnH    float64
		windowH  float64
		exhaustH float64 // hours after now; 0 = none
		uEnd     float64
	}{
		{"on track", 84 * h, []rd{{77 * h, 0.40}, {84*h - 10*time.Minute, 0.50}}, StatusOnTrack, 0.5, 0.1 / 6, 6, 30, 1},
		{"ahead", 48 * h, []rd{{41 * h, 0.30}, {48*h - time.Minute, 0.45}}, StatusAhead, 48.0 / 168, 0.15 / 6, 6, 22, 1},
		{"under, pace widens to 24h", 120 * h, []rd{{24 * h, 0.20}, {100 * h, 0.30}}, StatusUnder, 120.0 / 168, 0.1 / 24, 24, 168, 0.5},
		{"pace window clipped at cycle start", 2 * h, []rd{{h, 0.05}}, StatusOnTrack, 2.0 / 168, 0.025, 2, 38, 1},
		{"exhausted", 150 * h, []rd{{140 * h, 0.95}, {149 * h, 1.0}}, StatusExhausted, 150.0 / 168, 0.05 / 6, 6, 0, 1},
		{"idle", 100 * h, []rd{{10 * h, 0.30}}, StatusUnder, 100.0 / 168, 0, 24, 0, 0.3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := meter.CycleAssignment{StartsAt: t0, ResetAnchor: t0.Add(week), WindowSec: int64(week / time.Second), Tick: 0.01}
			for i, r := range tc.rs {
				c.Readings = append(c.Readings, meter.AssignedReading{MeterReading: domain.MeterReading{
					ID: int64(i + 1), Source: domain.SourceHeader, ObservedAt: t0.Add(r.at), UsedFraction: r.u}})
			}
			now := t0.Add(tc.now)
			f := ForecastCycle(c, v, 123, now)
			u := tc.rs[len(tc.rs)-1].u
			if f.Status != tc.status || !near(f.PTime, tc.pTime, 1e-9) || !near(f.BurnPerHour, tc.burnH, 1e-9) || !near(f.PaceWindowH, tc.windowH, 1e-9) {
				t.Fatalf("status %s p_time %v burn/h %v window %vh", f.Status, f.PTime, f.BurnPerHour, f.PaceWindowH)
			}
			if !near(f.UNow, u, 1e-12) || !near(f.RemainingUSD, (1-u)*1000, 1e-9) || !near(f.RemainingLo, (1-u)*900, 1e-9) ||
				!near(f.RemainingHi, (1-u)*1100, 1e-9) || !near(f.SpentUSD, u*1000, 1e-9) || f.ProxiedUSD != 123 {
				t.Fatalf("u %v remaining %v [%v,%v] spent %v", f.UNow, f.RemainingUSD, f.RemainingLo, f.RemainingHi, f.SpentUSD)
			}
			if !near(f.USDPerDay, tc.burnH*24*1000, 1e-6) || !near(f.UEnd, tc.uEnd, 1e-9) || !near(f.UnusedUSD, (1-tc.uEnd)*1000, 1e-6) {
				t.Fatalf("$/day %v u_end %v unused %v", f.USDPerDay, f.UEnd, f.UnusedUSD)
			}
			switch {
			case tc.exhaustH == 0 && f.TExhaust != nil && tc.burnH == 0:
				t.Fatalf("t_exhaust %v with no burn", f.TExhaust)
			case tc.exhaustH > 0 && (f.TExhaust == nil || absDur(f.TExhaust.Sub(now.Add(time.Duration(tc.exhaustH*float64(h))))) > time.Second):
				t.Fatalf("t_exhaust %v, want now+%vh", f.TExhaust, tc.exhaustH)
			}
		})
	}
}

func TestForecastRejectedStatusIsExhausted(t *testing.T) {
	c := meter.CycleAssignment{StartsAt: t0, ResetAnchor: t0.Add(168 * time.Hour), WindowSec: 604800, Tick: 0.01,
		Readings: []meter.AssignedReading{{MeterReading: domain.MeterReading{Source: domain.SourceHeader, ObservedAt: t0.Add(time.Hour), UsedFraction: 0.4, Status: "rejected"}}}}
	if f := ForecastCycle(c, Result{VHat: 100}, 0, t0.Add(2*time.Hour)); f.Status != StatusExhausted {
		t.Fatalf("status %s", f.Status)
	}
}

func TestCUSUM(t *testing.T) {
	noise := []float64{0.02, -0.01, 0.015, -0.02, 0.005, -0.015, 0.01, 0.0, -0.005, 0.02}
	var flat, step []float64
	for i := range 30 {
		flat = append(flat, noise[i%len(noise)])
		s := noise[(i*3)%len(noise)]
		if i >= 15 {
			s += math.Log(1.3)
		}
		step = append(step, s)
	}
	if _, ok := CUSUM(flat); ok {
		t.Fatal("flat series split")
	}
	cp, ok := CUSUM(step)
	if !ok || cp < 15 || cp > 17 {
		t.Fatalf("step at 15 detected at %d (ok=%v)", cp, ok)
	}
	if _, ok := CUSUM(step[:5]); ok {
		t.Fatal("too short a series split")
	}
}

func TestSubRegimesSplitBlocks(t *testing.T) {
	var bs []block
	for i := range 24 {
		ratio := 1000.0
		if i >= 12 {
			ratio = 1300
		}
		ratio *= 1 + 0.01*float64(i%3-1)
		bs = append(bs, block{usd: ratio * 0.05, d: 0.05, ticks: 5, fromSeq: 5 * i, toSeq: 5*i + 4})
	}
	sr := subRegimes(bs)
	if len(sr) != 2 || sr[1].FromSeq < 60 || sr[1].FromSeq > 70 || !near(sr[1].VHat, 1300, 20) || !near(sr[0].VHat, 1000, 20) {
		t.Fatalf("sub-regimes %+v", sr)
	}
}

func TestLevelShiftAndBlend(t *testing.T) {
	prev := Result{VHat: 100, SEQuant: 2}
	if !LevelShift(prev, Result{VHat: 110, SEQuant: 2}) || LevelShift(prev, Result{VHat: 105, SEQuant: 2}) {
		t.Fatal("level shift threshold 3·√(SE₁²+SE₂²) not applied")
	}
	cur := Result{VHat: 110, SEBoot: 11}
	v1, v0 := 0.01, 0.0004+0.35*0.35
	want := math.Exp((math.Log(110)/v1 + math.Log(100)/v0) / (1/v1 + 1/v0))
	if got := Blend(cur, prev, 1, 0.35); !near(got, want, 1e-9) || got <= 100 || got >= 110 {
		t.Fatalf("blend = %v, want %v", got, want)
	}
	if got := Blend(cur, Result{}, 1, 0.35); got != 110 {
		t.Fatalf("blend without a prior = %v", got)
	}
	if two, one := Blend(cur, prev, 2, 0.35), Blend(cur, prev, 1, 0.35); two <= one {
		t.Fatalf("an older prior should weigh less: %v vs %v", two, one)
	}
}

func TestChooseLagFallsBackAndPicksArgmin(t *testing.T) {
	good := segs(repeat(40, [2]float64{1, 10})...)
	var noisy []meter.Segment
	for i, s := range segs(repeat(40, [2]float64{1, 10})...) {
		s.USD += float64(i%3-1) * 3
		noisy = append(noisy, s)
	}
	h := []map[meter.Lag][]meter.Segment{{meter.LagIdx1: good, meter.LagTime: noisy, meter.LagIdx0: noisy, meter.LagIdx2: noisy}}
	c := ChooseLag(h, meter.LagTime, 15)
	if c.Lag != meter.LagIdx1 || !c.Stable || c.Fallback || c.Crossings < 15 {
		t.Fatalf("choice %+v", c)
	}
	short := []map[meter.Lag][]meter.Segment{{meter.LagIdx1: good[:10], meter.LagTime: noisy[:10]}}
	if c := ChooseLag(short, meter.LagIdx2, 15); c.Lag != meter.LagIdx2 || !c.Fallback {
		t.Fatalf("fallback to previous lag: %+v", c)
	}
	if c := ChooseLag(short, "", 15); c.Lag != meter.LagTime || !c.Fallback {
		t.Fatalf("fallback to time: %+v", c)
	}
}

func TestCoverageGapsNeedSilence(t *testing.T) {
	ix := meter.NewEventIndex([]domain.UsageEvent{{ID: 1, RequestedAt: t0.Add(10 * time.Minute), ObservedAt: t0.Add(10*time.Minute + time.Second), Latency: 30 * time.Second, APIUSD: 1}}, nil)
	poll := func(min int, u float64) meter.AssignedReading {
		return meter.AssignedReading{MeterReading: domain.MeterReading{Source: domain.SourcePoll, ObservedAt: t0.Add(time.Duration(min) * time.Minute), UsedFraction: u}}
	}
	// 0→20 min: activity explains the rise; 20→40: silence but within 5 min of
	// nothing, rise of 2 ticks is a gap; 40→60: no rise.
	ticks, gaps := CoverageGaps([]meter.AssignedReading{poll(0, 0.10), poll(20, 0.11), poll(40, 0.13), poll(60, 0.13)}, ix, 0.01)
	if ticks != 2 || len(gaps) != 1 || !gaps[0].From.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("ticks %d gaps %+v", ticks, gaps)
	}
	ss := segs(repeat(6, [2]float64{1, 10})...)
	markGaps(ss, gaps)
	var flagged []int
	for _, s := range ss {
		if s.Has(meter.FlagCoverageGap) {
			flagged = append(flagged, s.Seq)
		}
	}
	if !slices.Equal(flagged, []int{2, 3}) {
		t.Fatalf("flagged segments %v", flagged)
	}
}
