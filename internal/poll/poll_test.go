package poll

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

const (
	claudeToken = "sk-ant-oat01-SECRET-claude-token"
	codexToken  = "eyJ.SECRET-codex-token.sig"
)

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fakeHost struct {
	files     []abi.AuthFileEntry
	auths     map[string]string
	responses map[string]abi.HTTPResponse
	reqs      []abi.HTTPRequest
	listCalls int
}

func (f *fakeHost) AuthList() ([]abi.AuthFileEntry, error) {
	f.listCalls++
	return f.files, nil
}

func (f *fakeHost) AuthGet(idx string) (abi.AuthGetResponse, error) {
	j, ok := f.auths[idx]
	if !ok {
		return abi.AuthGetResponse{}, errors.New("not found")
	}
	return abi.AuthGetResponse{AuthIndex: idx, JSON: []byte(j)}, nil
}

func (f *fakeHost) HTTPDo(r abi.HTTPRequest) (abi.HTTPResponse, error) {
	f.reqs = append(f.reqs, r)
	resp, ok := f.responses[r.URL]
	if !ok {
		return abi.HTTPResponse{}, errors.New("no route")
	}
	return resp, nil
}

type row struct {
	Key    string
	Frac   float64
	Raw    string
	DP     int
	Reset  time.Time
	Win    int64
	Status string
}

func rows(rs []domain.MeterReading, now time.Time, t *testing.T) []row {
	t.Helper()
	out := make([]row, len(rs))
	for i, r := range rs {
		if r.Source != domain.SourcePoll || !r.ObservedAt.Equal(now) {
			t.Errorf("%s: source %q observed %v", r.MeterKey, r.Source, r.ObservedAt)
		}
		out[i] = row{r.MeterKey, r.UsedFraction, r.Raw, r.PrecisionDP, r.ResetAt, r.WindowSec, r.Status}
	}
	return out
}

func iso(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestFetchClaude(t *testing.T) {
	h := &fakeHost{responses: map[string]abi.HTTPResponse{ClaudeUsageURL: {StatusCode: 200, Body: fixture(t, "claude_usage.json")}}}
	rs, raw, err := FetchClaude(context.Background(), h, claudeToken, t0)
	if err != nil {
		t.Fatal(err)
	}
	wk := iso("2026-10-01T04:00:00.412876Z")
	want := []row{
		{"5h", 0.42, "42", 2, iso("2026-09-26T17:00:00.412855Z"), 18000, ""},
		{"7d", 0.18, "18", 2, wk, 604800, ""},
		{"weekly_scoped:fable", 0.07, "7", 2, wk, 604800, ""},
		{"weekly_scoped:sonnet-4-5", 0, "0", 2, time.Time{}, 604800, ""},
	}
	if got := rows(rs, t0, t); !reflect.DeepEqual(got, want) {
		t.Errorf("readings\n got %+v\nwant %+v", got, want)
	}
	if raw.StatusCode != 200 || string(raw.Body) != string(fixture(t, "claude_usage.json")) {
		t.Errorf("raw = %d %q", raw.StatusCode, raw.Body)
	}
	req := h.reqs[0]
	wantHeaders := http.Header{
		"Authorization":  {"Bearer " + claudeToken},
		"Accept":         {"application/json"},
		"anthropic-beta": {"oauth-2025-04-20"},
		"User-Agent":     {"claude-code/2.1.0"},
	}
	if req.Method != "GET" || req.URL != "https://api.anthropic.com/api/oauth/usage" || !reflect.DeepEqual(req.Headers, wantHeaders) {
		t.Errorf("request = %s %s %v", req.Method, req.URL, req.Headers)
	}
}

func TestFetchClaudeMigratedShape(t *testing.T) {
	h := &fakeHost{responses: map[string]abi.HTTPResponse{ClaudeUsageURL: {StatusCode: 200, Body: fixture(t, "claude_usage_migrated.json")}}}
	rs, _, err := FetchClaude(context.Background(), h, claudeToken, t0)
	if err != nil {
		t.Fatal(err)
	}
	want := []row{
		{"5h", 0.615, "61.5", 3, iso("2026-09-26T15:10:00Z"), 18000, ""},
		{"7d_oi", 1.125, "112.5", 3, iso("2026-09-30T18:00:00Z"), 604800, ""},
		{"weekly_scoped:opus", 0.09, "9", 2, iso("2026-10-01T04:00:00Z"), 604800, ""},
	}
	if got := rows(rs, t0, t); !reflect.DeepEqual(got, want) {
		t.Errorf("readings\n got %+v\nwant %+v", got, want)
	}
}

func TestFetchCodex(t *testing.T) {
	h := &fakeHost{responses: map[string]abi.HTTPResponse{CodexUsageURL: {StatusCode: 200, Body: fixture(t, "codex_usage.json")}}}
	rs, raw, err := FetchCodex(context.Background(), h, codexToken, "acct-123", t0)
	if err != nil {
		t.Fatal(err)
	}
	want := []row{
		{"5h", 0.37, "37", 2, time.Unix(1790000000, 0).UTC(), 18000, ""},
		{"7d", 0.12, "12", 2, time.Unix(1790400000, 0).UTC(), 604800, ""},
		{"additional:code-review", 0, "0", 2, time.Unix(1790604800, 0).UTC(), 604800, ""},
		{"additional:codex_bengalfox:5h", 1, "100", 2, time.Unix(1790001000, 0).UTC(), 18000, "rejected"},
		{"additional:codex_bengalfox:7d", 0.45, "45", 2, time.Unix(1790400000, 0).UTC(), 604800, "rejected"},
	}
	if got := rows(rs, t0, t); !reflect.DeepEqual(got, want) {
		t.Errorf("readings\n got %+v\nwant %+v", got, want)
	}
	if raw.PlanType != "pro" {
		t.Errorf("plan = %q", raw.PlanType)
	}
	wantHeaders := http.Header{
		"Authorization":      {"Bearer " + codexToken},
		"Accept":             {"application/json"},
		"Chatgpt-Account-Id": {"acct-123"},
		"User-Agent":         {"codex_cli_rs/0.76.0 (linux; amd64)"},
	}
	req := h.reqs[0]
	if req.Method != "GET" || req.URL != "https://chatgpt.com/backend-api/wham/usage" || !reflect.DeepEqual(req.Headers, wantHeaders) {
		t.Errorf("request = %s %s %v", req.Method, req.URL, req.Headers)
	}
}

func TestFetchCodexCamelCaseAndOddWindow(t *testing.T) {
	body := `{"planType":"plus","rateLimit":{"primaryWindow":{"usedPercent":"55","limitWindowSeconds":2592000,"resetAfterSeconds":60},
		"secondaryWindow":{"usedPercent":8.5,"limitWindowSeconds":604800}}}`
	h := &fakeHost{responses: map[string]abi.HTTPResponse{CodexUsageURL: {StatusCode: 200, Body: []byte(body)}}}
	rs, raw, err := FetchCodex(context.Background(), h, codexToken, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	want := []row{
		{"7d", 0.085, "8.5", 3, time.Time{}, 604800, ""},
		{"win:2592000", 0.55, "55", 2, t0.Add(time.Minute), 2592000, ""},
	}
	if got := rows(rs, t0, t); !reflect.DeepEqual(got, want) {
		t.Errorf("readings\n got %+v\nwant %+v", got, want)
	}
	if raw.PlanType != "plus" {
		t.Errorf("plan = %q", raw.PlanType)
	}
	if _, ok := h.reqs[0].Headers["Chatgpt-Account-Id"]; ok {
		t.Error("empty account id must not be sent")
	}
}

func TestFetchStatusClasses(t *testing.T) {
	cases := []struct {
		status       int
		header       http.Header
		backoff, uat bool
		retry        time.Duration
	}{
		{429, http.Header{"retry-after": {"120"}}, true, false, 2 * time.Minute},
		{503, nil, true, false, 0},
		{401, nil, false, true, 0},
		{403, nil, false, true, 0},
		{404, nil, false, false, 0},
	}
	for _, c := range cases {
		h := &fakeHost{responses: map[string]abi.HTTPResponse{ClaudeUsageURL: {StatusCode: c.status, Headers: c.header, Body: []byte(`{"error":"x"}`)}}}
		_, raw, err := FetchClaude(context.Background(), h, claudeToken, t0)
		var se *StatusError
		if !errors.As(err, &se) || se.StatusCode != c.status || se.RetryAfter != c.retry {
			t.Errorf("%d: err = %#v", c.status, err)
		}
		if errors.Is(err, ErrBackoff) != c.backoff || errors.Is(err, ErrUnauthorized) != c.uat {
			t.Errorf("%d: backoff=%v unauthorized=%v", c.status, errors.Is(err, ErrBackoff), errors.Is(err, ErrUnauthorized))
		}
		if raw.StatusCode != c.status || string(raw.Body) != `{"error":"x"}` {
			t.Errorf("%d: raw = %+v", c.status, raw)
		}
		if strings.Contains(err.Error(), claudeToken) {
			t.Errorf("%d: token in error", c.status)
		}
	}
}

func TestRawBodyCapped(t *testing.T) {
	big := make([]byte, MaxRawBody+10)
	h := &fakeHost{responses: map[string]abi.HTTPResponse{ClaudeUsageURL: {StatusCode: 500, Body: big}}}
	_, raw, _ := FetchClaude(context.Background(), h, claudeToken, t0)
	if len(raw.Body) != MaxRawBody || !raw.Truncated {
		t.Errorf("len %d truncated %v", len(raw.Body), raw.Truncated)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Fable":            "fable",
		"Sonnet 4.5":       "sonnet-4-5",
		"Claude 3.5 Fable": "claude-3-5-fable",
		" Opus  4.1 ":      "opus-4-1",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func jwt(claims string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".s"
}

func TestParseCredential(t *testing.T) {
	c := parseCredential([]byte(`{"type":"codex","access_token":"at","email":"a@b.c",
		"id_token":"` + jwt(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-jwt"}}`) + `"}`))
	if c.accessToken != "at" || c.accountID != "acct-jwt" || c.email != "a@b.c" {
		t.Errorf("%+v", c)
	}
	c = parseCredential([]byte(`{"token":{"access_token":"nested"},"account_id":"acct-direct"}`))
	if c.accessToken != "nested" || c.accountID != "acct-direct" {
		t.Errorf("%+v", c)
	}
}

// harness wires a Poller to a temp store, a fake host, a fixed clock and a
// log capture.
type harness struct {
	st    *store.Store
	host  *fakeHost
	p     *Poller
	clock time.Time
	logs  []string
}

func newHarness(t *testing.T, interval time.Duration) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "poll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{st: st, clock: t0, host: &fakeHost{
		files: []abi.AuthFileEntry{
			{AuthIndex: "c1", Name: "claude-me.json", Provider: "claude", Email: "me@example.com"},
			{AuthIndex: "x1", Name: "codex-me.json", Provider: "codex", Email: "me@chatgpt.example"},
		},
		auths: map[string]string{
			"c1": `{"type":"claude","access_token":"` + claudeToken + `","refresh_token":"rt-SECRET","email":"me@example.com"}`,
			"x1": `{"type":"codex","access_token":"` + codexToken + `","refresh_token":"rt-SECRET","account_id":"acct-9"}`,
		},
		responses: map[string]abi.HTTPResponse{},
	}}
	h.p = h.newPoller(interval)
	return h
}

func (h *harness) newPoller(interval time.Duration) *Poller {
	p := New(h.st, h.host, func(level, msg string, fields map[string]any) {
		h.logs = append(h.logs, fmt.Sprintf("%s %s %v", level, msg, fields))
	}, interval)
	p.now = func() time.Time { return h.clock }
	p.jitter = func(d time.Duration) time.Duration { return d }
	return p
}

func (h *harness) account(t *testing.T, p domain.Provider, idx string) domain.Account {
	t.Helper()
	a, err := h.st.UpsertAccount(domain.Account{Provider: p, AuthIndex: idx, AuthID: idx + ".json", LastSeen: t0.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (h *harness) status(t *testing.T, idx string) Status {
	t.Helper()
	var s Status
	if ok, err := h.st.GetSetting(SettingKey(idx), &s); err != nil || !ok {
		t.Fatalf("status %s: ok=%v err=%v", idx, ok, err)
	}
	return s
}

// assertNoSecrets checks every log line and every stored setting.
func (h *harness) assertNoSecrets(t *testing.T) {
	t.Helper()
	for _, l := range h.logs {
		for _, s := range []string{claudeToken, codexToken, "rt-SECRET", "SECRET"} {
			if strings.Contains(l, s) {
				t.Errorf("secret %q in log %q", s, l)
			}
		}
	}
	rows, err := h.st.DB().Query(`SELECT key, value_json FROM settings`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(v, "SECRET") {
			t.Errorf("secret in setting %s: %s", k, v)
		}
	}
}

func TestPollOnceStoresReadingsAndStatus(t *testing.T) {
	h := newHarness(t, 20*time.Minute)
	claude := h.account(t, domain.ProviderClaude, "c1")
	codex := h.account(t, domain.ProviderCodex, "x1")
	h.account(t, domain.Provider("gemini"), "g1")
	h.host.responses[ClaudeUsageURL] = abi.HTTPResponse{StatusCode: 200, Body: fixture(t, "claude_usage.json")}
	h.host.responses[CodexUsageURL] = abi.HTTPResponse{StatusCode: 200, Body: fixture(t, "codex_usage.json")}

	sum := h.p.PollOnce(context.Background())
	if sum.Polled != 2 || sum.OK != 2 || sum.Readings != 9 {
		t.Fatalf("summary %+v", sum)
	}
	if h.host.listCalls != 1 {
		t.Errorf("AuthList calls = %d", h.host.listCalls)
	}
	for acct, n := range map[int64]int{claude.ID: 4, codex.ID: 5} {
		rs, err := h.st.ListReadings(store.ReadingQuery{AccountID: acct, Source: domain.SourcePoll})
		if err != nil || len(rs) != n {
			t.Fatalf("account %d: %d readings, err %v", acct, len(rs), err)
		}
	}
	five, _ := h.st.ListReadings(store.ReadingQuery{AccountID: claude.ID, MeterKey: "5h"})
	if len(five) != 1 || five[0].UsedFraction != 0.42 || five[0].ResetAt.Unix() != iso("2026-09-26T17:00:00Z").Unix() {
		t.Errorf("claude 5h = %+v", five)
	}

	s := h.status(t, "x1")
	if !s.OK || s.Readings != 5 || s.HTTPStatus != 200 || !s.At.Equal(t0) || !s.NextAt.Equal(t0.Add(20*time.Minute)) {
		t.Errorf("codex status %+v", s)
	}
	var raw map[string]any
	if ok, _ := h.st.GetSetting(RawSettingKey("c1"), &raw); !ok || raw["body"].(map[string]any)["five_hour"] == nil {
		t.Errorf("raw body not recorded: %v", raw)
	}

	accts, _ := h.st.ListAccounts()
	for _, a := range accts {
		switch a.AuthIndex {
		case "c1":
			if a.Email != "me@example.com" || !a.LastSeen.Equal(t0.Add(-time.Hour)) {
				t.Errorf("claude account %+v", a)
			}
		case "x1":
			if a.Email != "me@chatgpt.example" || a.PlanType != "pro" {
				t.Errorf("codex account %+v", a)
			}
		}
	}

	// Not due yet: nothing polled, and a restarted poller honours the stored schedule.
	h.clock = t0.Add(10 * time.Minute)
	if sum := h.p.PollOnce(context.Background()); sum.Polled != 0 {
		t.Errorf("early poll %+v", sum)
	}
	if sum := h.newPoller(20 * time.Minute).PollOnce(context.Background()); sum.Polled != 0 {
		t.Errorf("restarted poller polled early %+v", sum)
	}
	h.clock = t0.Add(20 * time.Minute)
	if sum := h.p.PollOnce(context.Background()); sum.Polled != 2 {
		t.Errorf("due poll %+v", sum)
	}
	h.assertNoSecrets(t)
}

func TestBackoffDoublesAndResets(t *testing.T) {
	iv := 20 * time.Minute
	h := newHarness(t, iv)
	h.account(t, domain.ProviderClaude, "c1")
	h.host.responses[ClaudeUsageURL] = abi.HTTPResponse{StatusCode: 429, Body: []byte(`{"error":{"type":"rate_limit_error"}}`)}

	want := []time.Duration{2 * iv, 4 * iv, 8 * iv, 16 * iv, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		sum := h.p.PollOnce(context.Background())
		if sum.Polled != 1 || sum.Failed != 1 {
			t.Fatalf("round %d: %+v", i, sum)
		}
		if got := h.p.Delay("c1"); got != w {
			t.Errorf("round %d: delay %v, want %v", i, got, w)
		}
		s := h.status(t, "c1")
		if s.OK || s.HTTPStatus != 429 || s.Error != "upstream HTTP 429" || !s.NextAt.Equal(h.clock.Add(w)) {
			t.Errorf("round %d: status %+v", i, s)
		}
		// Just before the delay elapses the account is skipped.
		h.clock = h.clock.Add(w - time.Second)
		if sum := h.p.PollOnce(context.Background()); sum.Polled != 0 {
			t.Errorf("round %d: polled during backoff", i)
		}
		h.clock = h.clock.Add(time.Second)
	}

	h.host.responses[ClaudeUsageURL] = abi.HTTPResponse{StatusCode: 200, Body: fixture(t, "claude_usage.json")}
	if sum := h.p.PollOnce(context.Background()); sum.OK != 1 {
		t.Fatalf("recovery %+v", sum)
	}
	if got := h.p.Delay("c1"); got != iv {
		t.Errorf("delay after success %v", got)
	}
	h.assertNoSecrets(t)
}

func TestUnauthorizedLogsHourly(t *testing.T) {
	iv := 20 * time.Minute
	h := newHarness(t, iv)
	h.account(t, domain.ProviderClaude, "c1")
	h.host.responses[ClaudeUsageURL] = abi.HTTPResponse{StatusCode: 401, Body: []byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired."}}`)}

	warnings := func() int {
		n := 0
		for _, l := range h.logs {
			if strings.Contains(l, "access token rejected") {
				n++
			}
		}
		return n
	}
	for i := range 4 { // t0, +20m, +40m, +60m
		sum := h.p.PollOnce(context.Background())
		if sum.Polled != 1 || sum.Failed != 1 || sum.Readings != 0 {
			t.Fatalf("round %d: %+v", i, sum)
		}
		if h.p.Delay("c1") != iv {
			t.Errorf("round %d: 401 must not back off, delay %v", i, h.p.Delay("c1"))
		}
		h.clock = h.clock.Add(iv)
	}
	if n := warnings(); n != 2 {
		t.Errorf("unauthorized warnings = %d, want 2 (t0 and t0+60m)\n%s", n, strings.Join(h.logs, "\n"))
	}
	if s := h.status(t, "c1"); s.OK || s.HTTPStatus != 401 {
		t.Errorf("status %+v", s)
	}
	// Token is re-read from the host every poll, never cached or refreshed.
	for _, r := range h.host.reqs {
		if r.Method != http.MethodGet || r.URL != ClaudeUsageURL {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}
	h.assertNoSecrets(t)
}

func TestMissingTokenRecordsError(t *testing.T) {
	h := newHarness(t, 20*time.Minute)
	h.account(t, domain.ProviderClaude, "c1")
	h.host.auths["c1"] = `{"type":"claude","email":"me@example.com"}`
	sum := h.p.PollOnce(context.Background())
	if sum.Failed != 1 || len(h.host.reqs) != 0 {
		t.Fatalf("%+v reqs=%d", sum, len(h.host.reqs))
	}
	if s := h.status(t, "c1"); s.OK || s.Error != "auth file has no access_token" {
		t.Errorf("status %+v", s)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	h := newHarness(t, 20*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.p.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
