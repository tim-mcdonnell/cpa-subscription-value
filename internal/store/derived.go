package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Cycle is one reset window of one meter on one account. Its id is stable
// across recomputation because the row is keyed by
// (account_id, meter_key, reset_anchor, regime_seq).
type Cycle struct {
	ID             int64     `json:"id"`
	AccountID      int64     `json:"account_id"`
	MeterKey       string    `json:"meter_key"`
	WindowSec      int64     `json:"window_s"`
	ResetAnchor    time.Time `json:"reset_anchor"`
	StartsAt       time.Time `json:"starts_at"`
	FirstReadingAt time.Time `json:"first_reading_at"`
	// ClosedAt is zero while the cycle is open.
	ClosedAt     time.Time `json:"closed_at,omitempty"`
	EndReason    string    `json:"end_reason,omitempty"`
	RegimeSeq    int       `json:"regime_seq"`
	Tick         float64   `json:"tick"`
	PeakFraction float64   `json:"peak_fraction"`
	Flags        []string  `json:"flags"`
}

// Segment is the spend between two consecutive crossings under one lag.
type Segment struct {
	ID              int64           `json:"id"`
	CycleID         int64           `json:"cycle_id"`
	Lag             string          `json:"lag"`
	Seq             int             `json:"seq"`
	FromLevel       int64           `json:"from_level"`
	ToLevel         int64           `json:"to_level"`
	DeltaTicks      int64           `json:"delta_ticks"`
	FromReadingID   *int64          `json:"from_reading_id,omitempty"`
	ToReadingID     *int64          `json:"to_reading_id,omitempty"`
	TStart          time.Time       `json:"t_start"`
	TEnd            time.Time       `json:"t_end"`
	USD             float64         `json:"usd"`
	USDCacheWrite1h float64         `json:"usd_cw1h"`
	Features        json.RawMessage `json:"features,omitempty"`
	NEvents         int             `json:"n_events"`
	NFailed         int             `json:"n_failed"`
	Flags           []string        `json:"flags"`
}

// Estimate is one computed V̂ for a cycle. Kind is running/final/reprice.
type Estimate struct {
	ID               int64           `json:"id"`
	CycleID          int64           `json:"cycle_id"`
	ComputedAt       time.Time       `json:"computed_at"`
	Kind             string          `json:"kind"`
	Method           string          `json:"method"`
	Lag              string          `json:"lag"`
	VHat             float64         `json:"v_hat"`
	VHatCacheWrite1h float64         `json:"v_hat_cw1h"`
	CILo             float64         `json:"ci_lo"`
	CIHi             float64         `json:"ci_hi"`
	SEQuant          float64         `json:"se_quant"`
	SEBoot           float64         `json:"se_boot"`
	TicksUsed        int64           `json:"ticks_used"`
	TicksExcluded    int64           `json:"ticks_excluded"`
	Runs             int             `json:"runs"`
	UnexplainedFrac  float64         `json:"unexplained_frac"`
	Grade            string          `json:"grade"`
	Mix              json.RawMessage `json:"mix,omitempty"`
	VBlend           float64         `json:"v_blend"`
	Flags            []string        `json:"flags"`
	PriceHash        string          `json:"price_hash"`
}

// WeightFit is one learner run for an (account, meter).
type WeightFit struct {
	ID           int64           `json:"id"`
	AccountID    int64           `json:"account_id"`
	MeterKey     string          `json:"meter_key"`
	ComputedAt   time.Time       `json:"computed_at"`
	Lag          string          `json:"lag"`
	AnchorFamily string          `json:"anchor_family"`
	Factors      json.RawMessage `json:"factors,omitempty"`
	Scales       json.RawMessage `json:"scales,omitempty"`
	Loss         float64         `json:"loss"`
	Backtest     json.RawMessage `json:"backtest,omitempty"`
	PriceHash    string          `json:"price_hash"`
}

const cycleCols = `id, account_id, meter_key, window_s, reset_anchor, starts_at, first_reading_at,
	closed_at, end_reason, regime_seq, tick, peak_fraction, flags_json`

// UpsertCycle inserts or fully updates the cycle matching its natural key
// and returns it with ID set.
func (s *Store) UpsertCycle(c Cycle) (Cycle, error) {
	_, err := s.db.Exec(`INSERT INTO cycles(account_id, meter_key, window_s, reset_anchor, starts_at,
			first_reading_at, closed_at, end_reason, regime_seq, tick, peak_fraction, flags_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, meter_key, reset_anchor, regime_seq) DO UPDATE SET
			window_s = excluded.window_s, starts_at = excluded.starts_at,
			first_reading_at = excluded.first_reading_at, closed_at = excluded.closed_at,
			end_reason = excluded.end_reason, tick = excluded.tick,
			peak_fraction = excluded.peak_fraction, flags_json = excluded.flags_json`,
		c.AccountID, c.MeterKey, c.WindowSec, toSec(c.ResetAnchor), toNS(c.StartsAt),
		toNS(c.FirstReadingAt), nullNS(c.ClosedAt), c.EndReason, c.RegimeSeq, c.Tick, c.PeakFraction,
		strListText(c.Flags))
	if err != nil {
		return Cycle{}, fmt.Errorf("store: upsert cycle: %w", err)
	}
	row := s.db.QueryRow(`SELECT `+cycleCols+` FROM cycles
		WHERE account_id = ? AND meter_key = ? AND reset_anchor = ? AND regime_seq = ?`,
		c.AccountID, c.MeterKey, toSec(c.ResetAnchor), c.RegimeSeq)
	return scanCycle(row)
}

// GetCycle looks up by primary key.
func (s *Store) GetCycle(id int64) (Cycle, bool, error) {
	c, err := scanCycle(s.db.QueryRow(`SELECT `+cycleCols+` FROM cycles WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Cycle{}, false, nil
	}
	return c, err == nil, err
}

// ListCycles returns cycles for an account, newest anchor first. meterKey ""
// means all meters; accountID 0 means all accounts.
func (s *Store) ListCycles(accountID int64, meterKey string, limit int) ([]Cycle, error) {
	rows, err := s.db.Query(`SELECT `+cycleCols+` FROM cycles
		WHERE (? = 0 OR account_id = ?) AND (? = '' OR meter_key = ?)
		ORDER BY reset_anchor DESC, regime_seq DESC LIMIT ?`,
		accountID, accountID, meterKey, meterKey, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: list cycles: %w", err)
	}
	defer rows.Close()
	var out []Cycle
	for rows.Next() {
		c, err := scanCycle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCycle(r scanner) (Cycle, error) {
	var c Cycle
	var anchor, starts, first int64
	var closed sql.NullInt64
	var flags sql.NullString
	err := r.Scan(&c.ID, &c.AccountID, &c.MeterKey, &c.WindowSec, &anchor, &starts, &first,
		&closed, &c.EndReason, &c.RegimeSeq, &c.Tick, &c.PeakFraction, &flags)
	if err != nil {
		return Cycle{}, err
	}
	c.ResetAnchor = fromSec(anchor)
	c.StartsAt = fromNS(starts)
	c.FirstReadingAt = fromNS(first)
	c.ClosedAt = fromNullNS(closed)
	c.Flags = strList(flags)
	return c, nil
}

const segmentCols = `id, cycle_id, lag, seq, from_level, to_level, delta_ticks, from_reading_id,
	to_reading_id, t_start_ns, t_end_ns, usd, usd_cw1h, features_json, n_events, n_failed, flags_json`

// ReplaceSegments deletes every segment of the cycle and inserts segs (all
// lags at once) in one transaction. Segment IDs are set on return.
func (s *Store) ReplaceSegments(cycleID int64, segs []Segment) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM segments WHERE cycle_id = ?`, cycleID); err != nil {
		return fmt.Errorf("store: clear segments: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO segments(cycle_id, lag, seq, from_level, to_level, delta_ticks,
			from_reading_id, to_reading_id, t_start_ns, t_end_ns, usd, usd_cw1h, features_json,
			n_events, n_failed, flags_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := range segs {
		sg := &segs[i]
		sg.CycleID = cycleID
		res, err := stmt.Exec(cycleID, sg.Lag, sg.Seq, sg.FromLevel, sg.ToLevel, sg.DeltaTicks,
			nullInt(sg.FromReadingID), nullInt(sg.ToReadingID), toNS(sg.TStart), toNS(sg.TEnd),
			sg.USD, sg.USDCacheWrite1h, rawText(sg.Features), sg.NEvents, sg.NFailed, strListText(sg.Flags))
		if err != nil {
			return fmt.Errorf("store: insert segment %s/%d: %w", sg.Lag, sg.Seq, err)
		}
		sg.ID, _ = res.LastInsertId()
	}
	return tx.Commit()
}

// ListSegments returns a cycle's segments ordered by (lag, seq). lag "" means
// all lags.
func (s *Store) ListSegments(cycleID int64, lag string) ([]Segment, error) {
	rows, err := s.db.Query(`SELECT `+segmentCols+` FROM segments
		WHERE cycle_id = ? AND (? = '' OR lag = ?) ORDER BY lag, seq`, cycleID, lag, lag)
	if err != nil {
		return nil, fmt.Errorf("store: list segments: %w", err)
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		var sg Segment
		var from, to sql.NullInt64
		var start, end int64
		var features, flags sql.NullString
		if err := rows.Scan(&sg.ID, &sg.CycleID, &sg.Lag, &sg.Seq, &sg.FromLevel, &sg.ToLevel, &sg.DeltaTicks,
			&from, &to, &start, &end, &sg.USD, &sg.USDCacheWrite1h, &features, &sg.NEvents, &sg.NFailed, &flags); err != nil {
			return nil, err
		}
		sg.FromReadingID = fromNullInt(from)
		sg.ToReadingID = fromNullInt(to)
		sg.TStart = fromNS(start)
		sg.TEnd = fromNS(end)
		sg.Features = fromRawText(features)
		sg.Flags = strList(flags)
		out = append(out, sg)
	}
	return out, rows.Err()
}

const estimateCols = `id, cycle_id, computed_at, kind, method, lag, v_hat, v_hat_cw1h, ci_lo, ci_hi,
	se_quant, se_boot, ticks_used, ticks_excluded, runs, unexplained_frac, grade, mix_json, v_blend,
	flags_json, price_hash`

// InsertEstimate appends an estimate. A `final` for a (cycle, price_hash)
// that already has one overwrites it in place so history stays one row per
// hash; other kinds always append. Sets e.ID.
func (s *Store) InsertEstimate(e *Estimate) error {
	if e.ComputedAt.IsZero() {
		e.ComputedAt = time.Now()
	}
	mix := rawText(e.Mix)
	args := []any{e.CycleID, toNS(e.ComputedAt), e.Kind, e.Method, e.Lag, e.VHat, e.VHatCacheWrite1h,
		e.CILo, e.CIHi, e.SEQuant, e.SEBoot, e.TicksUsed, e.TicksExcluded, e.Runs, e.UnexplainedFrac,
		e.Grade, mix, e.VBlend, strListText(e.Flags), e.PriceHash}
	sqlText := `INSERT INTO estimates(cycle_id, computed_at, kind, method, lag, v_hat, v_hat_cw1h, ci_lo, ci_hi,
			se_quant, se_boot, ticks_used, ticks_excluded, runs, unexplained_frac, grade, mix_json, v_blend,
			flags_json, price_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if e.Kind == "final" {
		sqlText += ` ON CONFLICT(cycle_id, price_hash) WHERE kind = 'final' DO UPDATE SET
			computed_at = excluded.computed_at, method = excluded.method, lag = excluded.lag,
			v_hat = excluded.v_hat, v_hat_cw1h = excluded.v_hat_cw1h, ci_lo = excluded.ci_lo, ci_hi = excluded.ci_hi,
			se_quant = excluded.se_quant, se_boot = excluded.se_boot, ticks_used = excluded.ticks_used,
			ticks_excluded = excluded.ticks_excluded, runs = excluded.runs, unexplained_frac = excluded.unexplained_frac,
			grade = excluded.grade, mix_json = excluded.mix_json, v_blend = excluded.v_blend, flags_json = excluded.flags_json`
	}
	if _, err := s.db.Exec(sqlText, args...); err != nil {
		return fmt.Errorf("store: insert estimate: %w", err)
	}
	// LastInsertId is unreliable after DO UPDATE, so read the id back.
	var id int64
	err := s.db.QueryRow(`SELECT id FROM estimates WHERE cycle_id = ? AND kind = ? AND price_hash = ?
		ORDER BY id DESC LIMIT 1`, e.CycleID, e.Kind, e.PriceHash).Scan(&id)
	if err != nil {
		return fmt.Errorf("store: read estimate id: %w", err)
	}
	e.ID = id
	return nil
}

// LatestEstimate returns the newest estimate of the given kind ("" = any)
// for a cycle.
func (s *Store) LatestEstimate(cycleID int64, kind string) (Estimate, bool, error) {
	row := s.db.QueryRow(`SELECT `+estimateCols+` FROM estimates
		WHERE cycle_id = ? AND (? = '' OR kind = ?) ORDER BY computed_at DESC, id DESC LIMIT 1`,
		cycleID, kind, kind)
	e, err := scanEstimate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Estimate{}, false, nil
	}
	return e, err == nil, err
}

// LatestEstimatesPerCycle returns, for every cycle of an account and meter,
// its most recent estimate of any kind, ordered by cycle anchor descending.
// This is the week-over-week series: closed cycles carry their final, the
// open one its running estimate.
func (s *Store) LatestEstimatesPerCycle(accountID int64, meterKey string) ([]Estimate, error) {
	rows, err := s.db.Query(`SELECT `+prefixed("e", estimateCols)+` FROM estimates e
		JOIN cycles c ON c.id = e.cycle_id
		WHERE e.id IN (SELECT MAX(id) FROM estimates GROUP BY cycle_id)
		  AND (? = 0 OR c.account_id = ?) AND (? = '' OR c.meter_key = ?)
		ORDER BY c.reset_anchor DESC, c.regime_seq DESC`, accountID, accountID, meterKey, meterKey)
	if err != nil {
		return nil, fmt.Errorf("store: latest estimates: %w", err)
	}
	defer rows.Close()
	var out []Estimate
	for rows.Next() {
		e, err := scanEstimate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEstimates returns a cycle's full history, oldest first.
func (s *Store) ListEstimates(cycleID int64) ([]Estimate, error) {
	rows, err := s.db.Query(`SELECT `+estimateCols+` FROM estimates WHERE cycle_id = ? ORDER BY computed_at, id`, cycleID)
	if err != nil {
		return nil, fmt.Errorf("store: list estimates: %w", err)
	}
	defer rows.Close()
	var out []Estimate
	for rows.Next() {
		e, err := scanEstimate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanEstimate(r scanner) (Estimate, error) {
	var e Estimate
	var computed int64
	var mix, flags sql.NullString
	err := r.Scan(&e.ID, &e.CycleID, &computed, &e.Kind, &e.Method, &e.Lag, &e.VHat, &e.VHatCacheWrite1h,
		&e.CILo, &e.CIHi, &e.SEQuant, &e.SEBoot, &e.TicksUsed, &e.TicksExcluded, &e.Runs, &e.UnexplainedFrac,
		&e.Grade, &mix, &e.VBlend, &flags, &e.PriceHash)
	if err != nil {
		return Estimate{}, err
	}
	e.ComputedAt = fromNS(computed)
	e.Mix = fromRawText(mix)
	e.Flags = strList(flags)
	return e, nil
}

const weightFitCols = `id, account_id, meter_key, computed_at, lag, anchor_family, factors_json,
	scales_json, loss, backtest_json, price_hash`

// InsertWeightFit appends a learner run. Sets w.ID.
func (s *Store) InsertWeightFit(w *WeightFit) error {
	if w.ComputedAt.IsZero() {
		w.ComputedAt = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO weight_fits(account_id, meter_key, computed_at, lag, anchor_family,
			factors_json, scales_json, loss, backtest_json, price_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.AccountID, w.MeterKey, toNS(w.ComputedAt), w.Lag, w.AnchorFamily,
		rawText(w.Factors), rawText(w.Scales), w.Loss, rawText(w.Backtest), w.PriceHash)
	if err != nil {
		return fmt.Errorf("store: insert weight fit: %w", err)
	}
	w.ID, err = res.LastInsertId()
	return err
}

// LatestWeightFit returns the newest fit for an (account, meter).
func (s *Store) LatestWeightFit(accountID int64, meterKey string) (WeightFit, bool, error) {
	row := s.db.QueryRow(`SELECT `+weightFitCols+` FROM weight_fits
		WHERE account_id = ? AND meter_key = ? ORDER BY computed_at DESC, id DESC LIMIT 1`, accountID, meterKey)
	var w WeightFit
	var computed int64
	var factors, scales, backtest sql.NullString
	err := row.Scan(&w.ID, &w.AccountID, &w.MeterKey, &computed, &w.Lag, &w.AnchorFamily,
		&factors, &scales, &w.Loss, &backtest, &w.PriceHash)
	if errors.Is(err, sql.ErrNoRows) {
		return WeightFit{}, false, nil
	}
	if err != nil {
		return WeightFit{}, false, err
	}
	w.ComputedAt = fromNS(computed)
	w.Factors = fromRawText(factors)
	w.Scales = fromRawText(scales)
	w.Backtest = fromRawText(backtest)
	return w, true, nil
}

// prefixed qualifies each column in a comma-separated list with a table
// alias so the list can be reused in a join.
func prefixed(alias, cols string) string {
	out := ""
	for i, c := range splitCols(cols) {
		if i > 0 {
			out += ", "
		}
		out += alias + "." + c
	}
	return out
}

func splitCols(cols string) []string {
	var out []string
	cur := ""
	for _, ch := range cols {
		switch ch {
		case ',':
			out = append(out, cur)
			cur = ""
		case ' ', '\n', '\t':
		default:
			cur += string(ch)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
