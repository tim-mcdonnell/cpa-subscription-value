package sim

import (
	"math"
	"slices"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// request is one proxied upstream attempt before it becomes a UsageRecord.
type request struct {
	model, family   string
	requested       time.Time
	ttft, latency   time.Duration
	un, cr, cw, out int64
	reasoning       int64
	failed          bool
	status          int
	headers         bool
	usd             [4]float64 // by type, see typeUSD
	mIn, mOut       float64    // meter-$: Σ usd_t × family mult × type mult
}

func (r request) observed() time.Time  { return r.requested.Add(r.ttft) }
func (r request) completed() time.Time { return r.requested.Add(r.latency) }
func (r request) apiUSD() float64      { return r.usd[0] + r.usd[1] + r.usd[2] + r.usd[3] }

var models = map[domain.Provider][]struct {
	model, family string
	share         float64
}{
	domain.ProviderClaude: {
		{"claude-opus-5-5", "claude-opus-5", 0.55},
		{"claude-sonnet-5", "claude-sonnet-5", 0.30},
		{"claude-fable-5-1", "claude-fable-5", 0.15},
	},
	domain.ProviderCodex: {
		{"gpt-5.6-sol", "gpt-5.6-sol", 0.8},
		{"gpt-5.6-sol-mini", "gpt-5.6-sol-mini", 0.2},
	},
}

var typeNames = [4]string{TypeUncachedInput, TypeCacheRead, TypeCacheWrite, TypeOutput}

func (g *gen) lognormal(median, sigma float64) float64 {
	return median * math.Exp(sigma*g.rng.NormFloat64())
}

func (g *gen) exp(mean time.Duration) time.Duration {
	return time.Duration(g.rng.ExpFloat64() * float64(mean))
}

// traffic generates sessions day by day until each day's share of the
// cycle's target movement is spent. Weekdays weigh 1.2, weekends 0.5 (sum 7);
// activity runs 07:00–24:00 of the corpus clock; mix drifts daily.
func (g *gen) traffic() {
	sc := g.sc
	end := g.end()
	carry := 0.0
	for day := sc.Start.Truncate(24 * time.Hour); day.Before(end); day = day.Add(24 * time.Hour) {
		p := g.periodAt(day.Add(12 * time.Hour))
		if p == nil {
			p = &g.periods[len(g.periods)-1]
		}
		w := 1.2
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			w = 0.5
		}
		target := (1-sc.Beta)*p.peakTarget*w/7 + carry
		shares := g.dayShares()
		t := day.Add(7*time.Hour + g.exp(20*time.Minute))
		dayEnd := day.Add(24 * time.Hour)
		spent := 0.0
		for t.Before(dayEnd) && spent < target {
			var sEnd time.Time
			var mv float64
			sEnd, mv = g.session(t, shares, target-spent)
			spent += mv
			t = sEnd.Add(g.exp(35 * time.Minute))
		}
		carry = min(max(target-spent, 0), 0.1)
	}
	g.reqs = slices.DeleteFunc(g.reqs, func(r request) bool {
		return !r.requested.After(sc.Start) || !r.completed().Before(end)
	})
	slices.SortStableFunc(g.reqs, func(a, b request) int { return a.requested.Compare(b.requested) })
}

func (g *gen) dayShares() []float64 {
	ms := models[g.sc.Provider]
	out := make([]float64, len(ms))
	sum := 0.0
	for i, m := range ms {
		out[i] = m.share * math.Exp(0.35*g.rng.NormFloat64())
		sum += out[i]
	}
	for i := range out {
		out[i] /= sum
	}
	return out
}

func (g *gen) pickModel(shares []float64) int {
	x := g.rng.Float64()
	for i, s := range shares {
		if x < s {
			return i
		}
		x -= s
	}
	return len(shares) - 1
}

// session runs 1–4 concurrent streams of sequential requests, advanced in
// lockstep so a session that exhausts the day's budget stops cleanly. It
// returns when the last request completes and the weekly movement caused.
func (g *gen) session(start time.Time, shares []float64, budget float64) (time.Time, float64) {
	length := min(max(g.exp(50*time.Minute), 5*time.Minute), 3*time.Hour)
	n := 1
	switch x := g.rng.Float64(); {
	case x < 0.08:
		n = 4
	case x < 0.20:
		n = 3
	case x < 0.45:
		n = 2
	}
	type stream struct {
		model, family string
		next          time.Time
		ctx           int64
		fresh         bool
	}
	streams := make([]stream, n)
	for i := range streams {
		m := models[g.sc.Provider][g.pickModel(shares)]
		streams[i] = stream{model: m.model, family: m.family, ctx: int64(15000 + g.rng.IntN(25000)), fresh: true,
			next: start.Add(time.Duration(i) * time.Duration(g.rng.Float64()*float64(2*time.Minute)))}
	}
	stop := start.Add(length)
	last, mv := start, 0.0
	for mv < budget {
		s := &streams[0]
		for i := range streams {
			if streams[i].next.Before(s.next) {
				s = &streams[i]
			}
		}
		if !s.next.Before(stop) {
			break
		}
		r := g.request(s.model, s.family, s.next, &s.ctx, &s.fresh)
		g.reqs = append(g.reqs, r)
		if p := g.periodAt(r.requested); p != nil {
			mv += (r.mIn + r.mOut) / p.vbase
		}
		if r.completed().After(last) {
			last = r.completed()
		}
		s.next = r.completed().Add(g.exp(6 * time.Second))
	}
	return last, mv
}

// request draws one attempt's tokens and timing. Claude contexts grow as a
// cached prefix plus a fresh cache write per turn, compacting past 160k.
// Codex has no cache writes; its uncached input is the new turn.
func (g *gen) request(model, family string, t time.Time, ctx *int64, fresh *bool) request {
	r := request{model: model, family: family, requested: t, headers: g.rng.Float64() < g.sc.HeaderCoverage}
	r.out = min(int64(g.lognormal(600, 1.0)), 32000)
	claude := g.sc.Provider == domain.ProviderClaude
	switch {
	case claude && (*fresh || *ctx > 160000):
		if *ctx > 160000 {
			*ctx = int64(20000 + g.rng.IntN(20000))
		}
		r.cw = *ctx
	case claude:
		r.cr = *ctx
		r.cw = min(int64(g.lognormal(2500, 0.9)), 40000)
	default:
		if *ctx > 160000 {
			*ctx = int64(20000 + g.rng.IntN(20000))
		}
		r.cr = *ctx
	}
	*fresh = false
	if claude {
		r.un = min(int64(g.lognormal(150, 1.0)), 20000)
	} else {
		r.un = min(int64(g.lognormal(2500, 0.9)), 40000)
	}
	r.reasoning = int64(float64(r.out) * g.rng.Float64() * 0.5)
	r.ttft = time.Duration(g.lognormal(1.5, 0.5) * float64(time.Second))
	speed := 40 + 50*g.rng.Float64()
	r.latency = r.ttft + time.Duration(float64(r.out)/speed*float64(time.Second))

	if g.rng.Float64() < g.sc.FailureRate {
		r.failed = true
		r.status = []int{500, 529, 429}[g.rng.IntN(3)]
		if g.rng.Float64() < 0.5 {
			// Partial: the prompt was billed, generation cut short.
			frac := 0.8 * g.rng.Float64()
			r.out = int64(float64(r.out) * frac)
			r.reasoning = min(r.reasoning, r.out)
			r.latency = r.ttft + time.Duration(float64(r.latency-r.ttft)*frac)
		} else {
			r.un, r.cr, r.cw, r.out, r.reasoning = 0, 0, 0, 0, 0
			r.latency = time.Duration((0.3 + 2.7*g.rng.Float64()) * float64(time.Second))
			r.ttft = r.latency
		}
	} else {
		*ctx += r.cw + r.un + r.out
	}
	r.usd = typeUSD(family, r.un, r.cr, r.cw, r.out)
	fm := g.sc.FamilyMult[family]
	if fm == 0 {
		fm = 1
	}
	for i, u := range r.usd {
		m := u * fm * g.sc.TypeMult[typeNames[i]]
		if i == 3 {
			r.mOut += m
		} else {
			r.mIn += m
		}
	}
	return r
}

// registration is meter-$ landing on the hidden meter at t.
type registration struct {
	t       time.Time
	m       float64
	fable   bool
	proxied bool
	usd     float64 // API $ carried (proxied only)
}

// unrouted adds bypass usage per weekly period so that it is Beta of the
// period's total movement: bursts in ≥1 h silences (gaps mode) or spread
// over random proxied requests (diffuse mode).
func (g *gen) unrouted() {
	sc := g.sc
	if sc.Beta <= 0 {
		return
	}
	for pi := range g.periods {
		p := &g.periods[pi]
		var inP []request
		mv := 0.0
		for _, r := range g.reqs {
			if !r.requested.Before(p.start) && r.requested.Before(p.end) {
				inP = append(inP, r)
				mv += (r.mIn + r.mOut) / p.vbase
			}
		}
		if len(inP) == 0 {
			continue
		}
		total := sc.Beta / (1 - sc.Beta) * mv // movement to add
		switch sc.BetaMode {
		case BetaDiffuse:
			n := max(50, len(inP)/3)
			for range n {
				r := inP[g.rng.IntN(len(inP))]
				at := r.requested.Add(time.Duration(g.rng.Float64() * float64(r.latency)))
				g.bypass = append(g.bypass, registration{t: at, m: total / float64(n) * p.vbase})
			}
		default:
			// Only silences between proxied requests: a burst before the
			// period's first crossing or after its last is unobservable.
			gaps := silences(inP, time.Hour)
			k := max(1, int(math.Round(float64(sc.BurstsPerWeek)*p.end.Sub(p.start).Hours()/week.Hours())))
			k = min(k, len(gaps))
			g.rng.Shuffle(len(gaps), func(i, j int) { gaps[i], gaps[j] = gaps[j], gaps[i] })
			weights := make([]float64, k)
			sum := 0.0
			for i := range weights {
				weights[i] = 0.7 + 0.6*g.rng.Float64()
				sum += weights[i]
			}
			for i := 0; i < k; i++ {
				from := gaps[i][0].Add(20 * time.Minute)
				span := gaps[i][1].Add(-20 * time.Minute).Sub(from)
				const n = 12
				for j := range n {
					at := from.Add(span * time.Duration(j) / n)
					g.bypass = append(g.bypass, registration{t: at, m: total * weights[i] / sum / n * p.vbase})
				}
			}
		}
	}
}

// silences returns the [from, to) spans of at least minLen between
// consecutive requests (sorted by start) with none in flight.
func silences(rs []request, minLen time.Duration) [][2]time.Time {
	var out [][2]time.Time
	if len(rs) == 0 {
		return nil
	}
	busyUntil := rs[0].completed()
	for _, r := range rs[1:] {
		if r.requested.Sub(busyUntil) >= minLen {
			out = append(out, [2]time.Time{busyUntil, r.requested})
		}
		if r.completed().After(busyUntil) {
			busyUntil = r.completed()
		}
	}
	return out
}
