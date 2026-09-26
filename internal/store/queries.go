package store

import (
	"fmt"
	"strings"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// Read-only helpers for the management API's dashboard queries.

// MeterKeys lists the distinct meter keys an account has readings for,
// sorted by key. accountID 0 means all accounts.
func (s *Store) MeterKeys(accountID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT meter_key FROM meter_readings
		WHERE (? = 0 OR account_id = ?) ORDER BY meter_key`, accountID, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: meter keys: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// eventColsNoHeaders is eventCols with headers_json replaced by NULL.
var eventColsNoHeaders = strings.Replace(eventCols, "e.headers_json", "NULL", 1)

// ListEventsNoHeaders is ListEvents without the raw headers_json column,
// for callers that aggregate many events (HeadersJSON is always nil).
func (s *Store) ListEventsNoHeaders(q EventQuery) ([]domain.UsageEvent, error) {
	where := []string{"1 = 1"}
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
	sqlText := `SELECT ` + eventColsNoHeaders + ` FROM usage_events e JOIN accounts a ON a.id = e.account_id
		WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY e.observed_at_ns ` + dir(q.Desc) + `, e.id ` + dir(q.Desc) + ` LIMIT ?`
	args = append(args, clampLimit(q.Limit))
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
