package gateway

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

// Local session committed as commit_local_session reserves its row, route and receipt
func seedGatewayLocalSession(t *testing.T, scope string) string {
	t.Helper()
	sid, err := state.LocalIdentity("profile", "human", "r")
	if err != nil {
		t.Fatal(err)
	}
	route := "local:" + sid
	origin := pyjson.ObjectOf("platform", "local", "chat_id", sid, "user_id", "human", "chat_type", "dm")
	entry := pyjson.ObjectOf("session_key", route, "session_id", sid, "created_at", "2026-11-06T00:00:00",
		"updated_at", "2026-11-06T00:00:00", "origin", origin, "platform", "local", "chat_type", "dm", "display_name", "Title")
	policy := pyjson.ObjectOf("source", "cli", "cwd", t.TempDir(), "model", "m", "request_json", `{"source":"cli"}`)
	receipt := pyjson.ObjectOf("profile_id", "profile", "principal_id", "human", "request_id", "r", "session_id", sid,
		"route", route, "entry", entry, "policy", policy)
	dump := func(v any) string {
		text, err := pyjson.Dumps(v)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	conn, err := sql.Open("sqlite", filepath.Join(os.Getenv("ICEBATH_HOME"), "state.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sessions(id,source,created_source,user_id,session_key,chat_id,chat_type,model,cwd,origin_json,started_at)
			VALUES(?,'cli','cli','human',?,?,'dm','m','/',?,1.0)`, []any{sid, route, sid, dump(origin)}},
		{"INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('',?,?,1.0)", []any{route, dump(entry)}},
		{"INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES(?,?,?,1.0)", []any{scope, route, dump(entry)}},
		{"INSERT INTO state_meta(key,value) VALUES(?,?)", []any{state.LocalPolicyPrefix + sid, dump(receipt)}},
	} {
		if _, err := conn.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	return sid
}

func TestResetLocalSession(t *testing.T) {
	ctx, db := newTestStateDB(t)
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(ctx, t.TempDir(), DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	sid := seedGatewayLocalSession(t, store.routingScope())
	route := "local:" + sid
	old := store.LookupBySessionKey(ctx, route)
	if old == nil || old.Origin == nil || old.Origin.Platform != PlatformLocal {
		t.Fatalf("seeded route %v", old)
	}
	if _, err := store.ResetSession(ctx, route, nil); err == nil {
		t.Fatal("a reset ran before any authority bound its epoch")
	}
	key, err := pypath.Resolve(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	store.bindLocalAuthorityEpoch(key, epoch+1, true)
	var refusal *state.RuntimeStoreError
	if _, err := store.ResetSession(ctx, route, nil); !errors.As(err, &refusal) || refusal.Reason != "stale_epoch" {
		t.Fatalf("stale epoch reset %v", err)
	}
	store.bindLocalAuthorityEpoch(key, epoch, false)
	fresh, err := store.ResetSession(ctx, route, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.SessionID == sid || !fresh.IsFreshReset || fresh.DisplayName == nil || *fresh.DisplayName != "Title" ||
		fresh.Origin != old.Origin || fresh.ChatType != "dm" || fresh.Platform != old.Platform {
		t.Fatalf("fresh entry %+v", fresh)
	}
	if got := store.LookupBySessionKey(ctx, route); got == nil || got.SessionID != fresh.SessionID {
		t.Fatalf("store entry %v", got)
	}
	child, err := db.GetSession(ctx, fresh.SessionID)
	if err != nil || child == nil {
		t.Fatalf("child row %v %v", child, err)
	}
	if parent, _ := child.Get("parent_session_id"); parent != sid {
		t.Fatalf("child parent %v", parent)
	}
	named := "Renamed"
	again, err := store.ResetSession(ctx, route, &named)
	if err != nil || *again.DisplayName != "Renamed" {
		t.Fatalf("%v %v", again, err)
	}
}
