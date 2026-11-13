package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Startup watchdog lease long schema work declares before it starts
const schemaWorkLease = 600 * time.Second

// Meta keys of the message uid backfill, done and its resume cursor, with its chunk and per-open budget
const (
	messageUIDBackfillDone   = "message_uid_backfill"
	messageUIDBackfillCursor = "message_uid_backfill_id"
	messageUIDBackfillChunk  = 2000
	messageUIDBackfillBudget = time.Second
)

// Upserts of a stale breadcrumb and of a meta value, and deletes of the rebuild markers and retired tool mark
const (
	staleKeyUpsertSQL           = "INSERT INTO state_meta (key, value) VALUES (?, '1') ON CONFLICT(key) DO UPDATE SET value = excluded.value"
	stateMetaUpsertSQL          = "INSERT INTO state_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value"
	clearRebuildMarkersSQL      = "DELETE FROM state_meta WHERE key IN ('fts_rebuild_high_water', 'fts_rebuild_progress')"
	dropRetiredToolHighWaterSQL = "DELETE FROM state_meta WHERE key = 'fts_tool_full_content_high_water'"
)

// Creates tables and full-text indexes, reconciles columns, heals keys and runs the version gated data migrations
func initSessionSchema(ctx context.Context, s *Store, c Conn) error {
	st := s.tables.state
	st.options.progress(schemaWorkLease, "state_db_init_schema")
	if _, err := c.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	if err := reconcileColumns(ctx, c); err != nil {
		return err
	}
	if err := healGatewayRoutingPK(ctx, c); err != nil {
		return err
	}
	if err := healSessionModelUsagePK(ctx, c); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_messages_platform_msg_id "+
		"ON messages(session_id, platform_message_id) WHERE platform_message_id IS NOT NULL"); err != nil {
		if familyOf(err) != callFamily {
			return err
		}
		slog.Debug(fmt.Sprintf("idx_messages_platform_msg_id create skipped: %s", errText(err)))
	}
	if err := executeDDLSkippingSettledTriggers(ctx, c, deferredIndexSQL); err != nil {
		return err
	}
	if err := healNullActive(ctx, c); err != nil {
		return err
	}
	fts5, err := s.sqliteSupportsFTS5(ctx, c)
	if err != nil {
		return err
	}
	_, stale, err := metaValue(ctx, c, ftsStaleKey)
	if err != nil {
		return err
	}
	s.fts().set(func(f *ftsFlags) { f.stale = stale })
	if stale {
		if err := dropAllFTSTriggers(ctx, c); err != nil {
			return err
		}
	}
	if !fts5 {
		if err := dropFTSTriggers(ctx, c); err != nil {
			return err
		}
	}
	var version sql.NullInt64
	err = c.QueryRowContext(ctx, "SELECT version FROM schema_version LIMIT 1").Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := stampFreshStore(ctx, c); err != nil {
			return err
		}
	case err != nil:
		return err
	case !version.Valid:
		return contentErr(nil, "schema_version holds no version number")
	default:
		if err := s.runDataMigrations(ctx, c, version.Int64, fts5); err != nil {
			return err
		}
	}
	if err := ensureUniqueTitleIndex(ctx, c); err != nil {
		return err
	}
	if !fts5 {
		return nil
	}
	if err := s.initFTS(ctx, c); err != nil {
		return err
	}
	return s.stampSettledFTSLayout(ctx, c)
}

// Records the current version and the provenance that tells a fresh store from a wiped one
func stampFreshStore(ctx context.Context, c Conn) error {
	if _, err := c.ExecContext(ctx, "INSERT INTO schema_version (version) VALUES (?)", SchemaVersion); err != nil {
		return err
	}
	instance, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	for _, kv := range [][2]string{
		{"store_instance_id", instance.String()}, {"store_created_at_utc", isoUTC(time.Now())}, {messageUIDBackfillDone, "1"},
	} {
		if _, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta (key, value) VALUES (?, ?)", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// ISO 8601 UTC instant with microseconds when nonzero and an explicit zero offset
func isoUTC(t time.Time) string {
	t = t.UTC()
	text := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		text += fmt.Sprintf(".%06d", us)
	}
	return text + "+00:00"
}

// Un-hides rows a defaultless active column left NULL, reading first so a settled store takes no write lock
func healNullActive(ctx context.Context, c Conn) error {
	var one int
	err := c.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE active IS NULL LIMIT 1").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil {
		_, err = c.ExecContext(ctx, "UPDATE messages SET active = 1 WHERE active IS NULL")
	}
	if err != nil && familyOf(err) != callFamily {
		return err
	}
	return nil
}

// Value of one meta key read on c, false when absent
func metaValue(ctx context.Context, c Conn, key string) (string, bool, error) {
	var v sql.NullString
	err := c.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v.String, true, nil
}

// Deletes meta keys on c
func deleteMeta(ctx context.Context, c Conn, keys ...string) error {
	_, err := c.ExecContext(ctx, "DELETE FROM state_meta WHERE key IN ("+bindMarks(len(keys))+")", stringArgs(keys)...)
	return err
}

// Version gated data migrations in the reference order, advancing schema_version last unless FTS5 is missing
func (s *Store) runDataMigrations(ctx context.Context, c Conn, current int64, fts5 bool) error {
	s.tables.state.options.progress(schemaWorkLease, "state_db_data_migrations")
	if current < 16 {
		if err := migrateDelegateMarkers(ctx, c); err != nil {
			return err
		}
	}
	if current < 18 {
		if err := s.backfillGatewayMetadata(ctx, c); err != nil {
			slog.Debug(fmt.Sprintf("v18 gateway metadata backfill skipped: %s", errText(err)))
		}
	}
	if current < 20 {
		if _, err := c.ExecContext(ctx, sessionModelUsageV20SeedSQL); err != nil && familyOf(err) != callFamily {
			return err
		}
	}
	if current < 22 {
		if err := migrateV22SessionModelUsage(ctx, c); err != nil {
			return err
		}
	}
	if current < 23 && fts5 {
		drift, err := ftsLayoutDrift(ctx, c)
		if err != nil {
			return err
		}
		if len(drift) > 0 {
			if err := SetMetaIn(ctx, c, "fts_optimize_available", "1"); err != nil {
				return err
			}
		}
	}
	if current < 25 {
		if err := dedupeLegacySystemPrompts(ctx, c); err != nil {
			return err
		}
	}
	if err := advanceMessageUIDBackfill(ctx, c); err != nil {
		return err
	}
	complete := true
	if current < 30 && fts5 {
		var err error
		if complete, err = s.migrateTrigramCronExclusion(ctx, c); err != nil {
			return err
		}
	}
	if current < SchemaVersion && complete && fts5 {
		if _, err := c.ExecContext(ctx, "UPDATE schema_version SET version = ?", SchemaVersion); err != nil {
			return err
		}
	}
	return nil
}

// v16 tags delegate subagent rows and orphaned tool-only stubs so pickers stay clean after parent deletes
func migrateDelegateMarkers(ctx context.Context, c Conn) error {
	for _, q := range []string{
		"UPDATE sessions SET model_config = json_set(COALESCE(model_config, '{}'), '$._delegate_from', parent_session_id) " +
			"WHERE parent_session_id IS NOT NULL AND " + jsonExtractSQL("model_config", "$._delegate_from") + " IS NULL " +
			"AND " + ephemeralChildSQL("sessions"),
		"UPDATE sessions SET model_config = json_set(COALESCE(model_config, '{}'), '$._delegate_from', '__orphaned__') " +
			"WHERE parent_session_id IS NULL AND " + jsonExtractSQL("model_config", "$._delegate_from") + " IS NULL " +
			"AND " + jsonExtractSQL("model_config", "$._branched_from") + " IS NULL " +
			"AND title IS NULL AND message_count <= 25 AND EXISTS (SELECT 1 FROM messages m " +
			"            WHERE m.session_id = sessions.id AND m.role = 'tool') " +
			"AND NOT EXISTS (SELECT 1 FROM sessions ch " +
			"                WHERE ch.parent_session_id = sessions.id)",
	} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			if familyOf(err) == callFamily {
				return nil
			}
			return err
		}
	}
	return nil
}

// v25 moves inline prompt snapshots into the content addressed table, pausing on contention for the next open to resume
func dedupeLegacySystemPrompts(ctx context.Context, c Conn) error {
	type pending struct{ id, prompt string }
	rows, err := c.QueryContext(ctx, "SELECT id, system_prompt FROM sessions WHERE system_prompt IS NOT NULL")
	if err != nil {
		if familyOf(err) == callFamily {
			return nil
		}
		return err
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.prompt); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		if familyOf(err) == callFamily {
			return nil
		}
		return err
	}
	for _, p := range todo {
		hash, err := storeSystemPrompt(ctx, c, &p.prompt)
		if err == nil {
			_, err = c.ExecContext(ctx, "UPDATE sessions SET system_prompt_hash = ?, system_prompt = NULL WHERE id = ?", hash, p.id)
		}
		if err != nil {
			if familyOf(err) != callFamily {
				return err
			}
			slog.Warn(fmt.Sprintf("v25 prompt dedupe paused after contention (%s); unmigrated rows keep the legacy inline "+
				"prompt and the next schema init resumes the migration.", errText(err)))
			return nil
		}
	}
	return nil
}

// Mints message_uid onto rows that predate the column one bounded slice per open, keyed on rowid
func advanceMessageUIDBackfill(ctx context.Context, c Conn) error {
	if _, done, err := metaValue(ctx, c, messageUIDBackfillDone); err != nil || done {
		return err
	}
	cursorText, ok, err := metaValue(ctx, c, messageUIDBackfillCursor)
	if err != nil {
		return err
	}
	var doneThrough int64
	if ok {
		if _, err := fmt.Sscan(cursorText, &doneThrough); err != nil {
			return contentErr(err, "invalid %s value %q", messageUIDBackfillCursor, cursorText)
		}
	}
	var high sql.NullInt64
	if err := c.QueryRowContext(ctx, "SELECT MAX(rowid) FROM messages").Scan(&high); err != nil {
		return err
	}
	deadline := time.Now().Add(messageUIDBackfillBudget)
	for doneThrough < high.Int64 {
		upper := doneThrough + messageUIDBackfillChunk
		if _, err := c.ExecContext(ctx, "UPDATE messages SET message_uid = lower(hex(randomblob(16))) "+
			"WHERE rowid > ? AND rowid <= ? AND message_uid IS NULL", doneThrough, upper); err != nil {
			return err
		}
		doneThrough = upper
		if doneThrough < high.Int64 && !time.Now().Before(deadline) {
			return SetMetaIn(ctx, c, messageUIDBackfillCursor, fmt.Sprint(doneThrough))
		}
	}
	if err := SetMetaIn(ctx, c, messageUIDBackfillDone, "1"); err != nil {
		return err
	}
	return deleteMeta(ctx, c, messageUIDBackfillCursor)
}
