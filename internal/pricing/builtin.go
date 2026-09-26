package pricing

import (
	"regexp"
	"strconv"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/ingest"
)

// Claude cache-write multiples of input: 1.25× for the 5-minute TTL, 2× for
// the 1-hour TTL (claude-api skill, shared/prompt-caching.md "Economics").
// models.dev publishes one cache_write, which is the 5-minute rate, so the
// 1-hour rate is derived as 2.0/1.25 = 1.6× of it.
const (
	claudeCW5m = 1.25
	claudeCW1h = 2.0
)

const (
	srcClaudeSkill = "builtin:claude-api skill (shared/models.md, shared/model-migration.md) 2026-09"
	srcModelsDev   = "builtin:models.dev openai 2026-09-26"
	srcModelsDevAz = "builtin:models.dev azure listing 2026-09-26"
)

// builtinRows is the fallback and fill table, USD per MTok.
//
// Anthropic rows are from the claude-api skill reference (bundled skill
// 2.1.281: SKILL.md "Current Models", shared/models.md, shared/model-migration.md):
// Fable 5.1 $10/$50 with $0.25 cache reads; Fable 5 same but $1 reads; Opus 5.5
// $4/$20, $0.20 reads, fast mode $8/$40 (2×); Opus 5 $5/$25, fast $10/$50
// (2×); Opus 4.5–4.8 $5/$25; Sonnet 5 $2/$10; Sonnet 4.x $3/$15; Haiku 4.5
// $1/$5; cache reads 0.1× input unless stated. No long-context premium on any
// current Claude model (1M at standard pricing). Opus 4.8 has fast mode but the
// reference gives no price, so it carries none. Opus 4.0/4.1 ($15/$75) are
// legacy and come from models.dev's Azure/Vertex listings.
//
// OpenAI rows mirror models.dev's openai provider on 2026-09-26; older codex
// checkpoints no longer listed there use its Azure OpenAI listings (same list
// price). Long-context multiplier = cost.tiers[context].input / input.
var builtinRows = []struct {
	p                    domain.Provider
	key                  string
	in, out, cr, cw, lng float64
	fast                 float64
	src                  string
}{
	{domain.ProviderClaude, "claude-fable-5", 10, 50, 1.0, 12.5, 1, 0, srcClaudeSkill},
	{domain.ProviderClaude, "claude-fable-5-1", 10, 50, 0.25, 12.5, 1, 0, srcClaudeSkill},
	{domain.ProviderClaude, "claude-opus-5", 5, 25, 0.5, 6.25, 1, 2, srcClaudeSkill},
	{domain.ProviderClaude, "claude-opus-5-5", 4, 20, 0.2, 5, 1, 2, srcClaudeSkill},
	{domain.ProviderClaude, "claude-opus-4", 5, 25, 0.5, 6.25, 1, 0, srcClaudeSkill},
	{domain.ProviderClaude, "claude-opus-4-1", 15, 75, 1.5, 18.75, 1, 0, srcModelsDevAz},
	{domain.ProviderClaude, "claude-opus-4-0", 15, 75, 1.5, 18.75, 1, 0, srcModelsDevAz},
	{domain.ProviderClaude, "claude-opus-4-20250514", 15, 75, 1.5, 18.75, 1, 0, srcModelsDevAz},
	{domain.ProviderClaude, "claude-sonnet-5", 2, 10, 0.2, 2.5, 1, 0, srcClaudeSkill},
	{domain.ProviderClaude, "claude-sonnet-4", 3, 15, 0.3, 3.75, 1, 0, srcClaudeSkill},
	{domain.ProviderClaude, "claude-haiku-4", 1, 5, 0.1, 1.25, 1, 0, srcClaudeSkill},

	{domain.ProviderCodex, "gpt-5-codex", 1.25, 10, 0.125, 0, 1, 0, srcModelsDevAz},
	{domain.ProviderCodex, "gpt-5.1-codex", 1.25, 10, 0.125, 0, 1, 0, srcModelsDevAz},
	{domain.ProviderCodex, "gpt-5.1-codex-max", 1.25, 10, 0.125, 0, 1, 0, srcModelsDevAz},
	{domain.ProviderCodex, "gpt-5.1-codex-mini", 0.25, 2, 0.025, 0, 1, 0, srcModelsDevAz},
	{domain.ProviderCodex, "gpt-5.2-codex", 1.75, 14, 0.175, 0, 1, 0, srcModelsDevAz},
	{domain.ProviderCodex, "gpt-5.3-codex", 1.75, 14, 0.175, 0, 1, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.3-codex-spark", 1.75, 14, 0.175, 0, 1, 0, srcModelsDev},
	{domain.ProviderCodex, "codex-spark", 1.75, 14, 0.175, 0, 1, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5", 1.25, 10, 0.125, 0, 1, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5-mini", 0.25, 2, 0.025, 0, 1, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.1", 1.25, 10, 0.125, 0, 1, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.2", 1.75, 14, 0.175, 0, 1, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.4", 2.5, 15, 0.25, 0, 2, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.4-mini", 0.75, 4.5, 0.075, 0, 1, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.5", 5, 30, 0.5, 0, 2, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.6", 4, 20, 0.4, 5, 2, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.6-sol", 4, 20, 0.4, 5, 2, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.6-terra", 2, 12, 0.2, 2.5, 2, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-5.6-luna", 0.2, 1.2, 0.02, 0.25, 2, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-6-astra", 10, 50, 1, 12.5, 2, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-6-sol", 2, 10, 0.2, 2.5, 2, 0, srcModelsDev},
	{domain.ProviderCodex, "gpt-6-luna", 0.1, 0.5, 0.01, 0.125, 2, 0, srcModelsDev},
}

// Builtin returns the fallback table (list prices, 5-minute Claude cache
// writes as CacheWrite5m).
func Builtin() Table {
	t := make(Table, len(builtinRows))
	for _, b := range builtinRows {
		r := Rates{Input: b.in, Output: b.out, CacheRead: b.cr, CacheWrite5m: b.cw,
			LongCtxMult: b.lng, FastMult: b.fast, Source: b.src}
		t[Key{b.p, b.key}] = withPolicy(b.p, b.key, r, nil)
	}
	return t
}

// withPolicy fills what models.dev does not publish: the 1-hour cache-write
// rate and fast-mode multipliers. Claude fast multipliers come from the
// built-in row for the key, else for its family. OpenAI rows get
// openaiFastMult. builtin may be nil while Builtin itself is being built.
func withPolicy(p domain.Provider, key string, r Rates, builtin Table) Rates {
	switch p {
	case domain.ProviderClaude:
		if r.CacheWrite5m <= 0 {
			r.CacheWrite5m = r.Input * claudeCW5m
		}
		r.CacheWrite1h = round9(r.CacheWrite5m * claudeCW1h / claudeCW5m)
		if r.FastMult <= 1 && builtin != nil {
			if b, ok := builtin[Key{p, key}]; ok {
				r.FastMult = b.FastMult
			} else if b, ok := builtin[Key{p, ingest.ModelFamily(p, key)}]; ok {
				r.FastMult = b.FastMult
			}
		}
	case domain.ProviderCodex:
		// OpenAI has one cache-write price and no TTL tiers.
		r.CacheWrite1h = r.CacheWrite5m
		if r.FastMult <= 1 {
			r.FastMult = openaiFastMult(key)
		}
	}
	if r.LongCtxMult < 1 {
		r.LongCtxMult = 1
	}
	if r.FastMult < 1 {
		r.FastMult = 1
	}
	return r
}

var gptVersion = regexp.MustCompile(`^gpt-(\d+)(?:\.(\d+))?`)

// openaiFastMult is the priority / fast service-tier multiple. 2.0 matches
// models.dev's gpt-5.3-codex-fast listing (Vercel: $3.50/$28 vs $1.75/$14);
// 2.5 for gpt-5.5 and later follows docs/plan.md (§Weights: "refRate already
// 2.5×"). Neither is published per model by models.dev; treat as an estimate.
func openaiFastMult(key string) float64 {
	m := gptVersion.FindStringSubmatch(key)
	if m == nil {
		return 2.0
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > 5 || (major == 5 && minor >= 5) {
		return 2.5
	}
	return 2.0
}
