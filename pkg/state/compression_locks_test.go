package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func lockDB(t *testing.T) *SessionDB {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "sess1", Source: "test"})
	return db
}

func lockHolderIs(t *testing.T, db *SessionDB, sid, want string) {
	t.Helper()
	h, err := db.GetCompressionLockHolder(sid)
	if err != nil || h == nil || *h != want {
		t.Fatalf("holder of %s %v err %v, want %s", sid, h, err, want)
	}
}

func swapPIDProbe(t *testing.T, probe func(pid int) (bool, error)) {
	t.Helper()
	prev := holderPIDExists
	holderPIDExists = probe
	t.Cleanup(func() { holderPIDExists = prev })
}

func TestAcquireSucceedsWhenUnlocked(t *testing.T) {
	db := usageDB(t)
	if !db.TryAcquireCompressionLock("sess1", "holder1", 300) {
		t.Fatal("free lock refused")
	}
	lockHolderIs(t, db, "sess1", "holder1")
}

func TestAcquireBlocksSecondHolder(t *testing.T) {
	db := usageDB(t)
	if !db.TryAcquireCompressionLock("sess1", "holder1", 300) || db.TryAcquireCompressionLock("sess1", "holder2", 300) {
		t.Fatal("second holder must be refused")
	}
	lockHolderIs(t, db, "sess1", "holder1")
}

func TestProbeDoubtKeepsLeaseUntilTTL(t *testing.T) {
	db := usageDB(t)
	holder := "pid=424242" + HolderNamespaceToken() + ":tid=1:agent=abc:nonce=doubt"
	if !db.TryAcquireCompressionLock("sess1", holder, 300) {
		t.Fatal("free lock refused")
	}
	swapPIDProbe(t, func(int) (bool, error) { return false, errors.New("transient probe failure") })
	if db.TryAcquireCompressionLock("sess1", "pid=525252:tid=2:agent=def:nonce=other", 300) {
		t.Fatal("probe doubt reclaimed a live lease")
	}
	lockHolderIs(t, db, "sess1", holder)
}

func TestUnstructuredHolderWaitsForTTL(t *testing.T) {
	db := usageDB(t)
	if !db.TryAcquireCompressionLock("sess1", "legacy_holder", 300) {
		t.Fatal("free lock refused")
	}
	swapPIDProbe(t, func(int) (bool, error) {
		t.Error("unstructured holder must not probe a PID")
		return false, nil
	})
	if db.TryAcquireCompressionLock("sess1", "pid=525252:tid=2:agent=def:nonce=other", 300) {
		t.Fatal("unstructured holder lease stolen before its ttl")
	}
}

func seedCompressionLock(t *testing.T, db *SessionDB, sid, holder string, expired bool) {
	t.Helper()
	at := now()
	exp := at + 300
	if expired {
		exp = at - 10
	}
	profileExec(t, db, "INSERT INTO compression_locks (session_id, holder, acquired_at, expires_at) VALUES (?, ?, ?, ?)", sid, holder, at, exp)
}

func refreshPublish(holder string, refresh bool) CompressionChildPublish {
	p := NewCompressionChildPublish("parent-1", "child-1", "test", []map[string]any{{"role": "user", "content": "hello"}})
	p.CompressionLockHolder = &holder
	p.RequireLeaseRefresh = refresh
	return p
}

func lockExpiry(t *testing.T, db *SessionDB, sid string) float64 {
	t.Helper()
	row, err := db.readOne(context.Background(), "SELECT expires_at FROM compression_locks WHERE session_id = ?", sid)
	if err != nil || row == nil {
		t.Fatalf("lock row %v err %v", row, err)
	}
	v, _ := row.Get("expires_at")
	return v.(float64)
}

func wantLeaseLost(t *testing.T, err error) {
	t.Helper()
	var busy *CompressionSessionBusyError
	if !errors.As(err, &busy) || !strings.Contains(busy.Error(), "lease lost") {
		t.Fatalf("want lease lost, got %v", err)
	}
}

func TestRefresherStoppedFinalRefreshSucceeds(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "parent-1", Source: "test"})
	seedCompressionLock(t, db, "parent-1", "holder-1", true)
	if err := db.PublishCompressionChild(context.Background(), refreshPublish("holder-1", true)); err != nil {
		t.Fatal(err)
	}
	if exp := lockExpiry(t, db, "parent-1"); exp <= now() {
		t.Fatalf("lease not refreshed %v", exp)
	}
	parent, _ := db.GetSession(context.Background(), "parent-1")
	if rowValue(parent, "ended_at") == nil {
		t.Fatal("parent not ended by the publish")
	}
}

func TestRefresherStoppedFinalRefreshFailsWrongHolder(t *testing.T) {
	db := usageDB(t)
	seedCompressionLock(t, db, "parent-1", "other-holder", true)
	wantLeaseLost(t, db.PublishCompressionChild(context.Background(), refreshPublish("holder-1", true)))
}

func TestRefresherHealthyNoDuplicateBehavior(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "parent-1", Source: "test"})
	future := now() + 300
	profileExec(t, db, "INSERT INTO compression_locks (session_id, holder, acquired_at, expires_at) VALUES (?, ?, ?, ?)", "parent-1", "holder-1", now(), future)
	if err := db.PublishCompressionChild(context.Background(), refreshPublish("holder-1", true)); err != nil {
		t.Fatal(err)
	}
	if exp := lockExpiry(t, db, "parent-1"); exp < future {
		t.Fatalf("healthy lease shortened %v < %v", exp, future)
	}
}

func TestStaleHolderCannotRefreshAndPublish(t *testing.T) {
	db := usageDB(t)
	seedCompressionLock(t, db, "parent-1", "new-holder", false)
	wantLeaseLost(t, db.PublishCompressionChild(context.Background(), refreshPublish("old-holder", true)))
}

func TestNoRefreshWhenRequireLeaseRefreshFalse(t *testing.T) {
	db := usageDB(t)
	seedCompressionLock(t, db, "parent-1", "holder-1", true)
	wantLeaseLost(t, db.PublishCompressionChild(context.Background(), refreshPublish("holder-1", false)))
}

func TestRefreshAndLeaseCheckAreAtomic(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "parent-1", Source: "test"})
	seedCompressionLock(t, db, "parent-1", "holder-1", true)
	err := db.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := db.publishCompressionChildOnConn(ctx, conn, refreshPublish("holder-1", true)); err != nil {
			return err
		}
		lock, err := connRow(ctx, conn, "SELECT expires_at FROM compression_locks WHERE session_id = ?", "parent-1")
		if err != nil || lock == nil || lock["expires_at"].(float64) <= now() {
			t.Errorf("lock inside the publish transaction %v err %v", lock, err)
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func appendLandsQuickly(t *testing.T, db *SessionDB, fields map[string]any, guards AppendGuards) {
	t.Helper()
	started := time.Now()
	wmAppend(t, db, "sess1", fields, guards)
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("append waited %v on a compression lease", elapsed)
	}
	rows, err := db.GetMessages(context.Background(), "sess1", GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if rowValue(r, "content") == fields["content"] {
			return
		}
	}
	t.Fatalf("append %v missing from %d rows", fields["content"], len(rows))
}

func TestAppendIsNeverBlockedByAForeignCompressionLock(t *testing.T) {
	db := lockDB(t)
	if !db.TryAcquireCompressionLock("sess1", "compressor", 300) {
		t.Fatal("lock")
	}
	appendLandsQuickly(t, db, map[string]any{"role": "user", "content": "steered mid-compression"}, AppendGuards{})
}

func TestAppendIsNeverBlockedByAStaleDeadPidLock(t *testing.T) {
	db := lockDB(t)
	if !db.TryAcquireCompressionLock("sess1", "pid-9999999-long-gone", 3600) {
		t.Fatal("lock")
	}
	appendLandsQuickly(t, db, map[string]any{"role": "user", "content": "lands despite stale lock"}, AppendGuards{})
}

func TestTheLockOwnerAppendStillWorks(t *testing.T) {
	db := lockDB(t)
	if !db.TryAcquireCompressionLock("sess1", "compressor", 300) {
		t.Fatal("lock")
	}
	holder := "compressor"
	appendLandsQuickly(t, db, map[string]any{"role": "assistant", "content": "written by the compressor"}, AppendGuards{CompressionLockHolder: &holder})
}

func TestALostCompressionLeaseStillFailsFast(t *testing.T) {
	db := lockDB(t)
	holder := "not-the-holder"
	p := NewCompressionChildPublish("sess1", "child1", "test", []map[string]any{{"role": "user", "content": "compacted"}})
	p.CompressionLockHolder = &holder
	started := time.Now()
	var busy *CompressionSessionBusyError
	if err := db.PublishCompressionChild(context.Background(), p); !errors.As(err, &busy) {
		t.Fatalf("want busy error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("a lost lease spent the retry budget %v", elapsed)
	}
}
