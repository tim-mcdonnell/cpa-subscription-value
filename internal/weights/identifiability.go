package weights

import (
	"math"
	"sort"
)

// Unlock gates. A factor becomes a free parameter only when its $ share
// varies within cycles (weighted SD) and the conditional Fisher information
// of its log factor, given the cycle scale, clears the bar. Type factors also
// need their reference-$ share to differ across cycles.
//
// The cross-cycle gate is on $ share (Σ tok·refRate of the type / total),
// not token ratio, so one threshold serves every type and provider: the
// sensitivity of a prediction to g_t is exactly the type's $ share, whatever
// its token count or price. (Claude output is ~1 % of context tokens but, at
// 5× input price, 15–40 % of the $; a token-ratio gate never saw that.)
const (
	modelMinShareSD        = 0.05
	modelMinFisher         = 10
	cacheMinShareSD        = 0.08
	outputMinShareSD       = 0.02
	typeMinFisher          = 20
	typeMinCycleShareRange = 0.05 // max−min of a type's $ share across cycles
	shareMinCycleSegs      = 5
	shareMinCycleUSD       = 5.0 // reference USD a cycle needs to enter the share-range gate
)

type diagnostic struct {
	name                        string
	unlocked                    bool
	segments                    int
	shareSD, fisher, shareRange float64
	cyclesCompared              int
}

// assessIdentifiability returns one diagnostic per model family (sorted),
// per token type in params.Types, then fast and long. The anchor and
// uncached_input are reported but never unlocked.
func assessIdentifiability(all []*obs, params ProviderParams, anchor string) []diagnostic {
	families := map[string]bool{}
	for _, o := range all {
		for _, t := range o.terms {
			if t.usd > 0 {
				families[t.family] = true
			}
		}
	}
	names := make([]string, 0, len(families))
	for f := range families {
		names = append(names, f)
	}
	sort.Strings(names)

	var out []diagnostic
	for _, fam := range names {
		d := assess(all, "model:"+fam, func(t *term) bool { return t.family == fam })
		d.unlocked = fam != anchor && d.shareSD >= modelMinShareSD && d.fisher >= modelMinFisher
		out = append(out, d)
	}
	out = append(out, assess(all, "type:"+string(UncachedInput), func(t *term) bool { return t.typ == UncachedInput }))
	seen := map[TokenType]bool{UncachedInput: true}
	for _, typ := range params.Types {
		if seen[typ] {
			continue
		}
		seen[typ] = true
		d := assess(all, "type:"+string(typ), func(t *term) bool { return t.typ == typ })
		minSD := cacheMinShareSD
		if typ == Output {
			minSD = outputMinShareSD
		}
		d.shareRange, d.cyclesCompared = cycleShareRange(all, typ)
		d.unlocked = d.shareSD >= minSD && d.fisher >= typeMinFisher && d.shareRange >= typeMinCycleShareRange
		out = append(out, d)
	}
	out = append(out, assess(all, "fast", func(t *term) bool { return t.fast }))
	out = append(out, assess(all, "long", func(t *term) bool { return t.long }))
	return out
}

// assess computes, per cycle c with scale k_c = median(pct/usd):
// share SD = √(Σ_c Σ w(share − mean_c)² / Σ w), and
// Fisher = Σ_c (T − C²/S)/σ² with S = Σ w(k·usd)², C = Σ w(k·usd)(k·target), T = Σ w(k·target)².
func assess(all []*obs, name string, match func(*term) bool) diagnostic {
	d := diagnostic{name: name}
	groups := map[string][]*obs{}
	var keys []string
	for _, o := range all {
		if _, ok := groups[o.key]; !ok {
			keys = append(keys, o.key)
		}
		groups[o.key] = append(groups[o.key], o)
		for i := range o.terms {
			if match(&o.terms[i]) {
				d.segments++
				break
			}
		}
	}
	sort.Strings(keys)
	var variance, totalWeight, fisher float64
	for _, key := range keys {
		group := groups[key]
		k := medianRatio(group)
		var w, sh, sh2, s, c, t float64
		for _, o := range group {
			var target float64
			for i := range o.terms {
				if match(&o.terms[i]) {
					target += o.terms[i].usd
				}
			}
			share := target / o.usd
			w += o.weight
			sh += o.weight * share
			sh2 += o.weight * share * share
			s += o.weight * (k * o.usd) * (k * o.usd)
			c += o.weight * (k * o.usd) * (k * target)
			t += o.weight * (k * target) * (k * target)
		}
		if w > 0 {
			variance += sh2 - sh*sh/w
			totalWeight += w
		}
		if s > 0 {
			fisher += (t - c*c/s) / (obsSD * obsSD)
		}
	}
	if totalWeight > 0 {
		d.shareSD = math.Sqrt(math.Max(0, variance/totalWeight))
	}
	d.fisher = math.Max(0, fisher)
	return d
}

// cycleShareRange is max−min over cycles of the type's share of reference $
// (all factors 1), counting cycles with ≥ 5 segments and ≥ $5 reference USD;
// 0 when fewer than two cycles qualify.
func cycleShareRange(all []*obs, typ TokenType) (float64, int) {
	type totals struct {
		usd, target float64
		segments    int
	}
	byCycle := map[string]*totals{}
	var keys []string
	for _, o := range all {
		row := byCycle[o.key]
		if row == nil {
			row = &totals{}
			byCycle[o.key] = row
			keys = append(keys, o.key)
		}
		row.segments++
		row.usd += o.usd
		for _, t := range o.terms {
			if t.typ == typ {
				row.target += t.usd
			}
		}
	}
	var shares []float64
	for _, key := range keys {
		row := byCycle[key]
		if row.segments < shareMinCycleSegs || row.usd < shareMinCycleUSD {
			continue
		}
		shares = append(shares, row.target/row.usd)
	}
	if len(shares) < 2 {
		return 0, len(shares)
	}
	sort.Float64s(shares)
	return shares[len(shares)-1] - shares[0], len(shares)
}
