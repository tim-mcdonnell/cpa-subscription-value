package meter

import (
	"math"
	"regexp"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

func TestCrossingsMonotoneAndFiltered(t *testing.T) {
	A := t0.Add(week)
	us := []float64{0.10, 0.12, 0.11, 0.12, 0.15, 0.14, 0.16, 0.17, 0.18}
	var steps []step
	for i, u := range us {
		steps = append(steps, step{at: time.Duration(i) * time.Minute, u: u, anchor: A})
	}
	var rs []AssignedReading
	for i, r := range build(steps) {
		ar := AssignedReading{MeterReading: r}
		switch i {
		case 7:
			ar.Failed = true
		case 8:
			ar.QuotaDrop = true
		}
		rs = append(rs, ar)
	}
	rs[6].Source = domain.SourcePoll
	got := Crossings(rs, 0.01, domain.SourceHeader)
	want := []struct{ level, delta, id int64 }{{12, 2, 2}, {15, 3, 5}}
	if len(got) != len(want) {
		t.Fatalf("crossings = %+v", got)
	}
	for i, w := range want {
		if got[i].Level != w.level || got[i].Delta != w.delta || got[i].ReadingID != w.id {
			t.Fatalf("crossing %d = %+v, want %+v", i, got[i], w)
		}
	}
	polls := Crossings(rs, 0.01, domain.SourcePoll)
	if len(polls) != 0 {
		t.Fatalf("a lone poll reading is a baseline, not a crossing: %+v", polls)
	}
}

// Six events 10 s apart, $1 each (all input), crossings on events 2, 4 and 6.
func segmentFixture() (CycleAssignment, []Crossing, *EventIndex) {
	var evs []domain.UsageEvent
	for i := 1; i <= 6; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		evs = append(evs, domain.UsageEvent{
			ID: int64(i), Provider: domain.ProviderClaude, ModelFamily: "claude-opus-5",
			RequestedAt: at.Add(-2 * time.Second), ObservedAt: at, Latency: 5 * time.Second,
			UncachedInput: 1000, Output: 200, APIUSD: float64(i),
		})
	}
	var xs []Crossing
	for k, id := range []int64{2, 4, 6} {
		xs = append(xs, Crossing{ReadingID: id, EventID: id, At: t0.Add(time.Duration(id) * 10 * time.Second), Level: int64(k + 1), Delta: 1})
	}
	c := CycleAssignment{Tick: 0.01}
	return c, xs, NewEventIndex(evs, nil)
}

func TestBuildSegmentsIndexLags(t *testing.T) {
	c, xs, ix := segmentFixture()
	o := SegmentOptions{}
	tests := []struct {
		lag  Lag
		want []float64
	}{
		{LagIdx0, []float64{3 + 4, 5 + 6}},
		{LagIdx1, []float64{2 + 3, 4 + 5}},
		{LagIdx2, []float64{1 + 2, 3 + 4}},
	}
	for _, tc := range tests {
		segs := BuildSegments(c, xs, ix, tc.lag, o)
		if len(segs) != 2 {
			t.Fatalf("%s: %d segments", tc.lag, len(segs))
		}
		for i, s := range segs {
			if math.Abs(s.USD-tc.want[i]) > 1e-9 || s.NEvents != 2 || s.DeltaTicks != 1 {
				t.Fatalf("%s seg %d: usd=%v n=%d", tc.lag, i, s.USD, s.NEvents)
			}
		}
	}
}

func TestBuildSegmentsTimeLagSplitsInputAndOutput(t *testing.T) {
	c, xs, ix := segmentFixture()
	segs := BuildSegments(c, xs, ix, LagTime, SegmentOptions{})
	// Output share under RatioSplit: 5·200 / (1000 + 5·200) = 1/2.
	// Seg 1 = (20s, 40s]: inputs of events 3, 4 (requested at 28s, 38s) and
	// outputs of events 2, 3 (completing at 23s, 33s); event 4's output lands at 43s.
	want0 := 0.5*3 + 0.5*4 + 0.5*2 + 0.5*3
	if math.Abs(segs[0].USD-want0) > 1e-9 {
		t.Fatalf("seg 0 usd = %v, want %v", segs[0].USD, want0)
	}
	if got := segs[0].Tokens["claude-opus-5"]; got.UncachedInput != 2000 || got.Output != 400 {
		t.Fatalf("seg 0 tokens = %+v", got)
	}
	if segs[0].USDByType.Output+segs[0].USDByType.UncachedInput != segs[0].USD {
		t.Fatalf("by-type does not sum to usd: %+v", segs[0].USDByType)
	}
}

func TestBuildSegmentsFlags(t *testing.T) {
	c, xs, ix := segmentFixture()
	c.Readings = []AssignedReading{{MeterReading: domain.MeterReading{ObservedAt: t0.Add(30 * time.Second)}, QuotaDrop: true}}
	c.Ambiguous = []Interval{{From: t0.Add(45 * time.Second), To: t0.Add(50 * time.Second)}}
	o := SegmentOptions{Horizon: t0.Add(65 * time.Second), IncompleteWindow: 10 * time.Second, LongGap: time.Hour}
	segs := BuildSegments(c, xs, ix, LagTime, o)
	if !segs[0].Has(FlagQuotaDrop) || segs[0].Has(FlagRegimeChange) || segs[0].Has(FlagLagIncomplete) {
		t.Fatalf("seg 0 flags = %v", segs[0].Flags)
	}
	if segs[1].Has(FlagQuotaDrop) || !segs[1].Has(FlagRegimeChange) || !segs[1].Has(FlagLagIncomplete) {
		t.Fatalf("seg 1 flags = %v", segs[1].Flags)
	}
	o.LongGap = 15 * time.Second
	ix.Events[4].ObservedAt = t0.Add(58 * time.Second) // 18 s of silence inside seg 1
	if segs := BuildSegments(c, xs, ix, LagIdx0, o); !segs[1].Has(FlagLongGap) {
		t.Fatalf("expected long_gap, flags = %v", segs[1].Flags)
	}
}

func TestScopeEvents(t *testing.T) {
	evs := []domain.UsageEvent{{ID: 1, ModelFamily: "claude-opus-5"}, {ID: 2, ModelFamily: "claude-fable-5"}}
	scopes := DefaultScopes()
	if got := ScopeEvents("7d_oi", evs, scopes); len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("7d_oi scope = %+v", got)
	}
	if got := ScopeEvents("7d", evs, scopes); len(got) != 2 {
		t.Fatalf("7d scope = %+v", got)
	}
	custom := map[string]*regexp.Regexp{"weekly_scoped:opus": regexp.MustCompile(`opus`)}
	if !IsScoped("weekly_scoped:opus", custom) || IsScoped("7d", custom) {
		t.Fatal("IsScoped disagrees with the table")
	}
}
