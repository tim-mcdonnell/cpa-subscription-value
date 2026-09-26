package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

const accountCols = `id, provider, auth_index, auth_id, email, plan_type, first_seen, last_seen`

// UpsertAccount inserts or updates the row keyed by (provider, auth_index).
// Descriptive fields only overwrite when non-empty so a poll that lacks the
// email never blanks one learned earlier; last_seen only moves forward.
func (s *Store) UpsertAccount(a domain.Account) (domain.Account, error) {
	now := time.Now()
	last := a.LastSeen
	if last.IsZero() {
		last = now
	}
	first := a.FirstSeen
	if first.IsZero() {
		first = last
	}
	_, err := s.db.Exec(`INSERT INTO accounts(provider, auth_index, auth_id, email, plan_type, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, auth_index) DO UPDATE SET
			auth_id   = CASE WHEN excluded.auth_id   != '' THEN excluded.auth_id   ELSE auth_id   END,
			email     = CASE WHEN excluded.email     != '' THEN excluded.email     ELSE email     END,
			plan_type = CASE WHEN excluded.plan_type != '' THEN excluded.plan_type ELSE plan_type END,
			last_seen = MAX(last_seen, excluded.last_seen)`,
		string(a.Provider), a.AuthIndex, a.AuthID, a.Email, a.PlanType, toNS(first), toNS(last))
	if err != nil {
		return domain.Account{}, fmt.Errorf("store: upsert account %s/%s: %w", a.Provider, a.AuthIndex, err)
	}
	row := s.db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE provider = ? AND auth_index = ?`,
		string(a.Provider), a.AuthIndex)
	return scanAccount(row)
}

// ListAccounts returns every account ordered by provider then auth_index.
func (s *Store) ListAccounts() ([]domain.Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY provider, auth_index`)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	defer rows.Close()
	var out []domain.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccountByAuthIndex looks up by auth_index alone; CPA derives it from the
// auth file path, so it is unique across providers in practice.
func (s *Store) GetAccountByAuthIndex(authIndex string) (domain.Account, bool, error) {
	row := s.db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE auth_index = ? ORDER BY id LIMIT 1`, authIndex)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Account{}, false, nil
	}
	if err != nil {
		return domain.Account{}, false, err
	}
	return a, true, nil
}

// GetAccount looks up by primary key.
func (s *Store) GetAccount(id int64) (domain.Account, bool, error) {
	row := s.db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Account{}, false, nil
	}
	if err != nil {
		return domain.Account{}, false, err
	}
	return a, true, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(r scanner) (domain.Account, error) {
	var a domain.Account
	var provider string
	var first, last int64
	if err := r.Scan(&a.ID, &provider, &a.AuthIndex, &a.AuthID, &a.Email, &a.PlanType, &first, &last); err != nil {
		return domain.Account{}, err
	}
	a.Provider = domain.Provider(provider)
	a.FirstSeen = fromNS(first)
	a.LastSeen = fromNS(last)
	return a, nil
}
