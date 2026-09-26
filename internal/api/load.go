package api

import (
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// pageSize is the store's listing cap; the loaders below page past it.
const pageSize = 5000

// pageAll repeatedly fetches pages ordered by time ascending, restarting
// each page at the last timestamp seen and dropping ids already returned.
func pageAll[T any](from time.Time, fetch func(from time.Time) ([]T, error), key func(T) (int64, time.Time)) ([]T, error) {
	var out []T
	seen := map[int64]bool{}
	for {
		page, err := fetch(from)
		if err != nil {
			return nil, err
		}
		added := 0
		for _, v := range page {
			id, _ := key(v)
			if !seen[id] {
				seen[id] = true
				out = append(out, v)
				added++
			}
		}
		if len(page) < pageSize || added == 0 {
			return out, nil
		}
		_, from = key(page[len(page)-1])
	}
}

// loadEvents returns an account's events observed in [from, to) without
// their raw headers. A zero to is unbounded.
func (r *Router) loadEvents(accountID int64, from, to time.Time) ([]domain.UsageEvent, error) {
	return pageAll(from, func(f time.Time) ([]domain.UsageEvent, error) {
		return r.d.Store.ListEventsNoHeaders(store.EventQuery{AccountID: accountID, From: f, To: to, Limit: pageSize})
	}, func(e domain.UsageEvent) (int64, time.Time) { return e.ID, e.ObservedAt })
}

// loadReadings returns an account's readings of one meter (any meter when
// empty) and source (any when empty) observed in [from, to).
func (r *Router) loadReadings(accountID int64, meterKey, source string, from, to time.Time) ([]domain.MeterReading, error) {
	return pageAll(from, func(f time.Time) ([]domain.MeterReading, error) {
		return r.d.Store.ListReadings(store.ReadingQuery{AccountID: accountID, MeterKey: meterKey, Source: source, From: f, To: to, Limit: pageSize})
	}, func(m domain.MeterReading) (int64, time.Time) { return m.ID, m.ObservedAt })
}

// cycleReadings are the readings assigned to c.
func (r *Router) cycleReadings(accountID int64, c store.Cycle) ([]domain.MeterReading, error) {
	from := c.StartsAt
	if !c.FirstReadingAt.IsZero() && c.FirstReadingAt.Before(from) {
		from = c.FirstReadingAt
	}
	var to time.Time
	if !c.ClosedAt.IsZero() {
		to = c.ClosedAt
		if c.ResetAnchor.After(to) {
			to = c.ResetAnchor
		}
		to = to.Add(time.Hour)
	}
	all, err := r.loadReadings(accountID, c.MeterKey, "", from, to)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, m := range all {
		if m.CycleID != nil && *m.CycleID == c.ID {
			out = append(out, m)
		}
	}
	return out, nil
}

// eventsIn filters events to [from, to) by observed_at.
func eventsIn(evs []domain.UsageEvent, from, to time.Time) []domain.UsageEvent {
	var out []domain.UsageEvent
	for _, e := range evs {
		if !e.ObservedAt.Before(from) && e.ObservedAt.Before(to) {
			out = append(out, e)
		}
	}
	return out
}
