package state

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const naDay = 86400.0

// Inserts a keyed telegram row aged in days with the never active defaults and the overrides applied
func naInsert(t *testing.T, db *SessionDB, id string, ageDays float64, overrides ...any) {
	t.Helper()
	cols := []string{"id", "source", "user_id", "session_key", "chat_id", "chat_type", "started_at",
		"message_count", "tool_call_count", "api_call_count", "input_tokens", "output_tokens", "archived", "pinned"}
	vals := []any{id, "telegram", "user-1", "agent:main:telegram:dm:" + id, "chat-1", "dm", now() - ageDays*naDay, 0, 0, 0, 0, 0, 0, 0}
	for i := 0; i < len(overrides); i += 2 {
		col := overrides[i].(string)
		idx := -1
		for j, c := range cols {
			if c == col {
				idx = j
			}
		}
		if idx < 0 {
			cols, vals = append(cols, col), append(vals, overrides[i+1])
		} else {
			vals[idx] = overrides[i+1]
		}
	}
	profileExec(t, db, "INSERT INTO sessions ("+strings.Join(cols, ", ")+") VALUES ("+placeholders(len(cols))+")", vals...)
}

func naIDs(t *testing.T, db *SessionDB, days float64) []string {
	t.Helper()
	rows, err := db.ListNeverActiveKeyedSessions(context.Background(), days)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, pyStr(rowValue(r, "id")))
	}
	return out
}

func naPrune(t *testing.T, db *SessionDB, days float64) [3]int {
	t.Helper()
	installRetirementHooks(t)
	a, b, c, err := db.PruneNeverActiveKeyedSessions(context.Background(), NeverActivePruneOptions{OlderThanDays: days})
	if err != nil {
		t.Fatal(err)
	}
	return [3]int{a, b, c}
}

func naColumn(t *testing.T, db *SessionDB, query string) []string {
	t.Helper()
	rows, err := db.readAll(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, pyStr(r.Vals[r.Keys[0]]))
	}
	sort.Strings(out)
	return out
}

func naRoute(t *testing.T, db *SessionDB, sid, scope string) {
	t.Helper()
	if err := db.SaveGatewayRoutingEntry(context.Background(), "agent:main:telegram:dm:"+sid, `{"session_id": "`+sid+`"}`, scope); err != nil {
		t.Fatal(err)
	}
}

func TestSelectsOldNeverActiveKeyedRow(t *testing.T) {
	db := usageDB(t)
	naInsert(t, db, "junk-old", 45)
	if got := naIDs(t, db, 30); !reflect.DeepEqual(got, []string{"junk-old"}) {
		t.Fatalf("found %v", got)
	}
}

func TestIgnoresRowInsideTheAgeFloor(t *testing.T) {
	db := usageDB(t)
	naInsert(t, db, "junk-young", 3)
	if got := naIDs(t, db, 30); len(got) != 0 {
		t.Fatalf("found %v", got)
	}
}

func TestIgnoresUnkeyedRow(t *testing.T) {
	db := usageDB(t)
	naInsert(t, db, "cli-row", 45, "session_key", nil, "source", "cli")
	if got := naIDs(t, db, 30); len(got) != 0 {
		t.Fatalf("found %v", got)
	}
}

func TestIgnoresEndedRow(t *testing.T) {
	db := usageDB(t)
	naInsert(t, db, "ended", 45, "ended_at", now()-40*naDay)
	if got := naIDs(t, db, 30); len(got) != 0 {
		t.Fatalf("found %v", got)
	}
}

func TestAnySignOfUseOrIntentProtectsTheRow(t *testing.T) {
	cases := []struct {
		field string
		value any
	}{
		{"message_count", 1}, {"tool_call_count", 1}, {"api_call_count", 1}, {"input_tokens", 12}, {"output_tokens", 12},
		{"title", "kept by the user"}, {"last_activity_at", 1785354069.0}, {"pinned", 1}, {"archived", 1},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			db := usageDB(t)
			naInsert(t, db, "used", 45, c.field, c.value)
			if got := naIDs(t, db, 30); len(got) != 0 {
				t.Fatalf("found %v", got)
			}
		})
	}
}

func TestRowWithMessagesIsProtectedEvenIfCounterSaysZero(t *testing.T) {
	db := usageDB(t)
	naInsert(t, db, "stale-counter", 45)
	profileExec(t, db, "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, ?, ?, ?)", "stale-counter", "user", "hello", now()-44*naDay)
	if got := naIDs(t, db, 30); len(got) != 0 {
		t.Fatalf("found %v", got)
	}
}

func TestDeletesCandidatesAndLeavesEverythingElse(t *testing.T) {
	db := usageDB(t)
	naInsert(t, db, "junk-a", 45)
	naInsert(t, db, "junk-b", 60)
	naInsert(t, db, "keeper-young", 1)
	naInsert(t, db, "keeper-used", 45, "message_count", 3)
	if got := naPrune(t, db, 30); got[0] != 2 {
		t.Fatalf("deleted %v", got)
	}
	if got := naColumn(t, db, "SELECT id FROM sessions"); !reflect.DeepEqual(got, []string{"keeper-used", "keeper-young"}) {
		t.Fatalf("surviving %v", got)
	}
}

func TestDropsRoutingEntriesPointingAtDeletedRows(t *testing.T) {
	db := usageDB(t)
	naInsert(t, db, "junk", 45)
	naInsert(t, db, "keeper", 1)
	naRoute(t, db, "junk", "/tmp/pytest-of-dev/test0")
	naRoute(t, db, "keeper", "/home/dev/project")
	if got := naPrune(t, db, 30); got != [3]int{1, 1, 0} {
		t.Fatalf("result %v", got)
	}
	if got := naColumn(t, db, "SELECT session_key FROM gateway_routing"); !reflect.DeepEqual(got, []string{"agent:main:telegram:dm:keeper"}) {
		t.Fatalf("remaining %v", got)
	}
}

func TestNoCandidatesIsANoOp(t *testing.T) {
	db := usageDB(t)
	naInsert(t, db, "keeper", 1)
	if got := naPrune(t, db, 30); got != [3]int{} {
		t.Fatalf("result %v", got)
	}
	if got := naColumn(t, db, "SELECT id FROM sessions"); len(got) != 1 {
		t.Fatalf("sessions %v", got)
	}
}

func TestGuardedRowSurvivesAndKeepsItsRoutingEntry(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	installRetirementHooks(t)
	for _, sid := range []string{"live-junk", "dead-junk", "gone-junk"} {
		naInsert(t, db, sid, 45)
	}
	naRoute(t, db, "live-junk", "/tmp/pytest-live")
	naRoute(t, db, "gone-junk", "/tmp/pytest-gone")
	holder := "pid=" + strconv.Itoa(os.Getpid()) + ":turn=1"
	if ok, err := db.TryAcquireSessionTurnLease("live-junk", holder, 300, nil); err != nil || !ok {
		t.Fatalf("lease %v %v", ok, err)
	}
	db.neverActiveListSeam = func(ctx context.Context, days float64) ([]*pyjson.Object, error) {
		rows, err := db.ListNeverActiveKeyedSessions(ctx, days)
		profileExec(t, db, "DELETE FROM sessions WHERE id = 'gone-junk'")
		return rows, err
	}
	if got := naPrune(t, db, 30); got != [3]int{1, 1, 1} {
		t.Fatalf("guarded result %v", got)
	}
	db.neverActiveListSeam = nil
	if row, err := db.GetSession(ctx, "live-junk"); err != nil || row == nil {
		t.Fatalf("live row %v %v", row, err)
	}
	if got := naColumn(t, db, "SELECT session_key FROM gateway_routing"); !reflect.DeepEqual(got, []string{"agent:main:telegram:dm:live-junk"}) {
		t.Fatalf("remaining %v", got)
	}
	if err := db.ReleaseSessionTurnLease("live-junk", holder); err != nil {
		t.Fatal(err)
	}
	if got := naPrune(t, db, 30); got != [3]int{1, 1, 0} {
		t.Fatalf("released result %v", got)
	}
	if row, err := db.GetSession(ctx, "live-junk"); err != nil || row != nil {
		t.Fatalf("live row after release %v %v", row, err)
	}
}

// Installs a messaging platform double naming every source but the local and API labels
func installMessagingPlatform(t *testing.T) {
	t.Helper()
	prev := MessagingPlatformSource
	MessagingPlatformSource = func(source string) bool {
		switch source {
		case "telegram", "discord", "slack", "whatsapp", "signal", "matrix", "email", "sms":
			return true
		}
		return false
	}
	t.Cleanup(func() { MessagingPlatformSource = prev })
}

func TestRoutingLineageCteAgreesWithPythonWalkForResetFork(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	installMessagingPlatform(t)
	t0 := ago(1)
	readSession(t, db, NewSession{ID: "root", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason='compression' WHERE id=?", t0+10, "root")
	readSession(t, db, NewSession{ID: "mid1", Source: "cli", ParentSessionID: "root"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason='compression' WHERE id=?", t0+20, "mid1")
	readSession(t, db, NewSession{ID: "mid2", Source: "cli", ParentSessionID: "mid1"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason='compression' WHERE id=?", t0+30, "mid2")
	readSession(t, db, NewSession{ID: "tip", Source: "cli", ParentSessionID: "mid2"})
	readSession(t, db, NewSession{ID: "reset", Source: "cli", ParentSessionID: "mid2", ModelConfig: map[string]any{"_reset_from": "mid2"}})

	cte := map[string][]string{}
	err := db.readCtx(ctx, false, func(conn SQLiteConn) error {
		for _, start := range []string{"reset", "tip"} {
			rows, err := connRows(ctx, conn, compressionLineageCTE+" SELECT id FROM compression_lineage", start)
			if err != nil {
				return err
			}
			ids := []string{}
			for _, r := range rows {
				ids = append(ids, pyStr(r["id"]))
			}
			sort.Strings(ids)
			cte[start] = ids
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for start, want := range map[string][]string{"reset": {"reset"}, "tip": {"mid1", "mid2", "root", "tip"}} {
		walk, err := db.GetCompressionLineage(ctx, start)
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(walk)
		if !reflect.DeepEqual(cte[start], want) || !reflect.DeepEqual(walk, want) {
			t.Fatalf("%s cte %v walk %v want %v", start, cte[start], walk, want)
		}
	}
	str := func(s string) *string { return &s }
	if err := db.RecordGatewaySessionPeer(ctx, "reset", GatewayPeer{Source: "cli", UserID: str("u"), SessionKey: str("cli:reset-chat"),
		ChatID: str("reset-chat"), ChatType: str("dm"), IncludeCompressionAncestors: true}); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"reset", "root", "mid1", "mid2", "tip"} {
		row, err := db.GetSession(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if got := rowValue(row, "session_key"); (got == "cli:reset-chat") != (sid == "reset") {
			t.Fatalf("%s session_key %v", sid, got)
		}
	}
}

func peerFind(t *testing.T, db *SessionDB, q GatewayPeerQuery) *pyjson.Object {
	t.Helper()
	row, err := db.FindLatestGatewaySessionForPeer(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func peerQuery(source, user, key, chat, kind string) GatewayPeerQuery {
	return GatewayPeerQuery{Source: source, UserID: &user, SessionKey: &key, ChatID: &chat, ChatType: &kind}
}

func TestGatewaySessionPeerRoundTripAndRecovery(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "gw-session", Source: "telegram", UserID: "user-1", SessionKey: "agent:main:telegram:dm:chat-1", ChatID: "chat-1", ChatType: "dm"})
	readAppend(t, db, "gw-session", map[string]any{"role": "user", "content": "hello"})
	row, err := db.GetSession(context.Background(), "gw-session")
	if err != nil || rowValue(row, "session_key") != "agent:main:telegram:dm:chat-1" || rowValue(row, "chat_id") != "chat-1" || rowValue(row, "chat_type") != "dm" {
		t.Fatalf("row %v %v", row, err)
	}
	if got := peerFind(t, db, peerQuery("telegram", "user-1", "agent:main:telegram:dm:chat-1", "chat-1", "dm")); rowValue(got, "id") != "gw-session" {
		t.Fatalf("recovered %v", got)
	}
}

func TestGatewaySessionRecoveryDoesNotCrossNewerResetBoundary(t *testing.T) {
	for name, key := range map[string]string{"exact-key": "agent:main:telegram:dm:chat-1", "peer-fallback": ""} {
		t.Run(name, func(t *testing.T) {
			db := usageDB(t)
			for _, sid := range []string{"gw-before-reset", "gw-reset"} {
				readSession(t, db, NewSession{ID: sid, Source: "telegram", UserID: "user-1", SessionKey: key, ChatID: "chat-1", ChatType: "dm"})
			}
			readAppend(t, db, "gw-before-reset", map[string]any{"role": "user", "content": "old context"})
			readAppend(t, db, "gw-reset", map[string]any{"role": "user", "content": "/new"})
			if err := db.EndSession("gw-reset", "session_reset"); err != nil {
				t.Fatal(err)
			}
			if got := peerFind(t, db, peerQuery("telegram", "user-1", "agent:main:telegram:dm:chat-1", "chat-1", "dm")); got != nil {
				t.Fatalf("recovered behind a reset %v", got)
			}
		})
	}
}

// Store at the root of a fresh ICEBATH_HOME, owned by the default profile
func peerOwnedStore(t *testing.T) *SessionDB {
	t.Helper()
	root := filepath.Join(t.TempDir(), "icebath")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	return usageOpen(t, filepath.Join(root, "state.db"))
}

func TestPeerFallbackNeverAdoptsASiblingProfilesRow(t *testing.T) {
	store := peerOwnedStore(t)
	q := peerQuery("telegram", "42", "agent:main:telegram:dm:42", "42", "dm")
	readSession(t, store, NewSession{ID: "sibling", Source: "telegram", SessionKey: "agent:bot2:telegram:dm:42", ProfileName: "bot2", UserID: "42", ChatID: "42", ChatType: "dm"})
	readAppend(t, store, "sibling", map[string]any{"role": "user", "content": "bot2's conversation"})
	if got := peerFind(t, store, q); got != nil {
		t.Fatalf("sibling adopted %v", got)
	}
	readSession(t, store, NewSession{ID: "own", Source: "telegram", SessionKey: "agent:main:telegram:dm:42:old", UserID: "42", ChatID: "42", ChatType: "dm"})
	readAppend(t, store, "own", map[string]any{"role": "user", "content": "default's conversation"})
	profileExec(t, store, "UPDATE sessions SET last_activity_at = 1 WHERE id = 'own'")
	if got := peerFind(t, store, q); rowValue(got, "id") != "own" {
		t.Fatalf("recovered %v", got)
	}
}

func TestPeerFallbackResetBoundaryIsProfileFenced(t *testing.T) {
	store := peerOwnedStore(t)
	q := peerQuery("telegram", "42", "agent:main:telegram:dm:42", "42", "dm")
	reset := func(sid, key, profile string) {
		readSession(t, store, NewSession{ID: sid, Source: "telegram", SessionKey: key, ProfileName: profile, UserID: "42", ChatID: "42", ChatType: "dm"})
		readAppend(t, store, sid, map[string]any{"role": "user", "content": "/new"})
		if err := store.EndSession(sid, "session_reset"); err != nil {
			t.Fatal(err)
		}
	}
	readSession(t, store, NewSession{ID: "own", Source: "telegram", SessionKey: "agent:main:telegram:dm:42:old", ProfileName: "default", UserID: "42", ChatID: "42", ChatType: "dm"})
	readAppend(t, store, "own", map[string]any{"role": "user", "content": "default's conversation"})
	profileExec(t, store, "UPDATE sessions SET last_activity_at = 1 WHERE id = 'own'")
	reset("sibling-reset", "agent:bot2:telegram:dm:42", "bot2")
	if got := peerFind(t, store, q); rowValue(got, "id") != "own" {
		t.Fatalf("sibling reset fenced own row %v", got)
	}
	reset("own-reset", "agent:main:telegram:dm:42:r2", "default")
	if got := peerFind(t, store, q); got != nil {
		t.Fatalf("own reset did not fence %v", got)
	}
}

func TestListGatewaySessionsLastActiveUsesActivityHeartbeat(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "gw-1", Source: "telegram", SessionKey: "agent:main:telegram:dm:c1", ChatID: "c1", ChatType: "dm"})
	readAppend(t, db, "gw-1", map[string]any{"role": "user", "content": "ping"})
	profileExec(t, db, "UPDATE messages SET timestamp=? WHERE session_id=?", 1_700_000_000.0, "gw-1")
	heartbeat := 1_700_000_900.0
	if err := db.TouchSessionActivity("gw-1", &heartbeat, "compressing context", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListGatewaySessions(ctx, "", true)
	if err != nil || len(rows) != 1 || rowValue(rows[0], "last_active") != heartbeat {
		t.Fatalf("rows %v %v", rows, err)
	}
	if got := rowValue(orSession(t, db, "gw-1"), "last_activity_description"); got != "compressing context" {
		t.Fatalf("description %v", got)
	}
}
