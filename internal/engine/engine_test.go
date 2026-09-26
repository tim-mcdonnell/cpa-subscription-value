package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/config"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/pricing"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/sim"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// ingestCorpus mirrors the writer goroutine: events with their header
// readings, polls standalone.
func ingestCorpus(t *testing.T, st *store.Store, c sim.Corpus) domain.Account {
	t.Helper()
	acct, err := st.UpsertAccount(domain.Account{Provider: c.Scenario.Provider, AuthIndex: "51a1c0de", AuthID: "sim.json"})
	if err != nil {
		t.Fatal(err)
	}
	byEvent := map[int64][]domain.MeterReading{}
	var polls []domain.MeterReading
	for _, r := range c.Readings {
		r.AccountID = acct.ID
		if r.EventID == nil {
			polls = append(polls, r)
		} else {
			byEvent[*r.EventID] = append(byEvent[*r.EventID], r)
		}
	}
	for _, e := range c.Events {
		rs := byEvent[e.ID]
		e.ID, e.AccountID = 0, acct.ID
		if _, err := st.InsertEvent(&e, rs); err != nil {
			t.Fatal(err)
		}
	}
	for i := range polls {
		polls[i].ID = 0
		if err := st.InsertReading(&polls[i]); err != nil {
			t.Fatal(err)
		}
	}
	return acct
}

func newEngine(t *testing.T, st *store.Store) *Engine {
	t.Helper()
	cat := pricing.NewCatalog(st, config.Defaults(), nil, nil)
	return New(st, cat, nil, Options{Debounce: 10 * time.Millisecond})
}

func TestRecomputeThenFitWeights(t *testing.T) {
	st := openStore(t)
	sc := sim.DefaultScenario(7, domain.ProviderClaude)
	sc.Cycles = 2
	acct := ingestCorpus(t, st, sim.Generate(sc))
	e := newEngine(t, st)

	if err := e.Recompute(acct.ID, domain.MeterSevenDay); err != nil {
		t.Fatal(err)
	}
	cycles, err := st.ListCycles(acct.ID, domain.MeterSevenDay, 10)
	if err != nil || len(cycles) == 0 {
		t.Fatalf("cycles %v err %v", cycles, err)
	}
	ests, err := st.LatestEstimatesPerCycle(acct.ID, domain.MeterSevenDay)
	if err != nil || len(ests) == 0 {
		t.Fatalf("estimates %v err %v", ests, err)
	}
	var at time.Time
	if found, _ := st.GetSetting(store.RecomputeSettingKey("51a1c0de", domain.MeterSevenDay), &at); !found || at.IsZero() {
		t.Fatal("last_recompute setting not written")
	}

	if err := e.FitWeights(acct, domain.MeterSevenDay); err != nil {
		t.Fatal(err)
	}
	fit, found, err := st.LatestWeightFit(acct.ID, domain.MeterSevenDay)
	if err != nil || !found {
		t.Fatalf("weight fit missing: %v", err)
	}
	if fit.Lag == "" || len(fit.Factors) == 0 {
		t.Fatalf("fit %+v", fit)
	}
}

// Dirty marks coalesce and drain through Run; a second Run with no changes
// leaves the estimate tables unchanged.
func TestRunDrainsDirtyMarks(t *testing.T) {
	st := openStore(t)
	sc := sim.DefaultScenario(3, domain.ProviderCodex)
	sc.Cycles = 1
	acct := ingestCorpus(t, st, sim.Generate(sc))
	e := newEngine(t, st)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	e.MarkDirty(acct.ID, domain.MeterSevenDay)
	e.MarkDirty(acct.ID, domain.MeterSevenDay)
	e.MarkDirty(acct.ID, domain.MeterFiveHour)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var at time.Time
		if found, _ := st.GetSetting(store.RecomputeSettingKey("51a1c0de", domain.MeterFiveHour), &at); found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	for _, m := range []string{domain.MeterSevenDay, domain.MeterFiveHour} {
		cs, err := st.ListCycles(acct.ID, m, 10)
		if err != nil || len(cs) == 0 {
			t.Fatalf("meter %s: cycles %v err %v", m, cs, err)
		}
	}
}

func TestRecordRestartGap(t *testing.T) {
	st := openStore(t)
	e := newEngine(t, st)
	last := time.Now().Add(-2 * time.Hour)
	e.NoteEvent(last)

	// Too short to count.
	e.RecordRestart(last.Add(time.Minute))
	if len(e.gaps) != 0 {
		t.Fatalf("gap recorded for a short restart: %v", e.gaps)
	}
	started := time.Now()
	e.RecordRestart(started)
	if len(e.gaps) != 1 || !e.gaps[0].From.Equal(last) || !e.gaps[0].To.Equal(started) {
		t.Fatalf("gaps %v", e.gaps)
	}
	// Survives a new engine over the same store.
	e2 := newEngine(t, st)
	if len(e2.gaps) != 1 {
		t.Fatalf("gaps not persisted: %v", e2.gaps)
	}
}

func TestAdaptSegmentsSkipsFlaggedAndWeightsLowUSD(t *testing.T) {
	c := store.Cycle{ID: 9, RegimeSeq: 1, Tick: 0.01}
	rows := []store.Segment{
		{Lag: "time", DeltaTicks: 2, TEnd: time.Unix(100, 0), Features: []byte(`{"tokens":{"claude-opus-5":{"uncached_input":1000,"cache_read":5000,"output":200}}}`)},
		{Lag: "time", DeltaTicks: 1, Flags: []string{"low_usd"}, Features: []byte(`{"tokens":{"claude-opus-5":{"output":50}}}`)},
		{Lag: "time", DeltaTicks: 1, Flags: []string{"quota_drop"}, Features: []byte(`{"tokens":{"claude-opus-5":{"output":50}}}`)},
		{Lag: "time", DeltaTicks: 1, Features: []byte(`{}`)},
	}
	segs := adaptSegments(c, rows)
	if len(segs) != 2 {
		t.Fatalf("want 2 segments, got %d", len(segs))
	}
	if segs[0].CycleKey != "9" || segs[0].RegimeKey != "1" || segs[0].Tick != 0.01 || segs[0].DeltaTicks != 2 || len(segs[0].Features) != 3 {
		t.Fatalf("seg0 %+v", segs[0])
	}
	if segs[1].Weight != 0.5 {
		t.Fatalf("low_usd weight %v", segs[1].Weight)
	}
}
