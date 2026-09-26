package weights

import (
	"math"
	"sort"
)

// Unlock gates. A factor becomes a free parameter only when its $ share
// varies within cycles (weighted SD) and the conditional Fisher information
// of its log factor, given the cycle scale, clears the bar. Type factors also
// need their token ratio to differ across cycles.
const (
	modelMinShareSD      = 0.05
	modelMinFisher       = 10
	cacheMinShareSD      = 0.08
	outputMinShareSD     = 0.02
	typeMinFisher        = 20
	cacheMinCycleRange   = 0.10
	outputMinCycleRange  = 0.02
	ratioMinCycleSegs    = 5
	ratioMinCycleContext = 1_000_000 // context tokens a cycle needs to enter the ratio-range gate
)

type diagnostic struct {
	name                        string
	unlocked                    bool
	segments                    int
	shareSD, fisher, ratioRange float64
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
		minSD, minRange := cacheMinShareSD, cacheMinCycleRange
		if typ == Output {
			minSD, minRange = outputMinShareSD, outputMinCycleRange
		}
		d.ratioRange, d.cyclesCompared = cycleRatioRange(all, typ)
		d.unlocked = d.shareSD >= minSD && d.fisher >= typeMinFisher && d.ratioRange >= minRange
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

// cycleRatioRange is max−min over cycles of tokens(typ)/context tokens,
// counting cycles with ≥ 5 segments and ≥ 1M context tokens; 0 when fewer
// than two cycles qualify.
func cycleRatioRange(all []*obs, typ TokenType) (float64, int) {
	type totals struct {
		context, target int64
		segments        int
	}
	byCycle := map[string]*totals{}
	for _, o := range all {
		row := byCycle[o.key]
		if row == nil {
			row = &totals{}
			byCycle[o.key] = row
		}
		row.segments++
		for _, t := range o.terms {
			if t.typ != Output {
				row.context += t.tokens
			}
			if t.typ == typ {
				row.target += t.tokens
			}
		}
	}
	var ratios []float64
	for _, row := range byCycle {
		if row.segments < ratioMinCycleSegs || row.context < ratioMinCycleContext {
			continue
		}
		ratios = append(ratios, float64(row.target)/float64(row.context))
	}
	if len(ratios) < 2 {
		return 0, len(ratios)
	}
	sort.Float64s(ratios)
	return ratios[len(ratios)-1] - ratios[0], len(ratios)
}
