package weights

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	obsSD             = 0.35 // σ_obs of a residual, percentage points
	huberDelta        = 0.6  // Huber δ, percentage points
	firstScaleSD      = 2.5  // prior SD of the first cycle's s around its median ratio
	freeSD            = 1e6  // effectively flat prior
	lockedSD          = 1e-4 // pins a parameter at its stage-1 value in stage 2
	minEligible       = 5
	maxParams         = 64
	maxIterations     = 80
	paramClamp        = 20
	minBoundaryWeight = 0.05
	correlatedAbove   = 0.9
	identifiedShare   = 0.5
	z95               = 1.96
)

// term is one priced feature of an eligible segment; mi/ti/fi/li index the
// model/type/fast/long parameter it carries, −1 when that factor is fixed.
type term struct {
	family         string
	typ            TokenType
	fast, long     bool
	tokens         int64
	usd            float64 // tokens × refRate
	mi, ti, fi, li int
}

// obs is an eligible segment prepared for fitting.
type obs struct {
	key    string
	seg    Segment
	pct    float64 // observed 100·Δu
	usd    float64 // Σ term.usd (all factors 1)
	weight float64 // max(0.05, boundary) · 2^(−age/halfLife)
	terms  []term
	scale  int
}

func scaleKey(seg Segment) string { return seg.CycleKey + "\x1f" + seg.RegimeKey }

// prepare prices a segment. It is eligible when Δu > 0, every family has a
// reference price and the reference $ is positive.
func prepare(seg Segment, rate RateFunc) (o obs, unknown []string, ok bool) {
	o = obs{key: scaleKey(seg), seg: seg, pct: 100 * seg.DeltaTicks * seg.Tick}
	for _, ft := range seg.Features {
		if ft.Tokens <= 0 {
			continue
		}
		r, known := rate(ft.Family, ft.Type, ft.Fast, ft.Long)
		if !known {
			unknown = append(unknown, ft.Family)
			continue
		}
		u := float64(ft.Tokens) * r
		o.usd += u
		o.terms = append(o.terms, term{family: ft.Family, typ: ft.Type, fast: ft.Fast, long: ft.Long,
			tokens: ft.Tokens, usd: u, mi: -1, ti: -1, fi: -1, li: -1})
	}
	ok = len(unknown) == 0 && seg.DeltaTicks > 0 && seg.Tick > 0 && o.usd > 0 &&
		!math.IsInf(o.pct, 0) && !math.IsNaN(o.usd) && !math.IsInf(o.usd, 0)
	return o, unknown, ok
}

// fitWeight is the boundary weight (floored at 0.05) × exponential decay.
func fitWeight(seg Segment, now time.Time, halfLife time.Duration) float64 {
	bw := seg.Weight
	if bw <= 0 {
		bw = 1
	}
	age := math.Max(0, now.Sub(seg.EndAt).Hours())
	return math.Max(minBoundaryWeight, bw) * math.Exp(-math.Ln2*age/halfLife.Hours())
}

type rwEdge struct {
	prev, cur int
	sigma     float64
}

type cycleInfo struct {
	key, cycleKey, regimeKey string
	first                    time.Time
	param, segments          int
}

type model struct {
	names    []string
	index    map[string]int
	mean, sd []float64
	rw       []rwEdge
	cycles   []cycleInfo
}

func (m *model) add(name string, mean, sd float64) int {
	m.index[name] = len(m.names)
	m.names = append(m.names, name)
	m.mean = append(m.mean, mean)
	m.sd = append(m.sd, sd)
	return len(m.names) - 1
}

func (m *model) isScale(i int) bool { return i < len(m.cycles) }

// withPriors copies m with its own prior slices.
func (m *model) withPriors() *model {
	c := *m
	c.mean = append([]float64(nil), m.mean...)
	c.sd = append([]float64(nil), m.sd...)
	return &c
}

// collectCycles orders (cycle, regime) keys by their first segment end.
func collectCycles(obs []*obs) []cycleInfo {
	byKey := map[string]*cycleInfo{}
	for _, o := range obs {
		c := byKey[o.key]
		if c == nil {
			c = &cycleInfo{key: o.key, cycleKey: o.seg.CycleKey, regimeKey: o.seg.RegimeKey, first: o.seg.EndAt}
			byKey[o.key] = c
		}
		if o.seg.EndAt.Before(c.first) {
			c.first = o.seg.EndAt
		}
		c.segments++
	}
	out := make([]cycleInfo, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].first.Equal(out[j].first) {
			return out[i].first.Before(out[j].first)
		}
		return out[i].key < out[j].key
	})
	return out
}

// medianRatio is the upper median of pct/usd: the initial exp(s_c) in pct/USD.
func medianRatio(obs []*obs) float64 {
	var r []float64
	for _, o := range obs {
		if o.usd > 0 && o.pct > 0 {
			r = append(r, o.pct/o.usd)
		}
	}
	if len(r) == 0 {
		return 1
	}
	sort.Float64s(r)
	return r[len(r)/2]
}

// predict returns 100·exp(s_c)·W_k and, when grad is non-nil (zeroed by the
// caller), ∂prediction/∂x.
func (m *model) predict(o *obs, x, grad []float64) float64 {
	s := math.Exp(x[o.scale])
	var w float64
	for i := range o.terms {
		t := &o.terms[i]
		lf := 0.0
		for _, p := range [4]int{t.mi, t.ti, t.fi, t.li} {
			if p >= 0 {
				lf += x[p]
			}
		}
		v := t.usd * math.Exp(lf)
		w += v
		if grad != nil {
			g := 100 * s * v
			for _, p := range [4]int{t.mi, t.ti, t.fi, t.li} {
				if p >= 0 {
					grad[p] += g
				}
			}
		}
	}
	pred := 100 * s * w
	if grad != nil {
		grad[o.scale] = pred
	}
	return pred
}

func huber(r, delta float64) float64 {
	a := math.Abs(r)
	if a <= delta {
		return 0.5 * a * a
	}
	return delta * (a - 0.5*delta)
}

// objective = Σ w·Huber(r)/σ² + Σ ½((x−μ)/σ_prior)² + Σ ½((s_c−s_prev)/σ_rw)².
func (m *model) objective(obs []*obs, x []float64) float64 {
	var total float64
	for _, o := range obs {
		r := o.pct - m.predict(o, x, nil)
		total += o.weight * huber(r, huberDelta) / (obsSD * obsSD)
	}
	for i := range x {
		d := (x[i] - m.mean[i]) / m.sd[i]
		total += 0.5 * d * d
	}
	for _, e := range m.rw {
		d := (x[e.cur] - x[e.prev]) / e.sigma
		total += 0.5 * d * d
	}
	return total
}

// normal builds the IRLS Gauss–Newton system H·step = b: Huber weights
// δ/|r| beyond δ, prior and random-walk precisions added.
func (m *model) normal(obs []*obs, x []float64) ([][]float64, []float64) {
	n := len(x)
	h := make([][]float64, n)
	for i := range h {
		h[i] = make([]float64, n)
	}
	b := make([]float64, n)
	grad := make([]float64, n)
	var nz []int
	for _, o := range obs {
		clear(grad)
		r := o.pct - m.predict(o, x, grad)
		w := o.weight
		if a := math.Abs(r); a > huberDelta {
			w *= huberDelta / a
		}
		w /= obsSD * obsSD
		nz = nz[:0]
		for i, g := range grad {
			if g != 0 {
				nz = append(nz, i)
			}
		}
		for _, i := range nz {
			b[i] += w * grad[i] * r
			for _, j := range nz {
				h[i][j] += w * grad[i] * grad[j]
			}
		}
	}
	for i := 0; i < n; i++ {
		prec := 1 / (m.sd[i] * m.sd[i])
		h[i][i] += prec
		b[i] -= prec * (x[i] - m.mean[i])
	}
	for _, e := range m.rw {
		prec := 1 / (e.sigma * e.sigma)
		d := x[e.cur] - x[e.prev]
		h[e.cur][e.cur] += prec
		h[e.prev][e.prev] += prec
		h[e.cur][e.prev] -= prec
		h[e.prev][e.cur] -= prec
		b[e.cur] -= prec * d
		b[e.prev] += prec * d
	}
	return h, b
}

// optimize runs damped Gauss–Newton (Levenberg–Marquardt, λ·(1+H_ii)) from
// the prior means and returns the Laplace covariance H⁻¹ at the optimum.
func (m *model) optimize(obs []*obs) (x []float64, cov [][]float64, obj float64, err error) {
	x = append([]float64(nil), m.mean...)
	damping := 0.01
	obj = m.objective(obs, x)
	for iter := 0; iter < maxIterations; iter++ {
		h, b := m.normal(obs, x)
		for i := range h {
			h[i][i] += damping * (1 + h[i][i])
		}
		step, ok := solveDense(h, b)
		if !ok {
			return nil, nil, 0, errors.New("weights: LM normal equations are singular")
		}
		cand := append([]float64(nil), x...)
		var norm float64
		for i := range cand {
			cand[i] = math.Max(-paramClamp, math.Min(paramClamp, cand[i]+step[i]))
			norm += step[i] * step[i]
		}
		if o := m.objective(obs, cand); o < obj {
			x, obj = cand, o
			damping = math.Max(1e-8, damping/2)
			if norm < 1e-12 {
				break
			}
		} else if damping *= 4; damping > 1e12 {
			break
		}
	}
	h, _ := m.normal(obs, x)
	cov, ok := invertDense(h)
	if !ok {
		return nil, nil, 0, errors.New("weights: posterior Hessian is singular")
	}
	return x, cov, obj, nil
}

func solveDense(matrix [][]float64, rhs []float64) ([]float64, bool) {
	n := len(rhs)
	a := make([][]float64, n)
	b := append([]float64(nil), rhs...)
	for i := range a {
		a[i] = append([]float64(nil), matrix[i]...)
	}
	for col := 0; col < n; col++ {
		pivot := col
		for row := col + 1; row < n; row++ {
			if math.Abs(a[row][col]) > math.Abs(a[pivot][col]) {
				pivot = row
			}
		}
		if math.Abs(a[pivot][col]) < 1e-12 {
			return nil, false
		}
		a[col], a[pivot] = a[pivot], a[col]
		b[col], b[pivot] = b[pivot], b[col]
		f := a[col][col]
		for j := col; j < n; j++ {
			a[col][j] /= f
		}
		b[col] /= f
		for row := 0; row < n; row++ {
			if row == col {
				continue
			}
			f = a[row][col]
			for j := col; j < n; j++ {
				a[row][j] -= f * a[col][j]
			}
			b[row] -= f * b[col]
		}
	}
	return b, true
}

func invertDense(matrix [][]float64) ([][]float64, bool) {
	n := len(matrix)
	inv := make([][]float64, n)
	for i := range inv {
		inv[i] = make([]float64, n)
	}
	for col := 0; col < n; col++ {
		rhs := make([]float64, n)
		rhs[col] = 1
		sol, ok := solveDense(matrix, rhs)
		if !ok {
			return nil, false
		}
		for row := range sol {
			inv[row][col] = sol[row]
		}
	}
	return inv, true
}

// FitWeights fits the multiplicative log-factor model to one (account,
// meter)'s segments. Stage 1 estimates the shared factors with every cycle
// scale free, so cross-cycle allowance drift cannot masquerade as a factor;
// stage 2 holds the factors and smooths the scales with the random walk.
// Fewer than 5 eligible segments or more than 64 parameters is an error.
func FitWeights(segments []Segment, params ProviderParams, now time.Time) (Fit, error) {
	params = withDefaults(params)
	fit := Fit{Provider: params.Provider, Segments: len(segments), ComputedAt: now}
	if params.RefRate == nil {
		return fit, errors.New("weights: ProviderParams.RefRate is nil")
	}
	unknownSet := map[string]bool{}
	var all []*obs
	for _, seg := range segments {
		o, unknown, ok := prepare(seg, params.RefRate)
		for _, f := range unknown {
			unknownSet[f] = true
		}
		if ok {
			o.weight = fitWeight(seg, now, params.HalfLife)
			all = append(all, &o)
		}
	}
	for f := range unknownSet {
		fit.UnknownFamilies = append(fit.UnknownFamilies, f)
	}
	sort.Strings(fit.UnknownFamilies)
	fit.Eligible = len(all)
	if len(all) > 0 {
		fit.Lag = all[0].seg.Lag
	}
	if len(all) < minEligible {
		return fit, fmt.Errorf("weights: %d eligible segments, need %d", len(all), minEligible)
	}
	fit.Anchor = params.Anchor
	if fit.Anchor == "" {
		fit.Anchor = autoAnchor(all, now, params.HalfLife)
	}

	m := &model{index: map[string]int{}, cycles: collectCycles(all)}
	byCycle := map[string][]*obs{}
	for _, o := range all {
		byCycle[o.key] = append(byCycle[o.key], o)
	}
	for i := range m.cycles {
		c := &m.cycles[i]
		sd := firstScaleSD
		if i > 0 {
			sd = freeSD
		}
		c.param = m.add("scale:"+c.key, math.Log(medianRatio(byCycle[c.key])), sd)
		if i > 0 {
			m.rw = append(m.rw, rwEdge{prev: m.cycles[i-1].param, cur: c.param, sigma: params.RandomWalkSigma})
		}
	}
	for _, o := range all {
		o.scale = m.index["scale:"+o.key]
	}

	diags := assessIdentifiability(all, params, fit.Anchor)
	for _, d := range diags {
		switch {
		case d.name == "fast" || d.name == "long":
			// Not gated: present whenever the data carries such tokens.
			if d.segments > 0 {
				p := params.FastPrior
				if d.name == "long" {
					p = params.LongPrior
				}
				m.add(d.name, p.Mean, p.SD)
			}
		case d.unlocked:
			m.add(d.name, 0, factorPriorSD)
		}
	}
	if len(m.names) > maxParams {
		return fit, fmt.Errorf("weights: %d parameters exceeds %d", len(m.names), maxParams)
	}
	resolveTerms(all, m)

	stage1 := m.withPriors()
	stage1.rw = nil
	x1, cov1, _, err := stage1.optimize(all)
	if err != nil {
		return fit, err
	}
	stage2 := m.withPriors()
	for i := len(m.cycles); i < len(m.names); i++ {
		stage2.mean[i], stage2.sd[i] = x1[i], lockedSD
	}
	x2, cov2, loss, err := stage2.optimize(all)
	if err != nil {
		return fit, err
	}
	fit.Loss = loss
	fit.Factors = deriveFactors(m, diags, x1, cov1, params, fit.Anchor)
	for _, c := range m.cycles {
		s, sd := x2[c.param], math.Sqrt(math.Max(0, cov2[c.param][c.param]))
		fit.Scales = append(fit.Scales, CycleScale{CycleKey: c.cycleKey, RegimeKey: c.regimeKey,
			LogScale: s, SD: sd, ValuePer100: expc(-s),
			ValueCILo: expc(-s - z95*sd), ValueCIHi: expc(-s + z95*sd), Segments: c.segments})
	}
	return fit, nil
}

// autoAnchor is the family with the most reference $ among segments that
// ended within one half-life of now (all segments if none did).
func autoAnchor(all []*obs, now time.Time, halfLife time.Duration) string {
	usd := map[string]float64{}
	for pass := 0; pass < 2 && len(usd) == 0; pass++ {
		for _, o := range all {
			if pass == 0 && now.Sub(o.seg.EndAt) > halfLife {
				continue
			}
			for _, t := range o.terms {
				usd[t.family] += t.usd
			}
		}
	}
	best, bestUSD := "", -1.0
	for f, v := range usd {
		if v > bestUSD || v == bestUSD && f < best {
			best, bestUSD = f, v
		}
	}
	return best
}

func resolveTerms(all []*obs, m *model) {
	idx := func(name string) int {
		if i, ok := m.index[name]; ok {
			return i
		}
		return -1
	}
	for _, o := range all {
		for i := range o.terms {
			t := &o.terms[i]
			t.mi = idx("model:" + t.family)
			t.ti = idx("type:" + string(t.typ))
			if t.fast {
				t.fi = idx("fast")
			}
			if t.long {
				t.li = idx("long")
			}
		}
	}
}

// deriveFactors turns stage-1 estimates into reported factors. Status is
// identified when the factor is a free parameter with data share ≥ 0.5 and no
// |correlation| ≥ 0.9 with another factor; gated-out factors report their prior.
func deriveFactors(m *model, diags []diagnostic, x []float64, cov [][]float64, params ProviderParams, anchor string) map[string]Factor {
	out := map[string]Factor{
		"model:" + anchor:               {Name: "model:" + anchor, Estimate: 1, CILo: 1, CIHi: 1, Status: StatusAnchor},
		"type:" + string(UncachedInput): {Name: "type:" + string(UncachedInput), Estimate: 1, CILo: 1, CIHi: 1, Status: StatusAnchor},
	}
	for _, d := range diags {
		f := out[d.name]
		f.Name, f.Segments, f.ShareSD, f.Fisher = d.name, d.segments, d.shareSD, d.fisher
		if f.Status == StatusAnchor {
			out[d.name] = f
			continue
		}
		prior := Prior{0, factorPriorSD}
		switch d.name {
		case "fast":
			prior = params.FastPrior
		case "long":
			prior = params.LongPrior
		}
		i, ok := m.index[d.name]
		if !ok {
			f.Estimate = expc(prior.Mean)
			f.CILo, f.CIHi = expc(prior.Mean-z95*prior.SD), expc(prior.Mean+z95*prior.SD)
			f.Status = StatusPriorLocked
			out[d.name] = f
			continue
		}
		variance := math.Max(0, cov[i][i])
		sd := math.Sqrt(variance)
		f.Estimate = expc(x[i])
		f.CILo, f.CIHi = expc(x[i]-z95*sd), expc(x[i]+z95*sd)
		f.DataShare = math.Max(0, math.Min(1, 1-variance/(m.sd[i]*m.sd[i])))
		correlated := false
		for j := len(m.cycles); j < len(m.names); j++ {
			if j != i && cov[i][i] > 0 && cov[j][j] > 0 &&
				math.Abs(cov[i][j]/math.Sqrt(cov[i][i]*cov[j][j])) >= correlatedAbove {
				correlated = true
			}
		}
		f.Status = StatusPriorLocked
		if f.DataShare >= identifiedShare && !correlated {
			f.Status = StatusIdentified
		}
		out[d.name] = f
	}
	return out
}

// expc is exp with its argument clamped so reported values stay finite (JSON).
func expc(v float64) float64 { return math.Exp(math.Max(-700, math.Min(700, v))) }
