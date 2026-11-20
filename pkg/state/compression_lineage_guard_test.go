package state

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func guardParent(t *testing.T, db *SessionDB, sid string) {
	t.Helper()
	readSession(t, db, NewSession{ID: sid, Source: "webui"})
	readAppend(t, db, sid, map[string]any{"role": "user", "content": "before split"})
	leaseEnd(t, db, sid)
}

func guardSession(t *testing.T, db *SessionDB, sid string) map[string]any {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range row.Keys {
		out[k] = rowValue(row, k)
	}
	return out
}

func guardReopen(t *testing.T, db *SessionDB, sid string) bool {
	t.Helper()
	ok, err := db.ReopenOrphanedCompressionSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func guardChildID(t *testing.T, db *SessionDB, parent string) any {
	t.Helper()
	child, err := db.FindLiveCompressionChild(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if child == nil {
		return nil
	}
	return rowValue(child, "id")
}

func TestFindLiveCompressionChildReturnsUniqueDirectChild(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "parent")
	readSession(t, db, NewSession{ID: "child", Source: "webui", ParentSessionID: "parent"})
	child, err := db.FindLiveCompressionChild(context.Background(), "parent")
	if err != nil || child == nil || rowValue(child, "id") != "child" || rowValue(child, "parent_session_id") != "parent" || rowValue(child, "ended_at") != nil {
		t.Fatalf("child %v, %v", child, err)
	}
}

func TestFindLiveCompressionChildFailsClosedWhenAmbiguous(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "parent")
	readSession(t, db, NewSession{ID: "child-a", Source: "webui", ParentSessionID: "parent"})
	readSession(t, db, NewSession{ID: "child-b", Source: "webui", ParentSessionID: "parent"})
	if id := guardChildID(t, db, "parent"); id != nil {
		t.Fatalf("ambiguous children resolved to %v", id)
	}
}

func TestReopenOrphanedCompressionSessionReopensParentWithoutChild(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "orphan")
	if !guardReopen(t, db, "orphan") {
		t.Fatal("orphan must reopen")
	}
	row := guardSession(t, db, "orphan")
	if row["ended_at"] != nil || row["end_reason"] != nil {
		t.Fatalf("reopened row %v", row)
	}
	readAppend(t, db, "orphan", map[string]any{"role": "user", "content": "recovered turn"})
	if got := leaseContents(t, db, "orphan"); !reflect.DeepEqual(got, []string{"before split", "recovered turn"}) {
		t.Fatalf("contents %v", got)
	}
}

func TestReopenOrphanedCompressionSessionFailsClosedWithChild(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "parent-with-child")
	readSession(t, db, NewSession{ID: "child", Source: "webui", ParentSessionID: "parent-with-child"})
	if guardReopen(t, db, "parent-with-child") {
		t.Fatal("parent with a live child must stay ended")
	}
	row := guardSession(t, db, "parent-with-child")
	if row["end_reason"] != "compression" || row["ended_at"] == nil {
		t.Fatalf("parent %v", row)
	}
}

func TestReopenOrphanedCompressionSessionIgnoresNonContinuationChildren(t *testing.T) {
	db := leaseDB(t)
	p := "parent-with-non-continuation-children"
	guardParent(t, db, p)
	readSession(t, db, NewSession{ID: "branch", Source: "webui", ParentSessionID: p, ModelConfig: map[string]any{"_branched_from": p}})
	readSession(t, db, NewSession{ID: "delegate", Source: "tool", ParentSessionID: p, ModelConfig: map[string]any{"_delegate_from": p}})
	if !guardReopen(t, db, p) {
		t.Fatal("branch and delegate children are not continuations")
	}
}

func TestReopenFailsClosedWhenContinuationInheritsForeignMarkers(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "delegate-session")
	readSession(t, db, NewSession{ID: "delegate-continuation", Source: "subagent", ParentSessionID: "delegate-session", ModelConfig: map[string]any{"_delegate_from": "some-original-parent"}})
	if guardReopen(t, db, "delegate-session") {
		t.Fatal("foreign marker continuation must block reopen")
	}
	if row := guardSession(t, db, "delegate-session"); row["end_reason"] != "compression" {
		t.Fatalf("parent %v", row)
	}
}

func TestFindLiveChildReturnsContinuationWithForeignMarkers(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "delegate-session-2")
	readSession(t, db, NewSession{ID: "inherited-continuation", Source: "subagent", ParentSessionID: "delegate-session-2", ModelConfig: map[string]any{"_delegate_from": "some-original-parent"}})
	if id := guardChildID(t, db, "delegate-session-2"); id != "inherited-continuation" {
		t.Fatalf("child %v", id)
	}
}

func TestCompressionLineageIncludesContinuationWithForeignMarkers(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "delegate-session-3")
	readSession(t, db, NewSession{ID: "inherited-tip", Source: "subagent", ParentSessionID: "delegate-session-3", ModelConfig: map[string]any{"_delegate_from": "some-original-parent"}})
	want := []string{"delegate-session-3", "inherited-tip"}
	for _, sid := range []string{"inherited-tip", "delegate-session-3"} {
		got, err := db.GetCompressionLineage(context.Background(), sid)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("lineage of %s = %v, %v", sid, got, err)
		}
	}
}

func TestReopenOrphanedCompressionSessionFailsClosedWithActiveLease(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "leased-parent")
	if !db.TryAcquireCompressionLock("leased-parent", "compressor", 300) {
		t.Fatal("lock")
	}
	if guardReopen(t, db, "leased-parent") || guardSession(t, db, "leased-parent")["end_reason"] != "compression" {
		t.Fatal("active lease must block reopen")
	}
}

func guardExpiredLock(t *testing.T, db *SessionDB, sid, holder string) {
	t.Helper()
	n := now()
	profileExec(t, db, "INSERT INTO compression_locks (session_id, holder, acquired_at, expires_at) VALUES (?, ?, ?, ?)", sid, holder, n-60, n-30)
}

func TestReopenOrphanedCompressionSessionReclaimsExpiredLease(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "expired-lease-parent")
	guardExpiredLock(t, db, "expired-lease-parent", "old-compressor")
	if !guardReopen(t, db, "expired-lease-parent") {
		t.Fatal("expired lease must not block reopen")
	}
	if db.RefreshCompressionLock("expired-lease-parent", "old-compressor", 300) {
		t.Fatal("reclaimed lease refreshed")
	}
	if holder, err := db.GetCompressionLockHolder("expired-lease-parent"); err != nil || holder != nil {
		t.Fatalf("holder %v, %v", holder, err)
	}
}

func TestReopenOrphanedCompressionSessionLosesToExpiredLeaseRefresh(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "refreshed-lease-parent")
	guardExpiredLock(t, db, "refreshed-lease-parent", "live-compressor")
	if !db.RefreshCompressionLock("refreshed-lease-parent", "live-compressor", 300) {
		t.Fatal("expired lease refresh by its holder")
	}
	if guardReopen(t, db, "refreshed-lease-parent") || guardSession(t, db, "refreshed-lease-parent")["end_reason"] != "compression" {
		t.Fatal("refreshed lease must block reopen")
	}
}

func TestFindLiveCompressionChildIgnoresNonContinuationChildren(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "parent")
	readSession(t, db, NewSession{ID: "canonical", Source: "webui", ParentSessionID: "parent"})
	readSession(t, db, NewSession{ID: "branch", Source: "webui", ParentSessionID: "parent", ModelConfig: map[string]any{"_branched_from": "parent"}})
	readSession(t, db, NewSession{ID: "delegate", Source: "webui", ParentSessionID: "parent", ModelConfig: map[string]any{"_delegate_from": "parent"}})
	readSession(t, db, NewSession{ID: "tool-child", Source: "tool", ParentSessionID: "parent"})
	if id := guardChildID(t, db, "parent"); id != "canonical" {
		t.Fatalf("child %v", id)
	}
}

func TestResetForkIsNotACompressionContinuationForRecovery(t *testing.T) {
	db := leaseDB(t)
	guardParent(t, db, "parent")
	readSession(t, db, NewSession{ID: "canonical", Source: "webui", ParentSessionID: "parent"})
	readSession(t, db, NewSession{ID: "reset", Source: "webui", ParentSessionID: "parent", ModelConfig: map[string]any{"_reset_from": "parent"}})
	if id := guardChildID(t, db, "parent"); id != "canonical" {
		t.Fatalf("child %v", id)
	}
	if err := db.EndSession("canonical", "startup_orphan_reap"); err != nil {
		t.Fatal(err)
	}
	if id := guardChildID(t, db, "parent"); id != nil {
		t.Fatalf("reset fork recovered as continuation %v", id)
	}
	guardParent(t, db, "orphan")
	readSession(t, db, NewSession{ID: "orphan-reset", Source: "webui", ParentSessionID: "orphan", ModelConfig: map[string]any{"_reset_from": "orphan"}})
	if !guardReopen(t, db, "orphan") {
		t.Fatal("reset fork must not block reopen")
	}
}

func guardPublish(parent, child, holder, content string) CompressionChildPublish {
	p := NewCompressionChildPublish(parent, child, "webui", []map[string]any{{"role": "user", "content": content}})
	p.CompressionLockHolder = &holder
	return p
}

func TestPublishCompressionChildIsAtomicOnHandoffFailure(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "atomic-parent", Source: "webui"})
	readAppend(t, db, "atomic-parent", map[string]any{"role": "user", "content": "original"})
	if !db.TryAcquireCompressionLock("atomic-parent", "winner", 60) {
		t.Fatal("lock")
	}
	profileExec(t, db, "CREATE TRIGGER handoff_boom BEFORE INSERT ON messages WHEN NEW.session_id = 'atomic-child' "+
		"BEGIN SELECT RAISE(ABORT, 'handoff insert failed'); END")
	err := db.PublishCompressionChild(context.Background(), guardPublish("atomic-parent", "atomic-child", "winner", "summary"))
	if err == nil || !strings.Contains(err.Error(), "handoff insert failed") {
		t.Fatalf("publish %v", err)
	}
	if row := guardSession(t, db, "atomic-parent"); row == nil || row["ended_at"] != nil {
		t.Fatalf("parent %v", row)
	}
	if row := guardSession(t, db, "atomic-child"); row != nil {
		t.Fatalf("child %v", row)
	}
}

func TestPublishCompressionChildExposesCompleteChild(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "atomic-parent", Source: "webui"})
	readAppend(t, db, "atomic-parent", map[string]any{"role": "user", "content": "original"})
	if !db.TryAcquireCompressionLock("atomic-parent", "winner", 60) {
		t.Fatal("lock")
	}
	p := guardPublish("atomic-parent", "atomic-child", "winner", "summary")
	prompt := "compressed system"
	p.SystemPrompt = &prompt
	if err := db.PublishCompressionChild(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if row := guardSession(t, db, "atomic-parent"); row["end_reason"] != "compression" {
		t.Fatalf("parent %v", row)
	}
	child, err := db.FindLiveCompressionChild(context.Background(), "atomic-parent")
	if err != nil || child == nil || rowValue(child, "id") != "atomic-child" || rowValue(child, "system_prompt") != "compressed system" {
		t.Fatalf("child %v, %v", child, err)
	}
	if got := leaseContents(t, db, "atomic-child"); !reflect.DeepEqual(got, []string{"summary"}) {
		t.Fatalf("contents %v", got)
	}
}

func TestPublishCompressionChildRejectsLostOrExpiredLease(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "lease-parent", Source: "webui"})
	readAppend(t, db, "lease-parent", map[string]any{"role": "user", "content": "new durable turn"})
	if !db.TryAcquireCompressionLock("lease-parent", "new-winner", 60) {
		t.Fatal("lock")
	}
	err := db.PublishCompressionChild(context.Background(), guardPublish("lease-parent", "stale-child", "old-loser", "stale summary"))
	var busy *CompressionSessionBusyError
	if !errors.As(err, &busy) || !strings.Contains(err.Error(), "lease lost") {
		t.Fatalf("publish %v", err)
	}
	if row := guardSession(t, db, "lease-parent"); row == nil || row["ended_at"] != nil {
		t.Fatalf("parent %v", row)
	}
	if row := guardSession(t, db, "stale-child"); row != nil {
		t.Fatalf("child %v", row)
	}
	if got := leaseContents(t, db, "lease-parent"); !reflect.DeepEqual(got, []string{"new durable turn"}) {
		t.Fatalf("contents %v", got)
	}
}

func TestCompressionLeaseBlocksNonOwnerButAllowsOwnerFlush(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "leased", Source: "webui"})
	if !db.TryAcquireCompressionLock("leased", "winner", 60) {
		t.Fatal("lock")
	}
	readAppend(t, db, "leased", map[string]any{"role": "user", "content": "late concurrent turn"})
	winner := "winner"
	if _, err := db.AppendMessage(context.Background(), "leased", map[string]any{"role": "assistant", "content": "winner flush"}, AppendGuards{CompressionLockHolder: &winner}); err != nil {
		t.Fatal(err)
	}
	if got := leaseContents(t, db, "leased"); !reflect.DeepEqual(got, []string{"late concurrent turn", "winner flush"}) {
		t.Fatalf("contents %v", got)
	}
}
