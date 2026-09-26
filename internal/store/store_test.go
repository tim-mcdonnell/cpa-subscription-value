package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "dir", "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func mustAccount(t *testing.T, s *Store, provider domain.Provider, authIndex string) domain.Account {
	t.Helper()
	a, err := s.UpsertAccount(domain.Account{Provider: provider, AuthIndex: authIndex, AuthID: authIndex + ".json"})
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	return a
}

var base = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func event(accountID int64, key string, at time.Time) domain.UsageEvent {
	return domain.UsageEvent{
		AccountID:     accountID,
		DedupKey:      key,
		RequestedAt:   at.Add(-2 * time.Second),
		ObservedAt:    at,
		Latency:       2 * time.Second,
		Model:         "claude-opus-5-5",
		ModelFamily:   "claude-opus",
		UncachedInput: 100,
		CacheRead:     5000,
		CacheWrite:    200,
		Output:        300,
		APIUSD:        0.0123,
		PriceHash:     "h1",
		HeadersJSON:   json.RawMessage(`{"anthropic-ratelimit-unified-5h-utilization":["0.42"]}`),
	}
}

func reading(meter string, at time.Time, u float64) domain.MeterReading {
	return domain.MeterReading{
		MeterKey:     meter,
		Source:       domain.SourceHeader,
		ObservedAt:   at,
		UsedFraction: u,
		Raw:          "0.42",
		PrecisionDP:  2,
		ResetAt:      at.Add(3 * time.Hour).Truncate(time.Second),
		WindowSec:    18000,
	}
}

func TestMigrationsIdempotentOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := s.SchemaVersion()
	if v1 != len(migrations) {
		t.Fatalf("version %d, want %d", v1, len(migrations))
	}
	a := mustAccount(t, s, domain.ProviderClaude, "abc12345")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	v2, _ := s2.SchemaVersion()
	if v2 != v1 {
		t.Fatalf("version after reopen %d, want %d", v2, v1)
	}
	var n int
	if err := s2.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(migrations) {
		t.Fatalf("schema_migrations rows=%d err=%v", n, err)
	}
	got, ok, err := s2.GetAccountByAuthIndex("abc12345")
	if err != nil || !ok || got.ID != a.ID {
		t.Fatalf("account lost across reopen: ok=%v id=%d err=%v", ok, got.ID, err)
	}
	var fk int
	if err := s2.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys=%d err=%v", fk, err)
	}
	var mode string
	if err := s2.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode=%q err=%v", mode, err)
	}
}

func TestUpsertAccount(t *testing.T) {
	s, _ := openTemp(t)
	t0 := base
	a, err := s.UpsertAccount(domain.Account{
		Provider: domain.ProviderClaude, AuthIndex: "idx1", AuthID: "claude-tim.json",
		Email: "t@example.com", FirstSeen: t0, LastSeen: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || a.Email != "t@example.com" || !a.FirstSeen.Equal(t0) || !a.LastSeen.Equal(t0) {
		t.Fatalf("insert: %+v", a)
	}

	// Update: empty email/plan must not blank; older last_seen must not regress.
	b, err := s.UpsertAccount(domain.Account{
		Provider: domain.ProviderClaude, AuthIndex: "idx1", PlanType: "max_20x",
		FirstSeen: t0.Add(-time.Hour), LastSeen: t0.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID {
		t.Fatalf("id changed %d -> %d", a.ID, b.ID)
	}
	if b.Email != "t@example.com" || b.AuthID != "claude-tim.json" || b.PlanType != "max_20x" {
		t.Fatalf("update fields: %+v", b)
	}
	if !b.FirstSeen.Equal(t0) || !b.LastSeen.Equal(t0) {
		t.Fatalf("times regressed: %+v", b)
	}
	c, err := s.UpsertAccount(domain.Account{Provider: domain.ProviderClaude, AuthIndex: "idx1", LastSeen: t0.Add(time.Hour)})
	if err != nil || !c.LastSeen.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last_seen did not advance: %+v err=%v", c, err)
	}

	// Same auth_index under another provider is a distinct row.
	d := mustAccount(t, s, domain.ProviderCodex, "idx2")
	list, err := s.ListAccounts()
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%d err=%v", len(list), err)
	}
	if list[0].Provider != domain.ProviderClaude || list[1].ID != d.ID {
		t.Fatalf("order: %+v", list)
	}
	if _, ok, _ := s.GetAccountByAuthIndex("nope"); ok {
		t.Fatal("found missing account")
	}
	if got, ok, _ := s.GetAccount(d.ID); !ok || got.AuthIndex != "idx2" {
		t.Fatalf("GetAccount: %+v %v", got, ok)
	}
}

func TestInsertEventDedupAndReadings(t *testing.T) {
	s, _ := openTemp(t)
	acct := mustAccount(t, s, domain.ProviderClaude, "idx1")
	ev := event(acct.ID, "k1", base)
	rs := []domain.MeterReading{reading("5h", base, 0.42), reading("7d", base, 0.10)}
	ok, err := s.InsertEvent(&ev, rs)
	if err != nil || !ok {
		t.Fatalf("first insert ok=%v err=%v", ok, err)
	}
	if ev.ID == 0 {
		t.Fatal("event id not set")
	}
	for i, r := range rs {
		if r.ID == 0 || r.EventID == nil || *r.EventID != ev.ID || r.AccountID != acct.ID {
			t.Fatalf("reading %d not linked: %+v", i, r)
		}
	}

	dup := event(acct.ID, "k1", base.Add(time.Minute))
	dupReadings := []domain.MeterReading{reading("5h", base.Add(time.Minute), 0.43)}
	ok, err = s.InsertEvent(&dup, dupReadings)
	if err != nil || ok {
		t.Fatalf("dup insert ok=%v err=%v", ok, err)
	}
	if dup.ID != 0 || dupReadings[0].ID != 0 {
		t.Fatalf("dup mutated ids: ev=%d r=%d", dup.ID, dupReadings[0].ID)
	}
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Events != 1 || st.Readings != 2 {
		t.Fatalf("stats after dup: %+v", st)
	}

	got, err := s.ListReadings(ReadingQuery{AccountID: acct.ID, MeterKey: "5h"})
	if err != nil || len(got) != 1 {
		t.Fatalf("list readings: %d %v", len(got), err)
	}
	r := got[0]
	if r.EventID == nil || *r.EventID != ev.ID || r.UsedFraction != 0.42 || r.Raw != "0.42" ||
		r.PrecisionDP != 2 || !r.ResetAt.Equal(rs[0].ResetAt) || r.WindowSec != 18000 || !r.ObservedAt.Equal(base) {
		t.Fatalf("reading round trip: %+v", r)
	}

	// Poll reading with no event.
	poll := domain.MeterReading{AccountID: acct.ID, MeterKey: "7d", Source: domain.SourcePoll,
		ObservedAt: base.Add(time.Hour), UsedFraction: 0.11, Raw: "11", PrecisionDP: 2}
	if err := s.InsertReading(&poll); err != nil || poll.ID == 0 {
		t.Fatalf("poll insert: id=%d err=%v", poll.ID, err)
	}
	polls, err := s.ListReadings(ReadingQuery{Source: domain.SourcePoll})
	if err != nil || len(polls) != 1 || polls[0].EventID != nil || polls[0].CycleID != nil {
		t.Fatalf("poll list: %+v err=%v", polls, err)
	}

	// Event round trip incl. joined account fields and raw headers.
	evs, err := s.ListEvents(EventQuery{AccountID: acct.ID})
	if err != nil || len(evs) != 1 {
		t.Fatalf("list events: %d %v", len(evs), err)
	}
	e := evs[0]
	if e.ID != ev.ID || e.Provider != domain.ProviderClaude || e.AuthIndex != "idx1" || e.AuthID != "idx1.json" ||
		e.Latency != 2*time.Second || e.CacheRead != 5000 || e.APIUSD != 0.0123 || e.PriceHash != "h1" ||
		string(e.HeadersJSON) != string(ev.HeadersJSON) || !e.RequestedAt.Equal(ev.RequestedAt) {
		t.Fatalf("event round trip: %+v", e)
	}
}

func TestListEventsOrderingAndLimits(t *testing.T) {
	s, _ := openTemp(t)
	a := mustAccount(t, s, domain.ProviderClaude, "a")
	b := mustAccount(t, s, domain.ProviderCodex, "b")
	for i := 0; i < 10; i++ {
		acct := a.ID
		if i%2 == 1 {
			acct = b.ID
		}
		ev := event(acct, "k"+string(rune('0'+i)), base.Add(time.Duration(i)*time.Minute))
		if ok, err := s.InsertEvent(&ev, nil); err != nil || !ok {
			t.Fatal(err)
		}
	}
	asc, err := s.ListEvents(EventQuery{})
	if err != nil || len(asc) != 10 {
		t.Fatalf("all: %d %v", len(asc), err)
	}
	for i := 1; i < len(asc); i++ {
		if !asc[i].ObservedAt.After(asc[i-1].ObservedAt) {
			t.Fatalf("not ascending at %d", i)
		}
	}
	desc, err := s.ListEvents(EventQuery{Desc: true, Limit: 3})
	if err != nil || len(desc) != 3 || !desc[0].ObservedAt.Equal(base.Add(9*time.Minute)) {
		t.Fatalf("desc limit: %d %v", len(desc), err)
	}
	onlyA, err := s.ListEvents(EventQuery{AccountID: a.ID})
	if err != nil || len(onlyA) != 5 {
		t.Fatalf("account filter: %d %v", len(onlyA), err)
	}
	// From inclusive, To exclusive.
	win, err := s.ListEvents(EventQuery{From: base.Add(2 * time.Minute), To: base.Add(5 * time.Minute)})
	if err != nil || len(win) != 3 || !win[0].ObservedAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("window: %d %v", len(win), err)
	}
	if clampLimit(0) != 200 || clampLimit(99999) != 5000 || clampLimit(7) != 7 {
		t.Fatal("clampLimit")
	}

	// Reprice the first batch, then page through what is still on the old hash.
	ids := []int64{asc[0].ID, asc[1].ID}
	if err := s.UpdateEventPrices(ids, []float64{1.5, 2.5}, []float64{1.6, 2.6}, "h2"); err != nil {
		t.Fatal(err)
	}
	page1, err := s.ListEventsForReprice("h2", 0, 5)
	if err != nil || len(page1) != 5 || page1[0].ID != asc[2].ID {
		t.Fatalf("page1: %d %v", len(page1), err)
	}
	page2, err := s.ListEventsForReprice("h2", page1[4].ID, 5)
	if err != nil || len(page2) != 3 || page2[2].ID != asc[9].ID {
		t.Fatalf("page2: %d %v", len(page2), err)
	}
	if page3, _ := s.ListEventsForReprice("h2", page2[2].ID, 5); len(page3) != 0 {
		t.Fatalf("page3 should be empty: %d", len(page3))
	}
	repriced, _ := s.ListEvents(EventQuery{Limit: 2})
	if repriced[1].APIUSD != 2.5 || repriced[1].APIUSDCacheWrite1h != 2.6 || repriced[1].PriceHash != "h2" {
		t.Fatalf("repriced: %+v", repriced[1])
	}
	if err := s.UpdateEventPrices(ids, []float64{1}, []float64{1, 2}, "h3"); err == nil {
		t.Fatal("mismatched lengths accepted")
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	type cov struct {
		Mode string  `json:"mode"`
		Tol  float64 `json:"tol"`
	}
	var out cov
	found, err := s.GetSetting("coverage", &out)
	if err != nil || found {
		t.Fatalf("missing key: found=%v err=%v", found, err)
	}
	if err := s.SetSetting("coverage", cov{Mode: "full", Tol: 0.05}); err != nil {
		t.Fatal(err)
	}
	found, err = s.GetSetting("coverage", &out)
	if err != nil || !found || out != (cov{Mode: "full", Tol: 0.05}) {
		t.Fatalf("round trip: %+v found=%v err=%v", out, found, err)
	}
	if err := s.SetSetting("coverage", cov{Mode: "partial"}); err != nil {
		t.Fatal(err)
	}
	s.GetSetting("coverage", &out)
	if out.Mode != "partial" || out.Tol != 0 {
		t.Fatalf("overwrite: %+v", out)
	}
	ts := base
	s.SetSetting("last_event_at", ts)
	var back time.Time
	if _, err := s.GetSetting("last_event_at", &back); err != nil || !back.Equal(ts) {
		t.Fatalf("time setting: %v %v", back, err)
	}
	if err := s.DeleteSetting("coverage"); err != nil {
		t.Fatal(err)
	}
	if found, _ := s.GetSetting("coverage", &out); found {
		t.Fatal("still present after delete")
	}
}

func TestStatsAndPrune(t *testing.T) {
	s, _ := openTemp(t)
	acct := mustAccount(t, s, domain.ProviderClaude, "a")
	for i := 0; i < 6; i++ {
		ev := event(acct.ID, "k"+string(rune('0'+i)), base.Add(time.Duration(i)*time.Hour))
		// One reading per event; give the last one a reading timestamped later
		// than its event to prove linked readings are pruned with the event.
		r := reading("5h", ev.ObservedAt, 0.1*float64(i))
		if i == 2 {
			r.ObservedAt = base.Add(48 * time.Hour)
		}
		if ok, err := s.InsertEvent(&ev, []domain.MeterReading{r}); err != nil || !ok {
			t.Fatal(err)
		}
	}
	old := domain.MeterReading{AccountID: acct.ID, MeterKey: "7d", Source: domain.SourcePoll, ObservedAt: base.Add(-time.Hour)}
	if err := s.InsertReading(&old); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Accounts != 1 || st.Events != 6 || st.Readings != 7 ||
		!st.OldestEvent.Equal(base) || !st.NewestEvent.Equal(base.Add(5*time.Hour)) || st.DBBytes <= 0 {
		t.Fatalf("stats: %+v", st)
	}

	// Cutoff at +3h: events 0,1,2 go (3 events), readings for 0,1 by time,
	// reading for 2 via its event link, plus the old poll = 4 readings.
	evDel, rdDel, err := s.PruneOlderThan(base.Add(3 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if evDel != 3 || rdDel != 4 {
		t.Fatalf("pruned events=%d readings=%d", evDel, rdDel)
	}
	st, _ = s.Stats()
	if st.Events != 3 || st.Readings != 3 || !st.OldestEvent.Equal(base.Add(3*time.Hour)) {
		t.Fatalf("after prune: %+v", st)
	}
	evDel, rdDel, err = s.PruneOlderThan(base.Add(3 * time.Hour))
	if err != nil || evDel != 0 || rdDel != 0 {
		t.Fatalf("second prune: %d %d %v", evDel, rdDel, err)
	}
	empty, _ := Open(filepath.Join(t.TempDir(), "e.db"))
	defer empty.Close()
	est, err := empty.Stats()
	if err != nil || !est.OldestEvent.IsZero() || !est.NewestEvent.IsZero() {
		t.Fatalf("empty stats: %+v %v", est, err)
	}
}

func TestCloseCheckpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acct := mustAccount(t, s, domain.ProviderClaude, "a")
	for i := 0; i < 50; i++ {
		ev := event(acct.ID, "k"+string(rune('A'+i)), base.Add(time.Duration(i)*time.Second))
		if _, err := s.InsertEvent(&ev, []domain.MeterReading{reading("5h", ev.ObservedAt, 0.5)}); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("expected non-empty WAL before close: %v", err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file missing: %v", err)
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Fatalf("WAL still %d bytes after close", fi.Size())
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	st, _ := s2.Stats()
	if st.Events != 50 || st.Readings != 50 {
		t.Fatalf("data lost through checkpoint: %+v", st)
	}
}

func TestDerivedTables(t *testing.T) {
	s, _ := openTemp(t)
	acct := mustAccount(t, s, domain.ProviderClaude, "a")
	anchor := base.Add(7 * 24 * time.Hour).Truncate(time.Second)
	c, err := s.UpsertCycle(Cycle{AccountID: acct.ID, MeterKey: "7d", WindowSec: 604800, ResetAnchor: anchor,
		StartsAt: base, FirstReadingAt: base, Tick: 0.01, PeakFraction: 0.3, Flags: []string{"precision_change"}})
	if err != nil || c.ID == 0 || !c.ClosedAt.IsZero() || len(c.Flags) != 1 {
		t.Fatalf("upsert cycle: %+v %v", c, err)
	}
	c2, err := s.UpsertCycle(Cycle{AccountID: acct.ID, MeterKey: "7d", WindowSec: 604800, ResetAnchor: anchor,
		StartsAt: base, FirstReadingAt: base, ClosedAt: anchor, EndReason: "reset", Tick: 0.01, PeakFraction: 0.9})
	if err != nil || c2.ID != c.ID || !c2.ClosedAt.Equal(anchor) || c2.EndReason != "reset" || c2.PeakFraction != 0.9 || len(c2.Flags) != 0 {
		t.Fatalf("cycle update kept id/fields: %+v %v", c2, err)
	}
	regime, err := s.UpsertCycle(Cycle{AccountID: acct.ID, MeterKey: "7d", ResetAnchor: anchor, RegimeSeq: 1, Tick: 0.01})
	if err != nil || regime.ID == c.ID {
		t.Fatalf("regime cycle: %+v %v", regime, err)
	}
	cycles, err := s.ListCycles(acct.ID, "7d", 0)
	if err != nil || len(cycles) != 2 || cycles[0].ID != regime.ID {
		t.Fatalf("list cycles: %+v %v", cycles, err)
	}
	if got, ok, _ := s.GetCycle(c.ID); !ok || got.ResetAnchor != anchor {
		t.Fatalf("get cycle: %+v %v", got, ok)
	}

	rid := int64(7)
	segs := []Segment{
		{Lag: "time", Seq: 0, FromLevel: 0, ToLevel: 3, DeltaTicks: 3, ToReadingID: &rid, TStart: base, TEnd: base.Add(time.Hour),
			USD: 1.25, USDCacheWrite1h: 1.5, Features: json.RawMessage(`{"claude-opus":{"output":300}}`), NEvents: 4, Flags: []string{"low_usd"}},
		{Lag: "time", Seq: 1, FromLevel: 3, ToLevel: 4, DeltaTicks: 1, TStart: base.Add(time.Hour), TEnd: base.Add(2 * time.Hour), USD: 0.4},
		{Lag: "1", Seq: 0, FromLevel: 0, ToLevel: 3, DeltaTicks: 3, TStart: base, TEnd: base.Add(time.Hour), USD: 1.1},
	}
	if err := s.ReplaceSegments(c.ID, segs); err != nil {
		t.Fatal(err)
	}
	if segs[0].ID == 0 || segs[0].CycleID != c.ID {
		t.Fatalf("segment ids not set: %+v", segs[0])
	}
	got, err := s.ListSegments(c.ID, "time")
	if err != nil || len(got) != 2 || got[0].ToReadingID == nil || *got[0].ToReadingID != 7 ||
		string(got[0].Features) != `{"claude-opus":{"output":300}}` || got[0].Flags[0] != "low_usd" || len(got[1].Flags) != 0 {
		t.Fatalf("list segments: %+v %v", got, err)
	}
	if err := s.ReplaceSegments(c.ID, segs[:1]); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListSegments(c.ID, ""); len(all) != 1 {
		t.Fatalf("replace did not clear: %d", len(all))
	}

	run := Estimate{CycleID: c.ID, Kind: "running", Method: "header", Lag: "time", VHat: 900, CILo: 850, CIHi: 950,
		Grade: "medium", Mix: json.RawMessage(`{"claude-opus":0.8}`), PriceHash: "h1", ComputedAt: base}
	if err := s.InsertEstimate(&run); err != nil || run.ID == 0 {
		t.Fatalf("running: %v", err)
	}
	fin := Estimate{CycleID: c.ID, Kind: "final", VHat: 920, PriceHash: "h1", ComputedAt: base.Add(time.Hour), Flags: []string{"level_shift"}}
	if err := s.InsertEstimate(&fin); err != nil {
		t.Fatal(err)
	}
	fin2 := Estimate{CycleID: c.ID, Kind: "final", VHat: 925, PriceHash: "h1", ComputedAt: base.Add(2 * time.Hour)}
	if err := s.InsertEstimate(&fin2); err != nil || fin2.ID != fin.ID {
		t.Fatalf("second final should overwrite in place: id %d vs %d err=%v", fin2.ID, fin.ID, err)
	}
	rep := Estimate{CycleID: c.ID, Kind: "final", VHat: 1000, PriceHash: "h2", ComputedAt: base.Add(3 * time.Hour)}
	if err := s.InsertEstimate(&rep); err != nil || rep.ID == fin.ID {
		t.Fatalf("final under new hash should be a new row: %v", err)
	}
	hist, _ := s.ListEstimates(c.ID)
	if len(hist) != 3 || hist[1].VHat != 925 || len(hist[1].Flags) != 0 {
		t.Fatalf("history: %+v", hist)
	}
	latestFinal, ok, _ := s.LatestEstimate(c.ID, "final")
	if !ok || latestFinal.VHat != 1000 {
		t.Fatalf("latest final: %+v", latestFinal)
	}
	latestRun, ok, _ := s.LatestEstimate(c.ID, "running")
	if !ok || string(latestRun.Mix) != `{"claude-opus":0.8}` || latestRun.CILo != 850 {
		t.Fatalf("latest running: %+v", latestRun)
	}
	s.InsertEstimate(&Estimate{CycleID: regime.ID, Kind: "running", VHat: 5, ComputedAt: base})
	per, err := s.LatestEstimatesPerCycle(acct.ID, "7d")
	if err != nil || len(per) != 2 || per[0].CycleID != regime.ID || per[1].VHat != 1000 {
		t.Fatalf("per cycle: %+v %v", per, err)
	}

	if _, ok, _ := s.LatestWeightFit(acct.ID, "7d"); ok {
		t.Fatal("unexpected weight fit")
	}
	w := WeightFit{AccountID: acct.ID, MeterKey: "7d", Lag: "time", AnchorFamily: "claude-opus",
		Factors: json.RawMessage(`{"f":1}`), Loss: 0.2, PriceHash: "h1"}
	if err := s.InsertWeightFit(&w); err != nil || w.ID == 0 || w.ComputedAt.IsZero() {
		t.Fatalf("insert weight fit: %+v %v", w, err)
	}
	w2 := WeightFit{AccountID: acct.ID, MeterKey: "7d", ComputedAt: w.ComputedAt.Add(time.Hour), Loss: 0.1}
	s.InsertWeightFit(&w2)
	lw, ok, err := s.LatestWeightFit(acct.ID, "7d")
	if err != nil || !ok || lw.ID != w2.ID || lw.Loss != 0.1 || lw.Factors != nil {
		t.Fatalf("latest weight fit: %+v %v", lw, err)
	}

	// Readings pick up cycle ids.
	r := domain.MeterReading{AccountID: acct.ID, MeterKey: "7d", Source: domain.SourceHeader, ObservedAt: base}
	s.InsertReading(&r)
	if err := s.SetReadingCycles([]int64{r.ID}, &c.ID); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.ListReadings(ReadingQuery{MeterKey: "7d"})
	if len(rs) != 1 || rs[0].CycleID == nil || *rs[0].CycleID != c.ID {
		t.Fatalf("cycle id: %+v", rs)
	}
}

func TestPriceSnapshots(t *testing.T) {
	s, _ := openTemp(t)
	if _, _, ok, err := s.LatestPriceSnapshot(); ok || err != nil {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	rows := []Price{
		{Provider: "anthropic", ModelFamily: "claude-opus", Input: 4e-6, Output: 20e-6, CacheRead: 0.4e-6, CacheWrite5m: 5e-6, CacheWrite1h: 8e-6, LongCtxMult: 2, FastMult: 1, Source: "override"},
		{Provider: "openai", ModelFamily: "gpt-5-codex", Input: 1.25e-6, Output: 10e-6, CacheRead: 0.125e-6, LongCtxMult: 1, FastMult: 2.5, Source: "models.dev"},
	}
	ins, err := s.InsertPriceSnapshot(PriceSnapshot{Hash: "abc", Source: "models.dev", Raw: json.RawMessage(`{"x":1}`), CreatedAt: base}, rows)
	if err != nil || !ins {
		t.Fatalf("insert: %v %v", ins, err)
	}
	ins, err = s.InsertPriceSnapshot(PriceSnapshot{Hash: "abc"}, rows[:1])
	if err != nil || ins {
		t.Fatalf("dup snapshot: %v %v", ins, err)
	}
	snap, got, ok, err := s.GetPriceSnapshot("abc")
	if err != nil || !ok || snap.Source != "models.dev" || string(snap.Raw) != `{"x":1}` || !snap.CreatedAt.Equal(base) {
		t.Fatalf("get snapshot: %+v %v %v", snap, ok, err)
	}
	if len(got) != 2 || got[0] != rows[0] || got[1] != rows[1] {
		t.Fatalf("rows: %+v", got)
	}
	s.InsertPriceSnapshot(PriceSnapshot{Hash: "def", CreatedAt: base.Add(time.Hour)}, nil)
	latest, lrows, ok, _ := s.LatestPriceSnapshot()
	if !ok || latest.Hash != "def" || len(lrows) != 0 {
		t.Fatalf("latest: %+v %d", latest, len(lrows))
	}
	if _, _, ok, _ := s.GetPriceSnapshot("zzz"); ok {
		t.Fatal("found missing hash")
	}
}
