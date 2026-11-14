package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func leaseHolder(turn string) string { return fmt.Sprintf("pid=%d:turn=%s", os.Getpid(), turn) }

func leasePair(t *testing.T) (*SessionDB, *SessionDB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	first := usageOpen(t, path)
	second := usageOpen(t, path)
	readSession(t, first, NewSession{ID: "shared", Source: "test"})
	return first, second
}

func leaseTry(t *testing.T, db *SessionDB, sid, holder string, ttl float64) bool {
	t.Helper()
	ok, err := db.TryAcquireSessionTurnLease(sid, holder, ttl, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func leaseRelease(t *testing.T, db *SessionDB, sid, holder string) {
	t.Helper()
	if err := db.ReleaseSessionTurnLease(sid, holder); err != nil {
		t.Fatal(err)
	}
}

func TestTurnLeaseSerializesSeparateSessionDBInstances(t *testing.T) {
	first, second := leasePair(t)
	if !leaseTry(t, first, "shared", leaseHolder("first"), 5) {
		t.Fatal("first acquire failed")
	}
	var wg sync.WaitGroup
	released := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(200 * time.Millisecond)
		_ = first.ReleaseSessionTurnLease("shared", leaseHolder("first"))
		close(released)
	}()
	started := time.Now()
	o := NewTurnLeaseWait()
	o.TTLSeconds, o.WaitSeconds, o.PollIntervalSeconds = 5, 2, 0.02
	ok, err := second.AcquireSessionTurnLease("shared", leaseHolder("second"), o)
	wg.Wait()
	if err != nil || !ok {
		t.Fatalf("second acquire ok=%v err=%v", ok, err)
	}
	select {
	case <-released:
	default:
		t.Fatal("acquired before the release")
	}
	if time.Since(started) < 150*time.Millisecond {
		t.Fatal("acquire did not wait")
	}
	leaseRelease(t, second, "shared", leaseHolder("second"))
}

func TestTurnLeaseIsScopedToConversationRoot(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "root", Source: "test"})
	if err := db.EndSession("root", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "child", Source: "test", ParentSessionID: "root"})
	if !leaseTry(t, db, "root", leaseHolder("root"), 5) || leaseTry(t, db, "child", leaseHolder("child"), 5) {
		t.Fatal("child must share the root lease")
	}
	leaseRelease(t, db, "child", leaseHolder("root"))
	if !leaseTry(t, db, "child", leaseHolder("child"), 5) {
		t.Fatal("release through the child did not free the root lease")
	}
}

func TestTurnLeaseRefreshAndReleaseAreOwnerFenced(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	current, stale, next := leaseHolder("current"), leaseHolder("stale"), leaseHolder("next")
	if !leaseTry(t, db, "shared", current, 5) {
		t.Fatal("acquire failed")
	}
	if ok, err := db.RefreshSessionTurnLease("shared", stale, 5, nil); ok || err != nil {
		t.Fatalf("stale refresh ok=%v err=%v", ok, err)
	}
	leaseRelease(t, db, "shared", stale)
	if leaseTry(t, db, "shared", next, 5) {
		t.Fatal("stale release freed the lease")
	}
	if ok, err := db.RefreshSessionTurnLease("shared", current, 5, nil); !ok || err != nil {
		t.Fatalf("owner refresh ok=%v err=%v", ok, err)
	}
	if exp, err := db.SessionTurnLeaseExpiresAt("shared", current); err != nil || exp == nil || *exp <= now() {
		t.Fatalf("expires_at %v err %v", exp, err)
	}
	leaseRelease(t, db, "shared", current)
	if !leaseTry(t, db, "shared", next, 5) {
		t.Fatal("owner release did not free the lease")
	}
}

func TestExpiredTurnLeaseIsReclaimed(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	if !leaseTry(t, db, "shared", "legacy-holder", 0.05) {
		t.Fatal("acquire failed")
	}
	time.Sleep(150 * time.Millisecond)
	if !leaseTry(t, db, "shared", "pid=202:turn=reclaimer", 5) {
		t.Fatal("expired lease not reclaimed")
	}
}

func TestAcquireTurnLeaseNotifiesWaitCallback(t *testing.T) {
	first, second := leasePair(t)
	if !leaseTry(t, first, "shared", leaseHolder("first"), 5) {
		t.Fatal("first acquire failed")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(120 * time.Millisecond)
		_ = first.ReleaseSessionTurnLease("shared", leaseHolder("first"))
	}()
	var notices []float64
	o := NewTurnLeaseWait()
	o.TTLSeconds, o.WaitSeconds, o.PollIntervalSeconds, o.WaitNoticeIntervalSeconds = 5, 2, 0.02, 0.05
	o.OnWait = func(waited float64) error {
		notices = append(notices, waited)
		return nil
	}
	ok, err := second.AcquireSessionTurnLease("shared", leaseHolder("second"), o)
	wg.Wait()
	if err != nil || !ok || len(notices) == 0 || notices[0] >= 0.05 {
		t.Fatalf("ok=%v err=%v notices=%v", ok, err, notices)
	}
	leaseRelease(t, second, "shared", leaseHolder("second"))
}

func TestAcquireTurnLeaseHonorsShouldAbort(t *testing.T) {
	first, second := leasePair(t)
	if !leaseTry(t, first, "shared", leaseHolder("first"), 60) {
		t.Fatal("first acquire failed")
	}
	checks := 0
	o := NewTurnLeaseWait()
	o.WaitSeconds, o.PollIntervalSeconds = 30, 0.05
	o.ShouldAbort = func() (bool, error) {
		checks++
		return true, nil
	}
	started := time.Now()
	ok, err := second.AcquireSessionTurnLease("shared", leaseHolder("second"), o)
	if ok || err != nil || time.Since(started) >= time.Second || checks < 1 {
		t.Fatalf("ok=%v err=%v checks=%d", ok, err, checks)
	}
	leaseRelease(t, first, "shared", leaseHolder("first"))
}

func TestTurnLeaseRevivesExpiredRowStillOwnedByWriter(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	holder := leaseHolder("owner")
	if !leaseTry(t, db, "shared", holder, 0.05) {
		t.Fatal("acquire failed")
	}
	time.Sleep(120 * time.Millisecond)
	ttl := 0.2
	n, err := db.AppendMessagesBatch(context.Background(), "shared", []map[string]any{{"role": "assistant", "content": "after ttl"}}, AppendGuards{TurnLeaseHolder: &holder, TurnLeaseTTLSeconds: &ttl}, nil)
	if err != nil || n != 1 {
		t.Fatalf("append n=%d err=%v", n, err)
	}
	if leaseTry(t, db, "shared", leaseHolder("contender"), 5) {
		t.Fatal("revived lease was taken")
	}
}

func TestTurnLeaseAcquisitionSweepsLongExpiredRowsOfOtherConversations(t *testing.T) {
	db := usageDB(t)
	for _, sid := range []string{"abandoned", "recent", "within-grace", "fresh"} {
		readSession(t, db, NewSession{ID: sid, Source: "test"})
	}
	t0 := now()
	for _, r := range [][]any{{"abandoned", "pid=1:turn=crashed", t0 - 86400 - 120, t0 - 86400 - 60}, {"recent", leaseHolder("starved"), t0 - 60, t0 - 10},
		{"within-grace", "pid=1:turn=suspended", t0 - 86400, t0 - 86400 + 60}} {
		profileExec(t, db, "INSERT INTO session_turn_leases (conversation_id, holder, acquired_at, expires_at) VALUES (?, ?, ?, ?)", r...)
	}
	if !leaseTry(t, db, "fresh", leaseHolder("next"), 5) {
		t.Fatal("acquire failed")
	}
	rows, err := db.readAll(context.Background(), "SELECT conversation_id, holder FROM session_turn_leases ORDER BY conversation_id")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, pyStr(r.Vals["conversation_id"])+"="+pyStr(r.Vals["holder"]))
	}
	want := []string{"fresh=" + leaseHolder("next"), "recent=" + leaseHolder("starved"), "within-grace=pid=1:turn=suspended"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows %v", got)
	}
	holder, ttl := leaseHolder("starved"), 5.0
	if n, err := db.AppendMessagesBatch(context.Background(), "recent", []map[string]any{{"role": "assistant", "content": "after a starved refresher"}}, AppendGuards{TurnLeaseHolder: &holder, TurnLeaseTTLSeconds: &ttl}, nil); err != nil || n != 1 {
		t.Fatalf("append n=%d err=%v", n, err)
	}
}

func TestTurnLeaseFencesFlushWhenRowIsAbsent(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	holder := leaseHolder("owner")
	_, err := db.AppendMessagesBatch(context.Background(), "shared", []map[string]any{{"role": "assistant", "content": "after release"}}, AppendGuards{TurnLeaseHolder: &holder}, nil)
	var lost *SessionTurnLeaseLostError
	if !errors.As(err, &lost) || !strings.Contains(err.Error(), "turn lease lost") {
		t.Fatalf("err = %v", err)
	}
	if rows := displayPaged(t, db, "shared"); len(rows) != 0 {
		t.Fatalf("rows = %v", rows)
	}
}

func TestCompressionLockAcquireRefreshReleaseAndReclaim(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "s", Source: "test"})
	mine, other := leaseHolder("mine"), leaseHolder("other")
	if !db.TryAcquireCompressionLock("s", mine, 300) || db.TryAcquireCompressionLock("s", other, 300) || db.TryAcquireCompressionLock("", mine, 300) {
		t.Fatal("lock ownership wrong")
	}
	if h, err := db.GetCompressionLockHolder("s"); err != nil || h == nil || *h != mine {
		t.Fatalf("holder %v err %v", h, err)
	}
	profileExec(t, db, "UPDATE compression_locks SET expires_at = ? WHERE session_id = ?", now()-10, "s")
	if h, err := db.GetCompressionLockHolder("s"); err != nil || h != nil {
		t.Fatalf("expired holder %v err %v", h, err)
	}
	if !db.RefreshCompressionLock("s", mine, 300) || db.RefreshCompressionLock("s", other, 300) || db.RefreshCompressionLock("s", "", 300) {
		t.Fatal("refresh must revive only the owner's row")
	}
	db.ReleaseCompressionLock("s", other)
	if h, _ := db.GetCompressionLockHolder("s"); h == nil || *h != mine {
		t.Fatal("foreign release removed the lock")
	}
	db.ReleaseCompressionLock("s", mine)
	dead := "host:pid=4194303" + HolderNamespaceToken()
	if !db.TryAcquireCompressionLock("s", dead, 300) {
		t.Fatal("free lock not acquired")
	}
	if HolderPidCheckable(dead) && compressionLockHolderProcessIsDead(dead) && !db.TryAcquireCompressionLock("s", other, 300) {
		t.Fatal("dead pid holder not reclaimed")
	}
	db.ReleaseCompressionLock("s", dead)
	db.ReleaseCompressionLock("s", other)
	if !db.TryAcquireCompressionLock("s", other, 300) {
		t.Fatal("released lock not acquired")
	}
	profileExec(t, db, "UPDATE compression_locks SET expires_at = ? WHERE session_id = ?", now()-1, "s")
	if !db.TryAcquireCompressionLock("s", mine, 300) {
		t.Fatal("expired lock not reclaimed")
	}
}

func TestSetWallClockStampsLeaseExpiryFromTheInstalledClock(t *testing.T) {
	db, _ := leasePair(t)
	restore := SetWallClock(func() float64 { return 5000.0 })
	if !leaseTry(t, db, "shared", "h", 10) {
		restore()
		t.Fatal("lease not acquired")
	}
	restore()
	expiry, err := db.SessionTurnLeaseExpiresAt("shared", "h")
	if err != nil || expiry == nil || *expiry != 5010.0 {
		t.Fatalf("expiry %v err %v", expiry, err)
	}
	if now() < 1e9 {
		t.Fatalf("restore left the fake clock installed %v", now())
	}
}

func TestSetWriteRetrySleepRunsBetweenLockRetries(t *testing.T) {
	db, _ := leasePair(t)
	if !leaseTry(t, db, "shared", "h", 60) {
		t.Fatal("lease not acquired")
	}
	raw, err := sql.Open("sqlite", db.path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	db.SetWriteRetrySleep(func(deadline, patienceS float64) bool {
		calls++
		if calls == 1 {
			if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				t.Error(err)
			}
		}
		return db.SleepBeforeWriteRetryJitter(deadline, patienceS)
	})
	defer db.SetWriteRetrySleep(nil)
	patience := 5.0
	ok, err := db.RefreshSessionTurnLease("shared", "h", 60, &patience)
	if err != nil || !ok || calls == 0 {
		t.Fatalf("refresh %v err %v retry sleeps %d", ok, err, calls)
	}
}
