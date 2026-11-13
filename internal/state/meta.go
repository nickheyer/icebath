package state

import (
	"context"
	"database/sql"
	"errors"
)

// One key and value of the engine meta table
type MetaEntry struct {
	Key   string
	Value string
}

// Upsert of one meta key
const setMetaSQL = "INSERT INTO state_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value"

// Runs fn on the writer connection under the writer lock outside any transaction, reopening after a racing close
func (s *Store) withWriter(ctx context.Context, purpose string, fn func(c Conn) error) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.conn == nil {
		if err := s.reopenAfterClose(ctx, purpose); err != nil {
			return err
		}
	}
	return fn(s.conn.q())
}

// Meta value read on the writer under its lock so a WAL reader lag never hides it, false when absent
func (s *Store) GetMeta(ctx context.Context, key string) (string, bool, error) {
	var value sql.NullString
	found := true
	err := s.withWriter(ctx, "read", func(c Conn) error {
		err := c.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key = ?", key).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	if err != nil {
		return "", false, presentable(err)
	}
	return value.String, found, nil
}

// Upserts a meta key through its own write
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.WriteSQL(ctx, WritePatience, setMetaSQL, key, value)
}

// Upserts a meta key inline on the connection of a transaction the caller already holds
func SetMetaIn(ctx context.Context, c Conn, key, value string) error {
	_, err := c.ExecContext(ctx, setMetaSQL, key, value)
	return err
}

// Meta entries whose keys start with the literal prefix, wildcards escaped, none for an empty prefix
func (s *Store) ListMetaPrefix(ctx context.Context, prefix string) ([]MetaEntry, error) {
	if prefix == "" {
		return nil, nil
	}
	var out []MetaEntry
	err := s.Read(ctx, func(c Conn) error {
		out = out[:0]
		rows, err := c.QueryContext(ctx, `SELECT key, value FROM state_meta WHERE key LIKE ? ESCAPE '\'`, EscapeLike(prefix)+"%")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k, v sql.NullString
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			out = append(out, MetaEntry{Key: k.String, Value: v.String})
		}
		return rows.Err()
	})
	return out, err
}
