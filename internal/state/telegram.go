package state

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"

	sqlite3 "modernc.org/sqlite/lib"
)

var ErrTopicAlreadyLinked = errors.New("session is already linked to another Telegram topic")

type topicTable struct {
	name    string
	columns string
	ddl     string
}

var topicTables = []topicTable{
	{"telegram_dm_topic_mode",
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
                `},
	{"telegram_dm_topic_bindings",
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
                `},
}

var unlinkedSelectHead = `
                    SELECT s.*,
                        COALESCE(sp.prompt, s.system_prompt)
                            AS _system_prompt_resolved,
                        ` + previewRawSQL("s.id") + `,
                        ` + SessionLastActiveSQL("s") + ` AS last_active
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

const topicBindingColumns = "profile_name, chat_id, thread_id, user_id, session_key, session_id, managed_mode, linked_at, updated_at"

type TopicBinding struct {
	ProfileName string
	ChatID      string
	ThreadID    string
	UserID      string
	SessionKey  string
	SessionID   string
	ManagedMode string
	LinkedAt    float64
	UpdatedAt   float64
}

type TopicMode struct {
	ChatID                    string
	UserID                    string
	ProfileName               string
	HasTopicsEnabled          *bool
	AllowsUsersToCreateTopics *bool
}

type TopicBind struct {
	ChatID      string
	ThreadID    string
	UserID      string
	SessionKey  string
	SessionID   string
	ManagedMode string
	ProfileName string
}

func normalizeTopicProfile(name string) string {
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		return trimmed
	}
	return "default"
}

func operationalError(err error) bool {
	code, ok := primaryCode(err)
	return ok && code == sqlite3.SQLITE_ERROR
}

func (s *Store) topicRead(ctx context.Context, read func(c Conn) error, reset func()) error {
	run := func() error {
		return s.Read(ctx, func(c Conn) error {
			reset()
			return read(c)
		})
	}
	err := run()
	if err == nil || !operationalError(err) {
		return err
	}
	if !strings.Contains(errText(err), "no such column: profile_name") {
		reset()
		return nil
	}
	if merr := s.ApplyTelegramTopicMigration(ctx); merr != nil {
		if ctx.Err() != nil {
			return merr
		}
		slog.Warn("telegram topic tables are pre-v3 and the heal failed; reading as empty", "error", errText(merr))
		reset()
		return nil
	}
	return run()
}

func (s *Store) ApplyTelegramTopicMigration(ctx context.Context) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		for _, t := range topicTables {
			if _, err := c.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+t.name+" ("+t.ddl+")"); err != nil {
				return err
			}
			cols, err := tableInfo(ctx, c, t.name)
			if err != nil {
				return err
			}
			has := false
			for _, col := range cols {
				has = has || col.name == "profile_name"
			}
			if has {
				continue
			}
			legacyColumns := strings.Replace(t.columns, "profile_name, ", "", 1)
			live := ""
			if strings.Contains(t.columns, "session_id") {
				live = " WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.id = session_id)"
			}
			if _, err := c.ExecContext(ctx, "DROP TABLE IF EXISTS "+t.name+"_new"); err != nil {
				return err
			}
			if err := rebuildTable(ctx, c, t.name, t.name+"_legacy", "CREATE TABLE "+t.name+" ("+t.ddl+")",
				"INSERT INTO "+t.name+" ("+t.columns+") SELECT 'default', "+legacyColumns+" FROM "+t.name+"_legacy"+live, nil); err != nil {
				return err
			}
		}
		for _, q := range []string{
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_telegram_dm_topic_bindings_session ON telegram_dm_topic_bindings(session_id)",
			"CREATE INDEX IF NOT EXISTS idx_telegram_dm_topic_bindings_user ON telegram_dm_topic_bindings(profile_name, user_id, chat_id)",
		} {
			if _, err := c.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		_, err := c.ExecContext(ctx, "INSERT INTO state_meta (key, value) VALUES (?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value", "telegram_dm_topic_schema_version", "3")
		return err
	})
}

func boolArg(v *bool) any {
	if v == nil {
		return nil
	}
	return boolInt(*v)
}

func (s *Store) EnableTelegramTopicMode(ctx context.Context, m TopicMode) error {
	if err := s.ApplyTelegramTopicMigration(ctx); err != nil {
		return err
	}
	now := nowEpoch()
	return s.WriteSQL(ctx, WritePatience, `
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
            `, normalizeTopicProfile(m.ProfileName), m.ChatID, m.UserID, now, now, boolArg(m.HasTopicsEnabled),
		boolArg(m.AllowsUsersToCreateTopics), now)
}

func execTolerant(ctx context.Context, c Conn, query string, args ...any) (int64, bool, error) {
	n, err := rowsChanged(c.ExecContext(ctx, query, args...))
	if err != nil && operationalError(err) {
		return 0, false, nil
	}
	return n, err == nil, err
}

func (s *Store) DisableTelegramTopicMode(ctx context.Context, chatID, profileName string, clearBindings bool) error {
	profile := normalizeTopicProfile(profileName)
	return s.Write(ctx, WritePatience, func(c Conn) error {
		_, ok, err := execTolerant(ctx, c, "UPDATE telegram_dm_topic_mode SET enabled = 0, updated_at = ? "+
			"WHERE profile_name = ? AND chat_id = ?", nowEpoch(), profile, chatID)
		if err != nil || !ok || !clearBindings {
			return err
		}
		_, _, err = execTolerant(ctx, c, "DELETE FROM telegram_dm_topic_bindings WHERE profile_name = ? AND chat_id = ?", profile, chatID)
		return err
	})
}

func (s *Store) IsTelegramTopicModeEnabled(ctx context.Context, chatID, userID, profileName string) (bool, error) {
	enabled := false
	err := s.topicRead(ctx, func(c Conn) error {
		var v sql.NullInt64
		err := c.QueryRowContext(ctx, `
                    SELECT enabled FROM telegram_dm_topic_mode
                    WHERE profile_name = ? AND chat_id = ? AND user_id = ?
                    `, normalizeTopicProfile(profileName), chatID, userID).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		enabled = v.Valid && v.Int64 != 0
		return err
	}, func() { enabled = false })
	return enabled, err
}

func scanTopicBindings(rows *sql.Rows, err error) ([]TopicBinding, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TopicBinding
	for rows.Next() {
		var b TopicBinding
		var mode sql.NullString
		if err := rows.Scan(&b.ProfileName, &b.ChatID, &b.ThreadID, &b.UserID, &b.SessionKey, &b.SessionID, &mode, &b.LinkedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.ManagedMode = mode.String
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) topicBindings(ctx context.Context, query string, args ...any) ([]TopicBinding, error) {
	var out []TopicBinding
	err := s.topicRead(ctx, func(c Conn) error {
		var err error
		out, err = scanTopicBindings(c.QueryContext(ctx, "SELECT "+topicBindingColumns+" FROM telegram_dm_topic_bindings "+query, args...))
		return err
	}, func() { out = nil })
	return out, err
}

func firstBinding(bindings []TopicBinding, err error) (TopicBinding, bool, error) {
	if err != nil || len(bindings) == 0 {
		return TopicBinding{}, false, err
	}
	return bindings[0], true, nil
}

func (s *Store) GetTelegramTopicBinding(ctx context.Context, chatID, threadID, profileName string) (TopicBinding, bool, error) {
	return firstBinding(s.topicBindings(ctx, "WHERE profile_name = ? AND chat_id = ? AND thread_id = ? LIMIT 1",
		normalizeTopicProfile(profileName), chatID, threadID))
}

func (s *Store) ListTelegramTopicBindingsForChat(ctx context.Context, chatID, profileName string) ([]TopicBinding, error) {
	return s.topicBindings(ctx, "WHERE profile_name = ? AND chat_id = ? ORDER BY updated_at DESC", normalizeTopicProfile(profileName), chatID)
}

func (s *Store) GetTelegramTopicBindingBySession(ctx context.Context, sessionID string) (TopicBinding, bool, error) {
	return firstBinding(s.topicBindings(ctx, "WHERE session_id = ? LIMIT 1", sessionID))
}

func (s *Store) DeleteTelegramTopicBinding(ctx context.Context, chatID, threadID, profileName string) (int64, error) {
	profile := normalizeTopicProfile(profileName)
	var deleted int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		deleted, _, err = execTolerant(ctx, c, `
                    DELETE FROM telegram_dm_topic_bindings
                    WHERE profile_name = ? AND chat_id = ? AND thread_id = ?
                    `, profile, chatID, threadID)
		if err != nil || deleted == 0 {
			return err
		}
		remaining, err := queryStrings(ctx, c, `
                    SELECT 1 FROM telegram_dm_topic_bindings
                    WHERE profile_name = ? AND chat_id = ? LIMIT 1
                    `, profile, chatID)
		if err != nil {
			if operationalError(err) {
				return nil
			}
			return err
		}
		if len(remaining) == 0 {
			_, _, err = execTolerant(ctx, c, "UPDATE telegram_dm_topic_mode SET enabled = 0, updated_at = ? "+
				"WHERE profile_name = ? AND chat_id = ?", nowEpoch(), profile, chatID)
		}
		return err
	})
	return deleted, err
}

func (s *Store) BindTelegramTopic(ctx context.Context, b TopicBind) error {
	if err := s.ApplyTelegramTopicMigration(ctx); err != nil {
		return err
	}
	now := nowEpoch()
	profile := normalizeTopicProfile(b.ProfileName)
	mode := b.ManagedMode
	if mode == "" {
		mode = "auto"
	}
	return s.Write(ctx, WritePatience, func(c Conn) error {
		var linkedProfile, linkedChat, linkedThread string
		err := c.QueryRowContext(ctx, `
                SELECT profile_name, chat_id, thread_id
                FROM telegram_dm_topic_bindings
                WHERE session_id = ?
                `, b.SessionID).Scan(&linkedProfile, &linkedChat, &linkedThread)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case linkedProfile != profile || linkedChat != b.ChatID || linkedThread != b.ThreadID:
			return ErrTopicAlreadyLinked
		}
		_, err = c.ExecContext(ctx, `
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
                `, profile, b.ChatID, b.ThreadID, b.UserID, b.SessionKey, b.SessionID, mode, now, now)
		return err
	})
}

func (s *Store) IsTelegramSessionLinkedToTopic(ctx context.Context, sessionID string) (bool, error) {
	linked := false
	err := s.topicRead(ctx, func(c Conn) error {
		hits, err := queryStrings(ctx, c, `
                    SELECT 1 FROM telegram_dm_topic_bindings
                    WHERE session_id = ?
                    LIMIT 1
                    `, sessionID)
		linked = len(hits) > 0
		return err
	}, func() { linked = false })
	return linked, err
}

func (s *Store) ListUnlinkedTelegramSessionsForUser(ctx context.Context, userID, profileName string, limit int) ([]SessionListing, error) {
	profile := normalizeTopicProfile(profileName)
	var out []SessionListing
	err := s.Read(ctx, func(c Conn) error {
		var err error
		out, err = queryListings(c.QueryContext(ctx, unlinkedSelectHead+unlinkedScopeClauses+unlinkedSelectTail, userID, profile, limit))
		if err != nil && operationalError(err) {
			out, err = queryListings(c.QueryContext(ctx, unlinkedSelectHead+unlinkedSelectTail, userID, limit))
		}
		return err
	})
	return out, err
}
