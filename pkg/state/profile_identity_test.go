package state

import (
	"context"
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
