package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword hashes an account password with bcrypt.
func HashPassword(plain string) (string, error) {
	if plain == "" {
		return "", fmt.Errorf("password must not be empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// VerifyPassword reports whether plain matches the stored bcrypt hash.
func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

func (s *Store) scanAccount(sc interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var allowedRaw string
	if err := sc.Scan(&a.Email, &a.PasswordHash, &allowedRaw); err != nil {
		return Account{}, err
	}
	allowed, err := fromJSON[[]string](allowedRaw)
	if err != nil {
		return Account{}, fmt.Errorf("account %s: invalid allowed_from: %w", a.Email, err)
	}
	a.AllowedFrom = allowed
	return a, nil
}

// GetAccount returns the account for an email login, if it exists.
func (s *Store) GetAccount(email string) (Account, bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	row := s.db.QueryRowContext(context.Background(),
		`SELECT email, password_hash, allowed_from FROM accounts WHERE email = ?`, email)
	a, err := s.scanAccount(row)
	if err != nil {
		if err == sqlNoRows() {
			return Account{}, false, nil
		}
		return Account{}, false, err
	}
	return a, true, nil
}

// ListAccounts returns every account.
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT email, password_hash, allowed_from FROM accounts ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertAccounts inserts or updates seed accounts (plain-text passwords).
// Emails are normalized to lowercase and AllowedFrom always includes the
// account email. Accounts that match the stored state (same allowed list and
// password) are left untouched.
func (s *Store) UpsertAccounts(seeds []AccountSeed) (UpsertSummary, error) {
	ctx := context.Background()
	var summary UpsertSummary
	for _, seed := range seeds {
		email := strings.ToLower(strings.TrimSpace(seed.Email))
		if email == "" {
			return summary, fmt.Errorf("account with empty email")
		}
		if seed.Password == "" {
			return summary, fmt.Errorf("account %s: empty password", email)
		}
		allowed := normalizeAllowed(email, seed.AllowedFrom)

		existing, found, err := s.GetAccount(email)
		if err != nil {
			return summary, err
		}
		if found {
			allowedJSON := toJSON(allowed)
			_ = allowedJSON
			if VerifyPassword(existing.PasswordHash, seed.Password) && jsonEqual(existing.AllowedFrom, allowed) {
				summary.Unchanged = append(summary.Unchanged, email)
				continue
			}
		}

		hash, err := HashPassword(seed.Password)
		if err != nil {
			return summary, fmt.Errorf("account %s: %w", email, err)
		}
		// sqlite: INSERT ... ON CONFLICT; mysql: INSERT ... ON DUPLICATE KEY.
		var sqlText string
		if s.dialect == "mysql" {
			sqlText = `INSERT INTO accounts(email, password_hash, allowed_from) VALUES(?, ?, ?)
				ON DUPLICATE KEY UPDATE password_hash = VALUES(password_hash), allowed_from = VALUES(allowed_from)`
		} else {
			sqlText = `INSERT INTO accounts(email, password_hash, allowed_from) VALUES(?, ?, ?)
				ON CONFLICT(email) DO UPDATE SET password_hash = excluded.password_hash, allowed_from = excluded.allowed_from`
		}
		if _, err := s.db.ExecContext(ctx, sqlText, email, hash, toJSON(allowed)); err != nil {
			return summary, fmt.Errorf("upserting account %s: %w", email, err)
		}
		if found {
			summary.Updated = append(summary.Updated, email)
		} else {
			summary.Created = append(summary.Created, email)
		}
	}
	return summary, nil
}

// DeleteAccount removes an account. Deleting a nonexistent account is not an
// error.
func (s *Store) DeleteAccount(email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	_, err := s.db.ExecContext(context.Background(),
		`DELETE FROM accounts WHERE email = ?`, email)
	return err
}

// AllowsFrom reports whether the account may use the envelope address from.
func (a *Account) AllowsFrom(from string) bool {
	from = strings.ToLower(strings.TrimSpace(from))
	for _, f := range a.AllowedFrom {
		if f == from {
			return true
		}
	}
	return false
}

func normalizeAllowed(email string, extra []string) []string {
	seen := map[string]bool{email: true}
	out := []string{email}
	for _, f := range extra {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

func jsonEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
