package ingest

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// claudeWire is a UsageRecord exactly as the host marshals it: Go field
// names, RFC3339 time, durations in ns, headers as map[string][]string.
// CachedTokens mirrors CacheCreationTokens here because reads are 0.
const claudeWire = `{
  "RequestID": "req_01ABC",
  "TraceID": "trace-1",
  "Provider": "claude",
  "ExecutorType": "claude",
  "Model": "claude-opus-5-5-20260301",
  "Alias": "opus",
  "AuthID": "claude-user@example.com",
  "AuthIndex": "3",
  "AuthType": "oauth",
  "Source": "claude-code",
  "ReasoningEffort": "high",
  "ServiceTier": "priority",
  "ResponseServiceTier": "",
  "ResponseModel": "claude-opus-5-5-20260301",
  "Generate": true,
  "Stream": true,
  "RequestedAt": "2026-09-26T14:00:00Z",
  "Latency": 8500000000,
  "TTFT": 1200000000,
  "Failed": false,
  "Failure": {"StatusCode": 0, "Body": ""},
  "Detail": {
    "InputTokens": 1200,
    "OutputTokens": 900,
    "ReasoningTokens": 300,
    "CachedTokens": 45000,
    "CacheReadTokens": 0,
    "CacheCreationTokens": 45000,
    "TotalTokens": 47100
  },
  "ResponseHeaders": {
    "Anthropic-Ratelimit-Unified-5h-Status": ["allowed"],
    "Anthropic-Ratelimit-Unified-5h-Utilization": ["0.0"],
    "Anthropic-Ratelimit-Unified-5h-Reset": ["1787296800"],
    "Anthropic-Ratelimit-Unified-7d-Status": ["allowed"],
    "Anthropic-Ratelimit-Unified-7d-Utilization": ["0.53"],
    "Anthropic-Ratelimit-Unified-7d-Reset": ["1787695200"],
    "Anthropic-Ratelimit-Unified-Fallback-Percentage": ["0.5"],
    "Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": ["member_zero_credit_limit"],
    "Anthropic-Ratelimit-Unified-Overage-Status": ["rejected"],
    "Anthropic-Ratelimit-Unified-Representative-Claim": ["five_hour"],
    "Anthropic-Ratelimit-Unified-Reset": ["1787296800"],
    "Anthropic-Ratelimit-Unified-Status": ["allowed"],
    "Anthropic-Workspace-Id": ["ws-not-a-meter"],
    "Content-Type": ["application/json"]
  }
}`

func TestClaudeWireDecodeAndNormalize(t *testing.T) {
	var rec domain.UsageRecord
	if err := json.Unmarshal([]byte(claudeWire), &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.TTFT != 1200*time.Millisecond || rec.Latency != 8500*time.Millisecond {
		t.Fatalf("durations decoded as %v / %v", rec.TTFT, rec.Latency)
	}
	res, ok := Normalize(rec, nil)
	if !ok {
		t.Fatal("Normalize rejected a claude record")
	}
	ev := res.Event
	want := domain.UsageEvent{
		DedupKey:        DedupKey(rec),
		Provider:        domain.ProviderClaude,
		AuthIndex:       "3",
		AuthID:          "claude-user@example.com",
		RequestedAt:     rec.RequestedAt,
		ObservedAt:      rec.RequestedAt.Add(1200 * time.Millisecond),
		Latency:         8500 * time.Millisecond,
		Model:           "claude-opus-5-5-20260301",
		ModelFamily:     "claude-opus-5",
		Alias:           "opus",
		ServiceTier:     "priority",
		ReasoningEffort: "high",
		UncachedInput:   1200,
		CacheRead:       0,
		CacheWrite:      45000,
		Output:          900,
		Reasoning:       300,
		IsFast:          true,
		IsLong:          false,
		HeadersJSON:     ev.HeadersJSON,
		Flags:           0,
	}
	gotJ, _ := json.MarshalIndent(ev, "", " ")
	wantJ, _ := json.MarshalIndent(want, "", " ")
	if string(gotJ) != string(wantJ) {
		t.Fatalf("event mismatch\n got: %s\nwant: %s", gotJ, wantJ)
	}
	var hdr http.Header
	if err := json.Unmarshal(ev.HeadersJSON, &hdr); err != nil || hdr.Get("Anthropic-Ratelimit-Unified-7d-Utilization") != "0.53" {
		t.Fatalf("HeadersJSON did not round-trip: %v %v", err, string(ev.HeadersJSON))
	}

	if res.Provider != domain.ProviderClaude {
		t.Fatalf("Provider = %q", res.Provider)
	}
	wantEx := Extras{RepresentativeClaim: "five_hour", OverageStatus: "rejected", OverageDisabledReason: "member_zero_credit_limit", UnifiedStatus: "allowed"}
	if res.Extras != wantEx {
		t.Fatalf("Extras = %+v, want %+v", res.Extras, wantEx)
	}

	wantR := []domain.MeterReading{
		{MeterKey: "5h", Source: "header", ObservedAt: ev.ObservedAt, UsedFraction: 0, Raw: "0.0", PrecisionDP: 1, ResetAt: time.Unix(1787296800, 0), Status: "allowed", WindowSec: 18000},
		{MeterKey: "7d", Source: "header", ObservedAt: ev.ObservedAt, UsedFraction: 0.53, Raw: "0.53", PrecisionDP: 2, ResetAt: time.Unix(1787695200, 0), Status: "allowed", WindowSec: 604800},
	}
	assertReadings(t, res.Readings, wantR)
}

func TestNormalizeTable(t *testing.T) {
	at := time.Date(2026, 9, 26, 15, 0, 0, 0, time.UTC)
	type want struct {
		provider  domain.Provider
		family    string
		observed  time.Time
		uncached  int64
		cacheRead int64
		cacheWr   int64
		output    int64
		reasoning int64
		flags     int
		fast      bool
		long      bool
		status    int
		readings  []domain.MeterReading
		extras    Extras
	}
	cases := []struct {
		name string
		rec  domain.UsageRecord
		want want
	}{
		{
			name: "fable 7d and 7d_oi, overage exceeded, failed 429 without tokens",
			rec: domain.UsageRecord{
				Provider: "claude", Model: "claude-fable-5-1", AuthID: "a", AuthIndex: "1",
				RequestedAt: at, Latency: 400 * time.Millisecond,
				Failed: true, Failure: domain.UsageFailure{StatusCode: 429},
				ResponseHeaders: http.Header{
					"Anthropic-Ratelimit-Unified-Status":                  {"rejected"},
					"Anthropic-Ratelimit-Unified-Representative-Claim":    {"seven_day_overage_included"},
					"Anthropic-Ratelimit-Unified-7d-Status":               {"allowed"},
					"Anthropic-Ratelimit-Unified-7d-Utilization":          {"0.69"},
					"Anthropic-Ratelimit-Unified-7d-Reset":                {"1787695200"},
					"Anthropic-Ratelimit-Unified-5h-Utilization":          {"0.00"},
					"Anthropic-Ratelimit-Unified-7d_oi-Status":            {"rejected"},
					"Anthropic-Ratelimit-Unified-7d_oi-Utilization":       {"1.02"},
					"Anthropic-Ratelimit-Unified-7d_oi-Reset":             {"2026-10-03T15:00:00Z"},
					"Anthropic-Ratelimit-Unified-Overage-Status":          {"rejected"},
					"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"org_spend_cap_reached"},
				},
			},
			want: want{
				provider: domain.ProviderClaude, family: "claude-fable-5",
				observed: at.Add(400 * time.Millisecond), flags: domain.FlagZeroTokens, status: 429,
				readings: []domain.MeterReading{
					{MeterKey: "5h", UsedFraction: 0, Raw: "0.00", PrecisionDP: 2, WindowSec: 18000},
					{MeterKey: "7d", UsedFraction: 0.69, Raw: "0.69", PrecisionDP: 2, ResetAt: time.Unix(1787695200, 0), Status: "allowed", WindowSec: 604800},
					{MeterKey: "7d_oi", UsedFraction: 1.02, Raw: "1.02", PrecisionDP: 2, ResetAt: time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC), Status: "rejected", WindowSec: 604800},
				},
				extras: Extras{RepresentativeClaim: "seven_day_overage_included", OverageStatus: "rejected", OverageDisabledReason: "org_spend_cap_reached", UnifiedStatus: "rejected"},
			},
		},
		{
			name: "claude total mismatch, long context, long-float utilization, http-date reset",
			rec: domain.UsageRecord{
				Provider: "anthropic", ExecutorType: "claude", Model: "claude-sonnet-4-5-20250929",
				RequestedAt: at, ResponseServiceTier: "fast",
				Detail: domain.UsageDetail{InputTokens: 1000, OutputTokens: 10, CacheReadTokens: 250_000, CacheCreationTokens: 5, TotalTokens: 999},
				ResponseHeaders: http.Header{
					"anthropic-ratelimit-unified-5h-utilization": {"0.6312345678"},
					"anthropic-ratelimit-unified-5h-reset":       {"Sat, 26 Sep 2026 20:00:00 GMT"},
					"anthropic-ratelimit-unified-5h-status":      {"allowed_warning"},
				},
			},
			want: want{
				provider: domain.ProviderClaude, family: "claude-sonnet-4", observed: at,
				uncached: 1000, cacheRead: 250_000, cacheWr: 5, output: 10,
				flags: domain.FlagTotalMismatch, fast: true, long: true,
				readings: []domain.MeterReading{
					{MeterKey: "5h", UsedFraction: 0.6312345678, Raw: "0.6312345678", PrecisionDP: 10, ResetAt: time.Date(2026, 9, 26, 20, 0, 0, 0, time.UTC), Status: "allowed_warning", WindowSec: 18000},
				},
			},
		},
		{
			name: "codex http primary 5h + secondary 7d + plan type",
			rec: domain.UsageRecord{
				Provider: "codex", ExecutorType: "codex", Model: "gpt-5.6-sol-2026-03-01", AuthID: "c", AuthIndex: "2",
				RequestedAt: at, Latency: 3 * time.Second, ServiceTier: "flex",
				Detail: domain.UsageDetail{InputTokens: 20_000, OutputTokens: 1500, ReasoningTokens: 1000, CachedTokens: 18_000, CacheReadTokens: 18_000, TotalTokens: 21_500},
				ResponseHeaders: http.Header{
					"X-Codex-Plan-Type":                     {"pro"},
					"X-Codex-Primary-Used-Percent":          {"51"},
					"X-Codex-Primary-Window-Minutes":        {"300"},
					"X-Codex-Primary-Reset-After-Seconds":   {"9718"},
					"X-Codex-Primary-Reset-At":              {"1787588999"},
					"X-Codex-Secondary-Used-Percent":        {"63.5"},
					"X-Codex-Secondary-Window-Minutes":      {"10080"},
					"X-Codex-Secondary-Reset-After-Seconds": {"309718"},
					"X-Codex-Credits-Has-Credits":           {"False"},
				},
			},
			want: want{
				provider: domain.ProviderCodex, family: "gpt-5.6-sol", observed: at.Add(3 * time.Second),
				uncached: 2000, cacheRead: 18_000, output: 1500, reasoning: 1000,
				readings: []domain.MeterReading{
					{MeterKey: "5h", UsedFraction: 0.51, Raw: "51", PrecisionDP: 2, ResetAt: time.Unix(1787588999, 0), WindowSec: 18000},
					{MeterKey: "7d", UsedFraction: 0.635, Raw: "63.5", PrecisionDP: 3, ResetAt: at.Add(3*time.Second + 309718*time.Second), WindowSec: 604800},
				},
				extras: Extras{PlanType: "pro"},
			},
		},
		{
			name: "codex weekly occupies primary, legacy additional limit with secondary role",
			rec: domain.UsageRecord{
				Provider: "openai", ExecutorType: "codex", Model: "gpt-6-astra-latest",
				RequestedAt: at,
				Detail:      domain.UsageDetail{InputTokens: 100, OutputTokens: 5, CachedTokens: 120},
				ResponseHeaders: http.Header{
					"X-Codex-Plan-Type":                          {"plus"},
					"X-Codex-Primary-Used-Percent":               {"51"},
					"X-Codex-Primary-Window-Minutes":             {"10080"},
					"X-Codex-Primary-Reset-At":                   {"1787588999"},
					"X-Codex-Bengalfox-Limit-Name":               {"GPT-5.3-Codex-Spark"},
					"X-Codex-Bengalfox-Secondary-Used-Percent":   {"35"},
					"X-Codex-Bengalfox-Secondary-Window-Minutes": {"10080"},
				},
			},
			want: want{
				provider: domain.ProviderCodex, family: "gpt-6-astra", observed: at,
				uncached: 0, cacheRead: 120, output: 5,
				readings: []domain.MeterReading{
					{MeterKey: "7d", UsedFraction: 0.51, Raw: "51", PrecisionDP: 2, ResetAt: time.Unix(1787588999, 0), WindowSec: 604800},
					{MeterKey: "additional:bengalfox", UsedFraction: 0.35, Raw: "35", PrecisionDP: 2, WindowSec: 604800},
				},
				extras: Extras{PlanType: "plus"},
			},
		},
		{
			name: "codex websocket additional limit with both windows, unknown main window",
			rec: domain.UsageRecord{
				Provider: "codex", Model: "codex-spark", RequestedAt: at,
				Detail: domain.UsageDetail{InputTokens: 10, OutputTokens: 1},
				ResponseHeaders: http.Header{
					"X-Codex-Primary-Used-Percent":                                    {"12.25"},
					"X-Codex-Primary-Window-Minutes":                                  {"60"},
					"X-Codex-Additional-GPT-5.3-Codex-Spark-Primary-Used-Percent":     {"3"},
					"X-Codex-Additional-GPT-5.3-Codex-Spark-Primary-Window-Minutes":   {"300"},
					"X-Codex-Additional-GPT-5.3-Codex-Spark-Secondary-Used-Percent":   {"40"},
					"X-Codex-Additional-GPT-5.3-Codex-Spark-Secondary-Window-Minutes": {"10080"},
					"X-Codex-Additional-GPT-5.3-Codex-Spark-Limit-Name":               {"GPT-5.3-Codex-Spark"},
				},
			},
			want: want{
				provider: domain.ProviderCodex, family: "codex-spark", observed: at,
				uncached: 10, output: 1,
				readings: []domain.MeterReading{
					{MeterKey: "additional:gpt-5.3-codex-spark:5h", UsedFraction: 0.03, Raw: "3", PrecisionDP: 2, WindowSec: 18000},
					{MeterKey: "additional:gpt-5.3-codex-spark:7d", UsedFraction: 0.40, Raw: "40", PrecisionDP: 2, WindowSec: 604800},
					{MeterKey: "win:3600", UsedFraction: 0.1225, Raw: "12.25", PrecisionDP: 4, WindowSec: 3600},
				},
			},
		},
		{
			name: "failed attempt with tokens is flagged, codex never flags total mismatch",
			rec: domain.UsageRecord{
				Provider: "codex", Model: "gpt-5.6-sol", RequestedAt: at, TTFT: time.Second, Latency: 2 * time.Second,
				Failed: true, Failure: domain.UsageFailure{StatusCode: 529, Body: "overloaded"},
				Detail: domain.UsageDetail{InputTokens: 50, OutputTokens: 0, TotalTokens: 1},
			},
			want: want{
				provider: domain.ProviderCodex, family: "gpt-5.6-sol", observed: at.Add(time.Second),
				uncached: 50, flags: domain.FlagFailedWithTokens, status: 529,
			},
		},
		{
			name: "garbage utilization and reset never panic; bad meters are skipped",
			rec: domain.UsageRecord{
				Provider: "claude", Model: "claude-haiku-4-5", RequestedAt: at,
				Detail: domain.UsageDetail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
				ResponseHeaders: http.Header{
					"Anthropic-Ratelimit-Unified-5h-Utilization":     {"abc"},
					"Anthropic-Ratelimit-Unified-7d-Utilization":     {"NaN"},
					"Anthropic-Ratelimit-Unified-7d_oi-Utilization":  {"-0.5"},
					"Anthropic-Ratelimit-Unified-1d-Utilization":     {""},
					"Anthropic-Ratelimit-Unified-Weird!-Utilization": {"0.1"},
					"Anthropic-Ratelimit-Unified-30d-Utilization":    {"0.2"},
					"Anthropic-Ratelimit-Unified-30d-Reset":          {"next tuesday"},
					"Anthropic-Ratelimit-Unified-30d-Status":         {"allowed"},
				},
			},
			want: want{
				provider: domain.ProviderClaude, family: "claude-haiku-4", observed: at,
				uncached: 1, output: 1,
				readings: []domain.MeterReading{
					{MeterKey: "30d", UsedFraction: 0.2, Raw: "0.2", PrecisionDP: 1, Status: "allowed", WindowSec: 30 * 86400},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := Normalize(tc.rec, NoPricer{})
			if !ok {
				t.Fatal("Normalize returned false")
			}
			ev := res.Event
			w := tc.want
			if res.Provider != w.provider || ev.Provider != w.provider {
				t.Errorf("provider = %q/%q, want %q", res.Provider, ev.Provider, w.provider)
			}
			if ev.ModelFamily != w.family {
				t.Errorf("family = %q, want %q", ev.ModelFamily, w.family)
			}
			if !ev.ObservedAt.Equal(w.observed) {
				t.Errorf("observed = %v, want %v", ev.ObservedAt, w.observed)
			}
			got := [5]int64{ev.UncachedInput, ev.CacheRead, ev.CacheWrite, ev.Output, ev.Reasoning}
			want := [5]int64{w.uncached, w.cacheRead, w.cacheWr, w.output, w.reasoning}
			if got != want {
				t.Errorf("buckets (uncached, read, write, output, reasoning) = %v, want %v", got, want)
			}
			if ev.Flags != w.flags {
				t.Errorf("flags = %b, want %b", ev.Flags, w.flags)
			}
			if ev.IsFast != w.fast || ev.IsLong != w.long {
				t.Errorf("fast/long = %v/%v, want %v/%v", ev.IsFast, ev.IsLong, w.fast, w.long)
			}
			if ev.Failed != tc.rec.Failed || ev.StatusCode != w.status {
				t.Errorf("failed/status = %v/%d, want %v/%d", ev.Failed, ev.StatusCode, tc.rec.Failed, w.status)
			}
			if (ev.HeadersJSON == nil) != (len(tc.rec.ResponseHeaders) == 0) {
				t.Errorf("HeadersJSON presence wrong: %q", ev.HeadersJSON)
			}
			if ev.APIUSD != 0 || ev.PriceHash != "" {
				t.Errorf("NoPricer must leave price zero, got %v %q", ev.APIUSD, ev.PriceHash)
			}
			if res.Extras != w.extras {
				t.Errorf("extras = %+v, want %+v", res.Extras, w.extras)
			}
			for i := range w.readings {
				w.readings[i].Source = domain.SourceHeader
				w.readings[i].ObservedAt = w.observed
			}
			assertReadings(t, res.Readings, w.readings)
		})
	}
}

func assertReadings(t *testing.T, got, want []domain.MeterReading) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d readings, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.MeterKey != w.MeterKey || g.Source != w.Source || !g.ObservedAt.Equal(w.ObservedAt) ||
			g.UsedFraction != w.UsedFraction || g.Raw != w.Raw || g.PrecisionDP != w.PrecisionDP ||
			!g.ResetAt.Equal(w.ResetAt) || g.Status != w.Status || g.WindowSec != w.WindowSec ||
			g.EventID != nil || g.CycleID != nil {
			t.Errorf("reading %d\n got %+v\nwant %+v", i, g, w)
		}
	}
}

func TestPricerIsApplied(t *testing.T) {
	rec := domain.UsageRecord{Provider: "claude", Model: "claude-opus-5", RequestedAt: time.Now(),
		Detail: domain.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}
	res, _ := Normalize(rec, pricerFunc(func(ev *domain.UsageEvent) (float64, float64, string, bool) {
		if ev.UncachedInput != 10 {
			t.Errorf("pricer saw unfilled event: %+v", ev)
		}
		return 1.5, 2.5, "h1", true
	}))
	if res.Event.APIUSD != 1.5 || res.Event.APIUSDCacheWrite1h != 2.5 || res.Event.PriceHash != "h1" {
		t.Fatalf("price not applied: %+v", res.Event)
	}
}

type pricerFunc func(*domain.UsageEvent) (float64, float64, string, bool)

func (f pricerFunc) Price(ev *domain.UsageEvent) (float64, float64, string, bool) { return f(ev) }

func TestDetectProvider(t *testing.T) {
	cases := []struct {
		provider, executor string
		want               domain.Provider
		ok                 bool
	}{
		{"claude", "", domain.ProviderClaude, true},
		{"anthropic", "", domain.ProviderClaude, true},
		{"", "Claude", domain.ProviderClaude, true},
		{"codex", "", domain.ProviderCodex, true},
		{"openai", "openai", domain.ProviderCodex, true},
		{"", "codex", domain.ProviderCodex, true},
		{"openai-compatibility", "openai-compatibility", "", false},
		{"openai-compatible-groq", "", "", false},
		{"openai-compatible-x", "codex", domain.ProviderCodex, true},
		{"gemini", "gemini", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		got, ok := DetectProvider(domain.UsageRecord{Provider: tc.provider, ExecutorType: tc.executor})
		if got != tc.want || ok != tc.ok {
			t.Errorf("DetectProvider(%q, %q) = %q,%v want %q,%v", tc.provider, tc.executor, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := Normalize(domain.UsageRecord{Provider: "gemini"}, nil); ok {
		t.Fatal("Normalize accepted gemini")
	}
}

func TestModelFamily(t *testing.T) {
	cases := []struct {
		p     domain.Provider
		model string
		want  string
	}{
		{domain.ProviderClaude, "claude-opus-5-5-20260301", "claude-opus-5"},
		{domain.ProviderClaude, "claude-opus-5", "claude-opus-5"},
		{domain.ProviderClaude, "claude-sonnet-4-5-20250929", "claude-sonnet-4"},
		{domain.ProviderClaude, "claude-fable-5-1", "claude-fable-5"},
		{domain.ProviderClaude, "Claude-Haiku-4-5-20251001", "claude-haiku-4"},
		{domain.ProviderClaude, "claude-3-5-sonnet-20241022", "claude-sonnet-3"},
		{domain.ProviderClaude, "claude-opus-latest", "claude-opus"},
		{domain.ProviderClaude, "some-gateway-model", "some-gateway-model"},
		{domain.ProviderClaude, "", ""},
		{domain.ProviderCodex, "gpt-5.6-sol-2026-03-01", "gpt-5.6-sol"},
		{domain.ProviderCodex, "gpt-6-astra-latest", "gpt-6-astra"},
		{domain.ProviderCodex, "gpt-5.3-codex-spark", "gpt-5.3-codex-spark"},
		{domain.ProviderCodex, "codex-spark", "codex-spark"},
		{domain.ProviderCodex, "gpt-5-codex-2025-09-15-latest", "gpt-5-codex"},
	}
	for _, tc := range cases {
		if got := ModelFamily(tc.p, tc.model); got != tc.want {
			t.Errorf("ModelFamily(%s, %q) = %q, want %q", tc.p, tc.model, got, tc.want)
		}
	}
}

func TestDedupKey(t *testing.T) {
	at := time.Date(2026, 9, 26, 15, 0, 0, 123, time.UTC)
	base := domain.UsageRecord{AuthID: "a", AuthIndex: "1", RequestedAt: at, Model: "m",
		Detail: domain.UsageDetail{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4}}
	k := DedupKey(base)
	if len(k) != 40 || k != DedupKey(base) {
		t.Fatalf("unstable or malformed key %q", k)
	}
	variants := map[string]func(r *domain.UsageRecord){
		"auth":      func(r *domain.UsageRecord) { r.AuthID = "b" },
		"index":     func(r *domain.UsageRecord) { r.AuthIndex = "2" },
		"time":      func(r *domain.UsageRecord) { r.RequestedAt = at.Add(time.Nanosecond) },
		"model":     func(r *domain.UsageRecord) { r.Model = "n" },
		"input":     func(r *domain.UsageRecord) { r.Detail.InputTokens++ },
		"output":    func(r *domain.UsageRecord) { r.Detail.OutputTokens++ },
		"cacheread": func(r *domain.UsageRecord) { r.Detail.CacheReadTokens++ },
		"cachewr":   func(r *domain.UsageRecord) { r.Detail.CacheCreationTokens++ },
		"failed":    func(r *domain.UsageRecord) { r.Failed = true },
		"status":    func(r *domain.UsageRecord) { r.Failure.StatusCode = 500 },
	}
	for name, mut := range variants {
		r := base
		mut(&r)
		if DedupKey(r) == k {
			t.Errorf("changing %s did not change the key", name)
		}
	}
	// Fields outside the identity tuple do not matter.
	r := base
	r.Detail.CachedTokens = 99
	r.Latency = time.Hour
	r.ResponseHeaders = http.Header{"X": {"y"}}
	if DedupKey(r) != k {
		t.Error("non-identity fields changed the key")
	}
	// A RequestID takes over completely and is stable across the same rid.
	r = base
	r.RequestID = "req_1"
	rid := DedupKey(r)
	r.Detail.InputTokens = 999
	if DedupKey(r) != rid || rid == k {
		t.Error("RequestID path is not authoritative")
	}
	// The same key survives a JSON round trip of the record.
	b, _ := json.Marshal(base)
	var back domain.UsageRecord
	if err := json.Unmarshal(b, &back); err != nil || DedupKey(back) != k {
		t.Fatalf("key changed across JSON round trip: %v", err)
	}
}
