// Package ingest turns a host UsageRecord into the provider-neutral
// UsageEvent plus the MeterReadings carried by its response headers.
package ingest

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// Pricer freezes API list-price cost onto an event. hash identifies the price
// snapshot used so a later reprice can tell what changed.
type Pricer interface {
	Price(ev *domain.UsageEvent) (usd, usdCacheWrite1h float64, hash string, ok bool)
}

// NoPricer leaves every event unpriced.
type NoPricer struct{}

func (NoPricer) Price(*domain.UsageEvent) (float64, float64, string, bool) {
	return 0, 0, "", false
}

// Extras are account/response-level header signals that are not per-meter
// readings but that the store or estimator still want.
type Extras struct {
	RepresentativeClaim   string
	OverageStatus         string
	OverageDisabledReason string
	PlanType              string
	UnifiedStatus         string
}

// Result is one normalized record.
type Result struct {
	Event    domain.UsageEvent
	Readings []domain.MeterReading
	Extras   Extras
	Provider domain.Provider
}

// longContextTokens is the boundary above which Claude bills long-context rates.
const longContextTokens = 200_000

// Normalize adapts one record. The second return is false when the record's
// provider is neither claude nor codex; the record is then not ours to store.
func Normalize(rec domain.UsageRecord, pricer Pricer) (Result, bool) {
	provider, ok := DetectProvider(rec)
	if !ok {
		return Result{}, false
	}
	if pricer == nil {
		pricer = NoPricer{}
	}

	ev := domain.UsageEvent{
		DedupKey:        DedupKey(rec),
		Provider:        provider,
		AuthIndex:       rec.AuthIndex,
		AuthID:          rec.AuthID,
		RequestedAt:     rec.RequestedAt,
		ObservedAt:      observedAt(rec),
		Latency:         rec.Latency,
		Model:           rec.Model,
		ModelFamily:     ModelFamily(provider, rec.Model),
		Alias:           rec.Alias,
		ServiceTier:     firstNonEmpty(rec.ResponseServiceTier, rec.ServiceTier),
		ReasoningEffort: rec.ReasoningEffort,
		Failed:          rec.Failed,
		StatusCode:      rec.Failure.StatusCode,
		Output:          rec.Detail.OutputTokens,
		Reasoning:       rec.Detail.ReasoningTokens,
	}

	d := rec.Detail
	switch provider {
	case domain.ProviderClaude:
		// Messages API: input_tokens excludes both cache buckets. CachedTokens is
		// a host-side mirror of cache_read (falling back to cache_creation) and
		// is ignored so it can never double count.
		ev.UncachedInput = d.InputTokens
		ev.CacheRead = d.CacheReadTokens
		ev.CacheWrite = d.CacheCreationTokens
		if d.TotalTokens != d.InputTokens+d.OutputTokens+d.CacheReadTokens+d.CacheCreationTokens {
			ev.Flags |= domain.FlagTotalMismatch
		}
	case domain.ProviderCodex:
		// Responses API: input_tokens includes cached_tokens.
		ev.CacheRead = max(d.CacheReadTokens, d.CachedTokens)
		ev.CacheWrite = d.CacheCreationTokens
		ev.UncachedInput = max(d.InputTokens-ev.CacheRead-ev.CacheWrite, 0)
	}

	ev.IsFast = isFastTier(rec.ServiceTier) || isFastTier(rec.ResponseServiceTier)
	ev.IsLong = ev.ContextTokens() > longContextTokens

	hasTokens := ev.ContextTokens()+ev.Output > 0
	if !hasTokens {
		ev.Flags |= domain.FlagZeroTokens
	}
	if rec.Failed && hasTokens {
		ev.Flags |= domain.FlagFailedWithTokens
	}

	if len(rec.ResponseHeaders) > 0 {
		if b, err := json.Marshal(rec.ResponseHeaders); err == nil {
			ev.HeadersJSON = b
		}
	}

	if usd, usd1h, hash, ok := pricer.Price(&ev); ok {
		ev.APIUSD, ev.APIUSDCacheWrite1h, ev.PriceHash = usd, usd1h, hash
	}

	res := Result{Event: ev, Provider: provider}
	switch provider {
	case domain.ProviderClaude:
		res.Readings, res.Extras = claudeReadings(rec.ResponseHeaders, ev.ObservedAt)
	case domain.ProviderCodex:
		res.Readings, res.Extras = codexReadings(rec.ResponseHeaders, ev.ObservedAt)
	}
	return res, true
}

// DetectProvider maps the host's provider / executor naming onto ours.
// "openai-compatibility" style executors are third-party gateways whose
// headers and token semantics are not Codex's, so they are rejected.
func DetectProvider(rec domain.UsageRecord) (domain.Provider, bool) {
	for _, name := range []string{rec.Provider, rec.ExecutorType} {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if strings.Contains(name, "claude") || strings.Contains(name, "anthropic") {
			return domain.ProviderClaude, true
		}
		if strings.Contains(name, "openai-compatib") {
			continue
		}
		if strings.Contains(name, "codex") || strings.Contains(name, "openai") {
			return domain.ProviderCodex, true
		}
	}
	return "", false
}

// observedAt is when the upstream response headers were seen: first byte if
// the host measured it, else end of the request, else the request instant.
func observedAt(rec domain.UsageRecord) time.Time {
	switch {
	case rec.TTFT > 0:
		return rec.RequestedAt.Add(rec.TTFT)
	case rec.Latency > 0:
		return rec.RequestedAt.Add(rec.Latency)
	}
	return rec.RequestedAt
}

func isFastTier(tier string) bool {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "priority", "fast":
		return true
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// familyRule rewrites a lowercased model id into its price family. Rules are
// tried in order; the first match wins and its replacement (with $n groups)
// is the family. A model matching no rule keeps its id.
type familyRule struct {
	re   *regexp.Regexp
	repl string
}

// familyRules collapses dates and minor versions so that price snapshots and
// weight fits key on the tier the vendor prices, not on the checkpoint.
//
//	claude-opus-5-5-20260301   -> claude-opus-5
//	claude-sonnet-4-5-20250929 -> claude-sonnet-4
//	claude-fable-5-1           -> claude-fable-5
//	claude-3-5-haiku-20241022  -> claude-haiku-3   (legacy ordering)
//	gpt-5.6-sol-2026-03-01     -> gpt-5.6-sol
//	gpt-6-astra-latest         -> gpt-6-astra
//	codex-spark                -> codex-spark
var familyRules = map[domain.Provider][]familyRule{
	domain.ProviderClaude: {
		{regexp.MustCompile(`^(claude-(?:opus|sonnet|haiku|fable))-(\d+)(?:[-.].*)?$`), "$1-$2"},
		{regexp.MustCompile(`^(claude-(?:opus|sonnet|haiku|fable))(?:-.*)?$`), "$1"},
		{regexp.MustCompile(`^claude-(\d+)(?:-\d+)?-(opus|sonnet|haiku)(?:-.*)?$`), "claude-$2-$1"},
	},
	domain.ProviderCodex: {
		{regexp.MustCompile(`^(.+?)(?:-\d{4}-\d{2}-\d{2})?(?:-latest)?$`), "$1"},
	},
}

// ModelFamily applies familyRules for the provider. Input is lowercased and
// trimmed first so host aliases in any case map identically.
func ModelFamily(p domain.Provider, model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	for _, r := range familyRules[p] {
		if r.re.MatchString(m) {
			return r.re.ReplaceAllString(m, r.repl)
		}
	}
	return m
}

// DedupKey makes replays of the same attempt idempotent. A host RequestID is
// preferred when present, qualified by credential and outcome so a retry of
// the same execution on another account is a distinct attempt; otherwise the
// attempt's identity is the account, instant, model and token tuple.
func DedupKey(rec domain.UsageRecord) string {
	var s string
	if rec.RequestID != "" {
		s = fmt.Sprintf("rid:%s|%s|%t|%d", rec.RequestID, rec.AuthIndex, rec.Failed, rec.Failure.StatusCode)
	} else {
		d := rec.Detail
		s = fmt.Sprintf("%s|%s|%d|%s|%d|%d|%d|%d|%t|%d",
			rec.AuthID, rec.AuthIndex, rec.RequestedAt.UnixNano(), rec.Model,
			d.InputTokens, d.OutputTokens, d.CacheReadTokens, d.CacheCreationTokens,
			rec.Failed, rec.Failure.StatusCode)
	}
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
