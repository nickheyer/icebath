package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

type testClock struct {
	mu  sync.Mutex
	now float64
}

func (c *testClock) get() float64 { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *testClock) set(v float64) { c.mu.Lock(); c.now = v; c.mu.Unlock() }

type handleToken struct{ id int }

// test_session_db_recovery.py test_failed_open_obeys_backoff_then_recovers
func TestFailedOpenObeysBackoffThenRecovers(t *testing.T) {
	clock := &testClock{}
	cache := NewRecoverableHandleCacheWith(RecoverableHandleCacheOptions{Clock: clock.get, InitialRetryDelay: floatPtr(2), MaxRetryDelay: floatPtr(8)})
	handle := &handleToken{1}
	calls := 0
	opener := func() (any, error) {
		calls++
		if calls == 1 {
			return nil, &os.PathError{Op: "open", Path: "private/path/state.db", Err: errors.New("is unavailable")}
		}
		return handle, nil
	}
	expect := func(want any) {
		t.Helper()
		got, err := cache.Get("profile/state.db", opener, nil)
		if err != nil || got != want {
			t.Fatalf("got %v %v want %v", got, err, want)
		}
	}
	expect(nil)
	clock.set(1.99)
	expect(nil)
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
	clock.set(2.0)
	expect(handle)
	expect(handle)
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
}

// test_session_db_recovery.py test_retry_is_single_flight_for_concurrent_callers
func TestRetryIsSingleFlightForConcurrentCallers(t *testing.T) {
	clock := &testClock{}
	cache := NewRecoverableHandleCacheWith(RecoverableHandleCacheOptions{Clock: clock.get, InitialRetryDelay: floatPtr(1), MaxRetryDelay: floatPtr(8)})
	entered, release := make(chan struct{}), make(chan struct{})
	handle := &handleToken{1}
	var calls atomic.Int32
	opener := func() (any, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("first open fails")
		}
		close(entered)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			t.Error("release timed out")
		}
		return handle, nil
	}
	if got, _ := cache.Get("profile/state.db", opener, nil); got != nil {
		t.Fatalf("first get %v", got)
	}
	clock.set(1.0)
	result := make(chan any, 1)
	go func() { got, _ := cache.Get("profile/state.db", opener, nil); result <- got }()
	<-entered
	if got, _ := cache.Get("profile/state.db", opener, nil); got != nil || calls.Load() != 2 {
		t.Fatalf("concurrent get %v calls %d", got, calls.Load())
	}
	close(release)
	select {
	case got := <-result:
		if got != handle {
			t.Fatalf("thread result %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("thread did not finish")
	}
	if got, _ := cache.Get("profile/state.db", opener, nil); got != handle || calls.Load() != 2 {
		t.Fatalf("after %v calls %d", got, calls.Load())
	}
}

// test_session_db_recovery.py test_runtime_health_is_sanitized_and_recovers
func TestRuntimeHealthIsSanitizedAndRecovers(t *testing.T) {
	var writes []RuntimeStatusUpdate
	prev := handleCachePublishRuntimeStatus
	handleCachePublishRuntimeStatus = func(u RuntimeStatusUpdate) (int, error) { writes = append(writes, u); return 0, nil }
	t.Cleanup(func() { handleCachePublishRuntimeStatus = prev })
	clock := &testClock{}
	cache := NewRecoverableHandleCacheWith(RecoverableHandleCacheOptions{Clock: clock.get, InitialRetryDelay: floatPtr(1)})
	calls := 0
	opener := func() (any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("database disk image is malformed at secret/profile/state.db")
		}
		return &handleToken{2}, nil
	}
	if got, _ := cache.Get("secret/profile/state.db", opener, nil); got != nil {
		t.Fatalf("first get %v", got)
	}
	clock.set(1.0)
	if got, _ := cache.Get("secret/profile/state.db", opener, nil); got == nil {
		t.Fatal("recovered get returned nil")
	}
	last := fmt.Sprint(writes[len(writes)-1].Fields)
	if last != "map[session_store:map[status:ok]]" {
		t.Fatalf("last write %s", last)
	}
	serialized := fmt.Sprintf("%#v", writes)
	if strings.Contains(serialized, "secret/profile") || strings.Contains(serialized, "malformed") {
		t.Fatalf("health leaked %s", serialized)
	}
}

// test_session_db_recovery.py test_non_cacheable_guard_is_retried_immediately
func TestNonCacheableGuardIsRetriedImmediately(t *testing.T) {
	cache := NewRecoverableHandleCache()
	calls := 0
	opener := func() (any, error) { calls++; return nil, errors.New("live-system guard") }
	for range 2 {
		_, err := cache.Get("state.db", opener, func(err error) bool { return strings.Contains(err.Error(), "live-system guard") })
		if err == nil {
			t.Fatal("guard error swallowed")
		}
	}
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
}

// test_session_db_recovery.py test_close_all_rejects_and_closes_inflight_success
func TestCloseAllRejectsAndClosesInflightSuccess(t *testing.T) {
	cache := NewRecoverableHandleCache()
	entered, release := make(chan struct{}), make(chan struct{})
	handle, replacement := &handleToken{1}, &handleToken{2}
	var mu sync.Mutex
	var closed []any
	result := make(chan any, 1)
	go func() {
		got, _ := cache.Get("state.db", func() (any, error) { close(entered); <-release; return handle, nil }, nil)
		result <- got
	}()
	<-entered
	cache.CloseAll(func(h any) { mu.Lock(); closed = append(closed, h); mu.Unlock() })
	if got, _ := cache.Get("state.db", func() (any, error) { return replacement, nil }, nil); got != replacement {
		t.Fatalf("replacement get %v", got)
	}
	close(release)
	if got := <-result; got != nil {
		t.Fatalf("stale open returned %v", got)
	}
	mu.Lock()
	if len(closed) != 1 || closed[0] != handle {
		t.Fatalf("closed %v", closed)
	}
	mu.Unlock()
	if got, _ := cache.Get("state.db", func() (any, error) { return &handleToken{3}, nil }, nil); got != replacement {
		t.Fatalf("cached %v", got)
	}
}

// test_session_db_recovery.py test_close_all_preserves_inflight_failure
func TestCloseAllPreservesInflightFailure(t *testing.T) {
	cache := NewRecoverableHandleCache()
	entered, release := make(chan struct{}), make(chan struct{})
	failure := errors.New("original open failure")
	replacement := &handleToken{2}
	errs := make(chan error, 1)
	go func() {
		_, err := cache.GetWith("state.db", func() (any, error) { close(entered); <-release; return nil, failure }, HandleCacheGetOptions{RaiseOnError: true})
		errs <- err
	}()
	<-entered
	cache.CloseAll(func(any) {})
	if got, _ := cache.Get("state.db", func() (any, error) { return replacement, nil }, nil); got != replacement {
		t.Fatalf("replacement get %v", got)
	}
	close(release)
	if err := <-errs; err != failure {
		t.Fatalf("errors %v", err)
	}
	if got, _ := cache.Get("state.db", func() (any, error) { return &handleToken{3}, nil }, nil); got != replacement {
		t.Fatalf("cached %v", got)
	}
}

// Last error text, on recovered callback and registry backed reopen of a torn down handle
func TestHandleCacheLastErrorRecoveredAndRegistryReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	clock := &testClock{}
	cache := NewRecoverableHandleCacheWith(RecoverableHandleCacheOptions{Clock: clock.get, InitialRetryDelay: floatPtr(1)})
	recovered := 0
	opts := HandleCacheGetOptions{OnRecovered: func() { recovered++ }}
	if got, _ := cache.GetWith(path, func() (any, error) { return nil, errors.New("locked") }, opts); got != nil {
		t.Fatalf("failed open %v", got)
	}
	if got := cache.LastError(filepath.Join(dir, ".", "state.db")); got != "Exception: locked" {
		t.Fatalf("last error %q", got)
	}
	clock.set(1)
	first, err := cache.GetWith(path, func() (any, error) { return state.Acquire(context.Background(), path) }, opts)
	if err != nil || first == nil || recovered != 1 || cache.LastError(path) != "" {
		t.Fatalf("recover %v %v %d", first, err, recovered)
	}
	db := first.(*state.SessionDB)
	if !db.SharedRegistryOwned() {
		t.Fatal("acquired handle not registry owned")
	}
	state.ReleaseOrClose(db)
	second, err := cache.Get(path, func() (any, error) { return state.Acquire(context.Background(), path) }, nil)
	if err != nil || second == nil || second == first {
		t.Fatalf("torn down handle served again %v %v", second, err)
	}
	cache.CloseAll(func(h any) { state.ReleaseOrClose(h.(*state.SessionDB)) })
}

// An async wrapper is judged by the SessionDB it wraps so a torn down generation reopens
func TestHandleCacheJudgesAsyncWrapperByItsSessionDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	cache := NewRecoverableHandleCache()
	open := func() (any, error) {
		db, err := state.Acquire(context.Background(), path)
		if err != nil {
			return nil, err
		}
		return state.NewAsyncSessionDB(db), nil
	}
	first, err := cache.Get(path, open, nil)
	if err != nil || first == nil {
		t.Fatalf("open %v %v", first, err)
	}
	if again, _ := cache.Get(path, open, nil); again != first {
		t.Fatal("a registry owned async wrapper was not served from the cache")
	}
	state.ReleaseOrClose(first.(*state.AsyncSessionDB).DB())
	second, err := cache.Get(path, open, nil)
	if err != nil || second == nil || second == first {
		t.Fatalf("torn down async wrapper served again %v %v", second, err)
	}
	cache.CloseAll(func(h any) { state.ReleaseOrClose(h.(*state.AsyncSessionDB).DB()) })
}

// test_session_db_recovery.py test_session_store_and_runner_reopen_after_failed_construction store leg
func TestSessionStoreReopensAfterFailedConstruction(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	var opened []*state.SessionDB
	attempts := 0
	prev := acquireSessionDB
	acquireSessionDB = func(ctx context.Context, path string) (*state.DB, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("temporary open failure")
		}
		db, err := state.Open(path, state.Options{})
		opened = append(opened, db)
		return db, err
	}
	t.Cleanup(func() {
		acquireSessionDB = prev
		for _, db := range opened {
			_ = db.Close()
		}
	})
	clock := &testClock{}
	store := &SessionStore{dbHandleCache: NewRecoverableHandleCacheWith(RecoverableHandleCacheOptions{Clock: clock.get, InitialRetryDelay: floatPtr(1)})}
	if store.db(ctx) != nil || store.db(ctx) != nil || attempts != 1 {
		t.Fatalf("backoff not honoured, attempts %d", attempts)
	}
	clock.set(1.0)
	if got := store.db(ctx); got == nil || got != SessionStoreDB(opened[len(opened)-1]) {
		t.Fatalf("recovered db %v", got)
	}
}

// test_session_db_recovery.py test_recovered_db_rows_survive_fallback_structural_save
func TestRecoveredDBRowsSurviveFallbackStructuralSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	dbPath := filepath.Join(dir, "state.db")
	sessionsDir := filepath.Join(dir, "sessions")
	scope, _ := pypath.Resolve(sessionsDir)
	now := sessionNow()
	mk := func(name, sid string) *SessionEntry {
		e := NewSessionEntry("agent:main:telegram:dm:"+name, sid, now, now)
		e.Platform, e.ChatType = PlatformTelegram, "dm"
		e.Origin = &SessionSource{Platform: PlatformTelegram, ChatID: name}
		return e
	}
	durable, deleted, changed := mk("durable", "durable-session"), mk("deleted", "deleted-session"), mk("changed", "changed-before-recovery")
	seed, err := state.NewSessionDB(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []*SessionEntry{durable, deleted, changed} {
		raw, _ := pyjson.Dumps(e.ToDict())
		if err := seed.SaveGatewayRoutingEntry(ctx, e.SessionKey, raw, scope); err != nil {
			t.Fatal(err)
		}
	}
	_ = seed.Close()
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := pyjson.NewObject()
	legacy.Set(deleted.SessionKey, deleted.ToDict())
	legacy.Set(changed.SessionKey, changed.ToDict())
	raw, _ := pyjson.Dumps(legacy)
	if err := os.WriteFile(filepath.Join(sessionsDir, "sessions.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	prev := acquireSessionDB
	acquireSessionDB = func(ctx context.Context, path string) (*state.DB, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("temporary open failure")
		}
		return state.Acquire(ctx, path)
	}
	t.Cleanup(func() { acquireSessionDB = prev })
	store := NewSessionStore(ctx, sessionsDir, &GatewayConfig{SessionsDir: sessionsDir, WriteSessionsJSON: false}, nil)
	t.Cleanup(store.CloseAllDBHandles)
	store.ensureLoaded(ctx)
	store.dbHandleCache.Lock.Lock()
	store.dbHandleCache.unavailable[pypath.PathStr(dbPath)].nextRetryAt = 0
	store.dbHandleCache.Lock.Unlock()
	current := mk("current", "current-session")
	store.mu.Lock()
	store.entries.delete(deleted.SessionKey)
	store.entries.get(changed.SessionKey).SessionID = "changed-during-fallback"
	store.entries.set(current.SessionKey, current)
	saveErr := store.save(ctx)
	store.mu.Unlock()
	if saveErr != nil {
		t.Fatal(saveErr)
	}
	rows, err := store.db(ctx).(gatewayRoutingLoader).LoadGatewayRoutingEntries(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[durable.SessionKey] == "" || rows[changed.SessionKey] == "" || rows[current.SessionKey] == "" {
		t.Fatalf("rows %v", rows)
	}
	if got := store.entries.get(durable.SessionKey); got == nil || got.SessionID != durable.SessionID {
		t.Fatalf("durable entry %v", got)
	}
	changedRow, _ := pyjson.Loads(rows[changed.SessionKey])
	if sid, _ := changedRow.(*pyjson.Object).Get("session_id"); sid != "changed-during-fallback" {
		t.Fatalf("changed row session %v", sid)
	}
	if got := store.entries.get(current.SessionKey); got.SessionID != current.SessionID {
		t.Fatalf("current entry %v", got.SessionID)
	}
}
