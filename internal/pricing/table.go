// Package pricing turns token buckets into API list-price dollars. The active
// price table comes from models.dev (synced), filled by a built-in table, and
// is content-addressed so every event can record which prices it was frozen
// under and a reprice can find the ones that are stale.
package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/ingest"
)

// Rates are list prices in USD per million tokens for one model family.
// Multipliers of 0 or 1 mean "no premium". LongCtxMult scales the input-side
// buckets (uncached input, cache read, cache write) of a long-context request;
// FastMult scales every bucket of a fast / priority request.
type Rates struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	LongCtxMult  float64 `json:"long_ctx_mult"`
	FastMult     float64 `json:"fast_mult"`
	Source       string  `json:"source"`
}

// Key addresses one row. Family is usually ingest.ModelFamily's output; a
// full model id appears as its own key only when that checkpoint is priced
// differently from its family (claude-opus-5-5 inside claude-opus-5).
type Key struct {
	Provider domain.Provider `json:"provider"`
	Family   string          `json:"model_family"`
}

// Table is one compiled price table. Treat it as immutable once built.
type Table map[Key]Rates

// Long-context thresholds in prompt tokens. Claude's matches ingest.IsLong;
// OpenAI's is the models.dev cost.tiers[].tier.size for the gpt-5.4+ family.
const openaiLongContextTokens = 272_000

// Hash is a canonical content hash: sha256 over the sorted rows with every
// rate rounded to 1e-9, first 16 hex chars. Source strings are excluded so
// the same numbers from different origins hash identically.
func (t Table) Hash() string {
	lines := make([]string, 0, len(t))
	for k, r := range t {
		lines = append(lines, strings.Join([]string{
			string(k.Provider), k.Family,
			canon(r.Input), canon(r.Output), canon(r.CacheRead),
			canon(r.CacheWrite5m), canon(r.CacheWrite1h),
			canon(mult(r.LongCtxMult)), canon(mult(r.FastMult)),
		}, "\t"))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

func round9(v float64) float64 { return math.Round(v*1e9) / 1e9 }

func canon(v float64) string { return strconv.FormatFloat(round9(v), 'f', 9, 64) }

func mult(m float64) float64 {
	if m <= 1 {
		return 1
	}
	return m
}

// Lookup resolves a model id to its rates. Order: the longest key equal to
// the id or a prefix of it at a separator (so a separately priced checkpoint
// beats its family), then ingest.ModelFamily's family, then the longest key
// that prefixes that family. The returned string is the matched key.
func (t Table) Lookup(p domain.Provider, model string) (Rates, string, bool) {
	m := normalizeModel(model)
	if m == "" {
		return Rates{}, "", false
	}
	if k, ok := t.longestPrefix(p, m); ok {
		return t[Key{p, k}], k, true
	}
	fam := ingest.ModelFamily(p, m)
	if r, ok := t[Key{p, fam}]; ok {
		return r, fam, true
	}
	if k, ok := t.longestPrefix(p, fam); ok {
		return t[Key{p, k}], k, true
	}
	return Rates{}, "", false
}

func (t Table) longestPrefix(p domain.Provider, m string) (string, bool) {
	best := ""
	for k := range t {
		if k.Provider != p || len(k.Family) <= len(best) || !strings.HasPrefix(m, k.Family) {
			continue
		}
		if len(m) == len(k.Family) || strings.ContainsRune("-.@:_[", rune(m[len(k.Family)])) {
			best = k.Family
		}
	}
	return best, best != ""
}

// normalizeModel lowercases and drops any routing prefix ("anthropic/…").
func normalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	return m
}

// Cost prices one event: usd with cache writes at CacheWrite5m, usd1h at
// CacheWrite1h. Reasoning is never added: on both providers it is already
// inside Output. ok is false when the model is not in the table.
func (t Table) Cost(ev *domain.UsageEvent) (usd, usd1h float64, ok bool) {
	model := ev.Model
	if strings.TrimSpace(model) == "" {
		model = ev.ModelFamily
	}
	r, _, ok := t.Lookup(ev.Provider, model)
	if !ok {
		return 0, 0, false
	}
	in, cr, cw5, cw1, out := r.Input, r.CacheRead, r.CacheWrite5m, r.CacheWrite1h, r.Output
	if isLong(ev) && r.LongCtxMult > 1 {
		in, cr, cw5, cw1 = in*r.LongCtxMult, cr*r.LongCtxMult, cw5*r.LongCtxMult, cw1*r.LongCtxMult
	}
	if ev.IsFast && r.FastMult > 1 {
		f := r.FastMult
		in, cr, cw5, cw1, out = in*f, cr*f, cw5*f, cw1*f, out*f
	}
	base := float64(ev.UncachedInput)*in + float64(ev.CacheRead)*cr + float64(ev.Output)*out
	usd = (base + float64(ev.CacheWrite)*cw5) / 1e6
	usd1h = (base + float64(ev.CacheWrite)*cw1) / 1e6
	return usd, usd1h, true
}

// isLong uses ingest's 200k flag for Claude and OpenAI's own 272k tier edge.
func isLong(ev *domain.UsageEvent) bool {
	if ev.Provider == domain.ProviderCodex {
		return ev.ContextTokens() > openaiLongContextTokens
	}
	return ev.IsLong
}

// withClaudeCacheWrite sets every Claude row's primary cache-write rate to
// input × multiplier (config.ClaudeCacheWriteMultiplier: 1.25 = 5-minute TTL,
// 2.0 = 1-hour). CPA does not report the TTL, so api_usd follows the config
// and api_usd_cw1h is always the 1-hour rate. Derived from input, so applying
// it twice is harmless.
func (t Table) withClaudeCacheWrite(multiplier float64) Table {
	if multiplier <= 0 {
		multiplier = claudeCW5m
	}
	out := make(Table, len(t))
	for k, r := range t {
		if k.Provider == domain.ProviderClaude {
			r.CacheWrite5m = round9(r.Input * multiplier)
			r.CacheWrite1h = round9(r.Input * claudeCW1h)
		}
		out[k] = r
	}
	return out
}
