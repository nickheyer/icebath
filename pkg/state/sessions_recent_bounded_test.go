package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Sets the durable activity stamp of a session through the writer
func setActivity(t *testing.T, db *SessionDB, sid string, when float64) {
	t.Helper()
	if err := db.writeSQL(context.Background(), "UPDATE sessions SET last_activity_at = ? WHERE id = ?", []any{when, sid}, false, nil); err != nil {
		t.Fatal(err)
	}
}

// Bounded recent listing with the given knobs
func boundedRecent(t *testing.T, db *SessionDB, o RecentSessionsBoundedOptions) []*pyjson.Object {
	t.Helper()
	rows, err := db.ListRecentSessionsBounded(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Ids of listed rows in order
func boundedIDs(rows []*pyjson.Object) []string {
	out := []string{}
	for _, row := range rows {
		out = append(out, pyStr(rowValue(row, "id")))
	}
	return out
}

// Pointer to an int literal
func intOf(n int) *int { return &n }

// Session with a user message and its activity stamp
func boundedSession(t *testing.T, db *SessionDB, n NewSession, content string, when float64) {
	t.Helper()
	readSession(t, db, n)
	if content != "" {
		readAppend(t, db, n.ID, map[string]any{"role": "user", "content": content})
	}
	setActivity(t, db, n.ID, when)
}

func TestWritableStartupReconcilesLegacyActivityColumnBeforeIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-state.db")
	original := mustSessionDB(t, path, false)
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	conn := openRaw(t, path)
	execAll(t, conn, "DROP INDEX IF EXISTS idx_sessions_effective_activity", "ALTER TABLE sessions DROP COLUMN last_activity_at")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	healed := usageOpen(t, path)
	columns, err := healed.readAll(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		t.Fatal(err)
	}
	hasColumn := false
	for _, column := range columns {
		if rowValue(column, "name") == "last_activity_at" {
			hasColumn = true
		}
	}
	index, err := healed.readOne(ctx, "SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_sessions_effective_activity'")
	if err != nil || !hasColumn || index == nil {
		t.Fatalf("column %v index %v err %v", hasColumn, pyjson.Plain(index), err)
	}
	if rows := boundedRecent(t, healed, RecentSessionsBoundedOptions{Limit: intOf(1)}); len(rows) != 0 {
		t.Fatalf("rows %v", pyjson.Plain(rows))
	}
}

func TestBoundedRecentOrdersByDurableActivityAndShapesPreview(t *testing.T) {
	db := usageDB(t)
	at := now()
	boundedSession(t, db, NewSession{ID: "older", Source: "cli"}, "older preview", at-20)
	boundedSession(t, db, NewSession{ID: "newer", Source: "cli"}, "newer preview", at-10)
	rows := boundedRecent(t, db, RecentSessionsBoundedOptions{Limit: intOf(2)})
	if ids := boundedIDs(rows); !reflect.DeepEqual(ids, []string{"newer", "older"}) || rowValue(rows[0], "preview") != "newer preview" {
		t.Fatalf("rows %v", pyjson.Plain(rows))
	}
}

func TestBoundedRecentMapsRecentCompressionTipToLogicalRoot(t *testing.T) {
	db := usageDB(t)
	at := now()
	readSession(t, db, NewSession{ID: "root", Source: "cli"})
	readAppend(t, db, "root", map[string]any{"role": "user", "content": "root preview"})
	if err := db.EndSession("root", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "tip", Source: "cli", ParentSessionID: "root"})
	readAppend(t, db, "tip", map[string]any{"role": "user", "content": "tip preview"})
	setActivity(t, db, "root", at-1000)
	setActivity(t, db, "tip", at)
	rows := boundedRecent(t, db, RecentSessionsBoundedOptions{Limit: intOf(1)})
	if len(rows) != 1 || rowValue(rows[0], "id") != "tip" || rowValue(rows[0], "_lineage_root_id") != "root" || rowValue(rows[0], "preview") != "tip preview" {
		t.Fatalf("rows %v", pyjson.Plain(rows))
	}
}

func TestBoundedRecentKeepsResetChildUserVisible(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "before-reset", Source: "cli", SessionKey: "cli:one"})
	if err := db.EndSession("before-reset", "session_reset"); err != nil {
		t.Fatal(err)
	}
	boundedSession(t, db, NewSession{ID: "after-reset", Source: "cli", ParentSessionID: "before-reset", SessionKey: "cli:one"}, "fresh conversation", now())
	rows := boundedRecent(t, db, RecentSessionsBoundedOptions{Limit: intOf(5)})
	if ids := boundedIDs(rows); !slicesContains(ids, "after-reset") {
		t.Fatalf("rows %v", ids)
	}
}

// Membership test over a string slice
func slicesContains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestBoundedRecentKeepsBranchSeparateFromCompressionParent(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "branch-parent", Source: "cli"})
	if err := db.EndSession("branch-parent", "compression"); err != nil {
		t.Fatal(err)
	}
	boundedSession(t, db, NewSession{ID: "branch-child", Source: "cli", ParentSessionID: "branch-parent",
		ModelConfig: map[string]any{"_branched_from": "branch-parent"}}, "branch preview", now())
	rows := boundedRecent(t, db, RecentSessionsBoundedOptions{Limit: intOf(5)})
	for _, row := range rows {
		if rowValue(row, "id") == "branch-child" {
			if root := rowValue(row, "_lineage_root_id"); root != nil {
				t.Fatalf("branch mapped to root %v", root)
			}
			return
		}
	}
	t.Fatalf("branch child missing from %v", boundedIDs(rows))
}

func TestBoundedRecentExcludesDelegatedChildrenAndSources(t *testing.T) {
	db := usageDB(t)
	at := now()
	boundedSession(t, db, NewSession{ID: "visible", Source: "cli"}, "visible", at-1)
	boundedSession(t, db, NewSession{ID: "delegated", Source: "cli", ModelConfig: map[string]any{"_delegate_from": "parent"}}, "hidden delegate", at)
	boundedSession(t, db, NewSession{ID: "hidden-source", Source: "cron"}, "hidden source", at+1)
	rows := boundedRecent(t, db, RecentSessionsBoundedOptions{Limit: intOf(5), ExcludeSources: []string{"cron"}})
	if ids := boundedIDs(rows); !reflect.DeepEqual(ids, []string{"visible"}) {
		t.Fatalf("rows %v", ids)
	}
}

func TestBoundedRecentOmitsDeepLineageWhenTraversalCapIsReached(t *testing.T) {
	db := usageDB(t)
	at := now()
	parent := ""
	for i := range 40 {
		sid := fmt.Sprintf("deep-%d", i)
		readSession(t, db, NewSession{ID: sid, Source: "cli", ParentSessionID: parent})
		if parent != "" {
			if err := db.EndSession(parent, "compression"); err != nil {
				t.Fatal(err)
			}
		}
		setActivity(t, db, sid, at+float64(i))
		parent = sid
	}
	boundedSession(t, db, NewSession{ID: "visible-deep-peer", Source: "cli"}, "", at+100)
	rows := boundedRecent(t, db, RecentSessionsBoundedOptions{Limit: intOf(5), CandidateLimit: intOf(8), LineageLimit: intOf(8)})
	if ids := boundedIDs(rows); !reflect.DeepEqual(ids, []string{"visible-deep-peer"}) {
		t.Fatalf("rows %v", ids)
	}
}

func TestBoundedRecentOmitsBranchingLineageAtTotalRowCap(t *testing.T) {
	db := usageDB(t)
	at := now()
	readSession(t, db, NewSession{ID: "fanout-root", Source: "cli"})
	if err := db.EndSession("fanout-root", "compression"); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		boundedSession(t, db, NewSession{ID: fmt.Sprintf("fanout-%d", i), Source: "cli", ParentSessionID: "fanout-root"}, "", at+float64(i))
	}
	boundedSession(t, db, NewSession{ID: "visible-fanout-peer", Source: "cli"}, "", at+100)
	rows := boundedRecent(t, db, RecentSessionsBoundedOptions{Limit: intOf(5), CandidateLimit: intOf(8), LineageLimit: intOf(8)})
	if ids := boundedIDs(rows); !reflect.DeepEqual(ids, []string{"visible-fanout-peer"}) {
		t.Fatalf("rows %v", ids)
	}
}

func TestBoundedRecentCycleIsDeduplicatedAndOmitted(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	at := now()
	readSession(t, db, NewSession{ID: "cycle-a", Source: "cli"})
	readSession(t, db, NewSession{ID: "cycle-b", Source: "cli", ParentSessionID: "cycle-a"})
	for _, sid := range []string{"cycle-a", "cycle-b"} {
		if err := db.EndSession(sid, "compression"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.writeSQL(ctx, "UPDATE sessions SET parent_session_id = ? WHERE id = ?", []any{"cycle-b", "cycle-a"}, false, nil); err != nil {
		t.Fatal(err)
	}
	setActivity(t, db, "cycle-a", at)
	setActivity(t, db, "cycle-b", at+1)
	boundedSession(t, db, NewSession{ID: "visible-cycle-peer", Source: "cli"}, "", at+2)
	rows := boundedRecent(t, db, RecentSessionsBoundedOptions{Limit: intOf(5), CandidateLimit: intOf(8), LineageLimit: intOf(8)})
	if ids := boundedIDs(rows); !reflect.DeepEqual(ids, []string{"visible-cycle-peer"}) {
		t.Fatalf("rows %v", ids)
	}
}

func TestBoundedRecentDeadlineInterruptsSqlite(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	for i := range 40 {
		sid := fmt.Sprintf("session-%d", i)
		readSession(t, db, NewSession{ID: sid, Source: "cli"})
		readAppend(t, db, sid, map[string]any{"role": "user", "content": fmt.Sprintf("message %d", i)})
	}
	zero := 0.0
	_, err := db.ListRecentSessionsBounded(ctx, RecentSessionsBoundedOptions{Limit: intOf(20), CandidateLimit: intOf(300), TimeoutSeconds: &zero})
	var conv *pyConvError
	if !errors.As(err, &conv) || conv.Class != "TimeoutError" || !strings.Contains(conv.Msg, "recent-session browse exceeded") {
		t.Fatalf("deadline err %v", err)
	}
	row, err := db.GetSession(ctx, "session-0")
	if err != nil || row == nil || rowValue(row, "id") != "session-0" {
		t.Fatalf("connection poisoned after the deadline: %v %v", pyjson.Plain(row), err)
	}
}
