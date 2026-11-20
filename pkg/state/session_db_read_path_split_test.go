package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Store holding one session with a user and an assistant message
func readPathStore(t *testing.T) *SessionDB {
	t.Helper()
	ctx := context.Background()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(NewSession{ID: "s1", Source: "cli", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range [][2]string{{"user", "hello graphiti world"}, {"assistant", "the neo4j daemon is healthy"}} {
		if _, err := db.AppendMessage(ctx, "s1", map[string]any{"role": m[0], "content": m[1]}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if !db.walActive.Load() {
		t.Fatal("read path tests need WAL")
	}
	return db
}

// Runs fn on another goroutine while the writer lock is held, failing when it does not finish within five seconds
func whileWriterLocked(t *testing.T, db *SessionDB, what string, fn func()) {
	t.Helper()
	db.mu.Lock()
	defer db.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		db.mu.Unlock()
		<-done
		db.mu.Lock()
		t.Fatalf("%s blocked on writer lock", what)
	}
}

func TestReadsDoNotTakeWriterLock(t *testing.T) {
	db := readPathStore(t)
	ctx := context.Background()
	var session *pyjson.Object
	var search, messages []*pyjson.Object
	var errs [3]error
	whileWriterLocked(t, db, "read path", func() {
		session, errs[0] = db.GetSession(ctx, "s1")
		o := NewSearchMessagesOptions()
		o.Limit = 10
		search, errs[1] = db.SearchMessages(ctx, "graphiti", o)
		messages, errs[2] = db.GetMessages(ctx, "s1", GetMessagesOptions{})
	})
	if err := errors.Join(errs[:]...); err != nil {
		t.Fatal(err)
	}
	if objStr(session, "id") != "s1" {
		t.Fatalf("session = %v", session)
	}
	found := false
	for _, hit := range search {
		if strings.Contains(objStr(hit, "snippet"), "graphiti") {
			found = true
		}
	}
	if !found || len(messages) != 2 {
		t.Fatalf("search %v messages %d", search, len(messages))
	}
}

// Writer connection whose every call records a touch and fails
type poisonWriter struct{ touched atomic.Bool }

func (p *poisonWriter) fail() error {
	p.touched.Store(true)
	return errors.New("read touched shared writer connection")
}

func (p *poisonWriter) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, p.fail()
}

func (p *poisonWriter) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, p.fail()
}

func (p *poisonWriter) QueryRowContext(context.Context, string, ...any) *sql.Row {
	p.fail()
	return nil
}

func (p *poisonWriter) Close() error { return p.fail() }

func TestBackgroundStateReadsNeverTouchSharedWriter(t *testing.T) {
	db := readPathStore(t)
	ctx := context.Background()
	if !db.TryAcquireCompressionLock("s1", "holder", 60) {
		t.Fatal("lock not acquired")
	}
	if ok, err := db.RequestHandoff(ctx, "s1", "telegram"); err != nil || !ok {
		t.Fatalf("handoff %v %v", ok, err)
	}
	conn, err := db.checkoutReadConn(ctx)
	if err != nil || conn == nil {
		t.Fatalf("read conn %v %v", conn, err)
	}
	if !db.pushIdleReadConn(conn) {
		t.Fatal("read conn not pooled")
	}
	poison := &poisonWriter{}
	db.mu.Lock()
	writer := db.conn
	db.conn = poison
	db.mu.Unlock()
	defer func() {
		db.mu.Lock()
		db.conn = writer
		db.mu.Unlock()
	}()
	holder, err := db.GetCompressionLockHolder("s1")
	if err != nil || holder == nil || *holder != "holder" {
		t.Fatalf("holder %v %v", holder, err)
	}
	if err := db.ClearSessionActivityLabels("s1"); err != nil {
		t.Fatal(err)
	}
	state := db.GetHandoffState(ctx, "s1")
	want := pyjson.ObjectOf("state", "pending", "platform", "telegram", "error", nil)
	if !pyjson.Equal(state, want) {
		t.Fatalf("handoff state %v", state)
	}
	var ids []string
	for _, row := range db.ListPendingHandoffs(ctx) {
		ids = append(ids, objStr(row, "id"))
	}
	if strings.Join(ids, ",") != "s1" {
		t.Fatalf("pending handoffs %v", ids)
	}
	if poison.touched.Load() {
		t.Fatal("read touched shared writer connection")
	}
}

func TestReadYourWrites(t *testing.T) {
	db := readPathStore(t)
	ctx := context.Background()
	if _, err := db.AppendMessage(ctx, "s1", map[string]any{"role": "user", "content": "zanzibar checkpoint"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	o := NewSearchMessagesOptions()
	o.Limit = 5
	rows, err := db.SearchMessages(ctx, "zanzibar", o)
	if err != nil || len(rows) == 0 {
		t.Fatalf("committed write invisible to read connection %v", err)
	}
}

func TestNonWALUsesLockedPath(t *testing.T) {
	db := readPathStore(t)
	ctx := context.Background()
	db.walActive.Store(false)
	if conn, err := db.getReadConn(ctx); err != nil || conn != nil {
		t.Fatalf("read conn %v %v", conn, err)
	}
	if row, err := db.GetSession(ctx, "s1"); err != nil || objStr(row, "id") != "s1" {
		t.Fatalf("session %v %v", row, err)
	}
}

func TestReadConnOpenFailureMarksThread(t *testing.T) {
	ctx := context.Background()
	fresh := mustSessionDB(t, filepath.Join(t.TempDir(), "state2.db"), false)
	defer fresh.Close()
	if err := fresh.CreateSession(NewSession{ID: "x", Source: "cli", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	saved := sessionConnectTracked
	defer func() { sessionConnectTracked = saved }()
	sessionConnectTracked = func(p string, opts ...TrackedOption) (*TrackedConn, error) {
		if strings.HasPrefix(p, "file:") && strings.Contains(p, "mode=ro") {
			calls.Add(1)
			return nil, &SQLiteError{Class: SQLiteOperationalError, Msg: "simulated open failure"}
		}
		return saved(p, opts...)
	}
	for range 2 {
		if row, err := fresh.GetSession(ctx, "x"); err != nil || objStr(row, "id") != "x" {
			t.Fatalf("session %v %v", row, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("open failure should be remembered, %d opens", calls.Load())
	}
}

func TestAnchoredViewAndAroundUseReadPath(t *testing.T) {
	db := readPathStore(t)
	ctx := context.Background()
	msgs, err := db.GetMessages(ctx, "s1", GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	anchor, _ := msgs[0].Get("id")
	var around, view *pyjson.Object
	var errs [2]error
	whileWriterLocked(t, db, "anchored reads", func() {
		around, errs[0] = db.GetMessagesAround(ctx, "s1", anchor.(int64), 2)
		o := NewAnchoredViewOptions()
		o.Window, o.Bookend = 2, 1
		view, errs[1] = db.GetAnchoredView(ctx, "s1", anchor.(int64), o)
	})
	if err := errors.Join(errs[:]...); err != nil {
		t.Fatal(err)
	}
	for _, got := range []*pyjson.Object{around, view} {
		window, _ := got.Get("window")
		if !pyTruthy(window) {
			t.Fatalf("empty window in %v", got)
		}
	}
}

func TestSessionResumeReadsDoNotTakeWriterLock(t *testing.T) {
	db := readPathStore(t)
	ctx := context.Background()
	appendPair := func(sid, user, reply string) {
		for _, m := range [][2]string{{"user", user}, {"assistant", reply}} {
			if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": m[0], "content": m[1]}, AppendGuards{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.CreateSession(NewSession{ID: "parent1", Source: "cli", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	appendPair("parent1", "parent turn", "parent reply")
	if err := db.EndSession("parent1", "compression"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(NewSession{ID: "child1", Source: "cli", Model: "m", ParentSessionID: "parent1"}); err != nil {
		t.Fatal(err)
	}
	appendPair("child1", "child turn", "child reply")
	var conversation, model, display, prefix []map[string]any
	var errs [3]error
	whileWriterLocked(t, db, "session resume reads", func() {
		conversation, errs[0] = db.GetMessagesAsConversation(ctx, "s1", ConversationOptions{})
		model, display, errs[1] = db.GetResumeConversations(ctx, "child1")
		prefix, errs[2] = db.GetAncestorDisplayPrefix(ctx, "child1")
	})
	if err := errors.Join(errs[:]...); err != nil {
		t.Fatal(err)
	}
	if len(conversation) != 2 || len(model) != 2 || len(display) != 4 || len(prefix) != 2 {
		t.Fatalf("conversation %d model %d display %d prefix %d", len(conversation), len(model), len(display), len(prefix))
	}
}
