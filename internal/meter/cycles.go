package meter

import (
	"slices"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// Options tunes cycle assignment (§Estimator "Cycle assignment").
type Options struct {
	// Tol is how far two reset anchors may differ and still be the same cycle.
	Tol time.Duration
	// EarlyResetMax and Drop gate the pending state: u ≤ EarlyResetMax and
	// u ≤ P − Drop, where P is the cycle's peak so far.
	EarlyResetMax float64
	Drop          float64
	// ConfirmReadings successful non-decreasing header readings spanning at
	// least ConfirmSpan confirm a same-anchor early reset.
	ConfirmReadings int
	ConfirmSpan     time.Duration
	// RevertAfter separates a stale concurrent response carrying an older
	// anchor (arrives within this long of the switch) from a genuine A→B→A
	// revert (keeps arriving after it). Not in the plan; defaults to Tol.
	RevertAfter time.Duration
}

// DefaultOptions are the plan's values.
func DefaultOptions() Options {
	return Options{
		Tol:             120 * time.Second,
		EarlyResetMax:   0.05,
		Drop:            0.01,
		ConfirmReadings: 3,
		ConfirmSpan:     60 * time.Second,
		RevertAfter:     120 * time.Second,
	}
}

// AssignedReading is a reading placed in a cycle, annotated with what
// crossing detection needs.
type AssignedReading struct {
	domain.MeterReading
	// Failed: the reading came from a failed attempt.
	Failed bool `json:"failed,omitempty"`
	// QuotaDrop: part of a transient drop that did not confirm as a reset.
	QuotaDrop bool `json:"quota_drop,omitempty"`
}

func (r AssignedReading) successfulHeader() bool {
	return r.Source == domain.SourceHeader && !r.Failed
}

// CycleAssignment is one cycle (per regime) with the readings routed to it.
// Its fields map 1:1 onto store.Cycle.
type CycleAssignment struct {
	MeterKey       string            `json:"meter_key"`
	WindowSec      int64             `json:"window_s"`
	ResetAnchor    time.Time         `json:"reset_anchor"`
	StartsAt       time.Time         `json:"starts_at"`
	FirstReadingAt time.Time         `json:"first_reading_at"`
	ClosedAt       time.Time         `json:"closed_at"`
	EndReason      string            `json:"end_reason,omitempty"`
	RegimeSeq      int               `json:"regime_seq"`
	Tick           float64           `json:"tick"`
	PeakFraction   float64           `json:"peak_fraction"`
	Flags          []string          `json:"flags"`
	Readings       []AssignedReading `json:"readings"`
	// Ambiguous are spans where two anchors competed (A→B→A); segments
	// overlapping them are flagged regime_change.
	Ambiguous []Interval `json:"ambiguous,omitempty"`

	switchedAt time.Time
}

// Closed reports whether the cycle has ended.
func (c *CycleAssignment) Closed() bool { return !c.ClosedAt.IsZero() }

// End is ClosedAt, or now while open.
func (c *CycleAssignment) End(now time.Time) time.Time {
	if c.Closed() {
		return c.ClosedAt
	}
	return now
}

// assigner is the per-meter state machine; readings arrive in observed order.
type assigner struct {
	o       Options
	window  int64
	cycles  []*CycleAssignment
	cur     *CycleAssignment
	pending []AssignedReading // low readings awaiting early-reset confirmation
	aside   []AssignedReading // high readings seen while pending (stale suspects)
	// confirmation progress over successful header readings in pending
	pendMax   float64
	pendCount int
	pendFirst time.Time
	pendLast  time.Time
	highRun   int
}

// AssignCycles implements §Estimator "Cycle assignment" for one meter's
// readings (header and poll, any order). events supplies the failed status
// of header readings. Readings with no reset anchor, or with an anchor that
// has already passed and matches no open cycle, are dropped (reset_ambiguous)
// and appear in no cycle. Cycles are returned ordered by (anchor, regime).
func AssignCycles(readings []domain.MeterReading, events []domain.UsageEvent, now time.Time, o Options) []CycleAssignment {
	failed := make(map[int64]bool)
	for _, e := range events {
		if e.Failed {
			failed[e.ID] = true
		}
	}
	a := &assigner{o: o}
	for _, r := range sortReadings(readings) {
		if r.ResetAt.IsZero() {
			continue
		}
		ar := AssignedReading{MeterReading: r}
		if r.EventID != nil {
			ar.Failed = failed[*r.EventID]
		}
		a.step(ar)
	}
	a.flushPending()
	out := make([]CycleAssignment, 0, len(a.cycles))
	for _, c := range a.cycles {
		// Lazy expiry: an open cycle whose anchor has passed with no newer reading.
		if !c.Closed() && now.After(c.ResetAnchor.Add(o.Tol)) {
			c.ClosedAt, c.EndReason = c.ResetAnchor, EndExpired
		}
		var header []domain.MeterReading
		for _, r := range c.Readings {
			if r.Source == domain.SourceHeader {
				header = append(header, r.MeterReading)
			}
		}
		var changed bool
		c.Tick, changed = Tick(header)
		if changed {
			c.Flags = addFlag(c.Flags, FlagPrecisionChange)
		}
		if c.Flags == nil {
			c.Flags = []string{}
		}
		out = append(out, *c)
	}
	slices.SortStableFunc(out, func(x, y CycleAssignment) int {
		if c := x.ResetAnchor.Compare(y.ResetAnchor); c != 0 {
			return c
		}
		return x.RegimeSeq - y.RegimeSeq
	})
	return out
}

func near(a, b time.Time, tol time.Duration) bool {
	d := a.Sub(b)
	return d <= tol && d >= -tol
}

// find returns the latest-regime cycle whose anchor is within tol of t.
func (a *assigner) find(t time.Time) *CycleAssignment {
	for i := len(a.cycles) - 1; i >= 0; i-- {
		if near(a.cycles[i].ResetAnchor, t, a.o.Tol) {
			return a.cycles[i]
		}
	}
	return nil
}

func (a *assigner) open(anchor, startsAt time.Time, seq int, window int64) *CycleAssignment {
	c := &CycleAssignment{ResetAnchor: anchor, StartsAt: startsAt, RegimeSeq: seq, WindowSec: window}
	a.cycles = append(a.cycles, c)
	return c
}

func (a *assigner) add(c *CycleAssignment, r AssignedReading) {
	if c.FirstReadingAt.IsZero() || r.ObservedAt.Before(c.FirstReadingAt) {
		c.FirstReadingAt = r.ObservedAt
	}
	if c.MeterKey == "" {
		c.MeterKey = r.MeterKey
	}
	if c.WindowSec == 0 {
		c.WindowSec = r.WindowSec
	}
	if !r.QuotaDrop && !r.Failed && r.UsedFraction > c.PeakFraction {
		c.PeakFraction = r.UsedFraction
	}
	c.Readings = append(c.Readings, r)
}

func (a *assigner) step(r AssignedReading) {
	anchor := r.ResetAt
	win := time.Duration(r.WindowSec) * time.Second
	if a.cur == nil {
		a.cur = a.open(anchor, anchor.Add(-win), 0, r.WindowSec)
		a.cur.switchedAt = r.ObservedAt
		a.add(a.cur, r)
		return
	}
	cur := a.cur
	if near(anchor, cur.ResetAnchor, a.o.Tol) {
		a.sameAnchor(r)
		return
	}
	c := a.find(anchor)
	switch {
	case c != nil && !r.ObservedAt.After(c.ResetAnchor.Add(a.o.Tol)):
		// Another known cycle whose anchor is still in the future: a late
		// response from a concurrent stream, or a genuine revert.
		a.add(c, r)
		if r.ObservedAt.Sub(cur.switchedAt) > a.o.RevertAfter && r.ObservedAt.Before(c.ResetAnchor.Add(-a.o.Tol)) {
			a.revert(c, r.ObservedAt)
		}
	case c == nil && anchor.After(cur.ResetAnchor.Add(a.o.Tol)):
		a.flushPending()
		reason, closeAt := EndReset, cur.ResetAnchor
		if r.ObservedAt.Before(cur.ResetAnchor.Add(-a.o.Tol)) {
			reason, closeAt = EndEarlyReset, r.ObservedAt
		}
		cur.ClosedAt, cur.EndReason = closeAt, reason
		a.cur = a.open(anchor, anchor.Add(-win), 0, r.WindowSec)
		a.cur.switchedAt = r.ObservedAt
		a.add(a.cur, r)
	default:
		// Older unknown anchor, or a known one that has already passed.
	}
}

// revert switches the current cycle back to c: both are flagged
// regime_change over the span in which their anchors competed.
func (a *assigner) revert(c *CycleAssignment, at time.Time) {
	a.flushPending()
	cur := a.cur
	iv := Interval{From: cur.switchedAt, To: at}
	for _, x := range []*CycleAssignment{cur, c} {
		x.Flags = addFlag(x.Flags, FlagRegimeChange)
		x.Ambiguous = append(x.Ambiguous, iv)
	}
	cur.ClosedAt, cur.EndReason = at, EndRegime
	c.ClosedAt, c.EndReason = time.Time{}, ""
	c.switchedAt = at
	a.cur = c
}

// sameAnchor handles a reading for the current anchor, including the
// pending state that confirms or rejects a same-anchor early reset.
func (a *assigner) sameAnchor(r AssignedReading) {
	cur := a.cur
	peak := cur.PeakFraction
	low := r.UsedFraction <= peak-a.o.Drop
	if len(a.pending) == 0 {
		if r.successfulHeader() && low && r.UsedFraction <= a.o.EarlyResetMax {
			a.pending = []AssignedReading{r}
			a.pendMax, a.pendCount = r.UsedFraction, 1
			a.pendFirst, a.pendLast = r.ObservedAt, r.ObservedAt
			a.highRun = 0
			return
		}
		a.add(cur, r)
		return
	}
	if low {
		a.pending = append(a.pending, r)
		a.highRun = 0
		if r.successfulHeader() && r.UsedFraction >= a.pendMax {
			a.pendMax = r.UsedFraction
			a.pendCount++
			a.pendLast = r.ObservedAt
		}
		if a.pendCount >= a.o.ConfirmReadings && a.pendLast.Sub(a.pendFirst) >= a.o.ConfirmSpan {
			a.confirm()
		}
		return
	}
	// A high reading while pending. Refinement over the plan: one reading at
	// or below the old peak is a stale replica and is set aside; progress
	// past the peak, or two highs in a row, means the drop was transient.
	tick, _ := Tick([]domain.MeterReading{r.MeterReading})
	if r.successfulHeader() && Level(r.UsedFraction, tick) <= Level(peak, tick) && a.highRun == 0 {
		a.aside = append(a.aside, r)
		a.highRun++
		return
	}
	a.flushPending()
	a.add(cur, r)
}

// confirm closes the current cycle as an early reset and reopens the same
// anchor with regime_seq+1, seeded with the pending readings.
func (a *assigner) confirm() {
	old := a.cur
	start := a.pending[0].ObservedAt
	old.ClosedAt, old.EndReason = start, EndEarlyReset
	for _, r := range a.aside {
		a.add(old, r)
	}
	nc := a.open(old.ResetAnchor, start, old.RegimeSeq+1, old.WindowSec)
	nc.MeterKey = old.MeterKey
	nc.switchedAt = start
	for _, r := range a.pending {
		a.add(nc, r)
	}
	a.cur = nc
	a.pending, a.aside = nil, nil
}

// flushPending rejects a pending early reset: the low readings stay in the
// current cycle as quota_drop; set-aside highs are ordinary readings.
func (a *assigner) flushPending() {
	if len(a.pending) == 0 {
		return
	}
	merged := make([]AssignedReading, 0, len(a.pending)+len(a.aside))
	for _, r := range a.pending {
		r.QuotaDrop = true
		merged = append(merged, r)
	}
	merged = append(merged, a.aside...)
	slices.SortStableFunc(merged, func(x, y AssignedReading) int { return x.ObservedAt.Compare(y.ObservedAt) })
	for _, r := range merged {
		a.add(a.cur, r)
	}
	a.pending, a.aside = nil, nil
	a.highRun = 0
}
