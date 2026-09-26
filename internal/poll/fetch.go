// Package poll reads the providers' own usage endpoints (Claude oauth/usage,
// Codex wham/usage) with the credential CPA already holds, and turns them into
// MeterReading rows with source=poll. It is read-only: tokens are fetched via
// host.auth.get for each poll, never refreshed, stored or logged.
package poll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// Endpoint URLs and request headers are copied from
// giovannirco/cpa-prometheus-plugin internal/quota/fetch.go (providerRequest).
const (
	ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	CodexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"

	claudeUserAgent = "claude-code/2.1.0"
	codexUserAgent  = "codex_cli_rs/0.76.0 (linux; amd64)"
)

// MaxRawBody caps the body kept in RawResult.
const MaxRawBody = 64 << 10

const (
	window5h = 5 * 3600
	window7d = 7 * 86400
)

// ErrBackoff marks responses the scheduler must answer by slowing down
// (429, 5xx). ErrUnauthorized marks a rejected access token (401/403); the
// fix is CPA refreshing the credential, never us.
var (
	ErrBackoff      = errors.New("poll: upstream asked to back off")
	ErrUnauthorized = errors.New("poll: access token rejected")
)

// StatusError is a non-2xx upstream answer. errors.Is matches ErrBackoff or
// ErrUnauthorized when the status belongs to that class.
type StatusError struct {
	StatusCode int
	// RetryAfter is the upstream Retry-After hint, zero when absent.
	RetryAfter time.Duration
	kind       error
}

func (e *StatusError) Error() string { return fmt.Sprintf("upstream HTTP %d", e.StatusCode) }

func (e *StatusError) Unwrap() error { return e.kind }

// RawResult is the upstream answer kept for debugging; it never contains the
// request (and therefore never the token).
type RawResult struct {
	URL        string `json:"url"`
	StatusCode int    `json:"status_code"`
	Body       []byte `json:"-"`
	Truncated  bool   `json:"truncated,omitempty"`
	// PlanType is reported by Codex only.
	PlanType string `json:"plan_type,omitempty"`
}

// Doer performs one upstream request; *abi.Host satisfies it.
type Doer interface {
	HTTPDo(abi.HTTPRequest) (abi.HTTPResponse, error)
}

// FetchClaude polls oauth/usage.
func FetchClaude(ctx context.Context, doer Doer, accessToken string, now time.Time) ([]domain.MeterReading, RawResult, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Accept", "application/json")
	h["anthropic-beta"] = []string{"oauth-2025-04-20"}
	h.Set("User-Agent", claudeUserAgent)
	doc, raw, err := get(ctx, doer, ClaudeUsageURL, h)
	if err != nil {
		return nil, raw, err
	}
	return parseClaude(doc, now), raw, nil
}

// FetchCodex polls wham/usage. accountID is sent as Chatgpt-Account-Id when
// non-empty (the endpoint also answers without it).
func FetchCodex(ctx context.Context, doer Doer, accessToken, accountID string, now time.Time) ([]domain.MeterReading, RawResult, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Accept", "application/json")
	if accountID != "" {
		h.Set("Chatgpt-Account-Id", accountID)
	}
	h.Set("User-Agent", codexUserAgent)
	doc, raw, err := get(ctx, doer, CodexUsageURL, h)
	if err != nil {
		return nil, raw, err
	}
	readings, plan := parseCodex(doc, now)
	raw.PlanType = plan
	return readings, raw, nil
}

func get(ctx context.Context, doer Doer, url string, h http.Header) (map[string]any, RawResult, error) {
	raw := RawResult{URL: url}
	if err := ctx.Err(); err != nil {
		return nil, raw, err
	}
	resp, err := doer.HTTPDo(abi.HTTPRequest{Method: http.MethodGet, URL: url, Headers: h})
	if err != nil {
		return nil, raw, fmt.Errorf("poll: %s: %w", url, err)
	}
	raw.StatusCode = resp.StatusCode
	raw.Body = resp.Body
	if len(raw.Body) > MaxRawBody {
		raw.Body = raw.Body[:MaxRawBody]
		raw.Truncated = true
	}
	switch s := resp.StatusCode; {
	case s == http.StatusTooManyRequests || s >= 500:
		return nil, raw, &StatusError{StatusCode: s, RetryAfter: retryAfter(resp.Headers), kind: ErrBackoff}
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return nil, raw, &StatusError{StatusCode: s, kind: ErrUnauthorized}
	case s < 200 || s >= 300:
		return nil, raw, &StatusError{StatusCode: s}
	}
	dec := json.NewDecoder(bytes.NewReader(resp.Body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, raw, fmt.Errorf("poll: decode %s: %w", url, err)
	}
	return doc, raw, nil
}

func retryAfter(h http.Header) time.Duration {
	for k, vs := range h {
		if !strings.EqualFold(k, "Retry-After") || len(vs) == 0 {
			continue
		}
		v := strings.TrimSpace(vs[0])
		if s, err := strconv.Atoi(v); err == nil && s > 0 {
			return time.Duration(s) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 0
}

// parseClaude maps the flat windows and limits[]. Flat five_hour/seven_day
// win; limits[] kind session/weekly_all back them up on migrated accounts.
// weekly_scoped entries are authoritative for per-model meters; the legacy
// seven_day_<model> buckets (usually null now) only fill gaps.
func parseClaude(doc map[string]any, now time.Time) []domain.MeterReading {
	out := map[string]domain.MeterReading{}
	add := func(key string, win int64, pct any, reset any) {
		if _, dup := out[key]; dup {
			return
		}
		if r, ok := reading(key, win, pct, reset, now); ok {
			out[key] = r
		}
	}
	flat := func(key string) (any, any, bool) {
		w := obj(doc, key)
		if w == nil {
			return nil, nil, false
		}
		return field(w, "utilization", "percent"), field(w, "resets_at", "resetsAt"), true
	}
	limits := list(doc, "limits")
	limitOf := func(kind string) map[string]any {
		for _, l := range limits {
			if strings.EqualFold(str(l, "kind"), kind) && !placeholder(l) {
				return l
			}
		}
		return nil
	}

	for _, m := range []struct {
		key, flat, kind string
		win             int64
	}{
		{domain.MeterFiveHour, "five_hour", "session", window5h},
		{domain.MeterSevenDay, "seven_day", "weekly_all", window7d},
	} {
		if pct, reset, ok := flat(m.flat); ok && pct != nil {
			add(m.key, m.win, pct, reset)
		} else if l := limitOf(m.kind); l != nil {
			add(m.key, m.win, field(l, "percent", "utilization"), field(l, "resets_at", "resetsAt"))
		}
	}
	for _, k := range []string{"seven_day_oi", "7d_oi"} {
		if pct, reset, ok := flat(k); ok {
			add(domain.MeterSevenDayOI, window7d, pct, reset)
		}
	}

	// Prefer the active entry when a model appears twice.
	scoped := make([]map[string]any, 0, len(limits))
	for _, l := range limits {
		if strings.EqualFold(str(l, "kind"), "weekly_scoped") {
			scoped = append(scoped, l)
		}
	}
	sort.SliceStable(scoped, func(i, j int) bool { return truthy(scoped[i]["is_active"]) && !truthy(scoped[j]["is_active"]) })
	for _, l := range scoped {
		model := obj(obj(l, "scope"), "model")
		name := str(model, "display_name")
		if name == "" {
			name = str(model, "id")
		}
		if name == "" {
			name = str(l, "group")
		}
		if s := slug(name); s != "" {
			add("weekly_scoped:"+s, window7d, field(l, "percent", "utilization"), field(l, "resets_at", "resetsAt"))
		}
	}
	for _, legacy := range []struct{ key, model string }{
		{"seven_day_opus", "opus"}, {"seven_day_sonnet", "sonnet"}, {"iguana_necktie", "fable"},
	} {
		pct, reset, ok := flat(legacy.key)
		if !ok || hasScoped(out, legacy.model) {
			continue
		}
		add("weekly_scoped:"+legacy.model, window7d, pct, reset)
	}
	return sorted(out)
}

// placeholder mirrors ccstatusline: a non-model limits[] entry at 0% with no
// reset is not a live window.
func placeholder(l map[string]any) bool {
	if obj(obj(l, "scope"), "model") != nil {
		return false
	}
	_, f, ok := number(field(l, "percent", "utilization"))
	return (!ok || f == 0) && field(l, "resets_at", "resetsAt") == nil
}

func hasScoped(out map[string]domain.MeterReading, model string) bool {
	for k := range out {
		if s, ok := strings.CutPrefix(k, "weekly_scoped:"); ok && strings.Contains(s, model) {
			return true
		}
	}
	return false
}

// parseCodex keys account-wide windows by length, like the header adapter;
// named limits become additional:<name>, suffixed with the window label when
// one limit carries two windows.
func parseCodex(doc map[string]any, now time.Time) ([]domain.MeterReading, string) {
	plan := str(doc, "plan_type")
	if plan == "" {
		plan = str(doc, "planType")
	}
	out := map[string]domain.MeterReading{}
	emit := func(name string, rl map[string]any) {
		if rl == nil {
			return
		}
		status := ""
		if truthy(field(rl, "limit_reached", "limitReached")) {
			status = "rejected"
		}
		var ws []map[string]any
		for _, pair := range [][2]string{{"primary_window", "primaryWindow"}, {"secondary_window", "secondaryWindow"}} {
			if w := objAny(rl, pair[0], pair[1]); w != nil {
				ws = append(ws, w)
			}
		}
		for _, w := range ws {
			_, secs, ok := number(field(w, "limit_window_seconds", "limitWindowSeconds"))
			if !ok || secs <= 0 {
				continue
			}
			label := windowKey(int64(secs))
			key := label
			if name != "" {
				key = "additional:" + name
				if len(ws) > 1 {
					key += ":" + label
				}
			}
			if _, dup := out[key]; dup {
				continue
			}
			reset := field(w, "reset_at", "resetAt")
			if reset == nil {
				if _, after, ok := number(field(w, "reset_after_seconds", "resetAfterSeconds")); ok && after >= 0 {
					reset = now.Add(time.Duration(after * float64(time.Second)))
				}
			}
			r, ok := reading(key, int64(secs), field(w, "used_percent", "usedPercent"), reset, now)
			if !ok {
				continue
			}
			r.Status = status
			out[key] = r
		}
	}
	emit("", objAny(doc, "rate_limit", "rateLimit"))
	emit("code-review", objAny(doc, "code_review_rate_limit", "codeReviewRateLimit"))
	for _, l := range listAny(doc, "additional_rate_limits", "additionalRateLimits") {
		name := str(l, "limit_name")
		if name == "" {
			name = str(l, "limitName")
		}
		if name == "" {
			name = str(l, "metered_feature")
		}
		if name != "" {
			emit(name, objAny(l, "rate_limit", "rateLimit"))
		}
	}
	return sorted(out), plan
}

func windowKey(secs int64) string {
	switch secs {
	case window5h:
		return domain.MeterFiveHour
	case window7d:
		return domain.MeterSevenDay
	}
	return "win:" + strconv.FormatInt(secs, 10)
}

// reading builds one poll reading from a 0-100 percent value.
func reading(key string, win int64, pct, reset any, now time.Time) (domain.MeterReading, bool) {
	raw, f, ok := number(pct)
	if !ok || f < 0 {
		return domain.MeterReading{}, false
	}
	return domain.MeterReading{
		MeterKey:     key,
		Source:       domain.SourcePoll,
		ObservedAt:   now,
		UsedFraction: f / 100,
		Raw:          raw,
		PrecisionDP:  decimals(raw) + 2,
		ResetAt:      resetTime(reset),
		WindowSec:    win,
	}, true
}

func sorted(m map[string]domain.MeterReading) []domain.MeterReading {
	out := make([]domain.MeterReading, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MeterKey < out[j].MeterKey })
	return out
}

var slugJunk = regexp.MustCompile(`[^a-z0-9_]+`)

// slug lowercases a model display name and turns every run of spaces, dots
// or other punctuation into one "-": "Claude 3.5 Fable" → "claude-3-5-fable".
func slug(name string) string {
	return strings.Trim(slugJunk.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-")
}

// JSON helpers over a UseNumber-decoded document.

func obj(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]any)
	return v
}

func objAny(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if v := obj(m, k); v != nil {
			return v
		}
	}
	return nil
}

func list(m map[string]any, key string) []map[string]any {
	arr, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		if o, ok := v.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

func listAny(m map[string]any, keys ...string) []map[string]any {
	for _, k := range keys {
		if l := list(m, k); len(l) > 0 {
			return l
		}
	}
	return nil
}

// field returns the first non-null value among keys.
func field(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, _ := strconv.ParseBool(t)
		return b
	case json.Number:
		f, _ := t.Float64()
		return f != 0
	}
	return false
}

// number accepts a JSON number or a numeric string and returns its spelling.
func number(v any) (string, float64, bool) {
	var raw string
	switch t := v.(type) {
	case json.Number:
		raw = t.String()
	case string:
		raw = strings.TrimSpace(t)
	default:
		return "", 0, false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return "", 0, false
	}
	return raw, f, true
}

func decimals(raw string) int {
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		raw = raw[:i]
	}
	i := strings.IndexByte(raw, '.')
	if i < 0 {
		return 0
	}
	return len(raw) - i - 1
}

// resetTime accepts RFC3339 strings, unix seconds (ms when implausibly
// large) as number or string, or a precomputed time; otherwise zero.
func resetTime(v any) time.Time {
	if t, ok := v.(time.Time); ok {
		return t.UTC()
	}
	if s, ok := v.(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
			return t.UTC()
		}
	}
	_, f, ok := number(v)
	if !ok || f <= 0 {
		return time.Time{}
	}
	if f > 1e12 {
		f /= 1000
	}
	sec := math.Floor(f)
	return time.Unix(int64(sec), int64((f-sec)*1e9)).UTC()
}
