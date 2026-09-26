package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PriceSnapshot is one compiled price table, content-addressed by
// SnapshotHash so events can reference the exact prices they were frozen under.
type PriceSnapshot struct {
	Hash      string          `json:"hash"`
	CreatedAt time.Time       `json:"created_at"`
	Source    string          `json:"source"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// Price is one model family's rates in USD per token within a snapshot.
type Price struct {
	Provider     string  `json:"provider"`
	ModelFamily  string  `json:"model_family"`
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	LongCtxMult  float64 `json:"long_ctx_mult"`
	FastMult     float64 `json:"fast_mult"`
	Source       string  `json:"source"`
}

// InsertPriceSnapshot stores a snapshot and its rows. A hash already present
// is left untouched and reported as inserted=false, since the content is
// immutable by construction.
func (s *Store) InsertPriceSnapshot(snap PriceSnapshot, rows []Price) (bool, error) {
	if snap.CreatedAt.IsZero() {
		snap.CreatedAt = time.Now()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT OR IGNORE INTO price_snapshots(snapshot_hash, created_at, source, raw_json)
		VALUES (?, ?, ?, ?)`, snap.Hash, toNS(snap.CreatedAt), snap.Source, rawText(snap.Raw))
	if err != nil {
		return false, fmt.Errorf("store: insert price snapshot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	stmt, err := tx.Prepare(`INSERT INTO prices(snapshot_hash, provider, model_family, input, output, cache_read,
			cache_write_5m, cache_write_1h, long_ctx_mult, fast_mult, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	for _, p := range rows {
		if _, err := stmt.Exec(snap.Hash, p.Provider, p.ModelFamily, p.Input, p.Output, p.CacheRead,
			p.CacheWrite5m, p.CacheWrite1h, p.LongCtxMult, p.FastMult, p.Source); err != nil {
			return false, fmt.Errorf("store: insert price %s/%s: %w", p.Provider, p.ModelFamily, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// GetPriceSnapshot returns the snapshot and its rows for a hash.
func (s *Store) GetPriceSnapshot(hash string) (PriceSnapshot, []Price, bool, error) {
	row := s.db.QueryRow(`SELECT snapshot_hash, created_at, source, raw_json FROM price_snapshots WHERE snapshot_hash = ?`, hash)
	return s.loadSnapshot(row)
}

// LatestPriceSnapshot returns the most recently created snapshot.
func (s *Store) LatestPriceSnapshot() (PriceSnapshot, []Price, bool, error) {
	row := s.db.QueryRow(`SELECT snapshot_hash, created_at, source, raw_json FROM price_snapshots
		ORDER BY created_at DESC LIMIT 1`)
	return s.loadSnapshot(row)
}

func (s *Store) loadSnapshot(row *sql.Row) (PriceSnapshot, []Price, bool, error) {
	var snap PriceSnapshot
	var created int64
	var raw sql.NullString
	err := row.Scan(&snap.Hash, &created, &snap.Source, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return PriceSnapshot{}, nil, false, nil
	}
	if err != nil {
		return PriceSnapshot{}, nil, false, fmt.Errorf("store: read price snapshot: %w", err)
	}
	snap.CreatedAt = fromNS(created)
	snap.Raw = fromRawText(raw)
	rows, err := s.db.Query(`SELECT provider, model_family, input, output, cache_read, cache_write_5m,
			cache_write_1h, long_ctx_mult, fast_mult, source
		FROM prices WHERE snapshot_hash = ? ORDER BY provider, model_family`, snap.Hash)
	if err != nil {
		return PriceSnapshot{}, nil, false, fmt.Errorf("store: read prices: %w", err)
	}
	defer rows.Close()
	var prices []Price
	for rows.Next() {
		var p Price
		if err := rows.Scan(&p.Provider, &p.ModelFamily, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite5m,
			&p.CacheWrite1h, &p.LongCtxMult, &p.FastMult, &p.Source); err != nil {
			return PriceSnapshot{}, nil, false, err
		}
		prices = append(prices, p)
	}
	return snap, prices, true, rows.Err()
}
