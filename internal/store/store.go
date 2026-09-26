// Package store persists domain types in a single SQLite file. It is the
// source of truth for accounts, events and readings; the derived tables
// (cycles, segments, estimates, weight_fits) are recomputed by later phases
// and stored here so the dashboard never recomputes on read.
//
// Time encoding: columns suffixed _ns and the *_at / *_seen columns hold unix
// nanoseconds; reset_at and reset_anchor hold unix seconds because that is
// the resolution the providers report. A zero time is stored as 0 (or NULL
// where the column is nullable) and read back as time.Time{}.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps one SQLite database. The pool is capped at a single connection
// so every write is serialized and reads never race the WAL writer.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates the parent directory if needed, opens the database, applies
// pragmas and runs any pending migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	q := url.Values{}
	// modernc applies each _pragma to every new connection.
	for _, p := range []string{
		"busy_timeout(5000)",
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"foreign_keys(1)",
	} {
		q.Add("_pragma", p)
	}
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Path is the database file location.
func (s *Store) Path() string { return s.path }

// DB exposes the underlying pool for packages that need ad-hoc queries
// (tests, export). Prefer the typed methods.
func (s *Store) DB() *sql.DB { return s.db }

// Checkpoint folds the WAL back into the main file and truncates it.
func (s *Store) Checkpoint() error {
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	if err != nil {
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	return nil
}

// Close checkpoints then closes; the checkpoint error is reported but does
// not prevent closing.
func (s *Store) Close() error {
	cpErr := s.Checkpoint()
	if err := s.db.Close(); err != nil {
		return err
	}
	return cpErr
}

// migrate applies every migration whose version exceeds the recorded max,
// each in its own transaction so a failure leaves the schema consistent.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	for i, sqlText := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(sqlText); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
			version, time.Now().UnixNano()); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %d: %w", version, err)
		}
	}
	return nil
}

// SchemaVersion is the highest applied migration.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// GetSetting JSON-decodes the value stored under key into out.
func (s *Store) GetSetting(key string, out any) (bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value_json FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: get setting %q: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return true, fmt.Errorf("store: decode setting %q: %w", key, err)
	}
	return true, nil
}

// SetSetting JSON-encodes v and upserts it under key.
func (s *Store) SetSetting(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encode setting %q: %w", key, err)
	}
	_, err = s.db.Exec(`INSERT INTO settings(key, value_json) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json`, key, string(raw))
	if err != nil {
		return fmt.Errorf("store: set setting %q: %w", key, err)
	}
	return nil
}

// DeleteSetting removes key; missing keys are not an error.
func (s *Store) DeleteSetting(key string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	return err
}

// Stats summarizes stored volume for /health.
type Stats struct {
	Accounts    int64     `json:"accounts"`
	Events      int64     `json:"events"`
	Readings    int64     `json:"readings"`
	OldestEvent time.Time `json:"oldest_event"`
	NewestEvent time.Time `json:"newest_event"`
	// DBBytes is the main file plus the WAL.
	DBBytes int64 `json:"db_bytes"`
}

// Stats counts rows and measures the files on disk.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	var oldest, newest sql.NullInt64
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM accounts),
		(SELECT COUNT(*) FROM usage_events),
		(SELECT COUNT(*) FROM meter_readings),
		(SELECT MIN(observed_at_ns) FROM usage_events),
		(SELECT MAX(observed_at_ns) FROM usage_events)`).
		Scan(&st.Accounts, &st.Events, &st.Readings, &oldest, &newest)
	if err != nil {
		return st, fmt.Errorf("store: stats: %w", err)
	}
	if oldest.Valid {
		st.OldestEvent = fromNS(oldest.Int64)
	}
	if newest.Valid {
		st.NewestEvent = fromNS(newest.Int64)
	}
	for _, p := range []string{s.path, s.path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			st.DBBytes += fi.Size()
		}
	}
	return st, nil
}

// PruneOlderThan deletes events and readings observed before cutoff. Readings
// linked to a pruned event go with it even if their own timestamp is newer,
// so the foreign key never blocks the event delete.
func (s *Store) PruneOlderThan(cutoff time.Time) (eventsDeleted, readingsDeleted int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	ns := toNS(cutoff)
	res, err := tx.Exec(`DELETE FROM meter_readings WHERE observed_at_ns < ?
		OR event_id IN (SELECT id FROM usage_events WHERE observed_at_ns < ?)`, ns, ns)
	if err != nil {
		return 0, 0, fmt.Errorf("store: prune readings: %w", err)
	}
	readingsDeleted, _ = res.RowsAffected()
	res, err = tx.Exec(`DELETE FROM usage_events WHERE observed_at_ns < ?`, ns)
	if err != nil {
		return 0, 0, fmt.Errorf("store: prune events: %w", err)
	}
	eventsDeleted, _ = res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return eventsDeleted, readingsDeleted, nil
}

// toNS encodes a time as unix nanoseconds, zero time as 0.
func toNS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromNS decodes unix nanoseconds, 0 as the zero time.
func fromNS(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

func toSec(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromSec(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// nullNS is toNS for nullable columns: zero time becomes NULL.
func nullNS(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixNano(), Valid: true}
}

func fromNullNS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromNS(v.Int64)
}

func nullInt(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

func fromNullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// jsonText renders a value for a TEXT column; nil slices become "[]" so
// readers can always unmarshal.
func jsonText(v any) (string, error) {
	if v == nil {
		return "null", nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// rawText stores a json.RawMessage, NULL when empty.
func rawText(r json.RawMessage) sql.NullString {
	if len(r) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(r), Valid: true}
}

func fromRawText(v sql.NullString) json.RawMessage {
	if !v.Valid || v.String == "" {
		return nil
	}
	return json.RawMessage(v.String)
}

func strList(raw sql.NullString) []string {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil
	}
	return out
}

func strListText(list []string) string {
	if list == nil {
		list = []string{}
	}
	raw, _ := json.Marshal(list)
	return string(raw)
}
