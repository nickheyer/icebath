package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func leaseDB(t *testing.T) *SessionDB {
	t.Helper()
	return usageOpen(t, filepath.Join(t.TempDir(), "state.db"))
}

func leaseEnd(t *testing.T, db *SessionDB, sid string) {
	t.Helper()
	if err := db.EndSession(sid, EndReasonCompression); err != nil {
		t.Fatal(err)
	}
}

func leaseRefresh(t *testing.T, db *SessionDB, sid, holder string) bool {
	t.Helper()
	ok, err := db.RefreshSessionTurnLease(sid, holder, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func leaseAppend(db *SessionDB, sid, holder, role, content string) (int, error) {
	return db.AppendMessagesBatch(context.Background(), sid, []map[string]any{{"role": role, "content": content}}, AppendGuards{TurnLeaseHolder: &holder}, nil)
}

func leaseWantLost(t *testing.T, err error) {
	t.Helper()
	var lost *SessionTurnLeaseLostError
	if !errors.As(err, &lost) || !strings.Contains(err.Error(), "turn lease lost") {
		t.Fatalf("want SessionTurnLeaseLostError, got %v", err)
	}
}

func leaseContents(t *testing.T, db *SessionDB, sid string) []string {
	t.Helper()
	rows, err := db.GetMessages(context.Background(), sid, GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, pyStr(rowValue(r, "content")))
	}
	return out
}

// Builds the delegate and branch continuations whose model_config inherited fork markers
func leaseForkContinuations(t *testing.T, db *SessionDB, withOriginal bool) {
	t.Helper()
	if withOriginal {
		readSession(t, db, NewSession{ID: "original-parent", Source: "test"})
	}
	parent := ""
	if withOriginal {
		parent = "original-parent"
	}
	readSession(t, db, NewSession{ID: "delegate", Source: "delegate", ParentSessionID: parent, ModelConfig: map[string]any{"_delegate_from": "original-parent"}})
	leaseEnd(t, db, "delegate")
	readSession(t, db, NewSession{ID: "delegate-continuation", Source: "delegate", ParentSessionID: "delegate", ModelConfig: map[string]any{"_delegate_from": "original-parent"}})
	if !withOriginal {
		return
	}
	readSession(t, db, NewSession{ID: "branch", Source: "test", ParentSessionID: "original-parent", ModelConfig: map[string]any{"_branched_from": "original-parent"}})
	leaseEnd(t, db, "branch")
	readSession(t, db, NewSession{ID: "branch-continuation", Source: "test", ParentSessionID: "branch", ModelConfig: map[string]any{"_branched_from": "original-parent"}})
}

func TestTurnLeaseDoesNotSerializeDelegateChildWithParent(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "parent", Source: "test"})
	readSession(t, db, NewSession{ID: "delegate", Source: "delegate", ParentSessionID: "parent", ModelConfig: map[string]any{"_delegate_from": "parent"}})
	if !leaseTry(t, db, "parent", leaseHolder("parent"), 5) || !leaseTry(t, db, "delegate", leaseHolder("delegate"), 5) {
		t.Fatal("delegate child must hold its own lease beside the parent")
	}
}

func TestTurnLeaseWalksCompressionChildThatInheritedForkMarkers(t *testing.T) {
	db := leaseDB(t)
	leaseForkContinuations(t, db, true)
	ctx := context.Background()
	for sid, want := range map[string]string{"delegate-continuation": "delegate", "branch-continuation": "branch"} {
		if got, err := db.sessionTurnLeaseKey(ctx, sid); err != nil || got != want {
			t.Fatalf("key of %s = %q, %v want %q", sid, got, err, want)
		}
	}
	for _, name := range []string{"delegate", "branch"} {
		holder := leaseHolder(name)
		if !leaseTry(t, db, name, holder, 5) {
			t.Fatalf("acquire %s", name)
		}
		if leaseTry(t, db, name+"-continuation", leaseHolder(name+"-child"), 5) {
			t.Fatalf("continuation of %s took a second lease", name)
		}
		if !leaseRefresh(t, db, name+"-continuation", holder) {
			t.Fatalf("refresh through %s continuation", name)
		}
	}
	original := leaseHolder("original")
	if !leaseTry(t, db, "original-parent", original, 5) {
		t.Fatal("original parent keys separately")
	}
	leaseRelease(t, db, "delegate-continuation", leaseHolder("delegate"))
	leaseRelease(t, db, "branch-continuation", leaseHolder("branch"))
	leaseRelease(t, db, "original-parent", original)
}

func TestTurnLeaseWriteTxnDoesNotTrustFailOpenKeyHelper(t *testing.T) {
	db := leaseDB(t)
	leaseForkContinuations(t, db, false)
	db.leaseSeams.key = func(sid string) (string, error) { return sid, nil }
	holder := leaseHolder("delegate")
	if !leaseTry(t, db, "delegate", holder, 5) {
		t.Fatal("acquire delegate")
	}
	if leaseTry(t, db, "delegate-continuation", leaseHolder("child"), 5) {
		t.Fatal("continuation must key to the delegate lease")
	}
	if !leaseRefresh(t, db, "delegate-continuation", holder) {
		t.Fatal("refresh through continuation")
	}
	leaseRelease(t, db, "delegate-continuation", holder)
	if !leaseTry(t, db, "delegate", leaseHolder("next"), 5) {
		t.Fatal("release through continuation must free the delegate lease")
	}
}

func TestTurnLeaseRetriesLockedInTxnKeyWalk(t *testing.T) {
	db := leaseDB(t)
	leaseForkContinuations(t, db, false)
	attempts := 0
	db.leaseSeams.keyOnConn = func(ctx context.Context, conn SQLiteConn, sid string) (string, error) {
		attempts++
		if attempts == 1 {
			return "", &SQLiteError{Class: SQLiteOperationalError, Msg: "database is locked"}
		}
		return db.walkTurnLeaseKeyOnConn(ctx, conn, sid)
	}
	holder := leaseHolder("delegate")
	if !leaseTry(t, db, "delegate-continuation", holder, 5) {
		t.Fatal("locked walk must retry")
	}
	if attempts < 2 {
		t.Fatalf("attempts = %d", attempts)
	}
	db.leaseSeams.keyOnConn = nil
	if leaseTry(t, db, "delegate", leaseHolder("other"), 5) {
		t.Fatal("retried acquire must land under the delegate key")
	}
	if !leaseRefresh(t, db, "delegate", holder) {
		t.Fatal("refresh delegate")
	}
	leaseRelease(t, db, "delegate-continuation", holder)
}

func TestAcquireTurnLeaseRetriesSqliteLock(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	holder := leaseHolder("waiter")
	attempts := 0
	db.leaseSeams.tryAcquire = func(sid, h string, ttl float64, patience *float64) (bool, error) {
		attempts++
		if attempts == 1 {
			return false, &SQLiteError{Class: SQLiteOperationalError, Msg: "database is locked (another Hermes process held the state.db write lock for over 20s)"}
		}
		return db.TryAcquireSessionTurnLease(sid, h, ttl, patience)
	}
	var notices []float64
	var contended []bool
	o := NewTurnLeaseWait()
	o.WaitSeconds, o.PollIntervalSeconds, o.AcquirePatienceS = 10, 5, 0.05
	o.OnWait = func(w float64) error { notices = append(notices, w); return nil }
	o.OnContended = func() error { contended = append(contended, true); return nil }
	started := time.Now()
	ok, err := db.AcquireSessionTurnLease("shared", holder, o)
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	if time.Since(started) >= 2*time.Second {
		t.Fatal("lock retry sat out a poll interval")
	}
	if len(notices) != 0 || !reflect.DeepEqual(contended, []bool{true}) || attempts < 2 {
		t.Fatalf("notices %v contended %v attempts %d", notices, contended, attempts)
	}
	leaseRelease(t, db, "shared", holder)
}

func TestAcquireTurnLeaseLockedForTheWholeWaitTimesOut(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	db.leaseSeams.tryAcquire = func(string, string, float64, *float64) (bool, error) {
		return false, &SQLiteError{Class: SQLiteOperationalError, Msg: "database is locked"}
	}
	var notices []float64
	o := NewTurnLeaseWait()
	o.WaitSeconds, o.PollIntervalSeconds = 0.3, 5
	o.OnWait = func(w float64) error { notices = append(notices, w); return nil }
	ok, err := db.AcquireSessionTurnLease("shared", leaseHolder("waiter"), o)
	if err != nil || ok || len(notices) != 0 {
		t.Fatalf("acquire = %v, %v notices %v", ok, err, notices)
	}
}

func TestAcquireTurnLeaseReraisesNonLockSqliteError(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	db.leaseSeams.tryAcquire = func(string, string, float64, *float64) (bool, error) {
		return false, &SQLiteError{Class: SQLiteOperationalError, Msg: "database or disk is full"}
	}
	o := NewTurnLeaseWait()
	o.WaitSeconds, o.PollIntervalSeconds = 1, 0.02
	_, err := db.AcquireSessionTurnLease("shared", leaseHolder("waiter"), o)
	if !isOperationalError(err) || !strings.Contains(err.Error(), "disk is full") {
		t.Fatalf("want disk full OperationalError, got %v", err)
	}
}

func TestNonExpiredTurnLeaseFromDeadPidIsReclaimed(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	dead := "pid=424242" + HolderNamespaceToken() + ":turn=dead:platform=test"
	if !leaseTry(t, db, "shared", dead, 300) {
		t.Fatal("seed dead holder")
	}
	var probed []int
	prev := holderPIDExists
	holderPIDExists = func(pid int) (bool, error) { probed = append(probed, pid); return false, nil }
	t.Cleanup(func() { holderPIDExists = prev })
	if !leaseTry(t, db, "shared", "pid=525252:turn=fresh:platform=test", 300) {
		t.Fatal("dead holder must be reclaimed")
	}
	if !reflect.DeepEqual(probed, []int{424242}) {
		t.Fatalf("probed = %v", probed)
	}
}

func TestTurnLeaseFencesStaleTranscriptFlushAfterReclaim(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	stale, next := leaseHolder("stale"), leaseHolder("next")
	if !leaseTry(t, db, "shared", stale, 5) {
		t.Fatal("acquire stale")
	}
	if n, err := leaseAppend(db, "shared", stale, "user", "stale-owned"); err != nil || n != 1 {
		t.Fatalf("append = %d, %v", n, err)
	}
	leaseRelease(t, db, "shared", stale)
	if !leaseTry(t, db, "shared", next, 5) {
		t.Fatal("acquire next")
	}
	_, err := leaseAppend(db, "shared", stale, "assistant", "late stale reply")
	leaseWantLost(t, err)
	_, err = db.AppendMessage(context.Background(), "shared", map[string]any{"role": "assistant", "content": "late stale single-row"}, AppendGuards{TurnLeaseHolder: &stale})
	leaseWantLost(t, err)
	if n, err := leaseAppend(db, "shared", next, "assistant", "next reply"); err != nil || n != 1 {
		t.Fatalf("append = %d, %v", n, err)
	}
	if got := leaseContents(t, db, "shared"); !reflect.DeepEqual(got, []string{"stale-owned", "next reply"}) {
		t.Fatalf("contents %v", got)
	}
	leaseRelease(t, db, "shared", next)
}

func TestTurnLeaseFenceWalksCompressionChildToRoot(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "root", Source: "test"})
	leaseEnd(t, db, "root")
	readSession(t, db, NewSession{ID: "child", Source: "test", ParentSessionID: "root"})
	root := leaseHolder("root")
	if !leaseTry(t, db, "root", root, 5) {
		t.Fatal("acquire root")
	}
	if n, err := leaseAppend(db, "child", root, "user", "owner on tip"); err != nil || n != 1 {
		t.Fatalf("append = %d, %v", n, err)
	}
	_, err := leaseAppend(db, "child", leaseHolder("stale"), "assistant", "impostor")
	leaseWantLost(t, err)
	leaseRelease(t, db, "child", root)
}

func TestLostTurnLeaseFlushFailsFastWithoutPatienceRetry(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	stale, next := leaseHolder("stale"), leaseHolder("next")
	if !leaseTry(t, db, "shared", stale, 5) {
		t.Fatal("acquire stale")
	}
	leaseRelease(t, db, "shared", stale)
	if !leaseTry(t, db, "shared", next, 5) {
		t.Fatal("acquire next")
	}
	var sleeps []float64
	db.leaseSeams.sleepRetry = func(deadline, patience float64) bool {
		sleeps = append(sleeps, patience)
		return db.sleepBeforeWriteRetryJitter(deadline, patience)
	}
	started := time.Now()
	_, err := leaseAppend(db, "shared", stale, "assistant", "late stale reply")
	leaseWantLost(t, err)
	if time.Since(started) >= 500*time.Millisecond || len(sleeps) != 0 {
		t.Fatalf("lost lease retried, sleeps %v", sleeps)
	}
	if got := leaseContents(t, db, "shared"); len(got) != 0 {
		t.Fatalf("contents %v", got)
	}
	leaseRelease(t, db, "shared", next)
}

func TestTurnLeaseFenceWalksContinuationThatInheritedForkMarkers(t *testing.T) {
	db := leaseDB(t)
	leaseForkContinuations(t, db, true)
	for _, name := range []string{"delegate", "branch"} {
		holder := leaseHolder(name)
		if !leaseTry(t, db, name, holder, 5) {
			t.Fatalf("acquire %s", name)
		}
		owner := "owner on inherited tip"
		if name == "branch" {
			owner = "branch owner on inherited tip"
		}
		if n, err := leaseAppend(db, name+"-continuation", holder, "user", owner); err != nil || n != 1 {
			t.Fatalf("append = %d, %v", n, err)
		}
		_, err := leaseAppend(db, name+"-continuation", leaseHolder(name+"-impostor"), "assistant", "impostor")
		leaseWantLost(t, err)
		if got := leaseContents(t, db, name+"-continuation"); !reflect.DeepEqual(got, []string{owner}) {
			t.Fatalf("contents %v", got)
		}
	}
	leaseRelease(t, db, "delegate-continuation", leaseHolder("delegate"))
	leaseRelease(t, db, "branch-continuation", leaseHolder("branch"))
}

func TestCompressionLockHolderReadsRaceFree(t *testing.T) {
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "race-sess", Source: "test"})
	ctx := context.Background()
	var mu sync.Mutex
	var failures []string
	stop := make(chan struct{})
	var once sync.Once
	fail := func(who string, err error) {
		mu.Lock()
		failures = append(failures, who+": "+err.Error())
		mu.Unlock()
		once.Do(func() { close(stop) })
	}
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		for n := 0; !stopped(); n++ {
			_, err := db.AppendMessagesBatch(ctx, "race-sess", []map[string]any{
				{"role": "user", "content": fmt.Sprintf("u%d", n)},
				{"role": "assistant", "content": fmt.Sprintf("a%d", n)},
				{"role": "tool", "content": fmt.Sprintf("t%d", n), "tool_name": "x", "tool_call_id": fmt.Sprintf("c%d", n)},
			}, AppendGuards{}, nil)
			if err != nil {
				fail("writer", err)
				return
			}
		}
	}()
	for range 3 {
		go func() {
			defer wg.Done()
			for !stopped() {
				if _, err := db.GetCompressionLockHolder("race-sess"); err != nil {
					fail("reader", err)
					return
				}
			}
		}()
	}
	select {
	case <-stop:
	case <-time.After(3 * time.Second):
		once.Do(func() { close(stop) })
	}
	wg.Wait()
	if len(failures) != 0 {
		t.Fatalf("race failures %v", failures)
	}
}
