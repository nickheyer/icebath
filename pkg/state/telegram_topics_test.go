package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

const topicChat = "208214988"

type sqliteAuthorizerFunc = func(tls *libc.TLS, arg uintptr, action int32, arg1, arg2, dbName, trigger uintptr) int32

var keptAuthorizers []*struct{ f sqliteAuthorizerFunc }

// Installs fn as the writer connection authorizer, nil removing it, the sqlite3 set_authorizer of the test
func setWriterAuthorizer(t *testing.T, db *SessionDB, fn func(action int32, arg1, arg2, dbName, trigger string) int32) {
	t.Helper()
	db.mu.Lock()
	conn := db.conn
	db.mu.Unlock()
	var ptr uintptr
	if fn != nil {
		holder := &struct{ f sqliteAuthorizerFunc }{func(_ *libc.TLS, _ uintptr, action int32, a1, a2, a3, a4 uintptr) int32 {
			return fn(action, libc.GoString(a1), libc.GoString(a2), libc.GoString(a3), libc.GoString(a4))
		}}
		keptAuthorizers = append(keptAuthorizers, holder)
		ptr = *(*uintptr)(unsafe.Pointer(holder))
	}
	if err := withRawSQLite(conn, func(tls *libc.TLS, h uintptr) error {
		sqlite3.Xsqlite3_set_authorizer(tls, h, ptr, 0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Writes the pre v3 topic schema an install upgraded from before profile namespaces keeps on disk
func createV2TopicState(t *testing.T, path, version, bindingFK string, orphan bool) {
	t.Helper()
	conn := openRaw(t, path)
	defer conn.Close()
	statements := []string{
		"CREATE TABLE state_meta (key TEXT PRIMARY KEY, value TEXT)",
		"INSERT INTO state_meta(key, value) VALUES ('telegram_dm_topic_schema_version', '" + version + "')",
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT, user_id TEXT, model TEXT, model_config TEXT, system_prompt TEXT,
			parent_session_id TEXT, started_at REAL, ended_at REAL, end_reason TEXT, message_count INTEGER DEFAULT 0,
			tool_call_count INTEGER DEFAULT 0, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0)`,
		"INSERT INTO sessions(id, source, user_id, started_at) VALUES ('legacy-sess', 'telegram', '" + topicChat + "', 1.0)",
		`CREATE TABLE telegram_dm_topic_mode (chat_id TEXT PRIMARY KEY, user_id TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
			activated_at REAL NOT NULL, updated_at REAL NOT NULL, has_topics_enabled INTEGER, allows_users_to_create_topics INTEGER,
			capability_checked_at REAL, intro_message_id TEXT, pinned_message_id TEXT)`,
		"INSERT INTO telegram_dm_topic_mode(chat_id, user_id, enabled, activated_at, updated_at) VALUES ('" + topicChat + "', '" + topicChat + "', 1, 1.0, 1.0)",
		`CREATE TABLE telegram_dm_topic_bindings (chat_id TEXT NOT NULL, thread_id TEXT NOT NULL, user_id TEXT NOT NULL,
			session_key TEXT NOT NULL, session_id TEXT NOT NULL REFERENCES sessions(id)` + bindingFK + `, managed_mode TEXT NOT NULL DEFAULT 'auto',
			linked_at REAL NOT NULL, updated_at REAL NOT NULL, PRIMARY KEY (chat_id, thread_id))`,
		"INSERT INTO telegram_dm_topic_bindings VALUES ('" + topicChat + "', '99', '" + topicChat + "', 'k', 'legacy-sess', 'auto', 1.0, 1.0)",
	}
	if orphan {
		statements = append(statements,
			"INSERT INTO telegram_dm_topic_bindings VALUES ('"+topicChat+"', '7', '"+topicChat+"', 'k7', 'pruned-sess', 'auto', 1.0, 1.0)",
			"CREATE TABLE telegram_dm_topic_mode_new (profile_name TEXT, chat_id TEXT)")
	} else {
		statements = append(statements, "CREATE INDEX idx_telegram_dm_topic_bindings_user ON telegram_dm_topic_bindings(user_id, chat_id)")
	}
	execAll(t, conn, statements...)
}

func topicEnabled(t *testing.T, db *SessionDB, chat, user, profile string) bool {
	t.Helper()
	on, err := db.IsTelegramTopicModeEnabled(context.Background(), chat, user, profile)
	if err != nil {
		t.Fatal(err)
	}
	return on
}

func topicBinding(t *testing.T, db *SessionDB, chat, thread, profile string) map[string]any {
	t.Helper()
	row, err := db.GetTelegramTopicBinding(context.Background(), chat, thread, profile)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range row.Keys {
		out[k], _ = row.Get(k)
	}
	return out
}

func topicMeta(t *testing.T, db *SessionDB) any {
	t.Helper()
	v, ok, err := db.GetMeta(context.Background(), "telegram_dm_topic_schema_version")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return nil
	}
	return v
}

func seedTopicBinding(t *testing.T, db *SessionDB, chat, thread, user, sessionID, profile string) {
	t.Helper()
	if err := db.CreateSession(NewSession{ID: sessionID, Source: "telegram", UserID: user}); err != nil {
		t.Fatal(err)
	}
	if err := db.BindTelegramTopic(context.Background(), TelegramTopicBinding{ChatID: chat, ThreadID: thread, UserID: user,
		SessionKey: "agent:main:telegram:dm:" + chat + ":" + thread, SessionID: sessionID, ProfileName: profile}); err != nil {
		t.Fatal(err)
	}
}

func TestTopicReadsSelfhealPreV3Tables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2-upgrade.db")
	createV2TopicState(t, path, "2", " ON DELETE CASCADE", true)
	db := usageOpen(t, path)
	if !topicEnabled(t, db, topicChat, topicChat, "default") {
		t.Fatal("legacy topic mode read as off")
	}
	if b := topicBinding(t, db, topicChat, "99", "default"); b == nil || b["session_id"] != "legacy-sess" {
		t.Fatal(b)
	}
	rows, err := db.ListTelegramTopicBindingsForChat(context.Background(), topicChat, "default")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if sid, _ := rows[0].Get("session_id"); sid != "legacy-sess" {
		t.Fatal(sid)
	}
	if b := topicBinding(t, db, topicChat, "7", "default"); b != nil {
		t.Fatal("orphan binding survived the heal")
	}
	if got := repairScalar(t, db, "SELECT COUNT(*) FROM sqlite_master WHERE name = 'telegram_dm_topic_mode_new'"); got != int64(0) {
		t.Fatal("stranded scratch table survived")
	}
	if got := topicMeta(t, db); got != "3" {
		t.Fatal(got)
	}
	if topicEnabled(t, db, topicChat, topicChat, "coder") || topicBinding(t, db, topicChat, "99", "coder") != nil {
		t.Fatal("profile isolation broke after the heal")
	}
}

func TestAbsentTablesStillReadEmptyAndAreNotCreated(t *testing.T) {
	db := usageOpen(t, filepath.Join(t.TempDir(), "fresh.db"))
	if topicEnabled(t, db, topicChat, topicChat, "") || topicBinding(t, db, topicChat, "99", "") != nil {
		t.Fatal("absent tables read as populated")
	}
	if rows, err := db.ListTelegramTopicBindingsForChat(context.Background(), topicChat, ""); err != nil || len(rows) != 0 || rows == nil {
		t.Fatal(rows, err)
	}
	if got := topicMeta(t, db); got != nil {
		t.Fatal(got)
	}
}

func TestHealRebuildRollsBackAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2-upgrade.db")
	createV2TopicState(t, path, "2", " ON DELETE CASCADE", true)
	db := usageOpen(t, path)
	setWriterAuthorizer(t, db, func(action int32, arg1, _, _, _ string) int32 {
		if action == sqlite3.SQLITE_INSERT && arg1 == "telegram_dm_topic_mode" {
			return sqlite3.SQLITE_DENY
		}
		return sqlite3.SQLITE_OK
	})
	enabled := topicEnabled(t, db, topicChat, topicChat, "default")
	setWriterAuthorizer(t, db, nil)
	if enabled {
		t.Fatal("a failed heal read as on")
	}
	if got := topicMeta(t, db); got != "2" {
		t.Fatal(got)
	}
	if !topicEnabled(t, db, topicChat, topicChat, "default") {
		t.Fatal("the retry after the rolled back heal lost the legacy rows")
	}
}

func TestTelegramTopicBindingRoundtripRequiresExplicitSchema(t *testing.T) {
	db := usageDB(t)
	if err := db.CreateSession(NewSession{ID: "topic-session", Source: "telegram", UserID: topicChat}); err != nil {
		t.Fatal(err)
	}
	if b := topicBinding(t, db, topicChat, "17585", ""); b != nil {
		t.Fatal(b)
	}
	if err := db.BindTelegramTopic(context.Background(), TelegramTopicBinding{ChatID: topicChat, ThreadID: "17585", UserID: topicChat,
		SessionKey: "telegram:dm:208214988:thread:17585", SessionID: "topic-session"}); err != nil {
		t.Fatal(err)
	}
	b := topicBinding(t, db, topicChat, "17585", "")
	for k, want := range map[string]any{"chat_id": topicChat, "thread_id": "17585", "user_id": topicChat,
		"session_key": "telegram:dm:208214988:thread:17585", "session_id": "topic-session"} {
		if b[k] != want {
			t.Fatalf("%s = %v", k, b[k])
		}
	}
	if got := topicMeta(t, db); got != "3" {
		t.Fatal(got)
	}
}

func TestLegacyRowsMigrateOnlyToDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createV2TopicState(t, path, "1", "", false)
	db := usageOpen(t, path)
	if err := db.ApplyTelegramTopicMigration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := topicMeta(t, db); got != "3" {
		t.Fatal(got)
	}
	if !topicEnabled(t, db, topicChat, topicChat, "default") || topicEnabled(t, db, topicChat, topicChat, "coder") {
		t.Fatal("legacy mode row did not land in default only")
	}
	if b := topicBinding(t, db, topicChat, "99", "default"); b == nil || b["session_id"] != "legacy-sess" {
		t.Fatal(b)
	}
	if b := topicBinding(t, db, topicChat, "99", "coder"); b != nil {
		t.Fatal(b)
	}
	if got := repairScalar(t, db, "SELECT COUNT(*) FROM pragma_foreign_key_list('telegram_dm_topic_bindings') WHERE \"table\" = 'sessions' AND on_delete = 'CASCADE'"); got != int64(1) {
		t.Fatal("rebuilt bindings lack the cascading session key")
	}
}

func TestModeAndBindingsIsolatedAcrossProfiles(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	for _, s := range [][2]string{{"sess-a", "alpha"}, {"sess-b", "beta"}} {
		if err := db.CreateSession(NewSession{ID: s[0], Source: "telegram", UserID: topicChat, ProfileName: s[1]}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"alpha", "beta"} {
		if err := db.EnableTelegramTopicMode(ctx, TelegramTopicMode{ChatID: topicChat, UserID: topicChat, ProfileName: p}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DisableTelegramTopicMode(ctx, topicChat, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if topicEnabled(t, db, topicChat, topicChat, "alpha") || !topicEnabled(t, db, topicChat, topicChat, "beta") {
		t.Fatal("disable crossed profiles")
	}
	for _, b := range []TelegramTopicBinding{{ChatID: topicChat, ThreadID: "77", UserID: topicChat, SessionKey: "ka", SessionID: "sess-a", ProfileName: "alpha"},
		{ChatID: topicChat, ThreadID: "77", UserID: topicChat, SessionKey: "kb", SessionID: "sess-b", ProfileName: "beta"}} {
		if err := db.BindTelegramTopic(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if topicBinding(t, db, topicChat, "77", "alpha")["session_id"] != "sess-a" || topicBinding(t, db, topicChat, "77", "beta")["session_id"] != "sess-b" {
		t.Fatal("bindings crossed profiles")
	}
	if n, err := db.DeleteTelegramTopicBinding(ctx, topicChat, "77", "alpha"); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if topicBinding(t, db, topicChat, "77", "alpha") != nil || topicBinding(t, db, topicChat, "77", "beta") == nil {
		t.Fatal("delete crossed profiles")
	}
	if topicEnabled(t, db, topicChat, topicChat, "") || topicBinding(t, db, topicChat, "77", "") != nil {
		t.Fatal("the default namespace saw a named profile's rows")
	}
}

func TestDeleteTelegramTopicBindingRemovesMatchingRowAndReturnsCount(t *testing.T) {
	db := usageDB(t)
	seedTopicBinding(t, db, "5595856929", "15287", "5595856929", "sess-target", "")
	if topicBinding(t, db, "5595856929", "15287", "") == nil {
		t.Fatal("seed missing")
	}
	if n, err := db.DeleteTelegramTopicBinding(context.Background(), "5595856929", "15287", ""); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if topicBinding(t, db, "5595856929", "15287", "") != nil {
		t.Fatal("binding survived")
	}
}

func TestDeleteTelegramTopicBindingDoesNotTouchUnrelatedBindings(t *testing.T) {
	db := usageDB(t)
	seedTopicBinding(t, db, "5595856929", "15287", "5595856929", "sess-stale", "")
	seedTopicBinding(t, db, "5595856929", "15418", "5595856929", "sess-fresh", "")
	if n, err := db.DeleteTelegramTopicBinding(context.Background(), "5595856929", "15287", ""); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if topicBinding(t, db, "5595856929", "15287", "") != nil || topicBinding(t, db, "5595856929", "15418", "") == nil {
		t.Fatal("delete touched the wrong binding")
	}
}

func TestClearsEnabledWhenLastBindingPruned(t *testing.T) {
	db := usageDB(t)
	if err := db.EnableTelegramTopicMode(context.Background(), TelegramTopicMode{ChatID: "5595856929", UserID: "5595856929"}); err != nil {
		t.Fatal(err)
	}
	seedTopicBinding(t, db, "5595856929", "15287", "5595856929", "sess-target", "")
	if !topicEnabled(t, db, "5595856929", "5595856929", "") {
		t.Fatal("mode not enabled")
	}
	if n, err := db.DeleteTelegramTopicBinding(context.Background(), "5595856929", "15287", ""); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if topicEnabled(t, db, "5595856929", "5595856929", "") {
		t.Fatal("last binding gone but topic mode stayed on")
	}
	if n, err := db.DeleteTelegramTopicBinding(context.Background(), "5595856929", "15287", ""); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

func TestListTelegramTopicBindingsForChatNoTable(t *testing.T) {
	db := usageDB(t)
	if rows, err := db.ListTelegramTopicBindingsForChat(context.Background(), topicChat, ""); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if got := repairScalar(t, db, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE 'telegram_dm%'"); got != int64(0) {
		t.Fatal("a read created the topic tables")
	}
}

func TestGetTelegramTopicBindingBySessionReturnsBinding(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	if err := db.EnableTelegramTopicMode(ctx, TelegramTopicMode{ChatID: topicChat, UserID: topicChat}); err != nil {
		t.Fatal(err)
	}
	seedTopicBinding(t, db, topicChat, "17585", topicChat, "sess-27166", "")
	b, err := db.GetTelegramTopicBindingBySession(ctx, "sess-27166")
	if err != nil || b == nil {
		t.Fatal(b, err)
	}
	for k, want := range map[string]string{"chat_id": topicChat, "thread_id": "17585", "session_id": "sess-27166"} {
		if got, _ := b.Get(k); got != want {
			t.Fatalf("%s = %v", k, got)
		}
	}
	if linked, err := db.IsTelegramSessionLinkedToTopic(ctx, "sess-27166"); !linked || err != nil {
		t.Fatal(linked, err)
	}
}

func TestBindTelegramTopicRefusesASecondTopicAndListsOnlyUnlinkedSessions(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	seedTopicBinding(t, db, topicChat, "1", "u", "linked", "")
	for _, id := range []string{"free", "other-profile"} {
		profile := ""
		if id == "other-profile" {
			profile = "coder"
		}
		if err := db.CreateSession(NewSession{ID: id, Source: "telegram", UserID: "u", ProfileName: profile}); err != nil {
			t.Fatal(err)
		}
	}
	err := db.BindTelegramTopic(ctx, TelegramTopicBinding{ChatID: topicChat, ThreadID: "2", UserID: "u", SessionKey: "k", SessionID: "linked"})
	if err == nil || !strings.Contains(err.Error(), "session is already linked to another Telegram topic") {
		t.Fatal(err)
	}
	if err := db.BindTelegramTopic(ctx, TelegramTopicBinding{ChatID: topicChat, ThreadID: "1", UserID: "u", SessionKey: "k2", SessionID: "linked"}); err != nil {
		t.Fatal("rebinding the same pair must stay idempotent", err)
	}
	rows, err := db.ListUnlinkedTelegramSessionsForUser(ctx, "u", "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if id, _ := rows[0].Get("id"); id != "free" {
		t.Fatal(id)
	}
	if _, ok := rows[0].Get("preview"); !ok {
		t.Fatal("unlinked rows are not rich rows")
	}
}
