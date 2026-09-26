package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/config"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/estimate"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/poll"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/pricing"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/sim"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/weights"
)

const (
	testPluginID = "cpa-subscription-value"
	claudeIdx    = "c1a0de00"
	codexIdx     = "c0de0000"
	prefix       = "/cpa-subscription-value"
)

// fixture is a real store seeded from two simulated corpora (one Claude,
// one Codex account, two weekly cycles each), ingested and recomputed in
// daily stages so the open cycle has a running-estimate history.
type fixture struct {
	st             *store.Store
	catalog        *pricing.Catalog
	claude, codex  domain.Account
	now            time.Time
	claudeEvents   int // events stored for the Claude account
	claudeInCycle  int // Claude events inside the open 7d cycle
	claudeOpenFrom time.Time
}

var (
	fixOnce sync.Once
	fix     fixture
	fixErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fix.st != nil {
		dir := filepath.Dir(fix.st.Path())
		fix.st.Close()
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	fixOnce.Do(func() { fix, fixErr = buildFixture() })
	if fixErr != nil {
		t.Fatal(fixErr)
	}
	return fix
}

func buildFixture() (fixture, error) {
	dir, err := os.MkdirTemp("", "api-test-*")
	if err != nil {
		return fixture{}, err
	}
	st, err := store.Open(filepath.Join(dir, "api.db"))
	if err != nil {
		return fixture{}, err
	}
	f := fixture{st: st}
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	corpus := func(seed uint64, p domain.Provider) sim.Corpus {
		sc := sim.DefaultScenario(seed, p)
		sc.Start, sc.Cycles, sc.EarlyResetCycle, sc.ShiftCycle = start, 2, -1, -1
		return sim.Generate(sc)
	}
	cc, xc := corpus(7, domain.ProviderClaude), corpus(8, domain.ProviderCodex)
	if f.claude, err = st.UpsertAccount(domain.Account{Provider: domain.ProviderClaude, AuthIndex: claudeIdx, AuthID: "claude.json", Email: "me@example.com", PlanType: "max"}); err != nil {
		return f, err
	}
	if f.codex, err = st.UpsertAccount(domain.Account{Provider: domain.ProviderCodex, AuthIndex: codexIdx, AuthID: "codex.json", PlanType: "pro"}); err != nil {
		return f, err
	}
	f.now = start.Add(7*24*time.Hour + 4*24*time.Hour) // day 4 of the second week
	var from time.Time
	for _, stage := range []time.Time{f.now.Add(-72 * time.Hour), f.now.Add(-48 * time.Hour), f.now.Add(-24 * time.Hour), f.now} {
		for _, x := range []struct {
			c    sim.Corpus
			acct domain.Account
		}{{cc, f.claude}, {xc, f.codex}} {
			n, err := ingest(st, x.c, x.acct.ID, from, stage)
			if err != nil {
				return f, err
			}
			if x.acct.ID == f.claude.ID {
				f.claudeEvents += n
			}
			for _, m := range []string{domain.MeterSevenDay, domain.MeterFiveHour, domain.MeterSevenDayOI} {
				if err := estimate.Recompute(st, x.acct.ID, m, stage); err != nil {
					return f, fmt.Errorf("recompute %s: %w", m, err)
				}
			}
		}
		from = stage
	}
	cs, err := st.ListCycles(f.claude.ID, domain.MeterSevenDay, 1)
	if err != nil || len(cs) == 0 {
		return f, fmt.Errorf("no claude cycles (%v)", err)
	}
	f.claudeOpenFrom = cs[0].StartsAt
	for _, e := range cc.Events {
		if !e.ObservedAt.Before(cs[0].StartsAt) && !e.ObservedAt.After(f.now) {
			f.claudeInCycle++
		}
	}
	f.catalog = pricing.NewCatalog(st, config.Defaults(), nil, nil)
	if err := f.catalog.Load(); err != nil {
		return f, err
	}
	fit := weights.Fit{
		Provider: domain.ProviderClaude, Anchor: "claude-opus-5", Lag: "time", ComputedAt: f.now, Segments: 90, Eligible: 80,
		Factors: map[string]weights.Factor{
			"model:claude-opus-5":  {Estimate: 1, CILo: 1, CIHi: 1, Status: weights.StatusAnchor, Segments: 80},
			"model:claude-fable-5": {Estimate: 1.2, CILo: 1.1, CIHi: 1.3, Status: weights.StatusIdentified, Segments: 40, DataShare: 0.8},
			"type:cache_read":      {Estimate: 0.5, CILo: 0.45, CIHi: 0.56, Status: weights.StatusIdentified, Segments: 80, DataShare: 0.9},
			"fast":                 {Estimate: 1, CILo: 0.4, CIHi: 2.7, Status: weights.StatusPriorLocked},
		},
		Scales:   []weights.CycleScale{{CycleKey: "w1", ValuePer100: 400, ValueCILo: 380, ValueCIHi: 420, SD: 0.03, Segments: 80}},
		Backtest: map[string]float64{"time": 0.4, "idx1": 0.5},
	}
	w := fit.ToStore(f.claude.ID, domain.MeterSevenDay, sim.PriceHash)
	if err := st.InsertWeightFit(&w); err != nil {
		return f, err
	}
	return f, nil
}

// ingest stores the corpus's events and readings observed in (from, to].
func ingest(st *store.Store, c sim.Corpus, accountID int64, from, to time.Time) (int, error) {
	in := func(t time.Time) bool { return t.After(from) && !t.After(to) }
	byEvent := map[int64][]domain.MeterReading{}
	for _, r := range c.Readings {
		if r.EventID != nil {
			r.AccountID = accountID
			byEvent[*r.EventID] = append(byEvent[*r.EventID], r)
		}
	}
	n := 0
	for _, e := range c.Events {
		if !in(e.ObservedAt) {
			continue
		}
		rs := byEvent[e.ID]
		e.ID, e.AccountID = 0, accountID
		if _, err := st.InsertEvent(&e, rs); err != nil {
			return n, err
		}
		n++
	}
	for _, r := range c.Readings {
		if r.EventID == nil && in(r.ObservedAt) {
			r.ID, r.AccountID = 0, accountID
			if err := st.InsertReading(&r); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

type fakeActions struct {
	st          *store.Store
	now         time.Time
	recomputes  []string
	polls       int
	syncs       int
	reprices    int
	reconfigure int
	fail        error
}

func (a *fakeActions) Recompute(accountID int64, meterKey string) error {
	a.recomputes = append(a.recomputes, fmt.Sprintf("%d/%s", accountID, meterKey))
	if a.fail != nil {
		return a.fail
	}
	return estimate.Recompute(a.st, accountID, meterKey, a.now)
}

func (a *fakeActions) PollNow(context.Context) (any, error) {
	a.polls++
	return poll.Summary{Polled: 2, OK: 2, Accounts: []poll.AccountResult{}}, a.fail
}

func (a *fakeActions) SyncPrices(context.Context) (bool, error) { a.syncs++; return true, a.fail }
func (a *fakeActions) Reprice(context.Context) (int64, error)   { a.reprices++; return 42, a.fail }
func (a *fakeActions) Reconfigured()                            { a.reconfigure++ }

func newRouter(t *testing.T) (*Router, fixture, *fakeActions) {
	t.Helper()
	f := loadFixture(t)
	acts := &fakeActions{st: f.st, now: f.now}
	health := func() Health {
		return Health{PluginVersion: "0.1.0-test", SchemaVersion: 2, StartedAt: f.now.Add(-time.Hour), IngestQueued: 1, IngestDropped: 2, LastEventAt: f.now, Config: map[string]any{"data_dir": "/tmp/x"}}
	}
	r := New(testPluginID, Deps{Store: f.st, Health: health, Catalog: f.catalog, Actions: acts})
	r.now = func() time.Time { return f.now }
	return r, f, acts
}

func get(r *Router, path string, q url.Values) abi.ManagementResponse {
	return r.Handle(abi.ManagementRequest{Method: "GET", Path: path, Query: q})
}

func post(r *Router, path string, q url.Values, body string) abi.ManagementResponse {
	return r.Handle(abi.ManagementRequest{Method: "POST", Path: path, Query: q, Body: []byte(body)})
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

func into[T any](t *testing.T, resp abi.ManagementResponse, want int) T {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status %d, want %d: %s", resp.StatusCode, want, resp.Body)
	}
	var v T
	if err := json.Unmarshal(resp.Body, &v); err != nil {
		t.Fatalf("decode %s: %v", resp.Body, err)
	}
	return v
}

func wantStatus(t *testing.T, resp abi.ManagementResponse, want int, what string) {
	t.Helper()
	if resp.StatusCode != want {
		t.Errorf("%s: status %d, want %d (%s)", what, resp.StatusCode, want, resp.Body)
	}
}

func TestRegistration(t *testing.T) {
	r, _, _ := newRouter(t)
	reg := r.Registration()
	want := map[string]bool{}
	for _, p := range []string{"health", "accounts", "events", "readings", "summary", "cycles", "series", "weights", "prices", "settings", "export"} {
		want["GET "+prefix+"/"+p] = true
	}
	for _, p := range []string{"prices/sync", "prices/reprice", "settings", "recompute", "poll"} {
		want["POST "+prefix+"/"+p] = true
	}
	for _, rt := range reg.Routes {
		k := rt.Method + " " + rt.Path
		if !want[k] {
			t.Errorf("unexpected route %s", k)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Errorf("missing routes %v", want)
	}
	if len(reg.Resources) != 1 || reg.Resources[0].Path != "/dashboard" || reg.Resources[0].Menu != "Subscription Value" {
		t.Errorf("resources = %+v", reg.Resources)
	}
}

func TestHealth(t *testing.T) {
	r, f, _ := newRouter(t)
	at := f.now.Add(-10 * time.Minute)
	if err := f.st.SetSetting(poll.SettingKey(claudeIdx), poll.Status{At: at, OK: true, Readings: 3, NextAt: at.Add(20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetSetting(store.RecomputeSettingKey(claudeIdx, "7d"), at); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.st.DeleteSetting(poll.SettingKey(claudeIdx))
		f.st.DeleteSetting(store.RecomputeSettingKey(claudeIdx, "7d"))
	})
	for _, path := range []string{prefix + "/health", "/v0/management" + prefix + "/health"} {
		m := decode(t, get(r, path, nil))
		if m["ok"] != true || m["plugin_version"] != "0.1.0-test" || m["schema_version"] != float64(2) {
			t.Errorf("%s: body %v", path, m)
		}
		if st := m["store"].(map[string]any); st["events"].(float64) < float64(f.claudeEvents) || st["accounts"] != float64(2) {
			t.Errorf("store = %v", st)
		}
		if ing := m["ingest"].(map[string]any); ing["dropped"] != float64(2) {
			t.Errorf("ingest = %v", ing)
		}
		polls := m["polls"].([]any)
		if len(polls) != 2 {
			t.Fatalf("polls = %v", polls)
		}
		for _, p := range polls {
			p := p.(map[string]any)
			switch p["auth_index"] {
			case claudeIdx:
				if s, _ := p["status"].(map[string]any); s == nil || s["ok"] != true || s["readings"] != float64(3) {
					t.Errorf("claude poll status = %v", p)
				}
			case codexIdx:
				if p["status"] != nil {
					t.Errorf("codex poll status = %v", p)
				}
			}
		}
		prices := m["prices"].(map[string]any)
		if h, _ := f.catalog.Active(); prices["hash"] == "" || prices["families"] != float64(len(h)) || prices["source"] != "builtin" || prices["synced_at"] == nil {
			t.Errorf("prices = %v", prices)
		}
		rc := m["estimator"].(map[string]any)["recomputes"].([]any)
		if len(rc) != 1 || rc[0].(map[string]any)["meter"] != "7d" {
			t.Errorf("estimator = %v", rc)
		}
	}
	resp := New(testPluginID, Deps{}).Handle(abi.ManagementRequest{Method: "GET", Path: prefix + "/health"})
	wantStatus(t, resp, http.StatusServiceUnavailable, "nil store")
	if m := decode(t, resp); m["ok"] != false {
		t.Errorf("ok should be false: %v", m)
	}
	wantStatus(t, New(testPluginID, Deps{}).Handle(abi.ManagementRequest{Path: prefix + "/summary"}), http.StatusServiceUnavailable, "nil store summary")
}

func TestAccounts(t *testing.T) {
	r, _, _ := newRouter(t)
	body := into[struct {
		Accounts []accountView `json:"accounts"`
	}](t, get(r, prefix+"/accounts", nil), 200)
	if len(body.Accounts) != 2 {
		t.Fatalf("accounts = %+v", body.Accounts)
	}
	c := body.Accounts[0]
	if c.AuthIndex != claudeIdx || c.Provider != domain.ProviderClaude || c.CoverageMode != CoverageCPAOnly {
		t.Errorf("claude = %+v", c)
	}
	if strings.Join(c.Meters, ",") != "7d,5h,7d_oi" || strings.Join(c.WeightMeters, ",") != "7d" {
		t.Errorf("claude meters = %v", c.Meters)
	}
	if x := body.Accounts[1]; x.Provider != domain.ProviderCodex || strings.Join(x.Meters, ",") != "7d,5h" || len(x.WeightMeters) != 0 {
		t.Errorf("codex = %+v", x)
	}
}

func TestEvents(t *testing.T) {
	r, f, _ := newRouter(t)
	q := url.Values{"auth_index": {claudeIdx}, "from": {fmt.Sprint(f.claudeOpenFrom.Unix())}, "to": {f.now.Format(time.RFC3339)}, "limit": {"50"}, "order": {"asc"}}
	body := into[struct {
		AccountID int64               `json:"account_id"`
		Count     int                 `json:"count"`
		Events    []domain.UsageEvent `json:"events"`
	}](t, get(r, "/v0/management"+prefix+"/events", q), 200)
	if body.AccountID != f.claude.ID || body.Count != 50 || len(body.Events) != 50 {
		t.Fatalf("account %d count %d", body.AccountID, body.Count)
	}
	for i, e := range body.Events {
		if e.AccountID != f.claude.ID || e.ObservedAt.Before(f.claudeOpenFrom) {
			t.Errorf("event %d: account %d at %v", e.ID, e.AccountID, e.ObservedAt)
		}
		if i > 0 && e.ObservedAt.Before(body.Events[i-1].ObservedAt) {
			t.Errorf("not ascending at %d", i)
		}
	}
	all := decode(t, get(r, prefix+"/events", nil))
	if all["count"] != float64(defaultLimit) {
		t.Errorf("default count = %v", all["count"])
	}
	wantStatus(t, get(r, prefix+"/events", url.Values{"auth_index": {"nope"}}), 404, "unknown auth_index")
	for _, b := range []url.Values{{"from": {"yesterday"}}, {"order": {"sideways"}}, {"limit": {"ten"}}, {"from": {"200"}, "to": {"100"}}} {
		wantStatus(t, get(r, prefix+"/events", b), 400, fmt.Sprint(b))
	}
}

func TestReadings(t *testing.T) {
	r, _, _ := newRouter(t)
	body := into[struct {
		Readings []domain.MeterReading `json:"readings"`
	}](t, get(r, prefix+"/readings?auth_index="+codexIdx+"&source=poll", url.Values{"meter": {"7d"}, "limit": {"20"}}), 200)
	if len(body.Readings) != 20 {
		t.Fatalf("readings = %d", len(body.Readings))
	}
	for _, m := range body.Readings {
		if m.Source != "poll" || m.MeterKey != "7d" {
			t.Errorf("reading %+v", m)
		}
	}
	wantStatus(t, get(r, prefix+"/readings", url.Values{"auth_index": {"zzz"}}), 404, "unknown auth_index")
}

func TestEmptyStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := New(testPluginID, Deps{Store: st})
	for _, path := range []string{"/accounts", "/events", "/readings", "/summary", "/settings"} {
		resp := get(r, prefix+path, nil)
		wantStatus(t, resp, 200, path)
		if strings.Contains(string(resp.Body), "null") {
			t.Errorf("%s: body has null: %s", path, resp.Body)
		}
	}
	wantStatus(t, get(r, prefix+"/prices", nil), 503, "prices without catalog")
	wantStatus(t, post(r, prefix+"/poll", nil, ""), 503, "poll without actions")
	wantStatus(t, post(r, prefix+"/prices/sync", nil, ""), 503, "sync without catalog")
}

// summaryJSON decodes /summary with capacity fields as pointers.
type summaryJSON struct {
	Account struct {
		AuthIndex string `json:"auth_index"`
	} `json:"account"`
	CoverageMode           string `json:"coverage_mode"`
	CapacityDisabledReason string `json:"capacity_disabled_reason"`
	Meters                 []struct {
		Meter        string    `json:"meter"`
		Role         string    `json:"role"`
		Cycle        cycleView `json:"cycle"`
		UsedFraction *float64  `json:"used_fraction"`
		Latest       *struct {
			Kind  string   `json:"kind"`
			VHat  *float64 `json:"v_hat"`
			CILo  *float64 `json:"ci_lo"`
			CIHi  *float64 `json:"ci_hi"`
			Grade string   `json:"grade"`
			Lag   string   `json:"lag"`
			Ticks int64    `json:"ticks_used"`
			Flags []string `json:"flags"`
		} `json:"latest_estimate"`
		Forecast *struct {
			Status    string   `json:"status"`
			UNow      float64  `json:"u_now"`
			Remaining *float64 `json:"remaining_usd"`
			PerDay    *float64 `json:"usd_per_day"`
			Proxied   float64  `json:"proxied_usd"`
		} `json:"forecast"`
		SpentUSD float64 `json:"spent_usd"`
		Events   int     `json:"events"`
		Mix      mixView `json:"mix"`
	} `json:"meters"`
}

func TestSummary(t *testing.T) {
	r, f, _ := newRouter(t)
	all := into[struct {
		Accounts []summaryJSON `json:"accounts"`
	}](t, get(r, prefix+"/summary", nil), 200)
	if len(all.Accounts) != 2 {
		t.Fatalf("accounts = %d", len(all.Accounts))
	}
	for _, s := range all.Accounts {
		if s.CoverageMode != CoverageCPAOnly || s.CapacityDisabledReason != "" || len(s.Meters) < 2 {
			t.Fatalf("%s: %+v", s.Account.AuthIndex, s)
		}
		m := s.Meters[0]
		if m.Meter != "7d" || m.Role != "headline" || s.Meters[1].Meter != "5h" || s.Meters[1].Role != "secondary" {
			t.Errorf("meter order: %s/%s, %s", m.Meter, m.Role, s.Meters[1].Meter)
		}
		if !m.Cycle.Open || m.UsedFraction == nil || *m.UsedFraction <= 0 {
			t.Errorf("%s 7d cycle %+v used %v", s.Account.AuthIndex, m.Cycle, m.UsedFraction)
		}
		e := m.Latest
		if e == nil || e.Kind != "running" || e.VHat == nil || e.CILo == nil || !(*e.CILo < *e.VHat && *e.VHat < *e.CIHi) || e.Lag == "" {
			t.Fatalf("%s latest_estimate = %+v", s.Account.AuthIndex, e)
		}
		if fc := m.Forecast; fc == nil || fc.Status == "" || fc.Remaining == nil || fc.PerDay == nil || math.Abs(fc.Proxied-m.SpentUSD) > 1e-9 {
			t.Errorf("forecast = %+v", fc)
		}
		if m.SpentUSD <= 0 || m.Events == 0 {
			t.Errorf("spent %v events %d", m.SpentUSD, m.Events)
		}
		var sum float64
		for _, v := range m.Mix.ByFamily {
			sum += v
		}
		if math.Abs(sum-1) > 1e-9 || len(m.Mix.ByType) < 3 {
			t.Errorf("mix = %+v", m.Mix)
		}
	}
	claude := all.Accounts[0]
	if claude.Meters[0].Events != f.claudeInCycle {
		t.Errorf("claude 7d events = %d, want %d", claude.Meters[0].Events, f.claudeInCycle)
	}
	if len(claude.Meters) != 3 || claude.Meters[2].Meter != "7d_oi" || claude.Meters[2].Role != "scoped" {
		t.Errorf("claude meters = %d", len(claude.Meters))
	}
	one := into[summaryJSON](t, get(r, prefix+"/summary", url.Values{"auth_index": {codexIdx}}), 200)
	if one.Account.AuthIndex != codexIdx || len(one.Meters) != 2 {
		t.Errorf("codex summary = %+v", one)
	}
	wantStatus(t, get(r, prefix+"/summary", url.Values{"auth_index": {"nope"}}), 404, "unknown")
}

func TestSummaryMixedCoverageNullsCapacity(t *testing.T) {
	r, f, acts := newRouter(t)
	t.Cleanup(func() { f.st.DeleteSetting(coverageKey(claudeIdx)) })
	resp := post(r, prefix+"/settings", nil, `{"coverage":{"`+claudeIdx+`":"mixed"}}`)
	wantStatus(t, resp, 200, "set mixed")
	if acts.reconfigure != 1 {
		t.Errorf("Reconfigured calls = %d", acts.reconfigure)
	}
	s := into[summaryJSON](t, get(r, prefix+"/summary", url.Values{"auth_index": {claudeIdx}}), 200)
	if s.CoverageMode != CoverageMixed || s.CapacityDisabledReason == "" {
		t.Fatalf("mode %q reason %q", s.CoverageMode, s.CapacityDisabledReason)
	}
	for _, m := range s.Meters {
		if m.Latest == nil || m.Latest.VHat != nil || m.Latest.CILo != nil || m.Latest.CIHi != nil {
			t.Errorf("%s: capacity fields not null: %+v", m.Meter, m.Latest)
		}
		if m.Forecast != nil && (m.Forecast.Remaining != nil || m.Forecast.PerDay != nil) {
			t.Errorf("%s: forecast $ not null: %+v", m.Meter, m.Forecast)
		}
		if m.SpentUSD <= 0 || m.Latest.Grade == "" {
			t.Errorf("%s: non-capacity fields dropped: spent %v grade %q", m.Meter, m.SpentUSD, m.Latest.Grade)
		}
	}
	raw := string(get(r, prefix+"/summary", url.Values{"auth_index": {claudeIdx}}).Body)
	if !strings.Contains(raw, `"v_hat":null`) || !strings.Contains(raw, `"remaining_usd":null`) {
		t.Error("capacity fields should be present as null")
	}
}

func TestCycles(t *testing.T) {
	r, _, _ := newRouter(t)
	body := into[struct {
		Meter        string      `json:"meter"`
		CoverageMode string      `json:"coverage_mode"`
		Cycles       []cycleItem `json:"cycles"`
	}](t, get(r, prefix+"/cycles", url.Values{"auth_index": {claudeIdx}}), 200)
	if body.Meter != "7d" || len(body.Cycles) != 2 {
		t.Fatalf("meter %s cycles %d", body.Meter, len(body.Cycles))
	}
	open, closed := body.Cycles[0], body.Cycles[1]
	if !open.Open || open.Estimate == nil || open.Estimate.Kind != "running" {
		t.Errorf("open cycle = %+v", open)
	}
	if closed.Open || closed.ClosedAt == nil || closed.Estimate == nil || closed.Estimate.Kind != "final" || closed.Estimate.VHat == nil {
		t.Fatalf("closed cycle = %+v", closed)
	}
	if !closed.ResetAt.Before(open.ResetAt) || closed.SpentUSD <= 0 || len(closed.Mix.ByFamily) == 0 || closed.Mix.Source != "estimate" {
		t.Errorf("closed = %+v", closed)
	}
	five := into[struct {
		Cycles []cycleItem `json:"cycles"`
	}](t, get(r, prefix+"/cycles", url.Values{"auth_index": {claudeIdx}, "meter": {"5h"}, "limit": {"3"}}), 200)
	if len(five.Cycles) != 3 || five.Cycles[0].MeterKey != "5h" {
		t.Errorf("5h cycles = %d", len(five.Cycles))
	}
	wantStatus(t, get(r, prefix+"/cycles", nil), 400, "missing auth_index")
	wantStatus(t, get(r, prefix+"/cycles", url.Values{"auth_index": {claudeIdx}, "limit": {"x"}}), 400, "bad limit")
	wantStatus(t, get(r, prefix+"/cycles", url.Values{"auth_index": {"nope"}}), 404, "unknown")
}

func TestSeries(t *testing.T) {
	r, f, _ := newRouter(t)
	body := into[struct {
		Cycle    cycleView `json:"cycle"`
		Lag      string    `json:"lag"`
		Readings struct {
			Header []point `json:"header"`
			Poll   []point `json:"poll"`
		} `json:"readings"`
		Cum       []point         `json:"cumulative_usd"`
		Crossings []crossing      `json:"crossings"`
		Estimates []estimatePoint `json:"estimates"`
		Fit       map[string]any  `json:"fit"`
		Baseline  map[string]any  `json:"baseline"`
	}](t, get(r, prefix+"/series", url.Values{"auth_index": {claudeIdx}}), 200)
	if !body.Cycle.Open || body.Lag == "" {
		t.Fatalf("cycle %+v lag %q", body.Cycle, body.Lag)
	}
	if n := len(body.Readings.Header) + len(body.Readings.Poll); n == 0 || n > maxPoints || len(body.Readings.Poll) == 0 {
		t.Errorf("readings: %d header, %d poll", len(body.Readings.Header), len(body.Readings.Poll))
	}
	if len(body.Cum) == 0 || len(body.Cum) > maxPoints {
		t.Errorf("cumulative points = %d", len(body.Cum))
	}
	for i := 1; i < len(body.Cum); i++ {
		if body.Cum[i][0] < body.Cum[i-1][0] || body.Cum[i][1] < body.Cum[i-1][1] {
			t.Fatalf("cumulative $ not monotone at %d", i)
		}
	}
	if len(body.Crossings) < 5 || body.Baseline == nil {
		t.Fatalf("crossings = %d", len(body.Crossings))
	}
	last := body.Crossings[len(body.Crossings)-1]
	if last.CumUSD <= 0 || last.U <= body.Baseline["u"].(float64) {
		t.Errorf("last crossing %+v baseline %v", last, body.Baseline)
	}
	if len(body.Estimates) < 2 {
		t.Errorf("estimate history = %d", len(body.Estimates))
	}
	if body.Fit == nil || body.Fit["v_hat"].(float64) <= 0 {
		t.Errorf("fit = %v", body.Fit)
	}

	cs, _ := f.st.ListCycles(f.codex.ID, "7d", 5)
	wantStatus(t, get(r, prefix+"/series", url.Values{"auth_index": {claudeIdx}, "cycle_id": {fmt.Sprint(cs[0].ID)}}), 404, "other account's cycle")
	wantStatus(t, get(r, prefix+"/series", url.Values{"auth_index": {codexIdx}, "cycle_id": {fmt.Sprint(cs[1].ID)}}), 200, "own closed cycle")
	wantStatus(t, get(r, prefix+"/series", url.Values{"auth_index": {codexIdx}, "cycle_id": {fmt.Sprint(cs[1].ID)}, "meter": {"5h"}}), 404, "meter mismatch")
	wantStatus(t, get(r, prefix+"/series", url.Values{"auth_index": {codexIdx}, "cycle_id": {"abc"}}), 400, "bad cycle_id")
	wantStatus(t, get(r, prefix+"/series", url.Values{"auth_index": {codexIdx}, "meter": {"7d_oi"}}), 404, "no cycles")
	wantStatus(t, get(r, prefix+"/series", nil), 400, "missing auth_index")
}

func TestDownsample(t *testing.T) {
	var pts []point
	for i := 0; i < 10001; i++ {
		pts = append(pts, point{float64(i), float64(i % 7)})
	}
	out := downsample(pts, 2000)
	if len(out) > 2000 || len(out) < 1500 || out[len(out)-1] != pts[len(pts)-1] {
		t.Fatalf("len %d last %v", len(out), out[len(out)-1])
	}
	if got := downsample(pts[:5], 10); len(got) != 5 {
		t.Errorf("short input changed: %v", got)
	}
}

func TestWeights(t *testing.T) {
	r, _, _ := newRouter(t)
	body := into[struct {
		Anchor        string       `json:"anchor"`
		IdentifiedAny bool         `json:"identified_any"`
		Factors       []factorView `json:"factors"`
		Models        []modelValue `json:"models"`
		Scales        []weights.CycleScale
	}](t, get(r, prefix+"/weights", url.Values{"auth_index": {claudeIdx}}), 200)
	if body.Anchor != "claude-opus-5" || !body.IdentifiedAny || len(body.Factors) != 4 {
		t.Fatalf("weights = %+v", body)
	}
	if body.Factors[0].Kind != "model" || body.Factors[3].Kind != "fast" || body.Factors[2].Name != "cache_read" {
		t.Errorf("factor order = %+v", body.Factors)
	}
	byFam := map[string]modelValue{}
	for _, m := range body.Models {
		byFam[m.Family] = m
	}
	if v := byFam["claude-fable-5"]; math.Abs(v.ValuePer100-400/1.2) > 1e-9 || !(v.CILo < v.ValuePer100 && v.ValuePer100 < v.CIHi) {
		t.Errorf("fable V_m = %+v", v)
	}
	if byFam["claude-opus-5"].ValuePer100 != 400 {
		t.Errorf("opus V_m = %+v", byFam["claude-opus-5"])
	}
	wantStatus(t, get(r, prefix+"/weights", url.Values{"auth_index": {codexIdx}}), 404, "no fit")
	wantStatus(t, get(r, prefix+"/weights", nil), 400, "missing auth_index")
}

func TestPrices(t *testing.T) {
	r, f, acts := newRouter(t)
	body := into[struct {
		Hash string     `json:"hash"`
		Unit string     `json:"unit"`
		Rows []priceRow `json:"rows"`
	}](t, get(r, prefix+"/prices", nil), 200)
	if _, h := f.catalog.Active(); body.Hash != h || body.Unit != "usd_per_mtok" || len(body.Rows) == 0 {
		t.Fatalf("prices = %+v", body)
	}
	for i := 1; i < len(body.Rows); i++ {
		a, b := body.Rows[i-1], body.Rows[i]
		if a.Provider > b.Provider || (a.Provider == b.Provider && a.Family >= b.Family) {
			t.Fatalf("rows not sorted at %d", i)
		}
	}
	sync := into[map[string]any](t, post(r, prefix+"/prices/sync", nil, ""), 200)
	if sync["changed"] != true || sync["hash"] != body.Hash || acts.syncs != 1 {
		t.Errorf("sync = %v", sync)
	}
	rep := into[map[string]any](t, post(r, prefix+"/prices/reprice", nil, ""), 202)
	if rep["updated"] != float64(42) || acts.reprices != 1 {
		t.Errorf("reprice = %v", rep)
	}
	wantStatus(t, get(r, prefix+"/prices/sync", nil), 405, "GET sync")
	acts.fail = fmt.Errorf("boom")
	wantStatus(t, post(r, prefix+"/prices/sync", nil, ""), 502, "sync failure")
	wantStatus(t, post(r, prefix+"/prices/reprice", nil, ""), 500, "reprice failure")
}

func TestSettings(t *testing.T) {
	r, f, acts := newRouter(t)
	t.Cleanup(func() {
		f.st.DeleteSetting(coverageKey(codexIdx))
		f.st.DeleteSetting(displayKey)
	})
	type settings struct {
		Accounts []struct {
			AuthIndex    string `json:"auth_index"`
			CoverageMode string `json:"coverage_mode"`
		} `json:"accounts"`
		Modes   []string     `json:"coverage_modes"`
		Display displayPrefs `json:"display"`
	}
	s := into[settings](t, get(r, prefix+"/settings", nil), 200)
	if len(s.Accounts) != 2 || s.Accounts[1].CoverageMode != CoverageCPAOnly || s.Display != defaultDisplay() || len(s.Modes) != 3 {
		t.Fatalf("defaults = %+v", s)
	}
	for body, want := range map[string]int{
		``:                                      400,
		`{`:                                     400,
		`{"bogus":1}`:                           400,
		`{"coverage":{"` + codexIdx + `":"x"}}`: 400,
		`{"coverage":{"nope":"mixed"}}`:         404,
		`{"display":{"wow_view":"sideways"}}`:   400,
		`{"display":{"refresh_s":1}}`:           400,
		`{"display":{"cycles_shown":5,"x":1}}`:  400,
	} {
		wantStatus(t, post(r, prefix+"/settings", nil, body), want, body)
	}
	if acts.reconfigure != 0 {
		t.Errorf("rejected posts reconfigured %d times", acts.reconfigure)
	}
	s = into[settings](t, post(r, prefix+"/settings", nil, `{"coverage":{"`+codexIdx+`":"unknown"},"display":{"wow_view":"codex","cycles_shown":8}}`), 200)
	if s.Accounts[1].CoverageMode != CoverageUnknown || s.Display.WowView != "codex" || s.Display.CyclesShown != 8 || s.Display.RefreshSeconds != 60 {
		t.Errorf("after post = %+v", s)
	}
	if acts.reconfigure != 1 {
		t.Errorf("Reconfigured = %d", acts.reconfigure)
	}
	sum := into[summaryJSON](t, get(r, prefix+"/summary", url.Values{"auth_index": {codexIdx}}), 200)
	if sum.CoverageMode != CoverageUnknown || sum.Meters[0].Latest.VHat != nil {
		t.Errorf("unknown coverage summary = %+v", sum.Meters[0].Latest)
	}
	wantStatus(t, r.Handle(abi.ManagementRequest{Method: "DELETE", Path: prefix + "/settings"}), 405, "DELETE settings")
}

func TestRecomputeAndPoll(t *testing.T) {
	r, f, acts := newRouter(t)
	s := into[summaryJSON](t, post(r, prefix+"/recompute", url.Values{"auth_index": {claudeIdx}}, ""), 200)
	want := fmt.Sprintf("%d/7d,%d/5h,%d/7d_oi", f.claude.ID, f.claude.ID, f.claude.ID)
	if got := strings.Join(acts.recomputes, ","); got != want {
		t.Errorf("recomputes = %s, want %s", got, want)
	}
	if s.Account.AuthIndex != claudeIdx || s.Meters[0].Latest == nil {
		t.Errorf("summary after recompute = %+v", s)
	}
	acts.recomputes = nil
	wantStatus(t, post(r, prefix+"/recompute", url.Values{"auth_index": {codexIdx}, "meter": {"5h"}}, ""), 200, "one meter")
	if len(acts.recomputes) != 1 || !strings.HasSuffix(acts.recomputes[0], "/5h") {
		t.Errorf("recomputes = %v", acts.recomputes)
	}
	wantStatus(t, post(r, prefix+"/recompute", url.Values{"auth_index": {codexIdx}, "meter": {"7d_oi"}}, ""), 404, "meter not seen")
	wantStatus(t, post(r, prefix+"/recompute", nil, ""), 400, "missing auth_index")
	wantStatus(t, get(r, prefix+"/recompute", url.Values{"auth_index": {codexIdx}}), 405, "GET recompute")

	p := into[map[string]any](t, post(r, prefix+"/poll", nil, ""), 200)
	if res, _ := p["result"].(map[string]any); res == nil || res["polled"] != float64(2) || acts.polls != 1 {
		t.Errorf("poll = %v", p)
	}
	acts.fail = fmt.Errorf("down")
	wantStatus(t, post(r, prefix+"/poll", nil, ""), 502, "poll failure")
	wantStatus(t, post(r, prefix+"/recompute", url.Values{"auth_index": {codexIdx}}, ""), 500, "recompute failure")

	noActs := New(testPluginID, Deps{Store: f.st, Catalog: f.catalog})
	wantStatus(t, post(noActs, prefix+"/recompute", url.Values{"auth_index": {codexIdx}}, ""), 503, "no actions")
	wantStatus(t, post(noActs, prefix+"/prices/reprice", nil, ""), 503, "no actions reprice")
}

func TestExport(t *testing.T) {
	r, f, _ := newRouter(t)
	cs, _ := f.st.ListCycles(f.claude.ID, "7d", 1)
	resp := get(r, prefix+"/export", url.Values{"auth_index": {claudeIdx}, "cycle_id": {fmt.Sprint(cs[0].ID)}})
	wantStatus(t, resp, 200, "export")
	if ct := resp.Headers.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("content-type %q", ct)
	}
	if cd := resp.Headers.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".csv") {
		t.Errorf("content-disposition %q", cd)
	}
	rows, err := csv.NewReader(strings.NewReader(string(resp.Body))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := append(append([]string{}, exportColumns...), "used_7d", "used_5h", "used_7d_oi")
	if strings.Join(rows[0], ",") != strings.Join(wantHeader, ",") {
		t.Errorf("header = %v", rows[0])
	}
	if len(rows)-1 != f.claudeInCycle {
		t.Errorf("rows = %d, want %d", len(rows)-1, f.claudeInCycle)
	}
	with7d := 0
	for _, row := range rows[1:] {
		if len(row) != len(wantHeader) {
			t.Fatalf("row width %d", len(row))
		}
		if row[len(exportColumns)] != "" {
			with7d++
		}
	}
	if with7d < (len(rows)-1)/2 {
		t.Errorf("only %d of %d rows carry a 7d reading", with7d, len(rows)-1)
	}
	win := get(r, prefix+"/export", url.Values{"auth_index": {codexIdx}, "from": {fmt.Sprint(f.now.Add(-24 * time.Hour).Unix())}})
	wantStatus(t, win, 200, "window export")
	if !strings.HasPrefix(string(win.Body), strings.Join(exportColumns, ",")+",used_7d,used_5h\n") {
		t.Errorf("codex header = %q", strings.SplitN(string(win.Body), "\n", 2)[0])
	}
	wantStatus(t, get(r, prefix+"/export", url.Values{"auth_index": {codexIdx}, "cycle_id": {fmt.Sprint(cs[0].ID)}}), 404, "other account's cycle")
	wantStatus(t, get(r, prefix+"/export", nil), 400, "missing auth_index")
	wantStatus(t, get(r, prefix+"/export", url.Values{"auth_index": {codexIdx}, "from": {"x"}}), 400, "bad from")
}

func TestGradeThresholdsMatchEstimate(t *testing.T) {
	if g := estimate.Grade(estimate.GradeInput{Ticks: minTicks - 1, Crossings: 100, LagStable: true}); g != estimate.GradeInsufficient {
		t.Errorf("ticks below minTicks graded %s", g)
	}
	if g := estimate.Grade(estimate.GradeInput{Ticks: 100, Crossings: minCrossings - 1, LagStable: true}); g != estimate.GradeInsufficient {
		t.Errorf("crossings below minCrossings graded %s", g)
	}
	if g := estimate.Grade(estimate.GradeInput{Ticks: minTicks, Crossings: minCrossings, LagStable: true}); g == estimate.GradeInsufficient {
		t.Error("thresholds themselves should be gradeable")
	}
}

func TestDashboardResource(t *testing.T) {
	r, _, _ := newRouter(t)
	for _, path := range []string{"/v0/resource/plugins/cpa-subscription-value/dashboard", prefix + "/dashboard"} {
		resp := get(r, path, nil)
		wantStatus(t, resp, 200, path)
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
	r, _, _ := newRouter(t)
	for _, path := range []string{prefix + "/nope", "/v0/management" + prefix + "/", "/v0/management/other-plugin/health"} {
		resp := get(r, path, nil)
		wantStatus(t, resp, 404, path)
		if m := decode(t, resp); m["error"] == nil {
			t.Errorf("%s: no error field: %v", path, m)
		}
	}
	wantStatus(t, post(r, prefix+"/health", nil, ""), 405, "POST health")
	wantStatus(t, r.Handle(abi.ManagementRequest{Method: "HEAD", Path: prefix + "/accounts"}), 200, "HEAD accounts")
}

func TestParseTime(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
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
