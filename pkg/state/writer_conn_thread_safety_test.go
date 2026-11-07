package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Errors from three readers hammering the store for three seconds against a turn shaped batch writer
func runWriterReaderRace(t *testing.T, reader func(db *SessionDB, sid string) error) []string {
	t.Helper()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	defer db.Close()
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
				if err := reader(db, "race-sess"); err != nil {
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
	return failures
}

func TestPendingHandoffReadsRaceFree(t *testing.T) {
	ctx := context.Background()
	failures := runWriterReaderRace(t, func(db *SessionDB, sid string) error {
		db.ListPendingHandoffs(ctx)
		if db.GetHandoffState(ctx, sid) == nil {
			return errors.New("handoff state read failed")
		}
		return nil
	})
	if len(failures) != 0 {
		t.Fatal(failures)
	}
}

func TestActivityLabelNoopFastPathRaceFree(t *testing.T) {
	failures := runWriterReaderRace(t, func(db *SessionDB, sid string) error {
		return db.ClearSessionActivityLabels(sid)
	})
	if len(failures) != 0 {
		t.Fatal(failures)
	}
}

const nullWithoutExceptionText = "<TrackedConnection object at 0x0> returned NULL without setting an exception"

func TestMatchingErrorInsideCallbackIsNotReplayed(t *testing.T) {
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	defer db.Close()
	calls := 0
	err := db.executeWrite(context.Background(), func(SQLiteConn) error {
		calls++
		return &pyConvError{"SystemError", nullWithoutExceptionText}
	}, nil)
	var systemErr *pyConvError
	if !errors.As(err, &systemErr) || systemErr.Class != "SystemError" || !strings.Contains(systemErr.Msg, "returned NULL") {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times", calls)
	}
}

func TestPostCommitMaintenanceErrorDoesNotReplayMessage(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	defer db.Close()
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	before := db.writeCount.Load()
	maintenanceCalls := 0
	savedEvery, savedMerge := sessionFTSMergeEveryNWrites, sessionMergeFTSIncrementally
	defer func() { sessionFTSMergeEveryNWrites, sessionMergeFTSIncrementally = savedEvery, savedMerge }()
	sessionFTSMergeEveryNWrites = 1
	sessionMergeFTSIncrementally = func(*sessionSearch, context.Context, int, *int) (int, error) {
		maintenanceCalls++
		if maintenanceCalls == 1 {
			return 0, &pyConvError{"SystemError", nullWithoutExceptionText}
		}
		return 0, nil
	}
	messageID, err := db.AppendMessage(ctx, "s1", map[string]any{"role": "user", "content": "exactly-once"}, AppendGuards{})
	if err != nil || messageID <= 0 {
		t.Fatalf("append %d %v", messageID, err)
	}
	rows, err := db.GetMessages(ctx, "s1", GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	matching := 0
	for _, row := range rows {
		if objStr(row, "content") == "exactly-once" {
			matching++
		}
	}
	session, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	count, _ := session.Get("message_count")
	if matching != 1 || count != int64(1) {
		t.Fatalf("matching %d message_count %v", matching, count)
	}
	if got := db.writeCount.Load(); got != before+1 {
		t.Fatalf("write count %d, want %d", got, before+1)
	}
	if maintenanceCalls != 1 {
		t.Fatalf("maintenance ran %d times", maintenanceCalls)
	}
}
