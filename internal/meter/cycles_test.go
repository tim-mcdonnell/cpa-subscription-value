package meter

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

const week = 7 * 24 * time.Hour

// script builds readings from (offset, u, anchor) triples; ids ascend.
type step struct {
	at     time.Duration
	u      float64
	anchor time.Time
	source string
}

func build(steps []step) []domain.MeterReading {
	var out []domain.MeterReading
	for i, s := range steps {
		src := s.source
		if src == "" {
			src = domain.SourceHeader
		}
		id := int64(i + 1)
		ev := id
		r := domain.MeterReading{
			ID: id, MeterKey: "7d", Source: src, ObservedAt: t0.Add(s.at),
			UsedFraction: s.u, Raw: fmt.Sprintf("%.2f", s.u), PrecisionDP: 2,
			ResetAt: s.anchor, WindowSec: int64(week / time.Second),
		}
		if src == domain.SourceHeader {
			r.EventID = &ev
		}
		out = append(out, r)
	}
	return out
}

type cycleSummary struct {
	anchor time.Time
	regime int
	reason string
	closed time.Time
	ids    []int64
	drops  []int64
	flags  []string
}

func summarize(cs []CycleAssignment) []cycleSummary {
	var out []cycleSummary
	for _, c := range cs {
		s := cycleSummary{anchor: c.ResetAnchor, regime: c.RegimeSeq, reason: c.EndReason, closed: c.ClosedAt, flags: c.Flags}
		for _, r := range c.Readings {
			s.ids = append(s.ids, r.ID)
			if r.QuotaDrop {
				s.drops = append(s.drops, r.ID)
			}
		}
		out = append(out, s)
	}
	return out
}

func ids(n ...int64) []int64 { return n }

func TestAssignCyclesScripted(t *testing.T) {
	A := t0.Add(week)
	B := A.Add(week)
	min := time.Minute
	tests := []struct {
		name  string
		steps []step
		now   time.Time
		want  []cycleSummary
	}{
		{
			name: "scheduled reset",
			steps: []step{
				{at: 1 * min, u: 0.00, anchor: A}, {at: 2 * min, u: 0.10, anchor: A},
				{at: week - min, u: 0.70, anchor: A},
				{at: week + 5*min, u: 0.00, anchor: B}, {at: week + 6*min, u: 0.01, anchor: B},
			},
			now: A.Add(time.Hour),
			want: []cycleSummary{
				{anchor: A, reason: EndReset, closed: A, ids: ids(1, 2, 3), flags: []string{}},
				{anchor: B, ids: ids(4, 5), flags: []string{}},
			},
		},
		{
			name: "early reset with a new anchor",
			steps: []step{
				{at: 1 * min, u: 0.20, anchor: A}, {at: 2 * time.Hour, u: 0.30, anchor: A},
				{at: 3 * time.Hour, u: 0.00, anchor: t0.Add(3*time.Hour + week)},
			},
			now: t0.Add(4 * time.Hour),
			want: []cycleSummary{
				{anchor: A, reason: EndEarlyReset, closed: t0.Add(3 * time.Hour), ids: ids(1, 2), flags: []string{}},
				{anchor: t0.Add(3*time.Hour + week), ids: ids(3), flags: []string{}},
			},
		},
		{
			name: "same-anchor early reset confirmed after 3 readings over 60s",
			steps: []step{
				{at: 1 * min, u: 0.30, anchor: A}, {at: 2 * min, u: 0.40, anchor: A},
				{at: 10 * min, u: 0.00, anchor: A}, {at: 10*min + 30*time.Second, u: 0.00, anchor: A},
				{at: 11*min + 10*time.Second, u: 0.01, anchor: A}, {at: 12 * min, u: 0.02, anchor: A},
			},
			now: t0.Add(time.Hour),
			want: []cycleSummary{
				{anchor: A, reason: EndEarlyReset, closed: t0.Add(10 * min), ids: ids(1, 2), flags: []string{}},
				{anchor: A, regime: 1, ids: ids(3, 4, 5, 6), flags: []string{}},
			},
		},
		{
			name: "transient drop rejected as quota_drop",
			steps: []step{
				{at: 1 * min, u: 0.30, anchor: A}, {at: 2 * min, u: 0.40, anchor: A},
				{at: 10 * min, u: 0.00, anchor: A}, {at: 10*min + 20*time.Second, u: 0.01, anchor: A},
				{at: 11 * min, u: 0.41, anchor: A}, {at: 12 * min, u: 0.42, anchor: A},
			},
			now: t0.Add(time.Hour),
			want: []cycleSummary{
				{anchor: A, ids: ids(1, 2, 3, 4, 5, 6), drops: ids(3, 4), flags: []string{}},
			},
		},
		{
			name: "one stale high reading during pending does not block confirmation",
			steps: []step{
				{at: 1 * min, u: 0.40, anchor: A},
				{at: 10 * min, u: 0.00, anchor: A}, {at: 10*min + 5*time.Second, u: 0.40, anchor: A},
				{at: 10*min + 40*time.Second, u: 0.00, anchor: A}, {at: 11*min + 5*time.Second, u: 0.01, anchor: A},
			},
			now: t0.Add(time.Hour),
			want: []cycleSummary{
				{anchor: A, reason: EndEarlyReset, closed: t0.Add(10 * min), ids: ids(1, 3), flags: []string{}},
				{anchor: A, regime: 1, ids: ids(2, 4, 5), flags: []string{}},
			},
		},
		{
			name: "poll readings never start a pending early reset",
			steps: []step{
				{at: 1 * min, u: 0.40, anchor: A},
				{at: 10 * min, u: 0.00, anchor: A, source: domain.SourcePoll},
				{at: 11 * min, u: 0.41, anchor: A},
			},
			now:  t0.Add(time.Hour),
			want: []cycleSummary{{anchor: A, ids: ids(1, 2, 3), flags: []string{}}},
		},
		{
			name: "A→B→A revert flags both cycles",
			steps: []step{
				{at: 1 * min, u: 0.30, anchor: A},
				{at: time.Hour, u: 0.00, anchor: B.Add(-3 * 24 * time.Hour)}, // B'
				{at: time.Hour + 30*time.Second, u: 0.31, anchor: A},         // late stream: no revert
				{at: time.Hour + 20*min, u: 0.32, anchor: A},                 // persists: revert
				{at: time.Hour + 21*min, u: 0.33, anchor: A},
			},
			now: t0.Add(2 * time.Hour),
			want: []cycleSummary{
				{anchor: A, ids: ids(1, 3, 4, 5), flags: []string{FlagRegimeChange}},
				{anchor: B.Add(-3 * 24 * time.Hour), reason: EndRegime, closed: t0.Add(time.Hour + 20*min), ids: ids(2), flags: []string{FlagRegimeChange}},
			},
		},
		{
			name: "stale anchor that already passed is dropped",
			steps: []step{
				{at: week - min, u: 0.70, anchor: A},
				{at: week + 5*min, u: 0.00, anchor: B},
				{at: week + 6*min, u: 0.70, anchor: A},
				{at: week + 7*min, u: 0.01, anchor: B},
			},
			now: A.Add(time.Hour),
			want: []cycleSummary{
				{anchor: A, reason: EndReset, closed: A, ids: ids(1), flags: []string{}},
				{anchor: B, ids: ids(2, 4), flags: []string{}},
			},
		},
		{
			name:  "open cycle expires lazily once its anchor passes",
			steps: []step{{at: 1 * min, u: 0.10, anchor: A}, {at: 2 * min, u: 0.20, anchor: A}},
			now:   A.Add(3 * time.Minute),
			want:  []cycleSummary{{anchor: A, reason: EndExpired, closed: A, ids: ids(1, 2), flags: []string{}}},
		},
		{
			name:  "open cycle stays open before its anchor",
			steps: []step{{at: 1 * min, u: 0.10, anchor: A}},
			now:   A.Add(-time.Hour),
			want:  []cycleSummary{{anchor: A, ids: ids(1), flags: []string{}}},
		},
		{
			name: "anchor jitter within tolerance is one cycle",
			steps: []step{
				{at: 1 * min, u: 0.10, anchor: A}, {at: 2 * min, u: 0.11, anchor: A.Add(90 * time.Second)},
				{at: 3 * min, u: 0.12, anchor: A.Add(-60 * time.Second)},
			},
			now:  t0.Add(time.Hour),
			want: []cycleSummary{{anchor: A, ids: ids(1, 2, 3), flags: []string{}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize(AssignCycles(build(tc.steps), nil, tc.now, DefaultOptions()))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d cycles, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				g, w := got[i], tc.want[i]
				if !g.anchor.Equal(w.anchor) || g.regime != w.regime || g.reason != w.reason ||
					!g.closed.Equal(w.closed) || !slices.Equal(g.ids, w.ids) || !slices.Equal(g.drops, w.drops) ||
					!slices.Equal(g.flags, w.flags) {
					t.Fatalf("cycle %d\n got %+v\nwant %+v", i, g, w)
				}
			}
		})
	}
}

func TestAssignCyclesFailedReadingsDoNotConfirm(t *testing.T) {
	A := t0.Add(week)
	rs := build([]step{
		{at: time.Minute, u: 0.40, anchor: A},
		{at: 10 * time.Minute, u: 0.00, anchor: A},
		{at: 11 * time.Minute, u: 0.00, anchor: A},
		{at: 12 * time.Minute, u: 0.01, anchor: A},
	})
	evs := []domain.UsageEvent{{ID: 3, Failed: true}, {ID: 4, Failed: true}}
	cs := AssignCycles(rs, evs, t0.Add(time.Hour), DefaultOptions())
	if len(cs) != 1 {
		t.Fatalf("failed readings confirmed an early reset: %+v", summarize(cs))
	}
}

func TestTick(t *testing.T) {
	mk := func(dps ...int) []domain.MeterReading {
		var out []domain.MeterReading
		for _, d := range dps {
			out = append(out, domain.MeterReading{Source: domain.SourceHeader, PrecisionDP: d})
		}
		return out
	}
	rep := func(d, n int) []int {
		var out []int
		for range n {
			out = append(out, d)
		}
		return out
	}
	tests := []struct {
		name    string
		dps     []int
		tick    float64
		changed bool
	}{
		{"whole percent", rep(2, 20), 0.01, false},
		{"trimmed zeros are not a coarser grid", append([]int{1, 2, 2, 1}, rep(2, 20)...), 0.01, false},
		{"four dp with occasional trimmed values", append(append(rep(4, 30), 2, 3, 1), rep(4, 30)...), 1e-4, false},
		{"full float floors at 1e-4", rep(16, 12), 1e-4, false},
		{"precision change 4dp to 2dp", append(rep(4, 30), rep(2, 12)...), 0.01, true},
		{"no header readings", nil, 0.01, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tick, changed := Tick(mk(tc.dps...))
			if tick != tc.tick || changed != tc.changed {
				t.Fatalf("Tick = %v, %v; want %v, %v", tick, changed, tc.tick, tc.changed)
			}
		})
	}
}
