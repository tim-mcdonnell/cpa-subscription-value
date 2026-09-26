package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

const (
	defaultLimit = 200
	maxLimit     = 5000
)

// EventQuery selects usage events. Zero values mean "no bound". Its field
// layout is mirrored by api.EventQuery, which converts to it directly, so
// additions belong in a new type rather than here.
type EventQuery struct {
	AccountID int64
	From, To  time.Time
	Limit     int
	Desc      bool
}

// ReadingQuery selects meter readings. Empty strings mean "all".
type ReadingQuery struct {
	AccountID int64
	MeterKey  string
	Source    string
	From, To  time.Time
	Limit     int
	Desc      bool
}

const eventCols = `e.id, e.account_id, e.dedup_key, a.provider, a.auth_index, a.auth_id,
	e.requested_at_ns, e.observed_at_ns, e.latency_ns, e.model, e.model_family, e.alias,
	e.service_tier, e.reasoning_effort, e.failed, e.status_code,
	e.uncached_input, e.cache_read, e.cache_write, e.output, e.reasoning, e.is_fast, e.is_long,
	e.api_usd, e.api_usd_cw1h, e.price_hash, e.headers_json, e.flags`

const readingCols = `id, account_id, meter_key, source, event_id, observed_at_ns, used_fraction,
	raw, precision_dp, reset_at, status, window_s, cycle_id`

// InsertEvent stores one event and its header readings atomically. A repeat
// of an existing dedup_key is a no-op that returns false; nothing is written.
// On success ev.ID is set and every reading is linked to the event and account.
func (s *Store) InsertEvent(ev *domain.UsageEvent, readings []domain.MeterReading) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT OR IGNORE INTO usage_events(
			account_id, dedup_key, requested_at_ns, observed_at_ns, latency_ns,
			model, model_family, alias, service_tier, reasoning_effort, failed, status_code,
			uncached_input, cache_read, cache_write, output, reasoning, is_fast, is_long,
			api_usd, api_usd_cw1h, price_hash, headers_json, flags)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.AccountID, ev.DedupKey, toNS(ev.RequestedAt), toNS(ev.ObservedAt), int64(ev.Latency),
		ev.Model, ev.ModelFamily, ev.Alias, ev.ServiceTier, ev.ReasoningEffort, boolInt(ev.Failed), ev.StatusCode,
		ev.UncachedInput, ev.CacheRead, ev.CacheWrite, ev.Output, ev.Reasoning, boolInt(ev.IsFast), boolInt(ev.IsLong),
		ev.APIUSD, ev.APIUSDCacheWrite1h, ev.PriceHash, rawText(ev.HeadersJSON), ev.Flags)
	if err != nil {
		return false, fmt.Errorf("store: insert event: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return false, err
	}
	ev.ID = id
	for i := range readings {
		r := &readings[i]
		r.EventID = &ev.ID
		r.AccountID = ev.AccountID
		if err := insertReading(tx, r); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// InsertReading stores a standalone reading (polls). Sets r.ID.
func (s *Store) InsertReading(r *domain.MeterReading) error {
	return insertReading(s.db, r)
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertReading(x execer, r *domain.MeterReading) error {
	res, err := x.Exec(`INSERT INTO meter_readings(
			account_id, meter_key, source, event_id, observed_at_ns, used_fraction,
			raw, precision_dp, reset_at, status, window_s, cycle_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.AccountID, r.MeterKey, r.Source, nullInt(r.EventID), toNS(r.ObservedAt), r.UsedFraction,
		r.Raw, r.PrecisionDP, toSec(r.ResetAt), r.Status, r.WindowSec, nullInt(r.CycleID))
	if err != nil {
		return fmt.Errorf("store: insert reading: %w", err)
	}
	r.ID, err = res.LastInsertId()
	return err
}

// ListEvents returns events ordered by observed_at then id.
func (s *Store) ListEvents(q EventQuery) ([]domain.UsageEvent, error) {
	var where []string
	var args []any
	if q.AccountID != 0 {
		where = append(where, "e.account_id = ?")
		args = append(args, q.AccountID)
	}
	if !q.From.IsZero() {
		where = append(where, "e.observed_at_ns >= ?")
		args = append(args, toNS(q.From))
	}
	if !q.To.IsZero() {
		where = append(where, "e.observed_at_ns < ?")
		args = append(args, toNS(q.To))
	}
	order := "e.observed_at_ns " + dir(q.Desc) + ", e.id " + dir(q.Desc)
	return s.queryEvents(where, args, order, q.Limit)
}

// ListEventsForReprice pages, by ascending id, through events whose frozen
// price is not under excludeHash. Pass the last id seen as afterID to fetch
// the next batch; an empty result means the reprice is complete.
func (s *Store) ListEventsForReprice(excludeHash string, afterID int64, limit int) ([]domain.UsageEvent, error) {
	where := []string{"e.price_hash != ?", "e.id > ?"}
	args := []any{excludeHash, afterID}
	return s.queryEvents(where, args, "e.id ASC", limit)
}

func (s *Store) queryEvents(where []string, args []any, order string, limit int) ([]domain.UsageEvent, error) {
	sqlText := `SELECT ` + eventCols + ` FROM usage_events e JOIN accounts a ON a.id = e.account_id`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY " + order + " LIMIT ?"
	args = append(args, clampLimit(limit))
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()
	var out []domain.UsageEvent
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListReadings returns readings ordered by observed_at then id.
func (s *Store) ListReadings(q ReadingQuery) ([]domain.MeterReading, error) {
	var where []string
	var args []any
	if q.AccountID != 0 {
		where = append(where, "account_id = ?")
		args = append(args, q.AccountID)
	}
	if q.MeterKey != "" {
		where = append(where, "meter_key = ?")
		args = append(args, q.MeterKey)
	}
	if q.Source != "" {
		where = append(where, "source = ?")
		args = append(args, q.Source)
	}
	if !q.From.IsZero() {
		where = append(where, "observed_at_ns >= ?")
		args = append(args, toNS(q.From))
	}
	if !q.To.IsZero() {
		where = append(where, "observed_at_ns < ?")
		args = append(args, toNS(q.To))
	}
	sqlText := `SELECT ` + readingCols + ` FROM meter_readings`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY observed_at_ns " + dir(q.Desc) + ", id " + dir(q.Desc) + " LIMIT ?"
	args = append(args, clampLimit(q.Limit))
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list readings: %w", err)
	}
	defer rows.Close()
	var out []domain.MeterReading
	for rows.Next() {
		r, err := scanReading(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateEventPrices rewrites the frozen $ for the given events under a new
// price hash, in one transaction. ids, usd and usd1h must be parallel.
func (s *Store) UpdateEventPrices(ids []int64, usd, usd1h []float64, hash string) error {
	if len(ids) != len(usd) || len(ids) != len(usd1h) {
		return fmt.Errorf("store: update prices: %d ids, %d usd, %d usd1h", len(ids), len(usd), len(usd1h))
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE usage_events SET api_usd = ?, api_usd_cw1h = ?, price_hash = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, id := range ids {
		if _, err := stmt.Exec(usd[i], usd1h[i], hash, id); err != nil {
			return fmt.Errorf("store: update price for event %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// SetReadingCycles assigns cycle_id to readings in one transaction; a nil
// cycle clears the assignment.
func (s *Store) SetReadingCycles(readingIDs []int64, cycleID *int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE meter_readings SET cycle_id = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range readingIDs {
		if _, err := stmt.Exec(nullInt(cycleID), id); err != nil {
			return fmt.Errorf("store: set cycle for reading %d: %w", id, err)
		}
	}
	return tx.Commit()
}

func dir(desc bool) string {
	if desc {
		return "DESC"
	}
	return "ASC"
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	}
	return n
}

func scanEvent(r scanner) (domain.UsageEvent, error) {
	var ev domain.UsageEvent
	var provider string
	var requested, observed, latency int64
	var failed, isFast, isLong int
	var headers sql.NullString
	err := r.Scan(&ev.ID, &ev.AccountID, &ev.DedupKey, &provider, &ev.AuthIndex, &ev.AuthID,
		&requested, &observed, &latency, &ev.Model, &ev.ModelFamily, &ev.Alias,
		&ev.ServiceTier, &ev.ReasoningEffort, &failed, &ev.StatusCode,
		&ev.UncachedInput, &ev.CacheRead, &ev.CacheWrite, &ev.Output, &ev.Reasoning, &isFast, &isLong,
		&ev.APIUSD, &ev.APIUSDCacheWrite1h, &ev.PriceHash, &headers, &ev.Flags)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	ev.Provider = domain.Provider(provider)
	ev.RequestedAt = fromNS(requested)
	ev.ObservedAt = fromNS(observed)
	ev.Latency = time.Duration(latency)
	ev.Failed = failed != 0
	ev.IsFast = isFast != 0
	ev.IsLong = isLong != 0
	ev.HeadersJSON = fromRawText(headers)
	return ev, nil
}

func scanReading(r scanner) (domain.MeterReading, error) {
	var m domain.MeterReading
	var eventID, cycleID sql.NullInt64
	var observed, reset int64
	err := r.Scan(&m.ID, &m.AccountID, &m.MeterKey, &m.Source, &eventID, &observed, &m.UsedFraction,
		&m.Raw, &m.PrecisionDP, &reset, &m.Status, &m.WindowSec, &cycleID)
	if err != nil {
		return domain.MeterReading{}, err
	}
	m.EventID = fromNullInt(eventID)
	m.CycleID = fromNullInt(cycleID)
	m.ObservedAt = fromNS(observed)
	m.ResetAt = fromSec(reset)
	return m, nil
}
