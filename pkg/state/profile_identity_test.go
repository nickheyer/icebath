package state

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	legacyTopicModeDDL = `CREATE TABLE telegram_dm_topic_mode (chat_id TEXT PRIMARY KEY, user_id TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1, activated_at REAL NOT NULL, updated_at REAL NOT NULL)`
	legacyTopicBindingsDDL = `CREATE TABLE telegram_dm_topic_bindings (chat_id TEXT NOT NULL, thread_id TEXT NOT NULL,
		user_id TEXT NOT NULL, session_key TEXT NOT NULL, session_id TEXT NOT NULL, managed_mode TEXT NOT NULL DEFAULT 'auto',
		linked_at REAL NOT NULL, updated_at REAL NOT NULL, PRIMARY KEY (chat_id, thread_id))`
	legacyLedgerDDL = `CREATE TABLE delivery_obligations (obligation_id TEXT PRIMARY KEY, session_key TEXT NOT NULL,
		platform TEXT NOT NULL, chat_id TEXT NOT NULL, thread_id TEXT, content TEXT NOT NULL, state TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0, created_at REAL NOT NULL, updated_at REAL NOT NULL, owner_pid INTEGER,
		owner_started_at INTEGER, last_error TEXT`
)

func profileExec(t *testing.T, db *SessionDB, query string, args ...any) {
	t.Helper()
	if err := db.writeSQL(context.Background(), query, args, false, nil); err != nil {
		t.Fatal(err)
	}
}

func profileFixture(t *testing.T, withProfileColumns bool) *SessionDB {
	t.Helper()
	db := usageDB(t)
	if withProfileColumns {
		profileExec(t, db, strings.Replace(legacyTopicModeDDL, "user_id TEXT NOT NULL,", "user_id TEXT NOT NULL, profile_name TEXT,", 1))
		profileExec(t, db, strings.Replace(legacyTopicBindingsDDL, "user_id TEXT NOT NULL,", "user_id TEXT NOT NULL, profile_name TEXT,", 1))
		profileExec(t, db, legacyLedgerDDL+", adapter_profile TEXT)")
	} else {
		profileExec(t, db, legacyTopicModeDDL)
		profileExec(t, db, legacyTopicBindingsDDL)
		profileExec(t, db, legacyLedgerDDL+")")
	}
	for _, s := range []struct{ id, key, profile string }{{"s_gone", "agent:gone:feishu:dm:chatA", "gone"}, {"s_keep", "agent:keepme:feishu:dm:chatB", "keepme"},
		{"s_wild", "agent:fooXbar:feishu:dm:chatC", "fooXbar"}} {
		if err := db.CreateSession(NewSession{ID: s.id, Source: "feishu", SessionKey: s.key, ProfileName: s.profile}); err != nil {
			t.Fatal(err)
		}
		profileExec(t, db, "UPDATE sessions SET origin_json = ? WHERE id = ?", `{"platform": "feishu", "profile": "`+s.profile+`", "chat": "café"}`, s.id)
		profileExec(t, db, "INSERT INTO gateway_routing (scope, session_key, entry_json, updated_at) VALUES ('/root/sessions', ?, ?, 1)",
			s.key, `{"session_key": "`+s.key+`", "origin": {"profile": "`+s.profile+`"}}`)
		profileExec(t, db, "INSERT INTO gateway_heartbeats (backend_id, pid, started_at, last_heartbeat, profile, host) VALUES (?, 1, 1, 1, ?, 'h')", "be-"+s.profile, s.profile)
		if withProfileColumns {
			profileExec(t, db, "INSERT INTO delivery_obligations (obligation_id, session_key, platform, chat_id, content, state, created_at, updated_at, adapter_profile) VALUES (?, ?, 'feishu', 'c', 'hi', 'pending', 1, 1, ?)",
				"ob-"+s.profile, s.key, s.profile)
			profileExec(t, db, "INSERT INTO telegram_dm_topic_mode (chat_id, user_id, profile_name, activated_at, updated_at) VALUES (?, 'u', ?, 1, 1)", "chat-"+s.profile, s.profile)
			profileExec(t, db, "INSERT INTO telegram_dm_topic_bindings (chat_id, thread_id, user_id, profile_name, session_key, session_id, linked_at, updated_at) VALUES (?, 't', 'u', ?, ?, ?, 1, 1)",
				"chat-"+s.profile, s.profile, s.key, s.id)
		} else {
			profileExec(t, db, "INSERT INTO delivery_obligations (obligation_id, session_key, platform, chat_id, content, state, created_at, updated_at) VALUES (?, ?, 'feishu', 'c', 'hi', 'pending', 1, 1)",
				"ob-"+s.profile, s.key)
			profileExec(t, db, "INSERT INTO telegram_dm_topic_bindings (chat_id, thread_id, user_id, session_key, session_id, linked_at, updated_at) VALUES (?, 't', 'u', ?, ?, 1, 1)",
				"chat-"+s.profile, s.key, s.id)
		}
	}
	profileExec(t, db, "INSERT INTO delivery_obligations (obligation_id, session_key, platform, chat_id, content, state, created_at, updated_at) VALUES ('ob-done', 'agent:gone:feishu:dm:chatZ', 'feishu', 'c', 'hi', 'delivered', 1, 1)")
	return db
}

func TestRekeyProfileStateRewritesEveryIdentityStore(t *testing.T) {
	db := profileFixture(t, true)
	counts, err := db.RekeyProfileState(" gone ", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{"sessions_profile_name": 1, "gateway_heartbeats_profile": 1, "sessions_session_key": 1, "sessions_origin_json": 1,
		"delivery_obligations_adapter_profile": 1, "delivery_obligations_session_key": 2, "telegram_dm_topic_mode_profile_name": 1,
		"telegram_dm_topic_bindings_profile_name": 1, "telegram_dm_topic_bindings_session_key": 1, "gateway_routing": 1} {
		if counts[key] != want {
			t.Fatalf("%s = %d want %d (%v)", key, counts[key], want, counts)
		}
	}
	if got := repairScalar(t, db, "SELECT session_key FROM sessions WHERE id = 's_gone'"); got != "agent:fresh:feishu:dm:chatA" {
		t.Fatal(got)
	}
	if got := repairScalar(t, db, "SELECT origin_json FROM sessions WHERE id = 's_gone'"); got != `{"platform": "feishu", "profile": "fresh", "chat": "café"}` {
		t.Fatal(got)
	}
	if got := repairScalar(t, db, "SELECT entry_json FROM gateway_routing WHERE session_key = 'agent:fresh:feishu:dm:chatA'"); got != `{"session_key": "agent:fresh:feishu:dm:chatA", "origin": {"profile": "fresh"}}` {
		t.Fatal(got)
	}
	if got := repairScalar(t, db, "SELECT profile FROM gateway_heartbeats WHERE backend_id = 'be-keepme'"); got != "keepme" {
		t.Fatal(got)
	}
	if counts, _ := db.RekeyProfileState("same", "same"); len(counts) != 0 {
		t.Fatal(counts)
	}
}

func TestRekeyProfileStateRefusesARoutingCollision(t *testing.T) {
	db := profileFixture(t, true)
	profileExec(t, db, "INSERT INTO gateway_routing (scope, session_key, entry_json, updated_at) VALUES ('/root/sessions', 'agent:keepme:feishu:dm:chatA', '{}', 1)")
	if _, err := db.RekeyProfileState("gone", "keepme"); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("err %v", err)
	}
	if got := repairScalar(t, db, "SELECT profile_name FROM sessions WHERE id = 's_gone'"); got != "gone" {
		t.Fatal("collision left a partial rekey")
	}
}

func TestRekeyProfileStateLegacyTablesMatchOnSessionKey(t *testing.T) {
	db := profileFixture(t, false)
	counts, err := db.RekeyProfileState("gone", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := counts["delivery_obligations_adapter_profile"]; ok || counts["delivery_obligations_session_key"] != 2 || counts["telegram_dm_topic_bindings_session_key"] != 1 {
		t.Fatal(counts)
	}
	if _, ok := counts["telegram_dm_topic_mode_profile_name"]; ok {
		t.Fatal(counts)
	}
}

func TestPurgeProfileStateKeepsBystandersAndSessions(t *testing.T) {
	db := profileFixture(t, true)
	counts, err := db.PurgeProfileState("gone")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{"gateway_routing": 1, "gateway_heartbeats": 1, "delivery_obligations": 1, "telegram_dm_topic_mode": 1, "telegram_dm_topic_bindings": 1} {
		if counts[key] != want {
			t.Fatalf("%s = %d want %d", key, counts[key], want)
		}
	}
	for query, want := range map[string]any{
		"SELECT state FROM delivery_obligations WHERE obligation_id = 'ob-gone'":            "abandoned",
		"SELECT state FROM delivery_obligations WHERE obligation_id = 'ob-done'":            "delivered",
		"SELECT state FROM delivery_obligations WHERE obligation_id = 'ob-keepme'":          "pending",
		"SELECT COUNT(*) FROM gateway_routing WHERE session_key LIKE 'agent:keepme:%'":      int64(1),
		"SELECT COUNT(*) FROM sessions WHERE id = 's_gone'":                                 int64(1),
		"SELECT COUNT(*) FROM gateway_heartbeats WHERE profile = 'gone'":                    int64(0),
		"SELECT COUNT(*) FROM telegram_dm_topic_bindings WHERE session_key LIKE 'agent:f%'": int64(1),
	} {
		if got := repairScalar(t, db, query); got != want {
			t.Fatalf("%s = %v want %v", query, got, want)
		}
	}
}

func TestPurgeProfileStateUnderscoreIsNotAWildcard(t *testing.T) {
	db := profileFixture(t, false)
	counts, err := db.PurgeProfileState("foo_bar")
	if err != nil {
		t.Fatal(err)
	}
	if counts["gateway_routing"] != 0 || counts["delivery_obligations"] != 0 || counts["telegram_dm_topic_bindings"] != 0 {
		t.Fatal(counts)
	}
	counts, err = db.PurgeProfileState("gone")
	if err != nil || counts["delivery_obligations"] != 1 || counts["telegram_dm_topic_bindings"] != 1 {
		t.Fatal(counts, err)
	}
	if _, ok := counts["telegram_dm_topic_mode"]; ok {
		t.Fatal(counts)
	}
}

func createLegacyV2TopicTables(t *testing.T, db *SessionDB) {
	t.Helper()
	profileExec(t, db, `CREATE TABLE telegram_dm_topic_mode (chat_id TEXT PRIMARY KEY, user_id TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1, activated_at REAL NOT NULL, updated_at REAL NOT NULL, has_topics_enabled INTEGER,
		allows_users_to_create_topics INTEGER, capability_checked_at REAL, intro_message_id TEXT, pinned_message_id TEXT)`)
	profileExec(t, db, `CREATE TABLE telegram_dm_topic_bindings (chat_id TEXT NOT NULL, thread_id TEXT NOT NULL, user_id TEXT NOT NULL,
		session_key TEXT NOT NULL, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		managed_mode TEXT NOT NULL DEFAULT 'auto', linked_at REAL NOT NULL, updated_at REAL NOT NULL, PRIMARY KEY (chat_id, thread_id))`)
}

func createLegacyLedgerWithoutAdapterProfile(t *testing.T, db *SessionDB) {
	t.Helper()
	profileExec(t, db, legacyLedgerDDL+")")
	for _, ob := range [][2]string{{"ob_gone", "agent:gone:feishu:dm:chatA"}, {"ob_keep", "agent:keepme:feishu:dm:chatB"}} {
		profileExec(t, db, "INSERT INTO delivery_obligations (obligation_id, session_key, platform, chat_id, content, state, created_at, updated_at)"+
			" VALUES (?, ?, 'feishu', 'c', 'hi', 'pending', 1, 1)", ob[0], ob[1])
	}
}

// Creates the delivery ledger table in its current shape and writes one obligation, as the gateway ledger does
func writeObligation(t *testing.T, db *SessionDB, id, key, chat, profile, state string) {
	t.Helper()
	profileExec(t, db, "CREATE TABLE IF NOT EXISTS "+strings.TrimPrefix(legacyLedgerDDL, "CREATE TABLE ")+", adapter_profile TEXT)")
	ts := now()
	profileExec(t, db, "INSERT INTO delivery_obligations (obligation_id, session_key, platform, chat_id, content, state, created_at, updated_at, adapter_profile)"+
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", id, key, "feishu", chat, "hi", state, ts, ts, profile)
}

func identitySession(t *testing.T, db *SessionDB, id, source, key, profile, chat string) {
	t.Helper()
	if err := db.CreateSession(NewSession{ID: id, Source: source, SessionKey: key, ProfileName: profile, ChatID: chat, ChatType: "dm"}); err != nil {
		t.Fatal(err)
	}
}

func legacyTopicBinding(t *testing.T, db *SessionDB, chat, thread, user, key, sid string) {
	t.Helper()
	profileExec(t, db, "INSERT INTO telegram_dm_topic_bindings (chat_id, thread_id, user_id, session_key, session_id, linked_at, updated_at)"+
		" VALUES (?, ?, ?, ?, ?, 1, 1)", chat, thread, user, key, sid)
}

func TestRekeysSessionKeyNamespaceAndProfileColumns(t *testing.T) {
	db := usageDB(t)
	identitySession(t, db, "sess_old", "feishu", "agent:oldname:feishu:dm:chatA", "oldname", "chatA")
	identitySession(t, db, "sess_other", "feishu", "agent:keepme:feishu:dm:chatB", "keepme", "chatB")
	counts, err := db.RekeyProfileState("oldname", "newname")
	if err != nil || counts["sessions_session_key"] != 1 || counts["sessions_profile_name"] != 1 {
		t.Fatal(counts, err)
	}
	for id, want := range map[string][2]string{"sess_old": {"agent:newname:feishu:dm:chatA", "newname"}, "sess_other": {"agent:keepme:feishu:dm:chatB", "keepme"}} {
		if repairScalar(t, db, "SELECT session_key FROM sessions WHERE id = ?", id) != want[0] || repairScalar(t, db, "SELECT profile_name FROM sessions WHERE id = ?", id) != want[1] {
			t.Fatal(id)
		}
	}
}

func TestRekeysRoutingIndexKeyAndEmbeddedProfile(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	entry := `{"session_key": "agent:oldname:feishu:dm:chatA", "session_id": "sess_old", "origin": {"platform": "feishu", "chat_id": "chatA", "profile": "oldname"}}`
	if err := db.SaveGatewayRoutingEntry(ctx, "agent:oldname:feishu:dm:chatA", entry, "/root/sessions"); err != nil {
		t.Fatal(err)
	}
	counts, err := db.RekeyProfileState("oldname", "newname")
	if err != nil || counts["gateway_routing"] != 1 {
		t.Fatal(counts, err)
	}
	rows, err := db.LoadGatewayRoutingEntries(ctx, "/root/sessions")
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := rows["agent:oldname:feishu:dm:chatA"]; stale {
		t.Fatal("old key survived")
	}
	var payload struct {
		SessionKey string `json:"session_key"`
		Origin     struct {
			Profile string `json:"profile"`
		} `json:"origin"`
	}
	if err := json.Unmarshal([]byte(rows["agent:newname:feishu:dm:chatA"]), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SessionKey != "agent:newname:feishu:dm:chatA" || payload.Origin.Profile != "newname" {
		t.Fatal(payload)
	}
}

func TestRekeysHeartbeatsAndDeliveryObligations(t *testing.T) {
	db := usageDB(t)
	if err := db.RegisterBackendHeartbeat(context.Background(), BackendHeartbeat{BackendID: "be1", PID: 123, StartedAt: now(), Profile: "oldname", Host: "h"}); err != nil {
		t.Fatal(err)
	}
	writeObligation(t, db, "ob1", "agent:oldname:feishu:dm:chatA", "chatA", "oldname", "pending")
	counts, err := db.RekeyProfileState("oldname", "newname")
	if err != nil || counts["gateway_heartbeats_profile"] != 1 || counts["delivery_obligations_adapter_profile"] != 1 || counts["delivery_obligations_session_key"] != 1 {
		t.Fatal(counts, err)
	}
	if repairScalar(t, db, "SELECT profile FROM gateway_heartbeats WHERE backend_id = 'be1'") != "newname" ||
		repairScalar(t, db, "SELECT session_key FROM delivery_obligations WHERE obligation_id = 'ob1'") != "agent:newname:feishu:dm:chatA" ||
		repairScalar(t, db, "SELECT adapter_profile FROM delivery_obligations WHERE obligation_id = 'ob1'") != "newname" {
		t.Fatal("heartbeat or obligation kept the old profile")
	}
}

func TestRekeyUnderscoreInProfileNameIsNotALikeWildcard(t *testing.T) {
	db := usageDB(t)
	identitySession(t, db, "literal", "telegram", "agent:foo_bar:telegram:dm:a", "foo_bar", "a")
	identitySession(t, db, "bystander", "telegram", "agent:fooXbar:telegram:dm:b", "fooXbar", "b")
	if _, err := db.RekeyProfileState("foo_bar", "renamed"); err != nil {
		t.Fatal(err)
	}
	if repairScalar(t, db, "SELECT session_key FROM sessions WHERE id = 'literal'") != "agent:renamed:telegram:dm:a" ||
		repairScalar(t, db, "SELECT session_key FROM sessions WHERE id = 'bystander'") != "agent:fooXbar:telegram:dm:b" {
		t.Fatal("underscore matched as a wildcard")
	}
}

func TestRekeysOriginJSONAndTelegramTopicState(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	identitySession(t, db, "sess_old", "telegram", "agent:oldname:telegram:dm:chatA", "oldname", "chatA")
	profileExec(t, db, "UPDATE sessions SET origin_json = ? WHERE id = ?", `{"platform": "telegram", "profile": "oldname"}`, "sess_old")
	if err := db.EnableTelegramTopicMode(ctx, TelegramTopicMode{ChatID: "chatA", UserID: "userA", ProfileName: "oldname"}); err != nil {
		t.Fatal(err)
	}
	if err := db.BindTelegramTopic(ctx, TelegramTopicBinding{ChatID: "chatA", ThreadID: "threadA", UserID: "userA",
		SessionKey: "agent:oldname:telegram:dm:chatA", SessionID: "sess_old", ProfileName: "oldname"}); err != nil {
		t.Fatal(err)
	}
	counts, err := db.RekeyProfileState("oldname", "newname")
	if err != nil || counts["sessions_origin_json"] != 1 {
		t.Fatal(counts, err)
	}
	var origin struct {
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal([]byte(repairScalar(t, db, "SELECT origin_json FROM sessions WHERE id = 'sess_old'").(string)), &origin); err != nil || origin.Profile != "newname" {
		t.Fatal(origin, err)
	}
	if repairScalar(t, db, "SELECT profile_name FROM telegram_dm_topic_bindings WHERE chat_id = 'chatA' AND thread_id = 'threadA'") != "newname" ||
		repairScalar(t, db, "SELECT session_key FROM telegram_dm_topic_bindings WHERE chat_id = 'chatA' AND thread_id = 'threadA'") != "agent:newname:telegram:dm:chatA" {
		t.Fatal("topic binding kept the old profile")
	}
}

func TestRekeysLegacyV2TopicBindingBySessionKey(t *testing.T) {
	db := usageDB(t)
	identitySession(t, db, "sess_old", "telegram", "agent:oldname:telegram:dm:chatA", "oldname", "chatA")
	identitySession(t, db, "sess_keep", "telegram", "agent:keepme:telegram:dm:chatB", "keepme", "chatB")
	createLegacyV2TopicTables(t, db)
	profileExec(t, db, "INSERT INTO telegram_dm_topic_mode (chat_id, user_id, enabled, activated_at, updated_at) VALUES ('chatA', 'userA', 1, 1, 1)")
	legacyTopicBinding(t, db, "chatA", "threadA", "userA", "agent:oldname:telegram:dm:chatA", "sess_old")
	legacyTopicBinding(t, db, "chatB", "threadB", "userB", "agent:keepme:telegram:dm:chatB", "sess_keep")
	counts, err := db.RekeyProfileState("oldname", "newname")
	if err != nil || counts["telegram_dm_topic_bindings_session_key"] != 1 {
		t.Fatal(counts, err)
	}
	if repairScalar(t, db, "SELECT session_key FROM telegram_dm_topic_bindings WHERE chat_id = 'chatA'") != "agent:newname:telegram:dm:chatA" ||
		repairScalar(t, db, "SELECT session_key FROM telegram_dm_topic_bindings WHERE chat_id = 'chatB'") != "agent:keepme:telegram:dm:chatB" {
		t.Fatal("legacy binding rekey wrong")
	}
}

func TestRekeysLegacyLedgerWithoutAdapterProfileBySessionKey(t *testing.T) {
	db := usageDB(t)
	createLegacyLedgerWithoutAdapterProfile(t, db)
	counts, err := db.RekeyProfileState("gone", "newname")
	if err != nil || counts["delivery_obligations_session_key"] != 1 {
		t.Fatal(counts, err)
	}
	if _, ok := counts["delivery_obligations_adapter_profile"]; ok {
		t.Fatal(counts)
	}
	if repairScalar(t, db, "SELECT session_key FROM delivery_obligations WHERE obligation_id = 'ob_gone'") != "agent:newname:feishu:dm:chatA" ||
		repairScalar(t, db, "SELECT session_key FROM delivery_obligations WHERE obligation_id = 'ob_keep'") != "agent:keepme:feishu:dm:chatB" ||
		repairScalar(t, db, "SELECT COUNT(*) FROM delivery_obligations") != int64(2) {
		t.Fatal("legacy ledger rekey wrong")
	}
}

func TestPurgesRoutingAndHeartbeatsButKeepsBystanders(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	for _, key := range []string{"agent:gone:feishu:dm:chatA", "agent:keepme:feishu:dm:chatB"} {
		if err := db.SaveGatewayRoutingEntry(ctx, key, `{"session_key": "`+key+`"}`, "/root/sessions"); err != nil {
			t.Fatal(err)
		}
	}
	for _, hb := range []BackendHeartbeat{{BackendID: "be-gone", PID: 1, StartedAt: now(), Profile: "gone", Host: "h"}, {BackendID: "be-keep", PID: 2, StartedAt: now(), Profile: "keepme", Host: "h"}} {
		if err := db.RegisterBackendHeartbeat(ctx, hb); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := db.PurgeProfileState("gone")
	if err != nil || counts["gateway_routing"] != 1 || counts["gateway_heartbeats"] != 1 {
		t.Fatal(counts, err)
	}
	routing, err := db.LoadGatewayRoutingEntries(ctx, "/root/sessions")
	if err != nil {
		t.Fatal(err)
	}
	_, gone := routing["agent:gone:feishu:dm:chatA"]
	_, kept := routing["agent:keepme:feishu:dm:chatB"]
	if gone || !kept {
		t.Fatal(routing)
	}
	if repairScalar(t, db, "SELECT profile FROM gateway_heartbeats WHERE backend_id = 'be-keep'") != "keepme" ||
		repairScalar(t, db, "SELECT COUNT(*) FROM gateway_heartbeats WHERE profile = 'gone'") != int64(0) {
		t.Fatal("heartbeat purge wrong")
	}
}

func TestAbandonsPendingDeliveriesInsteadOfDeletingThem(t *testing.T) {
	db := usageDB(t)
	writeObligation(t, db, "ob-gone", "agent:gone:feishu:dm:chatA", "chatA", "gone", "pending")
	writeObligation(t, db, "ob-gone-done", "agent:gone:feishu:dm:chatB", "chatB", "gone", "delivered")
	writeObligation(t, db, "ob-keep", "agent:keepme:feishu:dm:chatC", "chatC", "keepme", "pending")
	counts, err := db.PurgeProfileState("gone")
	if err != nil || counts["delivery_obligations"] != 1 {
		t.Fatal(counts, err)
	}
	for id, want := range map[string]string{"ob-gone": "abandoned", "ob-gone-done": "delivered", "ob-keep": "pending"} {
		if got := repairScalar(t, db, "SELECT state FROM delivery_obligations WHERE obligation_id = ?", id); got != want {
			t.Fatalf("%s = %v", id, got)
		}
	}
}

func TestPurgeDoesNotDeleteSessionRows(t *testing.T) {
	db := usageDB(t)
	identitySession(t, db, "sess_gone", "feishu", "agent:gone:feishu:dm:chatA", "gone", "chatA")
	if _, err := db.PurgeProfileState("gone"); err != nil {
		t.Fatal(err)
	}
	if repairScalar(t, db, "SELECT COUNT(*) FROM sessions WHERE id = 'sess_gone'") != int64(1) {
		t.Fatal("purge deleted the session row")
	}
}

func TestPurgesTopicBindingsMatchedBySessionKeyNamespace(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	identitySession(t, db, "sess_gone", "telegram", "agent:gone:telegram:dm:chatA", "gone", "chatA")
	identitySession(t, db, "sess_keep", "telegram", "agent:keepme:telegram:dm:chatB", "keepme", "chatB")
	for _, b := range []TelegramTopicBinding{{ChatID: "chatA", ThreadID: "threadA", UserID: "userA", SessionKey: "agent:gone:telegram:dm:chatA", SessionID: "sess_gone", ProfileName: "otherholder"},
		{ChatID: "chatB", ThreadID: "threadB", UserID: "userB", SessionKey: "agent:keepme:telegram:dm:chatB", SessionID: "sess_keep", ProfileName: "keepme"}} {
		if err := db.BindTelegramTopic(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := db.PurgeProfileState("gone")
	if err != nil || counts["telegram_dm_topic_bindings"] != 1 {
		t.Fatal(counts, err)
	}
	if repairScalar(t, db, "SELECT COUNT(*) FROM telegram_dm_topic_bindings WHERE chat_id = 'chatA'") != int64(0) ||
		repairScalar(t, db, "SELECT COUNT(*) FROM telegram_dm_topic_bindings WHERE chat_id = 'chatB'") != int64(1) {
		t.Fatal("binding purge wrong")
	}
}

func TestPurgeUnderscoreInProfileNameIsNotALikeWildcard(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	for _, key := range []string{"agent:foo_bar:feishu:dm:chatA", "agent:fooXbar:feishu:dm:chatB"} {
		if err := db.SaveGatewayRoutingEntry(ctx, key, `{"session_key": "`+key+`"}`, "/root/sessions"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.PurgeProfileState("foo_bar"); err != nil {
		t.Fatal(err)
	}
	routing, err := db.LoadGatewayRoutingEntries(ctx, "/root/sessions")
	if err != nil {
		t.Fatal(err)
	}
	_, literal := routing["agent:foo_bar:feishu:dm:chatA"]
	_, bystander := routing["agent:fooXbar:feishu:dm:chatB"]
	if literal || !bystander {
		t.Fatal(routing)
	}
}

func TestPurgesLegacyV2TopicBindingBySessionKey(t *testing.T) {
	db := usageDB(t)
	identitySession(t, db, "sess_gone", "telegram", "agent:gone:telegram:dm:chatA", "gone", "chatA")
	identitySession(t, db, "sess_keep", "telegram", "agent:keepme:telegram:dm:chatB", "keepme", "chatB")
	createLegacyV2TopicTables(t, db)
	legacyTopicBinding(t, db, "chatA", "threadA", "userA", "agent:gone:telegram:dm:chatA", "sess_gone")
	legacyTopicBinding(t, db, "chatB", "threadB", "userB", "agent:keepme:telegram:dm:chatB", "sess_keep")
	counts, err := db.PurgeProfileState("gone")
	if err != nil || counts["telegram_dm_topic_bindings"] != 1 {
		t.Fatal(counts, err)
	}
	if repairScalar(t, db, "SELECT COUNT(*) FROM telegram_dm_topic_bindings WHERE chat_id = 'chatA'") != int64(0) ||
		repairScalar(t, db, "SELECT session_key FROM telegram_dm_topic_bindings WHERE chat_id = 'chatB'") != "agent:keepme:telegram:dm:chatB" {
		t.Fatal("legacy binding purge wrong")
	}
}

func TestPurgesLegacyLedgerWithoutAdapterProfileBySessionKey(t *testing.T) {
	db := usageDB(t)
	createLegacyLedgerWithoutAdapterProfile(t, db)
	counts, err := db.PurgeProfileState("gone")
	if err != nil || counts["delivery_obligations"] != 1 {
		t.Fatal(counts, err)
	}
	for id, want := range map[string]string{"ob_gone": "abandoned", "ob_keep": "pending"} {
		if got := repairScalar(t, db, "SELECT state FROM delivery_obligations WHERE obligation_id = ?", id); got != want {
			t.Fatalf("%s = %v", id, got)
		}
	}
	if repairScalar(t, db, "SELECT COUNT(*) FROM delivery_obligations") != int64(2) {
		t.Fatal("purge deleted an obligation")
	}
}
