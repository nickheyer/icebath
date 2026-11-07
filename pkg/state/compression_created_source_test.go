package state

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

func createdCompress(t *testing.T, db *SessionDB, parentCreated any, child string) string {
	t.Helper()
	parent := "parent-" + child
	readSession(t, db, NewSession{ID: parent, Source: "local"})
	profileExec(t, db, "UPDATE sessions SET created_source=? WHERE id=?", parentCreated, parent)
	wmAppend(t, db, parent, map[string]any{"role": "user", "content": "hi"}, AppendGuards{})
	createdPublish(t, db, parent, child, "local")
	return child
}

func createdPublish(t *testing.T, db *SessionDB, parent, child, source string) {
	t.Helper()
	p := NewCompressionChildPublish(parent, child, source, []map[string]any{{"role": "user", "content": "hi"}})
	p.RequireCompressionLease = false
	if err := db.PublishCompressionChild(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func createdOf(t *testing.T, db *SessionDB, ids ...string) []any {
	t.Helper()
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		row, err := db.GetSession(context.Background(), id)
		if err != nil || row == nil {
			t.Fatalf("session %s %v %v", id, row, err)
		}
		out = append(out, rowValue(row, "created_source"))
	}
	return out
}

func TestStampWalksThroughASameSurfaceMidSegment(t *testing.T) {
	for _, tc := range []struct{ mid, root any }{{"cli", "cli"}, {"tui", nil}} {
		db := usageDB(t)
		mid := createdCompress(t, db, nil, "mid")
		createdPublish(t, db, mid, "tip", "local")
		profileExec(t, db, "UPDATE sessions SET created_source=? WHERE id=?", tc.mid, mid)
		profileExec(t, db, "UPDATE sessions SET created_source=NULL WHERE id='tip'")
		model, source := "m", "cli"
		if err := db.UpdateTokenCounts("tip", TokenCountDelta{InputTokens: 1, Model: &model, APICallCount: 1, Source: &source}); err != nil {
			t.Fatal(err)
		}
		if got := createdOf(t, db, "parent-mid", mid, "tip"); !reflect.DeepEqual(got, []any{tc.root, tc.mid, "cli"}) {
			t.Fatalf("mid %v stamps %v", tc.mid, got)
		}
	}
}

func TestCompressingAStampedTipStampsItsLegacyRoot(t *testing.T) {
	for _, rootCreated := range []any{nil, "unknown", "local"} {
		db := usageDB(t)
		tip := createdCompress(t, db, rootCreated, "legacy-tip")
		profileExec(t, db, "UPDATE sessions SET created_source='cli' WHERE id=?", tip)
		createdPublish(t, db, tip, "new-tip", "local")
		if got := createdOf(t, db, "parent-"+tip, tip, "new-tip"); !reflect.DeepEqual(got, []any{"cli", "cli", "cli"}) {
			t.Fatalf("root %v stamps %v", rootCreated, got)
		}
	}
}

type countingConn struct {
	SQLiteConn
	statements []string
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.statements = append(c.statements, query)
	return c.SQLiteConn.ExecContext(ctx, query, args...)
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.statements = append(c.statements, query)
	return c.SQLiteConn.QueryContext(ctx, query, args...)
}

func (c *countingConn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.statements = append(c.statements, query)
	return c.SQLiteConn.QueryRowContext(ctx, query, args...)
}

func TestUnsetParentBehindAResetEdgeTakesTheFastPath(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "legacy", Source: "local"})
	readSession(t, db, NewSession{ID: "reset-root", Source: "cli", ParentSessionID: "legacy", ModelConfig: map[string]any{"_reset_from": "legacy"}})
	profileExec(t, db, "UPDATE sessions SET created_source=NULL, ended_at=1, end_reason='session_reset' WHERE id='legacy'")
	wmAppend(t, db, "reset-root", map[string]any{"role": "user", "content": "hi"}, AppendGuards{})
	createdPublish(t, db, "reset-root", "tip", "cli")
	var counted *countingConn
	if err := db.executeWrite(ctx, func(conn SQLiteConn) error {
		counted = &countingConn{SQLiteConn: conn}
		return stampCompressionAncestors(ctx, counted, "tip")
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(counted.statements) != 2 {
		t.Fatalf("fast path ran %d statements %v", len(counted.statements), counted.statements)
	}
	if got := createdOf(t, db, "legacy"); got[0] != nil {
		t.Fatalf("legacy stamped %v", got)
	}
}

func TestOwnerStartKeepsKnownProvenance(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	createdCompress(t, db, "tui", "known")
	createdCompress(t, db, nil, "unset")
	branched := createdCompress(t, db, nil, "branched")
	readSession(t, db, NewSession{ID: "branch", Source: "cli", ParentSessionID: "parent-" + branched, ModelConfig: map[string]any{"_branched_from": "parent-" + branched}})
	profileExec(t, db, "UPDATE sessions SET created_source='cli' WHERE id IN ('known', 'branch')")
	profileExec(t, db, "UPDATE sessions SET created_source=NULL WHERE id IN ('unset', ?)", branched)
	if n, err := db.StampLegacyCompressionChains(ctx); n != 0 || err != nil {
		t.Fatalf("stamped %d err %v", n, err)
	}
	if got := createdOf(t, db, "parent-known", "parent-unset", "parent-branched", "branch"); !reflect.DeepEqual(got, []any{"tui", nil, nil, "cli"}) {
		t.Fatalf("provenance %v", got)
	}
}

func TestOwnerStartSkipsABranchChildOfAnUnsetCompressionParent(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	branched := createdCompress(t, db, nil, "branched")
	readSession(t, db, NewSession{ID: "branch", Source: "cli", ParentSessionID: "parent-" + branched, ModelConfig: map[string]any{"_branched_from": "parent-" + branched}})
	profileExec(t, db, "UPDATE sessions SET created_source='cli' WHERE id='branch'")
	profileExec(t, db, "UPDATE sessions SET created_source=NULL WHERE id=?", branched)
	if n, err := db.StampLegacyCompressionChains(ctx); n != 0 || err != nil {
		t.Fatalf("stamped %d err %v", n, err)
	}
	if rows, err := db.readAll(ctx, unstampedEdgeChildSQL); err != nil || len(rows) != 0 {
		t.Fatalf("unstamped edge children %v err %v", rows, err)
	}
}

func TestUnsetAncestorProbeReadsOnlyTheWalkedRows(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	rows, err := db.readAll(ctx, "EXPLAIN QUERY PLAN "+unsetAncestorSQL, "tip", "cli", chainCap)
	if err != nil || len(rows) == 0 {
		t.Fatalf("plan %v err %v", rows, err)
	}
	for _, r := range rows {
		step := pyStr(rowValue(r, "detail"))
		if strings.HasPrefix(step, "SCAN") && !strings.Contains(" "+step+" ", " up ") {
			t.Fatalf("probe scans a table %q", step)
		}
	}
}

func publishTUI(db *SessionDB, parent, child string) error {
	p := NewCompressionChildPublish(parent, child, "tui", []map[string]any{{"role": "user", "content": "[CONTEXT COMPACTION] summary"}})
	p.RequireCompressionLease = false
	return db.PublishCompressionChild(context.Background(), p)
}

func TestAutomaticEndReasonTaxonomy(t *testing.T) {
	for _, reason := range []any{"tui_shutdown", "ws_disconnect", "idle_timeout", "lru_evict", "ws_orphan_reap", "agent_close", "startup_orphan_reap", "superseded_by_resume"} {
		if !IsAutomaticEndReason(reason) {
			t.Fatalf("%v must be automatic", reason)
		}
	}
	for _, reason := range []any{"compression", "session_reset", "session_switch", "tui_close", nil, ""} {
		if IsAutomaticEndReason(reason) {
			t.Fatalf("%v must not be automatic", reason)
		}
	}
}

func TestRotationPublishesThroughAutomaticStamp(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"tui_shutdown", "ws_disconnect", "ws_orphan_reap", "idle_timeout"} {
		db := usageDB(t)
		parent, child := "P_"+reason, "C_"+reason
		readSession(t, db, NewSession{ID: parent, Source: "tui"})
		wmAppend(t, db, parent, map[string]any{"role": "user", "content": "hello"}, AppendGuards{})
		if err := db.EndSession(parent, reason); err != nil {
			t.Fatal(err)
		}
		if row, _ := db.GetSession(ctx, parent); rowValue(row, "ended_at") == nil {
			t.Fatal("dirty stamp missing")
		}
		if err := publishTUI(db, parent, child); err != nil {
			t.Fatalf("%s publish %v", reason, err)
		}
		row, _ := db.GetSession(ctx, parent)
		if rowValue(row, "end_reason") != "compression" || rowValue(row, "ended_at") == nil {
			t.Fatalf("%s parent %v", reason, row)
		}
		if c, _ := db.GetSession(ctx, child); c == nil || rowValue(c, "parent_session_id") != parent {
			t.Fatalf("%s child %v", reason, c)
		}
	}
}

func TestDeliberateBoundaryStillFailsClosed(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"compression", "session_reset", "tui_close"} {
		db := usageDB(t)
		parent, child := "P_"+reason, "C_"+reason
		readSession(t, db, NewSession{ID: parent, Source: "tui"})
		if err := db.EndSession(parent, reason); err != nil {
			t.Fatal(err)
		}
		err := publishTUI(db, parent, child)
		var pe *pyint.Error
		if err == nil || !strings.Contains(err.Error(), "already ended") || !errors.As(err, &pe) || pe.Kind != "RuntimeError" {
			t.Fatalf("%s publish %v", reason, err)
		}
		if c, _ := db.GetSession(ctx, child); c != nil {
			t.Fatalf("%s child published %v", reason, c)
		}
	}
}

func TestLiveParentUnaffected(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "P_live", Source: "tui"})
	if err := publishTUI(db, "P_live", "C_live"); err != nil {
		t.Fatal(err)
	}
	if row, _ := db.GetSession(ctx, "P_live"); rowValue(row, "end_reason") != "compression" {
		t.Fatalf("parent %v", row)
	}
	if c, _ := db.GetSession(ctx, "C_live"); c == nil {
		t.Fatal("child missing")
	}
}

func TestGetCompressionLineageReturnsOnlyCompressionChain(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "root", Source: "cli"})
	if err := db.EndSession("root", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "child", Source: "cli", ParentSessionID: "root"})
	if err := db.EndSession("child", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "tip", Source: "cli", ParentSessionID: "child"})
	readSession(t, db, NewSession{ID: "branch", Source: "cli", ParentSessionID: "root", ModelConfig: map[string]any{"_branched_from": "root"}})
	readSession(t, db, NewSession{ID: "delegate", Source: "delegate", ParentSessionID: "child", ModelConfig: map[string]any{"_delegate_from": "child"}})
	readSession(t, db, NewSession{ID: "tool", Source: "tool", ParentSessionID: "child"})
	for id, want := range map[string][]string{"tip": {"root", "child", "tip"}, "branch": {"branch"}, "delegate": {"delegate"}, "tool": {"tool"}} {
		if got, err := db.GetCompressionLineage(ctx, id); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("lineage of %s %v err %v", id, got, err)
		}
	}
}

func TestForkChildrenCreatedBeforeContinuationDoNotHijackLineage(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "root", Source: "cli"})
	wmAppend(t, db, "root", map[string]any{"role": "user", "content": "root msg"}, AppendGuards{})
	readSession(t, db, NewSession{ID: "delegate", Source: "delegate", ParentSessionID: "root", ModelConfig: map[string]any{"_delegate_from": "root"}})
	wmAppend(t, db, "delegate", map[string]any{"role": "user", "content": "delegate private msg"}, AppendGuards{})
	if err := db.EndSession("root", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "continuation", Source: "cli", ParentSessionID: "root"})
	wmAppend(t, db, "continuation", map[string]any{"role": "user", "content": "continuation msg"}, AppendGuards{})
	readSession(t, db, NewSession{ID: "root2", Source: "cli"})
	readSession(t, db, NewSession{ID: "toolchild", Source: "tool", ParentSessionID: "root2"})
	if err := db.EndSession("root2", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "cont2", Source: "cli", ParentSessionID: "root2"})
	for id, want := range map[string][]string{"root": {"root", "continuation"}, "continuation": {"root", "continuation"}, "root2": {"root2", "cont2"}} {
		if got, err := db.GetCompressionLineage(ctx, id); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("lineage of %s %v err %v", id, got, err)
		}
	}
}
