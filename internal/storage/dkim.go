package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// sqlNoRows normalizes driver-specific no-row errors for GetAccount.
func sqlNoRows() error { return sql.ErrNoRows }

// GetDKIM returns the DKIM key for a domain, if it exists.
func (s *Store) GetDKIM(domain string) (DKIMKey, bool, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	var k DKIMKey
	err := s.db.QueryRowContext(context.Background(),
		`SELECT domain, selector, key_data FROM dkim_keys WHERE domain = ?`, domain).
		Scan(&k.Domain, &k.Selector, &k.KeyData)
	if err == sql.ErrNoRows {
		return DKIMKey{}, false, nil
	}
	if err != nil {
		return DKIMKey{}, false, err
	}
	return k, true, nil
}

// ListDKIM returns every DKIM key.
func (s *Store) ListDKIM() ([]DKIMKey, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT domain, selector, key_data FROM dkim_keys ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DKIMKey
	for rows.Next() {
		var k DKIMKey
		if err := rows.Scan(&k.Domain, &k.Selector, &k.KeyData); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// UpsertDKIM inserts or updates DKIM keys. The key text must already be PEM.
func (s *Store) UpsertDKIM(keys []DKIMKey) (UpsertSummary, error) {
	var summary UpsertSummary
	for _, k := range keys {
		domain := strings.ToLower(strings.TrimSpace(k.Domain))
		if domain == "" || k.Selector == "" || strings.TrimSpace(k.KeyData) == "" {
			return summary, fmt.Errorf("dkim: domain, selector and key_data are required")
		}
		existing, found, err := s.GetDKIM(domain)
		if err != nil {
			return summary, err
		}
		if found && existing.Selector == k.Selector && existing.KeyData == k.KeyData {
			summary.Unchanged = append(summary.Unchanged, domain)
			continue
		}
		var sqlText string
		if s.dialect == "mysql" {
			sqlText = `INSERT INTO dkim_keys(domain, selector, key_data) VALUES(?, ?, ?)
				ON DUPLICATE KEY UPDATE selector = VALUES(selector), key_data = VALUES(key_data)`
		} else {
			sqlText = `INSERT INTO dkim_keys(domain, selector, key_data) VALUES(?, ?, ?)
				ON CONFLICT(domain) DO UPDATE SET selector = excluded.selector, key_data = excluded.key_data`
		}
		if _, err := s.db.ExecContext(context.Background(), sqlText, domain, k.Selector, k.KeyData); err != nil {
			return summary, err
		}
		if found {
			summary.Updated = append(summary.Updated, domain)
		} else {
			summary.Created = append(summary.Created, domain)
		}
	}
	return summary, nil
}

// DeleteDKIM removes the DKIM key of a domain. Deleting a nonexistent key is
// not an error.
func (s *Store) DeleteDKIM(domain string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	_, err := s.db.ExecContext(context.Background(),
		`DELETE FROM dkim_keys WHERE domain = ?`, domain)
	return err
}
