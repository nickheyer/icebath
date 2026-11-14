package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type StateDBStats struct {
	PageCount               sql.Null[int64]
	PageSize                sql.Null[int64]
	FreelistCount           sql.Null[int64]
	LogicalSizeBytes        sql.Null[int64]
	WALSizeBytes            sql.Null[int64]
	JournalMode             sql.Null[string]
	Messages                sql.Null[int64]
	Sessions                sql.Null[int64]
	FTSTables               map[string]bool
	FTSStorageVersion       sql.Null[int64]
	FTSStorageUpgradeNeeded sql.Null[bool]
	FTSTeardownPending      sql.Null[bool]
	FTSRebuildPending       sql.Null[bool]
	FTSRebuildHighWater     sql.Null[int64]
	FTSRebuildProgress      sql.Null[int64]
	FTSRebuildDeferral      json.RawMessage
}

func statsInt(src any) (int64, bool) {
	switch v := src.(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case bool:
		return int64(boolInt(v)), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	case []byte:
		return statsInt(string(v))
	}
	return 0, false
}

func (e *Engine) CollectStateDBStats(ctx context.Context, path string) StateDBStats {
	var st StateDBStats
	if fi, err := os.Stat(path + "-wal"); err == nil {
		st.WALSizeBytes = sql.Null[int64]{V: fi.Size(), Valid: true}
	} else if os.IsNotExist(err) {
		st.WALSizeBytes = sql.Null[int64]{Valid: true}
	}
	db, err := e.OpenDB(ctx, path, DBOptions{Label: storeLabel, ReadOnly: true, BusyTimeout: 2 * time.Second})
	if err != nil {
		slog.Debug(fmt.Sprintf("state.db stats: cannot open %s read-only: %s", path, errText(err)))
		return st
	}
	defer func() { _ = db.Close() }()
	scalar := func(query string, args ...any) (any, bool) {
		var v any
		if err := db.QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
			return nil, false
		}
		return v, v != nil
	}
	integer := func(query string, args ...any) sql.Null[int64] {
		v, ok := scalar(query, args...)
		if !ok {
			return sql.Null[int64]{}
		}
		n, ok := statsInt(v)
		return sql.Null[int64]{V: n, Valid: ok}
	}
	metaInt := func(key string) sql.Null[int64] {
		return integer("SELECT value FROM state_meta WHERE key = ?", key)
	}
	st.PageCount = integer("PRAGMA page_count")
	st.PageSize = integer("PRAGMA page_size")
	if st.PageCount.Valid && st.PageSize.Valid {
		st.LogicalSizeBytes = sql.Null[int64]{V: st.PageCount.V * st.PageSize.V, Valid: true}
	}
	st.FreelistCount = integer("PRAGMA freelist_count")
	if jm, ok := scalar("PRAGMA journal_mode"); ok {
		text, _ := cellText(jm)
		st.JournalMode = sql.Null[string]{V: text, Valid: true}
	}
	st.Messages = integer("SELECT COUNT(*) FROM messages")
	st.Sessions = integer("SELECT COUNT(*) FROM sessions")
	if names, err := queryStrings(ctx, db, "SELECT name FROM sqlite_master WHERE type = 'table' AND name IN (?, ?, ?)",
		ftsTables[0], ftsTables[1], ftsTables[2]); err == nil {
		st.FTSTables = map[string]bool{}
		for _, t := range ftsTables {
			st.FTSTables[t] = containsString(names, t)
		}
	}
	st.FTSStorageVersion = metaInt("fts_storage_version")
	if drift, err := ftsLayoutDrift(ctx, db); err == nil {
		st.FTSStorageUpgradeNeeded = sql.Null[bool]{V: len(drift) > 0, Valid: true}
	}
	var one int
	err = db.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name LIKE 'fts\_v22\_trash\_%' ESCAPE '\' `+
		"LIMIT 1").Scan(&one)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		st.FTSTeardownPending = sql.Null[bool]{V: err == nil, Valid: true}
	}
	st.FTSRebuildHighWater = metaInt("fts_rebuild_high_water")
	st.FTSRebuildProgress = metaInt("fts_rebuild_progress")
	st.FTSRebuildPending = sql.Null[bool]{V: st.FTSRebuildHighWater.Valid && st.FTSRebuildProgress.V < st.FTSRebuildHighWater.V, Valid: true}
	var raw sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key = ? LIMIT 1", ftsRebuildDeferralKey).Scan(&raw); err == nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(raw.String), &obj) == nil && obj != nil {
			st.FTSRebuildDeferral = json.RawMessage(raw.String)
		}
	}
	return st
}

func FTSLayoutDrift(ctx context.Context, c Conn) ([]string, error) { return ftsLayoutDrift(ctx, c) }
