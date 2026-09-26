package sim

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/ingest"
)

// meterModel is the provider's hidden state: every registration of
// meter-$ in time order, with prefix sums per meter.
type meterModel struct {
	times       []time.Time
	regs        []registration
	p7, p5, pOI []float64 // prefix sums of movement; len(regs)+1
	periods     []period
	windows     [][2]time.Time // 5h windows [start, end)
	v5          float64
	oiRatio     float64
}

func (g *gen) buildMeter() *meterModel {
	var regs []registration
	for _, r := range g.reqs {
		fable := strings.Contains(r.family, "fable")
		switch g.sc.LagMode {
		case LagIdx1:
			// Registered just after its own header went out: the next
			// observed response is the first to include it.
			regs = append(regs, registration{t: r.observed().Add(time.Nanosecond), m: r.mIn + r.mOut, fable: fable, proxied: true, usd: r.apiUSD()})
		default:
			regs = append(regs,
				registration{t: r.requested, m: r.mIn, fable: fable, proxied: true, usd: r.usd[0] + r.usd[1] + r.usd[2]},
				registration{t: r.completed(), m: r.mOut, fable: fable, proxied: true, usd: r.usd[3]})
		}
	}
	regs = append(regs, g.bypass...)
	slices.SortStableFunc(regs, func(a, b registration) int { return a.t.Compare(b.t) })
	m := &meterModel{regs: regs, periods: g.periods, v5: g.v5, oiRatio: 0.3}
	m.times = make([]time.Time, len(regs))
	m.p7 = make([]float64, len(regs)+1)
	m.p5 = make([]float64, len(regs)+1)
	m.pOI = make([]float64, len(regs)+1)
	for i, r := range regs {
		m.times[i] = r.t
		var mv7, mvOI float64
		if p := g.periodAt(r.t); p != nil {
			mv7 = r.m / p.vbase
			if r.fable {
				mvOI = r.m / (p.vbase * m.oiRatio)
			}
		}
		m.p7[i+1] = m.p7[i] + mv7
		m.p5[i+1] = m.p5[i] + r.m/g.v5
		m.pOI[i+1] = m.pOI[i] + mvOI
	}
	// 5h windows open at the hour of the first activity after the last one closed.
	acts := slices.Clone(m.times)
	for _, r := range g.reqs {
		acts = append(acts, r.observed())
	}
	slices.SortFunc(acts, func(a, b time.Time) int { return a.Compare(b) })
	var cur [2]time.Time
	for _, t := range acts {
		if !t.Before(cur[1]) {
			start := t.Truncate(time.Hour)
			cur = [2]time.Time{start, start.Add(5 * time.Hour)}
			m.windows = append(m.windows, cur)
		}
	}
	return m
}

// sum is Σ movement of registrations with from ≤ t ≤ to.
func (m *meterModel) sum(prefix []float64, from, to time.Time) float64 {
	lo := sort.Search(len(m.times), func(i int) bool { return !m.times[i].Before(from) })
	hi := sort.Search(len(m.times), func(i int) bool { return m.times[i].After(to) })
	if hi <= lo {
		return 0
	}
	return prefix[hi] - prefix[lo]
}

func (m *meterModel) period(t time.Time) *period {
	for i := range m.periods {
		if !t.Before(m.periods[i].start) && t.Before(m.periods[i].end) {
			return &m.periods[i]
		}
	}
	return nil
}

func (m *meterModel) u7(t time.Time) float64 {
	p := m.period(t)
	if p == nil {
		return 0
	}
	return m.sum(m.p7, p.start, t)
}

func (m *meterModel) uOI(t time.Time) float64 {
	p := m.period(t)
	if p == nil {
		return 0
	}
	return m.sum(m.pOI, p.start, t)
}

func (m *meterModel) u5(t time.Time) (float64, time.Time, bool) {
	i := sort.Search(len(m.windows), func(i int) bool { return m.windows[i][0].After(t) }) - 1
	if i < 0 || !t.Before(m.windows[i][1]) {
		return 0, time.Time{}, false
	}
	return m.sum(m.p5, m.windows[i][0], t), m.windows[i][1], true
}

// claudeRaw floors u to dp decimals and spells it the way upstream does:
// shortest float repr with trailing zeros trimmed, "0.0" for zero.
func claudeRaw(u float64, dp int) string {
	s := math.Pow10(dp)
	s2 := strconv.FormatFloat(math.Floor(u*s+1e-9)/s, 'f', -1, 64)
	if !strings.Contains(s2, ".") {
		s2 += ".0"
	}
	return s2
}

func codexPct(u float64) int { return int(math.Floor(u*100 + 1e-9)) }

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func (g *gen) headers(m *meterModel, r request, te time.Time) http.Header {
	h := http.Header{}
	p := m.period(te)
	if p == nil {
		return h
	}
	u7 := m.u7(te)
	u5, a5, ok5 := m.u5(te)
	if g.sc.Provider == domain.ProviderCodex {
		h.Set("X-Codex-Plan-Type", "pro")
		if ok5 {
			h.Set("X-Codex-Primary-Used-Percent", strconv.Itoa(codexPct(u5)))
			h.Set("X-Codex-Primary-Window-Minutes", "300")
			h.Set("X-Codex-Primary-Reset-At", unix(a5))
		}
		h.Set("X-Codex-Secondary-Used-Percent", strconv.Itoa(codexPct(u7)))
		h.Set("X-Codex-Secondary-Window-Minutes", "10080")
		h.Set("X-Codex-Secondary-Reset-At", unix(p.anchor))
		return h
	}
	dp := 2
	if !g.pcAt.IsZero() && r.observed().Before(g.pcAt) {
		dp = 4
	}
	const pre = "Anthropic-Ratelimit-Unified-"
	set := func(claim string, u float64, anchor time.Time) {
		h.Set(pre+claim+"-Utilization", claudeRaw(u, dp))
		h.Set(pre+claim+"-Reset", unix(anchor))
		status := "allowed"
		if u >= 0.8 {
			status = "allowed_warning"
		}
		h.Set(pre+claim+"-Status", status)
	}
	h.Set(pre+"Status", "allowed")
	h.Set(pre+"Representative-Claim", "seven_day")
	if ok5 {
		set("5h", u5, a5)
	}
	set("7d", u7, p.anchor)
	if strings.Contains(r.family, "fable") {
		set("7d_oi", m.uOI(te), p.anchor)
	}
	return h
}

func (g *gen) record(i int, r request, h http.Header) domain.UsageRecord {
	prov := string(g.sc.Provider)
	rec := domain.UsageRecord{
		RequestID: fmt.Sprintf("sim-%d-%06d", g.sc.Seed, i), Provider: prov, ExecutorType: prov,
		Model: r.model, AuthID: "sim-" + prov + ".json", AuthIndex: "51a1c0de", AuthType: "oauth",
		Source: "sim", Generate: true, Stream: true,
		RequestedAt: r.requested, Latency: r.latency, TTFT: r.ttft,
		Failed: r.failed, Failure: domain.UsageFailure{StatusCode: r.status},
		ResponseHeaders: h,
	}
	d := &rec.Detail
	d.OutputTokens, d.ReasoningTokens = r.out, r.reasoning
	if g.sc.Provider == domain.ProviderCodex {
		d.InputTokens = r.un + r.cr
		d.CachedTokens, d.CacheReadTokens = r.cr, r.cr
		d.TotalTokens = d.InputTokens + r.out
	} else {
		d.InputTokens, d.CacheReadTokens, d.CacheCreationTokens = r.un, r.cr, r.cw
		d.CachedTokens = r.cr
		if r.cr == 0 {
			d.CachedTokens = r.cw
		}
		d.TotalTokens = r.un + r.cr + r.cw + r.out
	}
	return rec
}

// emit turns requests into records, normalizes them through ingest, adds
// polls, and numbers everything in ingest order (events at completion,
// polls when taken), as the store would.
func (g *gen) emit(m *meterModel) Corpus {
	sc := g.sc
	type item struct {
		at   time.Time
		req  int // -1 for polls
		poll []domain.MeterReading
	}
	var items []item
	var recs []domain.UsageRecord
	var results []ingest.Result
	for i, r := range g.reqs {
		var h http.Header
		if r.headers {
			te := r.observed()
			if g.rng.Float64() < sc.StaleProb {
				te = te.Add(-time.Duration((10 + 80*g.rng.Float64()) * float64(time.Second)))
			}
			h = g.headers(m, r, te)
		}
		rec := g.record(i, r, h)
		res, _ := ingest.Normalize(rec, Pricer{})
		recs = append(recs, rec)
		results = append(results, res)
		items = append(items, item{at: r.completed(), req: i})
	}
	if sc.PollInterval > 0 {
		end := g.end()
		for t := sc.Start.Add(time.Duration(g.rng.Float64() * float64(sc.PollInterval))); t.Before(end); t = t.Add(sc.PollInterval) {
			items = append(items, item{at: t, req: -1, poll: g.polls(m, t)})
		}
	}
	slices.SortStableFunc(items, func(a, b item) int { return a.at.Compare(b.at) })
	c := Corpus{Scenario: sc, Records: recs, End: g.end()}
	var evID, rdID int64
	for _, it := range items {
		rs := it.poll
		if it.req >= 0 {
			evID++
			ev := results[it.req].Event
			ev.ID, ev.AccountID = evID, 1
			c.Events = append(c.Events, ev)
			rs = results[it.req].Readings
			for j := range rs {
				id := evID
				rs[j].EventID = &id
			}
		}
		for _, r := range rs {
			rdID++
			r.ID, r.AccountID = rdID, 1
			c.Readings = append(c.Readings, r)
		}
	}
	return c
}

func (g *gen) polls(m *meterModel, t time.Time) []domain.MeterReading {
	mk := func(key string, u float64, anchor time.Time, win int64) domain.MeterReading {
		pct := codexPct(u)
		return domain.MeterReading{
			MeterKey: key, Source: domain.SourcePoll, ObservedAt: t, UsedFraction: float64(pct) / 100,
			Raw: strconv.Itoa(pct), PrecisionDP: 2, ResetAt: anchor, WindowSec: win,
		}
	}
	var out []domain.MeterReading
	if u5, a5, ok := m.u5(t); ok {
		out = append(out, mk(domain.MeterFiveHour, u5, a5, 18000))
	}
	if p := m.period(t); p != nil {
		out = append(out, mk(domain.MeterSevenDay, m.u7(t), p.anchor, 604800))
	}
	return out
}

func (g *gen) truth(m *meterModel) Truth {
	t := Truth{LagMode: g.sc.LagMode, V7d: g.v7, V5h: g.v5, EarlyResetAt: g.erAt, ShiftAt: g.shiftAt, PrecisionChangeAt: g.pcAt}
	for _, p := range g.periods {
		pt := PeriodTruth{Cycle: p.cycle, Regime: p.regime, Anchor: p.anchor, Start: p.start, End: p.end, VBase: p.vbase}
		var prox, byp float64
		for _, r := range m.regs {
			if r.t.Before(p.start) || !r.t.Before(p.end) {
				continue
			}
			if r.proxied {
				prox += r.m / p.vbase
				pt.ProxiedUSD += r.usd
			} else {
				byp += r.m / p.vbase
			}
		}
		if prox > 0 {
			pt.VEff = pt.ProxiedUSD / prox
		}
		if prox+byp > 0 {
			pt.UnroutedFrac = byp / (prox + byp)
		}
		pt.Peak = m.u7(p.end.Add(-time.Nanosecond))
		t.Periods = append(t.Periods, pt)
	}
	return t
}
