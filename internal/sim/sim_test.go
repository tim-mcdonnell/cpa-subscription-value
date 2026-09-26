package sim

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/ingest"
)

func TestGenerateIsDeterministic(t *testing.T) {
	render := func(seed uint64) string {
		c := Generate(DefaultScenario(seed, domain.ProviderCodex))
		b, err := json.Marshal([]any{c.Events, c.Readings, c.Truth})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if render(9) != render(9) {
		t.Fatal("same seed, different corpus")
	}
	if render(9) == render(10) {
		t.Fatal("different seeds, same corpus")
	}
}

func TestCorpusMatchesIngestShapes(t *testing.T) {
	for _, p := range []domain.Provider{domain.ProviderClaude, domain.ProviderCodex} {
		c := Generate(DefaultScenario(2, p))
		if len(c.Events) != len(c.Records) || len(c.Events) < 5000 {
			t.Fatalf("%s: %d events from %d records", p, len(c.Events), len(c.Records))
		}
		byID := map[int64]domain.UsageEvent{}
		var lastID int64
		for _, e := range c.Events {
			if e.ID <= lastID || e.AccountID != 1 || e.PriceHash != PriceHash || e.Provider != p {
				t.Fatalf("%s: event %+v", p, e)
			}
			lastID = e.ID
			byID[e.ID] = e
		}
		// Re-normalizing a record reproduces its event exactly.
		for _, i := range []int{0, len(c.Records) / 2, len(c.Records) - 1} {
			res, ok := ingest.Normalize(c.Records[i], Pricer{})
			if !ok {
				t.Fatalf("%s: record %d rejected by ingest", p, i)
			}
			found := false
			for _, e := range c.Events {
				if e.DedupKey == res.Event.DedupKey {
					res.Event.ID, res.Event.AccountID = e.ID, e.AccountID
					a, _ := json.Marshal(e)
					b, _ := json.Marshal(res.Event)
					found = string(a) == string(b)
				}
			}
			if !found {
				t.Fatalf("%s: record %d does not normalize to its stored event", p, i)
			}
		}
		keys := map[string]int{}
		polls := 0
		for _, r := range c.Readings {
			keys[r.MeterKey]++
			if r.Source == domain.SourcePoll {
				polls++
				if r.EventID != nil || r.PrecisionDP != 2 {
					t.Fatalf("%s: poll %+v", p, r)
				}
				continue
			}
			ev, ok := byID[*r.EventID]
			if !ok || !r.ObservedAt.Equal(ev.ObservedAt) || r.ResetAt.IsZero() {
				t.Fatalf("%s: header reading %+v", p, r)
			}
			if r.MeterKey == domain.MeterSevenDayOI && !strings.Contains(ev.ModelFamily, "fable") {
				t.Fatalf("7d_oi on a %s response", ev.ModelFamily)
			}
			switch {
			case p == domain.ProviderCodex && r.PrecisionDP != 2:
				t.Fatalf("codex integer percent should be 2 dp: %+v", r)
			case p == domain.ProviderClaude && r.ObservedAt.After(c.Truth.PrecisionChangeAt) && r.PrecisionDP > 2:
				t.Fatalf("claude after the precision change: %+v", r)
			}
		}
		wantPolls := int(c.End.Sub(c.Scenario.Start) / (20 * time.Minute))
		if keys[domain.MeterSevenDay] == 0 || keys[domain.MeterFiveHour] == 0 || polls < wantPolls {
			t.Fatalf("%s: meters %v, %d polls (want ≥ %d)", p, keys, polls, wantPolls)
		}
		if p == domain.ProviderClaude && keys[domain.MeterSevenDayOI] == 0 {
			t.Fatal("no 7d_oi readings on fable responses")
		}
	}
}

func TestClaudeRawSpelling(t *testing.T) {
	for _, tc := range []struct {
		u    float64
		dp   int
		want string
	}{{0, 2, "0.0"}, {0.5, 2, "0.5"}, {0.639, 2, "0.63"}, {0.63, 2, "0.63"}, {1, 2, "1.0"}, {0.63129, 4, "0.6312"}} {
		if got := claudeRaw(tc.u, tc.dp); got != tc.want {
			t.Errorf("claudeRaw(%v, %d) = %q, want %q", tc.u, tc.dp, got, tc.want)
		}
	}
}

func TestTruthMatchesScenario(t *testing.T) {
	sc := DefaultScenario(4, domain.ProviderClaude)
	sc.Beta = 0.2
	c := Generate(sc)
	tr := c.Truth
	if len(tr.Periods) != sc.Cycles+1 || tr.EarlyResetAt.IsZero() || tr.ShiftAt.IsZero() || tr.PrecisionChangeAt.IsZero() {
		t.Fatalf("truth %+v", tr)
	}
	for _, p := range tr.Periods {
		if p.Regime == 0 && p.Cycle != sc.EarlyResetCycle && (p.Peak < 0.5 || p.Peak > 0.95) {
			t.Errorf("cycle %d peaks at %.3f, outside the 60–90%% target band", p.Cycle, p.Peak)
		}
		if math.Abs(p.UnroutedFrac-0.2) > 0.01 {
			t.Errorf("cycle %d/%d: unrouted share %.3f, want 0.2", p.Cycle, p.Regime, p.UnroutedFrac)
		}
		// Cache reads count 0.5× on the hidden meter, so a dollar of API
		// spend moves it less than at list price: V_eff > V_base.
		if p.VEff <= p.VBase {
			t.Errorf("cycle %d: V_eff %.1f ≤ V_base %.1f", p.Cycle, p.VEff, p.VBase)
		}
		if want := tr.V7d; p.Cycle >= sc.ShiftCycle {
			if math.Abs(p.VBase-want*sc.ShiftFactor) > 1e-9 {
				t.Errorf("cycle %d VBase %.2f, want shifted %.2f", p.Cycle, p.VBase, want*sc.ShiftFactor)
			}
		}
	}
	// The hidden meter is monotone within a period and resets at its end.
	p := tr.Periods[0]
	prev := -1.0
	for at := p.Start; at.Before(p.End); at = at.Add(time.Hour) {
		u := c.TrueU7(at)
		if u < prev {
			t.Fatalf("hidden meter fell at %s", at)
		}
		prev = u
	}
	if u := c.TrueU7(tr.Periods[1].Start); u > 0.01 {
		t.Fatalf("meter did not reset: %.3f", u)
	}
}
