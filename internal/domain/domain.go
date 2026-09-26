// Package domain holds the provider-neutral types every other package speaks.
// Both provider adapters (claude, codex) must produce exactly these shapes so
// that the store, estimator and dashboard never branch on provider.
package domain

import (
	"encoding/json"
	"net/http"
	"time"
)

// Provider is the normalized provider key.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// Meter keys shared across providers. Anything else is stored but not estimated
// unless a scope is configured for it.
const (
	MeterFiveHour = "5h"
	MeterSevenDay = "7d"
	// MeterSevenDayOI is Claude's Fable-scoped "overage included" weekly meter.
	MeterSevenDayOI = "7d_oi"
)

// Reading sources.
const (
	SourceHeader = "header"
	SourcePoll   = "poll"
)

// UsageRecord mirrors pluginapi.UsageRecord on the wire. The host marshals the
// Go struct without JSON tags, so keys are Go field names. Durations are
// nanoseconds; RequestedAt is RFC3339. Body-less: only fields we consume.
type UsageRecord struct {
	RequestID           string
	TraceID             string
	Provider            string
	ExecutorType        string
	Model               string
	Alias               string
	AuthID              string
	AuthIndex           string
	AuthType            string
	Source              string
	ReasoningEffort     string
	ServiceTier         string
	ResponseServiceTier string
	ResponseModel       string
	Generate            bool
	Stream              bool
	RequestedAt         time.Time
	Latency             time.Duration
	TTFT                time.Duration
	Failed              bool
	Failure             UsageFailure
	Detail              UsageDetail
	ResponseHeaders     http.Header
}

// UsageFailure mirrors pluginapi.UsageFailure.
type UsageFailure struct {
	StatusCode int
	Body       string
}

// UsageDetail mirrors pluginapi.UsageDetail. Semantics differ per provider:
// Claude InputTokens excludes cache fields; Codex InputTokens includes cached.
type UsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// Account is one credential in CPA. AuthIndex is the stable key.
type Account struct {
	ID        int64     `json:"id"`
	Provider  Provider  `json:"provider"`
	AuthIndex string    `json:"auth_index"`
	AuthID    string    `json:"auth_id"`
	Email     string    `json:"email,omitempty"`
	PlanType  string    `json:"plan_type,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// Event flags (bitmask on UsageEvent.Flags).
const (
	FlagZeroTokens       = 1 << 0
	FlagTotalMismatch    = 1 << 1
	FlagFailedWithTokens = 1 << 2
)

// UsageEvent is one upstream attempt, normalized. Token buckets are mutually
// exclusive: uncached_input + cache_read + cache_write is the full context.
// Reasoning is informational (a subset of Output) and never priced on Claude.
type UsageEvent struct {
	ID          int64     `json:"id"`
	AccountID   int64     `json:"account_id"`
	DedupKey    string    `json:"dedup_key"`
	Provider    Provider  `json:"provider"`
	AuthIndex   string    `json:"auth_index"`
	AuthID      string    `json:"auth_id"`
	RequestedAt time.Time `json:"requested_at"`
	// ObservedAt is when the response headers were seen: RequestedAt + TTFT,
	// or + Latency when TTFT is zero. Events are ordered by this.
	ObservedAt      time.Time     `json:"observed_at"`
	Latency         time.Duration `json:"latency_ns"`
	Model           string        `json:"model"`
	ModelFamily     string        `json:"model_family"`
	Alias           string        `json:"alias,omitempty"`
	ServiceTier     string        `json:"service_tier,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	Failed          bool          `json:"failed"`
	StatusCode      int           `json:"status_code,omitempty"`
	UncachedInput   int64         `json:"uncached_input"`
	CacheRead       int64         `json:"cache_read"`
	CacheWrite      int64         `json:"cache_write"`
	Output          int64         `json:"output"`
	Reasoning       int64         `json:"reasoning"`
	IsFast          bool          `json:"is_fast"`
	IsLong          bool          `json:"is_long"`
	// APIUSD is the API list-price cost frozen at ingest under PriceHash.
	// APIUSDCacheWrite1h prices cache writes at the 1-hour rate instead.
	APIUSD             float64 `json:"api_usd"`
	APIUSDCacheWrite1h float64 `json:"api_usd_cw1h"`
	PriceHash          string  `json:"price_hash,omitempty"`
	// HeadersJSON is the raw upstream response header map, kept verbatim.
	HeadersJSON json.RawMessage `json:"headers_json,omitempty"`
	Flags       int             `json:"flags"`
}

// ContextTokens is the full prompt size.
func (e UsageEvent) ContextTokens() int64 {
	return e.UncachedInput + e.CacheRead + e.CacheWrite
}

// MeterReading is one utilization sample for one meter of one account, taken
// either from a response header (linked to an event) or from a poll.
type MeterReading struct {
	ID         int64     `json:"id"`
	AccountID  int64     `json:"account_id"`
	MeterKey   string    `json:"meter_key"`
	Source     string    `json:"source"`
	EventID    *int64    `json:"event_id,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	// UsedFraction is utilization in [0, ∞): 0.63 means 63%. Claude 7d_oi may exceed 1.
	UsedFraction float64 `json:"used_fraction"`
	// Raw is the header value as received (Claude: "0.63"; Codex: "63").
	Raw string `json:"raw"`
	// PrecisionDP is the number of decimal places the reading carries when
	// expressed as a fraction; whole-percent readings have 2.
	PrecisionDP int       `json:"precision_dp"`
	ResetAt     time.Time `json:"reset_at"`
	Status      string    `json:"status,omitempty"`
	WindowSec   int64     `json:"window_s"`
	CycleID     *int64    `json:"cycle_id,omitempty"`
}

// Tick is the utilization quantum implied by the reading's precision.
func (r MeterReading) Tick() float64 {
	t := 1.0
	for i := 0; i < r.PrecisionDP; i++ {
		t /= 10
	}
	if t < 1e-4 {
		return 1e-4
	}
	return t
}

// IsEstimable reports whether a meter is fed by all of the account's traffic
// and therefore safe to regress total spend against.
func IsEstimable(meterKey string) bool {
	return meterKey == MeterFiveHour || meterKey == MeterSevenDay
}
