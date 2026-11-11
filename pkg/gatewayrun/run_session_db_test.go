package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Root home under ICEBATH_HOME like the home fixture
func sessionDBHome(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	return root
}

// Session store over the home with its handles swept at cleanup
func sessionDBStore(t *testing.T, root string) *gateway.SessionStore {
	t.Helper()
	ctx := context.Background()
	s := gateway.NewSessionStore(ctx, filepath.Join(root, "sessions"), gateway.DefaultGatewayConfig(ctx), nil)
	t.Cleanup(s.CloseAllDBHandles)
	return s
}

// Bare runner wired to store with an unpinned database and a fresh handle cache
func runnerWithStore(store *gateway.SessionStore) *GatewayRunner {
	r := allocGatewayRunner()
	r.sessionStore = store
	r.sessionDBHandles = map[string]any{}
	r.sessionDBHandlesLock = &sync.Mutex{}
	r.sessionDBHandleCache = gateway.NewRecoverableHandleCacheWith(gateway.RecoverableHandleCacheOptions{
		Handles: r.sessionDBHandles, Lock: r.sessionDBHandlesLock})
	return r
}

// Concrete handle the store resolves for the active scope
func storeHandle(t *testing.T, store *gateway.SessionStore) *state.SessionDB {
	t.Helper()
	raw, err := store.DB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	db, _ := raw.(*state.SessionDB)
	return db
}

func TestRunnerBorrowsTheStoresHandleInsteadOfOpeningASecond(t *testing.T) {
	store := sessionDBStore(t, sessionDBHome(t))
	storeDB := storeHandle(t, store)
	if storeDB == nil {
		t.Fatal("fixture must have a usable SQLite handle")
	}
	before := state.LiveConnectionCount(storeDB.Path())
	if before < 1 {
		t.Fatal("the store's writer connection should be live")
	}
	r := runnerWithStore(store)
	wrapper, err := r.openSessionDBForActiveScope(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if wrapper == nil {
		t.Fatal("wrapper is None")
	}
	if wrapper.DB() != storeDB {
		t.Fatal("the runner opened its own SessionDB")
	}
	if after := state.LiveConnectionCount(storeDB.Path()); after != before {
		t.Fatalf("live connections went %d -> %d", before, after)
	}
}

func TestRunnerShutdownSweepLeavesTheBorrowedHandleOpen(t *testing.T) {
	store := sessionDBStore(t, sessionDBHome(t))
	storeDB := storeHandle(t, store)
	r := runnerWithStore(store)
	if db, _ := r.openSessionDBForActiveScope(context.Background(), false); db == nil {
		t.Fatal("open returned None")
	}
	r.closeAllSessionDBHandles()
	if state.LiveConnectionCount(storeDB.Path()) < 1 {
		t.Fatal("the runner closed the handle the store owns")
	}
	if !storeDB.HasWriterConn() {
		t.Fatal("borrowed writer connection was closed")
	}
	if len(r.sessionDBHandles) != 0 {
		t.Fatalf("handles %v", r.sessionDBHandles)
	}
}

func TestRunnerWithoutASessionStoreStillOpensItsOwn(t *testing.T) {
	sessionDBHome(t)
	r := runnerWithStore(nil)
	wrapper, err := r.openSessionDBForActiveScope(context.Background(), false)
	defer r.closeAllSessionDBHandles()
	if err != nil {
		t.Fatal(err)
	}
	if wrapper == nil || wrapper.DB() == nil {
		t.Fatal("no own handle opened")
	}
}

func TestUnavailableStoreHandleDoesNotResurrectASecondOpen(t *testing.T) {
	store := sessionDBStore(t, sessionDBHome(t))
	store.PinDB(nil)
	r := runnerWithStore(store)
	if db, _ := r.openSessionDBForActiveScope(context.Background(), false); db != nil {
		t.Fatal("expected None")
	}
	if len(r.sessionDBHandles) != 0 {
		t.Fatal("a duplicate handle was cached on the store's failure path")
	}
}

func TestAHandleTheRegistryToreDownIsReopenedThroughTheRegistry(t *testing.T) {
	ctx := context.Background()
	root := sessionDBHome(t)
	store := sessionDBStore(t, root)
	r := runnerWithStore(store)
	first := storeHandle(t, store)
	if err := first.CreateSession(state.NewSession{ID: "before-unserve", Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	firstWrapper, _ := r.openSessionDBForActiveScope(ctx, false)
	if firstWrapper.DB() != first {
		t.Fatal("first wrapper does not wrap the store handle")
	}
	path := first.Path()
	if n := state.CloseAllUnder(root); n != 1 {
		t.Fatalf("close_all_under = %d", n)
	}
	second := storeHandle(t, store)
	if second == first {
		t.Fatal("second is first")
	}
	if err := second.CreateSession(state.NewSession{ID: "after-unserve", Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	if first.HasWriterConn() {
		t.Fatal("the torn-down handle was revived outside the registry")
	}
	acquired, err := state.Acquire(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if acquired != second {
		state.Release(acquired)
		t.Fatal("one file, one writer: the agent must share the store's handle")
	}
	state.Release(acquired)
	secondWrapper, _ := r.openSessionDBForActiveScope(ctx, false)
	if secondWrapper == firstWrapper || secondWrapper.DB() != second {
		t.Fatal("runner kept the torn down wrapper")
	}
}

func TestRunnerSessionDbFollowsTheActiveProfileScope(t *testing.T) {
	root := sessionDBHome(t)
	profile := filepath.Join(root, "profiles", "fitness")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := allocGatewayRunner()
	r.sessionDBHandles = map[string]any{}
	r.sessionDBHandlesLock = &sync.Mutex{}

	rootDB := r.sessionDB(ctx)
	if rootDB.DB().Path() != filepath.Join(root, "state.db") {
		t.Fatalf("root db path %s", rootDB.DB().Path())
	}
	scoped := home.WithOverride(ctx, profile)
	profileDB := r.sessionDB(scoped)
	if profileDB.DB().Path() != filepath.Join(profile, "state.db") {
		t.Fatalf("profile db path %s", profileDB.DB().Path())
	}
	if r.sessionDB(scoped) != profileDB {
		t.Fatal("profile handle not cached")
	}
	if r.sessionDB(ctx) != rootDB {
		t.Fatal("root handle not cached")
	}
	r.pinSessionDB(nil)
	if r.sessionDB(scoped) != nil {
		t.Fatal("pinned None did not win")
	}
	r.sessionDBPinned = sessionDBPin{}
	r.closeAllSessionDBHandles()
	if len(r.sessionDBHandles) != 0 {
		t.Fatal("handles not swept")
	}
	if rootDB.DB().HasWriterConn() || profileDB.DB().HasWriterConn() {
		t.Fatal("handle left open")
	}
}

func TestSessionStoreAndRunnerReopenAfterFailedConstruction(t *testing.T) {
	root := sessionDBHome(t)
	dbPath := filepath.Join(root, "state.db")
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatal(err)
	}
	now := 0.0
	delay := 1.0
	r := allocGatewayRunner()
	r.sessionDBHandles = map[string]any{}
	r.sessionDBHandlesLock = &sync.Mutex{}
	r.sessionDBHandleCache = gateway.NewRecoverableHandleCacheWith(gateway.RecoverableHandleCacheOptions{
		Handles: r.sessionDBHandles, Lock: r.sessionDBHandlesLock, Clock: func() float64 { return now }, InitialRetryDelay: &delay})
	initErr := "temporary open failure"
	r.sessionDBInitError = &initErr
	ctx := context.Background()
	if r.sessionDB(ctx) != nil {
		t.Fatal("failed open served a handle")
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	if r.sessionDB(ctx) != nil {
		t.Fatal("retried inside the backoff window")
	}
	now = 2.0
	db := r.sessionDB(ctx)
	if db == nil || db.DB() == nil {
		t.Fatal("did not reopen after the backoff")
	}
	defer r.closeAllSessionDBHandles()
	if r.sessionDBInitError != nil {
		t.Fatalf("init error kept %q", *r.sessionDBInitError)
	}
}
