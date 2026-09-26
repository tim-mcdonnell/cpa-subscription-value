package estimate

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
)

// block is the bootstrap unit: consecutive used segments of one run.
type block struct {
	usd, d         float64
	ticks          int64
	fromSeq, toSeq int
}

// makeBlocks merges consecutive used segments until a block spans
// BlockTicks ticks or BlockDur; blocks never cross a run boundary, and a
// run's short tail is its own block.
func makeBlocks(segs []meter.Segment, used []bool, o Options) []block {
	var out []block
	var cur block
	var open bool
	var start time.Time
	flush := func() {
		if open {
			out = append(out, cur)
		}
		cur, open = block{}, false
	}
	for k, s := range segs {
		if !used[k] {
			flush()
			continue
		}
		if !open {
			cur, open, start = block{fromSeq: s.Seq}, true, s.TStart
		}
		cur.usd += s.USD
		cur.d += s.D()
		cur.ticks += s.DeltaTicks
		cur.toSeq = s.Seq
		if cur.ticks >= o.BlockTicks || s.TEnd.Sub(start) >= o.BlockDur {
			flush()
		}
	}
	flush()
	return out
}

// seedFor derives a PCG seed from a series identity with FNV-1a.
func seedFor(id string) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(id))
	s := h.Sum64()
	return rand.New(rand.NewPCG(s, s^0x9e3779b97f4a7c15))
}

// bootstrapSE is the moving-block bootstrap SE of V* = Σ$*/ΣD* with block
// length ⌈B^{1/3}⌉ over the B blocks.
func bootstrapSE(bs []block, resamples int, seriesID string) float64 {
	n := len(bs)
	if n < 2 || resamples < 2 {
		return 0
	}
	l := int(math.Ceil(math.Cbrt(float64(n))))
	rng := seedFor(seriesID)
	vals := make([]float64, 0, resamples)
	for range resamples {
		var usd, d float64
		for taken := 0; taken < n; {
			st := rng.IntN(n - l + 1)
			for j := 0; j < l && taken < n; j++ {
				usd += bs[st+j].usd
				d += bs[st+j].d
				taken++
			}
		}
		if d > 0 {
			vals = append(vals, usd/d)
		}
	}
	return stddev(vals)
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var m float64
	for _, x := range xs {
		m += x
	}
	m /= float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - m) * (x - m)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// CUSUM runs a two-sided tabular CUSUM over x with allowance k = σ/2 and
// threshold h = 4σ, where σ = SE_block is the sample SD of x. The reference
// mean is that of the first quarter (≥3 points), the regime being tested
// against. It returns the index of the first point of the new regime.
func CUSUM(x []float64) (int, bool) {
	n := len(x)
	if n < 6 {
		return 0, false
	}
	sigma := stddev(x)
	if sigma == 0 {
		return 0, false
	}
	w := max(3, n/4)
	var mu float64
	for _, v := range x[:w] {
		mu += v
	}
	mu /= float64(w)
	k, h := sigma/2, 4*sigma
	var sp, sn float64
	zeroP, zeroN := -1, -1
	for i, v := range x {
		sp = max(0, sp+v-mu-k)
		sn = max(0, sn-(v-mu)-k)
		if sp == 0 {
			zeroP = i
		}
		if sn == 0 {
			zeroN = i
		}
		cp := -1
		switch {
		case sp > h:
			cp = zeroP + 1
		case sn > h:
			cp = zeroN + 1
		}
		if cp >= 0 {
			if cp < 1 || cp >= n {
				return 0, false
			}
			return cp, true
		}
	}
	return 0, false
}

// subRegimes splits the blocks at a CUSUM change point in log(Σ$/ΣD).
func subRegimes(bs []block) []SubRegime {
	var x []float64
	var idx []int
	for i, b := range bs {
		if b.usd > 0 && b.d > 0 {
			x = append(x, math.Log(b.usd/b.d))
			idx = append(idx, i)
		}
	}
	cp, ok := CUSUM(x)
	if !ok {
		return nil
	}
	split := idx[cp]
	part := func(bs []block) SubRegime {
		s := SubRegime{FromSeq: bs[0].fromSeq, ToSeq: bs[len(bs)-1].toSeq}
		var usd, d float64
		for _, b := range bs {
			usd += b.usd
			d += b.d
			s.Ticks += b.ticks
		}
		s.VHat = usd / d
		return s
	}
	return []SubRegime{part(bs[:split]), part(bs[split:])}
}

// LevelShift reports whether consecutive finals differ by more than
// 3·√(SE₁² + SE₂²).
func LevelShift(prev, cur Result) bool {
	return math.Abs(cur.VHat-prev.VHat) > 3*math.Hypot(prev.SE(), cur.SE())
}

// Blend is the precision-weighted log-space blend of a running estimate
// with the previous final, whose variance is inflated by σ_rw² per cycle
// elapsed (random walk on log V).
func Blend(cur, prevFinal Result, cyclesApart int, sigmaRW float64) float64 {
	if cur.VHat <= 0 {
		return 0
	}
	if prevFinal.VHat <= 0 {
		return cur.VHat
	}
	v1 := math.Pow(cur.SE()/cur.VHat, 2)
	v0 := math.Pow(prevFinal.SE()/prevFinal.VHat, 2) + float64(max(cyclesApart, 1))*sigmaRW*sigmaRW
	if v1 == 0 {
		return cur.VHat
	}
	m := (math.Log(cur.VHat)/v1 + math.Log(prevFinal.VHat)/v0) / (1/v1 + 1/v0)
	return math.Exp(m)
}
