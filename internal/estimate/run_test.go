package estimate

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/sim"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// ingestCorpus stores a corpus the way the writer goroutine would: each
// event with its header readings, polls standalone. Returns the account id.
func ingestCorpus(t *testing.T, st *store.Store, c sim.Corpus) int64 {
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
	return acct.ID
}

type dump struct {
	Cycles    []store.Cycle
	Segments  [][]store.Segment
	Estimates [][]store.Estimate
	Assigned  int
}

func dumpMeter(t *testing.T, st *store.Store, acct int64, meterKey string) dump {
	t.Helper()
	cs, err := st.ListCycles(acct, meterKey, 5000)
	if err != nil {
		t.Fatal(err)
	}
	d := dump{Cycles: cs}
	for _, c := range cs {
		sg, err := st.ListSegments(c.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		es, err := st.ListEstimates(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		d.Segments = append(d.Segments, sg)
		d.Estimates = append(d.Estimates, es)
	}
	rs, err := loadReadings(st, acct, meterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.CycleID != nil {
			d.Assigned++
		}
	}
	return d
}

func TestRecomputePersistsAndIsIdempotent(t *testing.T) {
	sc := sim.DefaultScenario(5, domain.ProviderClaude)
	sc.Cycles, sc.EarlyResetCycle, sc.ShiftCycle = 2, -1, -1
	c := sim.Generate(sc)
	st, err := store.Open(filepath.Join(t.TempDir(), "run.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acct := ingestCorpus(t, st, c)
	mid := c.End.Add(-3 * 24 * time.Hour) // second cycle still open

	if err := Recompute(st, acct, domain.MeterSevenDay, mid); err != nil {
		t.Fatal(err)
	}
	first := dumpMeter(t, st, acct, domain.MeterSevenDay)
	if len(first.Cycles) != 2 {
		t.Fatalf("cycles = %d", len(first.Cycles))
	}
	// Newest first: [open running, closed final].
	open, closed := first.Estimates[0], first.Estimates[1]
	if len(open) != 1 || open[0].Kind != KindRunning || len(closed) != 1 || closed[0].Kind != KindFinal {
		t.Fatalf("estimates: open %+v closed %+v", open, closed)
	}
	if closed[0].PriceHash != sim.PriceHash || closed[0].Grade == GradeInsufficient || closed[0].Lag == "" {
		t.Fatalf("final %+v", closed[0])
	}
	var mix storedMix
	if err := json.Unmarshal(open[0].Mix, &mix); err != nil || mix.Diag.Forecast == nil || len(mix.ByFamily) == 0 {
		t.Fatalf("running mix_json %s (%v)", open[0].Mix, err)
	}
	for i, sg := range first.Segments {
		lags := map[string]int{}
		for _, s := range sg {
			lags[s.Lag]++
		}
		if len(lags) != 4 {
			t.Fatalf("cycle %d segments by lag %v", i, lags)
		}
	}
	if first.Assigned == 0 {
		t.Fatal("no readings assigned to cycles")
	}

	// Pure Analyze over the same stored data agrees with what was persisted.
	rs, _ := loadReadings(st, acct, domain.MeterSevenDay)
	evs, _ := loadEvents(st, acct, time.Time{})
	o := DefaultAnalyzeOptions()
	o.Series = "1"
	res := Analyze(domain.MeterSevenDay, rs, evs, mid, o)
	if res[0].Result.VHat != closed[0].VHat || res[1].Result.VHat != open[0].VHat {
		t.Fatalf("persisted V̂ %v/%v, analyze %v/%v", closed[0].VHat, open[0].VHat, res[0].Result.VHat, res[1].Result.VHat)
	}

	// Same data, same now: nothing changes. Replaying every event (dedup
	// makes it a no-op) and recomputing again: still byte-identical.
	if err := Recompute(st, acct, domain.MeterSevenDay, mid); err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Events {
		e.ID = 0
		if ok, err := st.InsertEvent(&e, nil); err != nil || ok {
			t.Fatalf("replayed event inserted (ok=%v err=%v)", ok, err)
		}
	}
	if err := Recompute(st, acct, domain.MeterSevenDay, mid); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(dumpMeter(t, st, acct, domain.MeterSevenDay))
	if string(a) != string(b) {
		t.Fatal("recompute on unchanged data changed the tables")
	}

	// After the end every cycle is final; the frozen one is untouched and the
	// open one gains its final.
	if err := Recompute(st, acct, domain.MeterSevenDay, c.End.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	last := dumpMeter(t, st, acct, domain.MeterSevenDay)
	if last.Cycles[1].ID != first.Cycles[1].ID || len(last.Estimates[1]) != 1 || last.Estimates[1][0].ComputedAt != closed[0].ComputedAt {
		t.Fatalf("frozen cycle was rewritten: %+v", last.Estimates[1])
	}
	if es := last.Estimates[0]; len(es) != 2 || es[1].Kind != KindFinal || last.Cycles[0].EndReason == "" {
		t.Fatalf("open cycle did not close: %+v / %+v", last.Cycles[0], es)
	}
}
