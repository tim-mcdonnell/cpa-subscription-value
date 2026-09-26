package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

const testPluginID = "cpa-subscription-value"

type fakeStore struct {
	accounts     []domain.Account
	events       []domain.UsageEvent
	readings     []domain.MeterReading
	stats        Stats
	statsErr     error
	lastEventQ   EventQuery
	lastReadingQ ReadingQuery
}

func (f *fakeStore) ListAccounts() ([]domain.Account, error) { return f.accounts, nil }

func (f *fakeStore) GetAccountByAuthIndex(idx string) (domain.Account, bool, error) {
	for _, a := range f.accounts {
		if a.AuthIndex == idx {
			return a, true, nil
		}
	}
	return domain.Account{}, false, nil
}

func (f *fakeStore) ListEvents(q EventQuery) ([]domain.UsageEvent, error) {
	f.lastEventQ = q
	var out []domain.UsageEvent
	for _, e := range f.events {
		if q.AccountID != 0 && e.AccountID != q.AccountID {
			continue
		}
		if !q.From.IsZero() && e.ObservedAt.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && e.ObservedAt.After(q.To) {
			continue
		}
		out = append(out, e)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (f *fakeStore) ListReadings(q ReadingQuery) ([]domain.MeterReading, error) {
	f.lastReadingQ = q
	var out []domain.MeterReading
	for _, r := range f.readings {
		if q.AccountID != 0 && r.AccountID != q.AccountID {
			continue
		}
		if q.MeterKey != "" && r.MeterKey != q.MeterKey {
			continue
		}
		if q.Source != "" && r.Source != q.Source {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeStore) Stats() (Stats, error) { return f.stats, f.statsErr }

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func newFixture() (*Router, *fakeStore) {
	st := &fakeStore{
		accounts: []domain.Account{
			{ID: 1, Provider: domain.ProviderClaude, AuthIndex: "aaaa1111", AuthID: "claude.json", Email: "a@x"},
			{ID: 2, Provider: domain.ProviderCodex, AuthIndex: "bbbb2222", AuthID: "codex.json"},
		},
		events: []domain.UsageEvent{
			{ID: 10, AccountID: 1, AuthIndex: "aaaa1111", ObservedAt: t0, Model: "claude-opus-4"},
			{ID: 11, AccountID: 1, AuthIndex: "aaaa1111", ObservedAt: t0.Add(time.Hour), Model: "claude-sonnet-4"},
			{ID: 12, AccountID: 2, AuthIndex: "bbbb2222", ObservedAt: t0, Model: "gpt-5"},
		},
		readings: []domain.MeterReading{
			{ID: 20, AccountID: 1, MeterKey: "7d", Source: "header", ObservedAt: t0, UsedFraction: 0.5},
			{ID: 21, AccountID: 1, MeterKey: "5h", Source: "header", ObservedAt: t0, UsedFraction: 0.1},
			{ID: 22, AccountID: 2, MeterKey: "7d", Source: "poll", ObservedAt: t0, UsedFraction: 0.3},
		},
		stats: Stats{Accounts: 2, Events: 3, Readings: 3, OldestEvent: t0, NewestEvent: t0.Add(time.Hour), DBBytes: 4096},
	}
	health := func() Health {
		return Health{PluginVersion: "0.1.0-test", SchemaVersion: 2, StartedAt: t0, IngestQueued: 1, IngestDropped: 2, LastEventAt: t0.Add(time.Hour), Config: map[string]any{"data_dir": "/tmp/x"}}
	}
	return New(testPluginID, st, health), st
}

func get(r *Router, path string, q url.Values) abi.ManagementResponse {
	return r.Handle(abi.ManagementRequest{Method: "GET", Path: path, Query: q})
}

func decode(t *testing.T, resp abi.ManagementResponse) map[string]any {
	t.Helper()
	if ct := resp.Headers.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		t.Fatalf("decode body %s: %v", resp.Body, err)
	}
	return m
}

func TestRegistration(t *testing.T) {
	r, _ := newFixture()
	reg := r.Registration()
	want := map[string]bool{"/cpa-subscription-value/health": true, "/cpa-subscription-value/accounts": true, "/cpa-subscription-value/events": true, "/cpa-subscription-value/readings": true}
	for _, rt := range reg.Routes {
		if rt.Method != "GET" || !want[rt.Path] {
			t.Errorf("unexpected route %+v", rt)
		}
		delete(want, rt.Path)
	}
	if len(want) != 0 {
		t.Errorf("missing routes %v", want)
	}
	if len(reg.Resources) != 1 || reg.Resources[0].Path != "/dashboard" || reg.Resources[0].Menu != "Subscription Value" {
		t.Errorf("resources = %+v", reg.Resources)
	}
}

func TestHealth(t *testing.T) {
	r, st := newFixture()
	for _, path := range []string{"/cpa-subscription-value/health", "/v0/management/cpa-subscription-value/health"} {
		resp := get(r, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d body %s", path, resp.StatusCode, resp.Body)
		}
		m := decode(t, resp)
		if m["ok"] != true || m["plugin_version"] != "0.1.0-test" || m["schema_version"] != float64(2) {
			t.Errorf("%s: body %v", path, m)
		}
		store := m["store"].(map[string]any)
		if store["events"] != float64(3) || store["db_bytes"] != float64(4096) {
			t.Errorf("store = %v", store)
		}
		ingest := m["ingest"].(map[string]any)
		if ingest["dropped"] != float64(2) {
			t.Errorf("ingest = %v", ingest)
		}
		if _, ok := m["uptime_s"].(float64); !ok {
			t.Errorf("uptime_s missing: %v", m)
		}
	}

	st.statsErr = errors.New("locked")
	resp := get(r, "/cpa-subscription-value/health", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stats error: status %d", resp.StatusCode)
	}
	if m := decode(t, resp); m["ok"] != false {
		t.Errorf("ok should be false: %v", m)
	}
}

func TestAccounts(t *testing.T) {
	r, _ := newFixture()
	resp := get(r, "/cpa-subscription-value/accounts", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body struct {
		Accounts []domain.Account `json:"accounts"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 2 || body.Accounts[0].AuthIndex != "aaaa1111" || body.Accounts[1].Provider != domain.ProviderCodex {
		t.Errorf("accounts = %+v", body.Accounts)
	}
}

func TestEventsByAuthIndex(t *testing.T) {
	r, st := newFixture()
	q := url.Values{"auth_index": {"aaaa1111"}, "from": {"1790424000"}, "to": {t0.Add(2 * time.Hour).Format(time.RFC3339)}, "limit": {"50"}, "order": {"asc"}}
	resp := get(r, "/v0/management/cpa-subscription-value/events", q)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, resp.Body)
	}
	var body struct {
		AccountID int64               `json:"account_id"`
		Count     int                 `json:"count"`
		Events    []domain.UsageEvent `json:"events"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.AccountID != 1 || body.Count != 2 || len(body.Events) != 2 {
		t.Errorf("body = %+v", body)
	}
	for _, e := range body.Events {
		if e.AccountID != 1 {
			t.Errorf("event %d from account %d", e.ID, e.AccountID)
		}
	}
	want := EventQuery{AccountID: 1, From: time.Unix(1790424000, 0).UTC(), To: t0.Add(2 * time.Hour), Limit: 50, Desc: false}
	if !st.lastEventQ.From.Equal(want.From) || !st.lastEventQ.To.Equal(want.To) || st.lastEventQ.Limit != 50 || st.lastEventQ.Desc || st.lastEventQ.AccountID != 1 {
		t.Errorf("query = %+v, want %+v", st.lastEventQ, want)
	}

	// No auth_index: every account, default order desc, default limit.
	resp = get(r, "/cpa-subscription-value/events", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if m := decode(t, resp); m["count"] != float64(3) {
		t.Errorf("count = %v", m["count"])
	}
	if !st.lastEventQ.Desc || st.lastEventQ.Limit != defaultLimit || st.lastEventQ.AccountID != 0 {
		t.Errorf("default query = %+v", st.lastEventQ)
	}

	// Unknown account.
	resp = get(r, "/cpa-subscription-value/events", url.Values{"auth_index": {"nope"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown auth_index: status %d", resp.StatusCode)
	}
	decode(t, resp)

	// Bad parameters.
	for _, bad := range []url.Values{{"from": {"yesterday"}}, {"order": {"sideways"}}, {"limit": {"ten"}}, {"from": {"200"}, "to": {"100"}}} {
		if resp := get(r, "/cpa-subscription-value/events", bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestEmptyListsEncodeAsArrays(t *testing.T) {
	r := New(testPluginID, &fakeStore{}, nil)
	for _, path := range []string{"/accounts", "/events", "/readings"} {
		resp := get(r, "/cpa-subscription-value"+path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		if strings.Contains(string(resp.Body), "null") {
			t.Errorf("%s: body has null: %s", path, resp.Body)
		}
	}
}

func TestReadingsParams(t *testing.T) {
	r, st := newFixture()
	q := url.Values{"auth_index": {"aaaa1111"}, "meter": {"7d"}, "source": {"header"}, "limit": {"200"}}
	resp := get(r, "/cpa-subscription-value/readings", q)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, resp.Body)
	}
	want := ReadingQuery{AccountID: 1, MeterKey: "7d", Source: "header", Limit: 200, Desc: true}
	if st.lastReadingQ != want {
		t.Errorf("query = %+v, want %+v", st.lastReadingQ, want)
	}
	var body struct {
		Readings []domain.MeterReading `json:"readings"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Readings) != 1 || body.Readings[0].ID != 20 {
		t.Errorf("readings = %+v", body.Readings)
	}

	// Query string embedded in Path is honored when Query is empty.
	resp = get(r, "/cpa-subscription-value/readings?auth_index=bbbb2222&source=poll", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if st.lastReadingQ.AccountID != 2 || st.lastReadingQ.Source != "poll" {
		t.Errorf("query = %+v", st.lastReadingQ)
	}

	if resp := get(r, "/cpa-subscription-value/readings", url.Values{"auth_index": {"zzz"}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown auth_index: %d", resp.StatusCode)
	}
}

func TestDashboardResource(t *testing.T) {
	r, _ := newFixture()
	for _, path := range []string{"/v0/resource/plugins/cpa-subscription-value/dashboard", "/cpa-subscription-value/dashboard"} {
		resp := get(r, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		if ct := resp.Headers.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: content-type %q", path, ct)
		}
		body := string(resp.Body)
		if !strings.Contains(body, "<html") || !strings.Contains(body, "/v0/management/") {
			t.Errorf("%s: body does not look like the dashboard (%d bytes)", path, len(body))
		}
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	r, _ := newFixture()
	for _, path := range []string{"/cpa-subscription-value/nope", "/v0/management/cpa-subscription-value/", "/v0/management/other-plugin/health"} {
		resp := get(r, path, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
		if m := decode(t, resp); m["error"] == nil {
			t.Errorf("%s: no error field: %v", path, m)
		}
	}
	resp := r.Handle(abi.ManagementRequest{Method: "POST", Path: "/cpa-subscription-value/health"})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST health: status %d", resp.StatusCode)
	}
}

func TestParseTime(t *testing.T) {
	cases := map[string]time.Time{
		"":                     {},
		"1790424000":           time.Unix(1790424000, 0).UTC(),
		"1790424000000":        time.UnixMilli(1790424000000).UTC(),
		"2026-09-26T12:00:00Z": t0,
	}
	for in, want := range cases {
		got, err := parseTime(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseTime("2026-09-26"); err == nil {
		t.Error("date-only should be rejected")
	}
}
