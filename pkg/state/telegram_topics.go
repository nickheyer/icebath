package state

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type telegramTopicTable struct {
	name, columns, ddl string
}

var telegramTopicTables = []telegramTopicTable{
	{
		"telegram_dm_topic_mode",
		"profile_name, chat_id, user_id, enabled, activated_at, updated_at, " +
			"has_topics_enabled, allows_users_to_create_topics, capability_checked_at, intro_message_id, pinned_message_id",
		`
                    profile_name TEXT NOT NULL DEFAULT 'default',
                    chat_id TEXT NOT NULL,
                    user_id TEXT NOT NULL,
                    enabled INTEGER NOT NULL DEFAULT 1,
                    activated_at REAL NOT NULL,
                    updated_at REAL NOT NULL,
                    has_topics_enabled INTEGER,
                    allows_users_to_create_topics INTEGER,
                    capability_checked_at REAL,
                    intro_message_id TEXT,
                    pinned_message_id TEXT,
                    PRIMARY KEY (profile_name, chat_id)
                `,
	},
	{
		"telegram_dm_topic_bindings",
		"profile_name, chat_id, thread_id, user_id, session_key, session_id, managed_mode, linked_at, updated_at",
		`
                    profile_name TEXT NOT NULL DEFAULT 'default',
                    chat_id TEXT NOT NULL,
                    thread_id TEXT NOT NULL,
                    user_id TEXT NOT NULL,
                    session_key TEXT NOT NULL,
                    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
                    managed_mode TEXT NOT NULL DEFAULT 'auto',
                    linked_at REAL NOT NULL,
                    updated_at REAL NOT NULL,
                    PRIMARY KEY (profile_name, chat_id, thread_id)
                `,
	},
}

var unlinkedSelectHead = `
                    SELECT s.*,
                        COALESCE(sp.prompt, s.system_prompt)
                            AS _system_prompt_resolved,
                        ` + sqlPreviewRaw("s.id") + `,
                        ` + sqlSessionLastActive("s") + ` AS last_active
                    FROM sessions s
                    LEFT JOIN system_prompts sp
                      ON sp.hash = s.system_prompt_hash
                    WHERE s.source = 'telegram'
                      AND s.user_id = ?
`

const unlinkedSelectTail = `                    ORDER BY last_active DESC, s.started_at DESC
                    LIMIT ?
                    `

const unlinkedScopeClauses = `                      AND COALESCE(NULLIF(TRIM(s.profile_name), ''), 'default') = ?
                      AND NOT EXISTS (
                          SELECT 1 FROM telegram_dm_topic_bindings b
                          WHERE b.session_id = s.id
                      )
`

// Stripped profile name with empty or missing mapped to default
func normalizeTelegramTopicProfileName(profileName string) string {
	if name := pyStrip(profileName); name != "" {
		return name
	}
	return "default"
}

// Runs a topic read, absent tables reading as empty and a pre v3 table healed once before the retry
func topicRead[T any](ctx context.Context, d *SessionDB, read func() (T, error), empty T) (T, error) {
	out, err := read()
	if err == nil {
		return out, nil
	}
	if !isOperationalError(err) {
		return out, err
	}
	if !strings.Contains(errText(err), "no such column: profile_name") {
		return empty, nil
	}
	if herr := d.ApplyTelegramTopicMigration(ctx); herr != nil {
		var replaced *StateDBReplacedError
		if _, isSQLite := sqliteClassOf(herr); isSQLite || errors.As(herr, &replaced) {
			slog.Warn("telegram topic tables are pre-v3 and the heal failed; reading as empty", "error", herr)
			return empty, nil
		}
		return empty, herr
	}
	return read()
}

// One topic row or nil, through the self healing topic read
func (d *SessionDB) topicReadOne(ctx context.Context, query string, args ...any) (*pyjson.Object, error) {
	return topicRead(ctx, d, func() (*pyjson.Object, error) { return d.readOne(ctx, query, args...) }, nil)
}

// Every topic row, empty when the tables are absent, through the self healing topic read
func (d *SessionDB) topicReadAll(ctx context.Context, query string, args ...any) ([]*pyjson.Object, error) {
	return topicRead(ctx, d, func() ([]*pyjson.Object, error) { return d.readAll(ctx, query, args...) }, []*pyjson.Object{})
}

// Creates the topic tables, rebuilds pre v3 tables into the profile keyed shape and stamps schema version 3
func (d *SessionDB) ApplyTelegramTopicMigration(ctx context.Context) error {
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		for _, t := range telegramTopicTables {
			if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+t.name+" ("+t.ddl+")"); err != nil {
				return err
			}
			info, err := connRows(ctx, conn, "PRAGMA table_info('"+t.name+"')")
			if err != nil {
				return err
			}
			has := false
			for _, row := range info {
				if pyStr(row["name"]) == "profile_name" {
					has = true
				}
			}
			if has {
				continue
			}
			legacyColumns := strings.Replace(t.columns, "profile_name, ", "", 1)
			live := ""
			if strings.Contains(t.columns, "session_id") {
				live = " WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.id = session_id)"
			}
			if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+t.name+"_new"); err != nil {
				return err
			}
			if err := rebuildTable(ctx, conn, t.name, t.name+"_legacy", "CREATE TABLE "+t.name+" ("+t.ddl+")",
				"INSERT INTO "+t.name+" ("+t.columns+") SELECT 'default', "+legacyColumns+" FROM "+t.name+"_legacy"+live, nil); err != nil {
				return err
			}
		}
		for _, statement := range []string{
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_telegram_dm_topic_bindings_session ON telegram_dm_topic_bindings(session_id)",
			"CREATE INDEX IF NOT EXISTS idx_telegram_dm_topic_bindings_user ON telegram_dm_topic_bindings(profile_name, user_id, chat_id)",
		} {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO state_meta (key, value) VALUES (?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value", "telegram_dm_topic_schema_version", "3")
		return err
	}, nil)
}

type TelegramTopicMode struct {
	ChatID, UserID, ProfileName string
	HasTopicsEnabled            *bool
	AllowsUsersToCreateTopics   *bool
}

// Migrates the topic tables and upserts the enabled topic mode row of one private chat and user
func (d *SessionDB) EnableTelegramTopicMode(ctx context.Context, m TelegramTopicMode) error {
	if err := d.ApplyTelegramTopicMigration(ctx); err != nil {
		return err
	}
	ts := now()
	toInt := func(v *bool) any {
		if v == nil {
			return nil
		}
		if *v {
			return 1
		}
		return 0
	}
	return d.writeSQL(ctx, `
            INSERT INTO telegram_dm_topic_mode (
                profile_name, chat_id, user_id, enabled, activated_at, updated_at,
                has_topics_enabled, allows_users_to_create_topics,
                capability_checked_at
            ) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?)
            ON CONFLICT(profile_name, chat_id) DO UPDATE SET
                user_id = excluded.user_id,
                enabled = 1,
                updated_at = excluded.updated_at,
                has_topics_enabled = excluded.has_topics_enabled,
                allows_users_to_create_topics = excluded.allows_users_to_create_topics,
                capability_checked_at = excluded.capability_checked_at
            `, []any{normalizeTelegramTopicProfileName(m.ProfileName), m.ChatID, m.UserID, ts, ts,
		toInt(m.HasTopicsEnabled), toInt(m.AllowsUsersToCreateTopics), ts}, false, nil)
}

// Turns topic mode off for one chat and drops its bindings when asked, absent tables a silent no op
func (d *SessionDB) DisableTelegramTopicMode(ctx context.Context, chatID, profileName string, clearBindings bool) error {
	profileName = normalizeTelegramTopicProfileName(profileName)
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "UPDATE telegram_dm_topic_mode SET enabled = 0, updated_at = ? "+
			"WHERE profile_name = ? AND chat_id = ?", now(), profileName, chatID); err != nil {
			return suppressOperational(err)
		}
		if clearBindings {
			_, err := conn.ExecContext(ctx, "DELETE FROM telegram_dm_topic_bindings WHERE profile_name = ? AND chat_id = ?", profileName, chatID)
			return suppressOperational(err)
		}
		return nil
	}, nil)
}

// Whether topic mode is on for this chat and user under the profile namespace
func (d *SessionDB) IsTelegramTopicModeEnabled(ctx context.Context, chatID, userID, profileName string) (bool, error) {
	row, err := d.topicReadOne(ctx, `
                    SELECT enabled FROM telegram_dm_topic_mode
                    WHERE profile_name = ? AND chat_id = ? AND user_id = ?
                    `, normalizeTelegramTopicProfileName(profileName), chatID, userID)
	if err != nil || row == nil {
		return false, err
	}
	enabled, _ := row.Get("enabled")
	return pyTruthy(enabled), nil
}

// Session binding row of one chat topic thread, nil when unbound
func (d *SessionDB) GetTelegramTopicBinding(ctx context.Context, chatID, threadID, profileName string) (*pyjson.Object, error) {
	return d.topicReadOne(ctx, `
                    SELECT * FROM telegram_dm_topic_bindings
                    WHERE profile_name = ? AND chat_id = ? AND thread_id = ?
                    `, normalizeTelegramTopicProfileName(profileName), chatID, threadID)
}

// Every binding of one chat newest first, empty when the table is absent
func (d *SessionDB) ListTelegramTopicBindingsForChat(ctx context.Context, chatID, profileName string) ([]*pyjson.Object, error) {
	return d.topicReadAll(ctx, "SELECT * FROM telegram_dm_topic_bindings WHERE profile_name = ? AND chat_id = ? ORDER BY updated_at DESC",
		normalizeTelegramTopicProfileName(profileName), chatID)
}

// Binding row that holds one session, nil when the session is unbound
func (d *SessionDB) GetTelegramTopicBindingBySession(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	return d.topicReadOne(ctx, `
                    SELECT * FROM telegram_dm_topic_bindings
                    WHERE session_id = ?
                    `, sessionID)
}

// Deletes one chat thread binding and disables topic mode when it was the chat's last, returning rows deleted
func (d *SessionDB) DeleteTelegramTopicBinding(ctx context.Context, chatID, threadID, profileName string) (int, error) {
	profileName = normalizeTelegramTopicProfileName(profileName)
	deleted := 0
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		deleted = 0
		res, err := conn.ExecContext(ctx, `
                    DELETE FROM telegram_dm_topic_bindings
                    WHERE profile_name = ? AND chat_id = ? AND thread_id = ?
                    `, profileName, chatID, threadID)
		if err != nil {
			return suppressOperational(err)
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		deleted = int(n)
		remaining, err := connRows(ctx, conn, `
                    SELECT 1 FROM telegram_dm_topic_bindings
                    WHERE profile_name = ? AND chat_id = ? LIMIT 1
                    `, profileName, chatID)
		if err != nil {
			return suppressOperational(err)
		}
		if len(remaining) == 0 {
			_, err = conn.ExecContext(ctx, "UPDATE telegram_dm_topic_mode SET enabled = 0, updated_at = ? "+
				"WHERE profile_name = ? AND chat_id = ?", now(), profileName, chatID)
			return suppressOperational(err)
		}
		return nil
	}, nil)
	return deleted, err
}

type TelegramTopicBinding struct {
	ChatID, ThreadID, UserID, SessionKey, SessionID string
	ManagedMode, ProfileName                        string
}

// Binds one topic thread to one session, a ValueError when the session already holds a different topic
func (d *SessionDB) BindTelegramTopic(ctx context.Context, b TelegramTopicBinding) error {
	if err := d.ApplyTelegramTopicMigration(ctx); err != nil {
		return err
	}
	ts := now()
	profileName := normalizeTelegramTopicProfileName(b.ProfileName)
	managedMode := b.ManagedMode
	if managedMode == "" {
		managedMode = "auto"
	}
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		existing, err := connRows(ctx, conn, `
                SELECT profile_name, chat_id, thread_id
                FROM telegram_dm_topic_bindings
                WHERE session_id = ?
                `, b.SessionID)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			e := existing[0]
			if pyStr(e["profile_name"]) != profileName || pyStr(e["chat_id"]) != b.ChatID || pyStr(e["thread_id"]) != b.ThreadID {
				return &pyConvError{"ValueError", "session is already linked to another Telegram topic"}
			}
		}
		_, err = conn.ExecContext(ctx, `
                INSERT INTO telegram_dm_topic_bindings (
                    profile_name, chat_id, thread_id, user_id, session_key, session_id,
                    managed_mode, linked_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(profile_name, chat_id, thread_id) DO UPDATE SET
                    user_id = excluded.user_id,
                    session_key = excluded.session_key,
                    session_id = excluded.session_id,
                    managed_mode = excluded.managed_mode,
                    updated_at = excluded.updated_at
                `, profileName, b.ChatID, b.ThreadID, b.UserID, b.SessionKey, b.SessionID, managedMode, ts, ts)
		return err
	}, nil)
}

// Whether the session is bound to any topic, false when the tables are absent
func (d *SessionDB) IsTelegramSessionLinkedToTopic(ctx context.Context, sessionID string) (bool, error) {
	row, err := d.topicReadOne(ctx, `
                    SELECT 1 FROM telegram_dm_topic_bindings
                    WHERE session_id = ?
                    LIMIT 1
                    `, sessionID)
	return row != nil, err
}

// Rich rows of the user's telegram sessions bound to no topic, profile scoped once the bindings table exists
func (d *SessionDB) ListUnlinkedTelegramSessionsForUser(ctx context.Context, userID, profileName string, limit int) ([]*pyjson.Object, error) {
	profileName = normalizeTelegramTopicProfileName(profileName)
	var rows []*pyjson.Object
	err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		var err error
		rows, err = queryObjects(ctx, conn, unlinkedSelectHead+unlinkedScopeClauses+unlinkedSelectTail, userID, profileName, limit)
		if isOperationalError(err) {
			rows, err = queryObjects(ctx, conn, unlinkedSelectHead+unlinkedSelectTail, userID, limit)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]*pyjson.Object, 0, len(rows))
	for _, row := range rows {
		out = append(out, richRow(row))
	}
	return out, nil
}
