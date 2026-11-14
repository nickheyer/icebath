package kanban

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Notify subscriptions of a task on the default board
func listSubsForTask(t *testing.T, taskID string) []*pyjson.Object {
	t.Helper()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	subs, err := ListNotifySubs(context.Background(), conn, &taskID, NotifyOwnerScope{})
	if err != nil {
		t.Fatal(err)
	}
	return subs
}

// Session store holding a TUI session ended by compression and its live continuation
func compactionForkStore(t *testing.T, path, stale, live, liveRole string) {
	t.Helper()
	db, err := state.Open(path, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateSession(state.NewSession{ID: stale, Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, stale, map[string]any{"role": "user", "content": "pre-fork turn"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(stale, "compression"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: live, Source: "tui", ParentSessionID: stale}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, live, map[string]any{"role": liveRole, "content": "post-fork turn"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateSubscribesGatewaySession(t *testing.T) {
	toolWorkerEnv(t)
	t.Setenv("ICEBATH_SESSION_PLATFORM", "telegram")
	t.Setenv("ICEBATH_SESSION_CHAT_ID", "chat-42")
	t.Setenv("ICEBATH_SESSION_THREAD_ID", "thread-7")
	t.Setenv("ICEBATH_SESSION_USER_ID", "user-9")
	t.Setenv("ICEBATH_SESSION_USER_ID_ALT", "alt-user-9")
	t.Setenv("ICEBATH_SESSION_CHAT_TYPE", "forum")
	d := runKanbanTool(t, "kanban_create", `{"title": "auto-sub gateway", "assignee": "peer"}`)
	if d["ok"] != true || d["subscribed"] != true {
		t.Fatal(d)
	}
	subs := listSubsForTask(t, d["task_id"].(string))
	if len(subs) != 1 {
		t.Fatal(subs)
	}
	s := subs[0]
	for name, want := range map[string]any{"platform": "telegram", "chat_id": "chat-42", "thread_id": "thread-7", "user_id": "user-9",
		"user_id_alt": "alt-user-9", "chat_type": "forum", "delivery_mode": "notify+wake"} {
		if got := subField(s, name); got != want {
			t.Fatalf("%s %v want %v", name, got, want)
		}
	}
}

func TestCreateSubscribesTUISessionViaSessionKey(t *testing.T) {
	toolWorkerEnv(t)
	for _, name := range []string{"ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_CHAT_ID", "ICEBATH_SESSION_THREAD_ID", "ICEBATH_SESSION_USER_ID", "ICEBATH_SESSION_ID"} {
		unsetEnv(t, name)
	}
	t.Setenv("ICEBATH_SESSION_KEY", "tui-session-abc")
	d := runKanbanTool(t, "kanban_create", `{"title": "auto-sub tui", "assignee": "peer"}`)
	if d["ok"] != true || d["subscribed"] != true {
		t.Fatal(d)
	}
	subs := listSubsForTask(t, d["task_id"].(string))
	if len(subs) != 1 {
		t.Fatal(subs)
	}
	for name, want := range map[string]any{"platform": "tui", "chat_id": "tui-session-abc", "chat_type": "dm", "delivery_mode": "notify"} {
		if got := subField(subs[0], name); got != want {
			t.Fatalf("%s %v want %v", name, got, want)
		}
	}
}

func TestCreateDoesNotSubscribeInCLISession(t *testing.T) {
	toolWorkerEnv(t)
	for _, name := range []string{"ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_CHAT_ID", "ICEBATH_SESSION_KEY", "ICEBATH_SESSION_ID"} {
		unsetEnv(t, name)
	}
	d := runKanbanTool(t, "kanban_create", `{"title": "no sub cli", "assignee": "peer"}`)
	if d["ok"] != true || d["subscribed"] != false {
		t.Fatal(d)
	}
	if subs := listSubsForTask(t, d["task_id"].(string)); len(subs) != 0 {
		t.Fatal(subs)
	}
}

func TestCreateTUISubscriptionBindsToLiveSessionAfterCompactionFork(t *testing.T) {
	toolWorkerEnv(t)
	stale, live := "20260912_231110_stale", "20260913_000649_live"
	store := filepath.Join(filepath.Dir(os.Getenv("ICEBATH_HOME")), "state.db")
	compactionForkStore(t, store, stale, live, "assistant")
	prev := state.DefaultDBPath
	t.Cleanup(func() { state.DefaultDBPath = prev })
	state.DefaultDBPath = store
	unsetEnv(t, "ICEBATH_SESSION_PLATFORM")
	unsetEnv(t, "ICEBATH_SESSION_CHAT_ID")
	t.Setenv("ICEBATH_SESSION_KEY", stale)
	unsetEnv(t, "ICEBATH_SESSION_ID")
	t.Setenv("ICEBATH_PROFILE", "default")
	d := runKanbanTool(t, "kanban_create", `{"title": "auto-sub tui post-fork", "assignee": "peer"}`)
	if d["ok"] != true || d["subscribed"] != true {
		t.Fatal(d)
	}
	subs := listSubsForTask(t, d["task_id"].(string))
	if len(subs) != 1 || subField(subs[0], "platform") != "tui" || subField(subs[0], "chat_id") != live {
		t.Fatal(subs)
	}
}

// Swaps the profile seams of the subscription path for one test
func patchProfiles(t *testing.T, exists func(string) bool, dir func(string) (string, error)) {
	t.Helper()
	prevExists, prevDir := profileExistsFn, profileDirFn
	t.Cleanup(func() { profileExistsFn, profileDirFn = prevExists, prevDir })
	if exists != nil {
		profileExistsFn = exists
	}
	if dir != nil {
		profileDirFn = dir
	}
}

func TestLiveTUISessionKeyUsesProfileStore(t *testing.T) {
	stale, live := "stale-teamx", "live-teamx"
	store := filepath.Join(t.TempDir(), "profiles", "teamx")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	compactionForkStore(t, filepath.Join(store, "state.db"), stale, live, "user")
	patchProfiles(t, func(name string) bool { return name == "teamx" }, func(string) (string, error) { return store, nil })
	if got := liveTUISessionKey(context.Background(), stale, "teamx"); got != live {
		t.Fatal(got)
	}
}

func TestLiveTUISessionKeyFailsOpenToOriginalKey(t *testing.T) {
	prev := acquireStateFn
	t.Cleanup(func() { acquireStateFn = prev })
	acquireStateFn = func(context.Context, string) (*state.DB, error) { return nil, &RuntimeError{"store unavailable"} }
	ctx := context.Background()
	if got := liveTUISessionKey(ctx, "stale-key", "default"); got != "stale-key" {
		t.Fatal(got)
	}
	if got := liveTUISessionKey(ctx, "stale-key", ""); got != "stale-key" {
		t.Fatal(got)
	}
	patchProfiles(t, func(string) bool { return false }, nil)
	if got := liveTUISessionKey(ctx, "stale-key", "ghost-profile"); got != "stale-key" {
		t.Fatal(got)
	}
}

func TestCreateRespectsAutoSubscribeOnCreateFalse(t *testing.T) {
	toolWorkerEnv(t)
	h := filepath.Join(filepath.Dir(os.Getenv("ICEBATH_HOME")), "gate-home", ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("kanban:\n  auto_subscribe_on_create: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_SESSION_PLATFORM", "discord")
	t.Setenv("ICEBATH_SESSION_CHAT_ID", "channel-1")
	d := runKanbanTool(t, "kanban_create", `{"title": "no sub gated", "assignee": "peer"}`)
	if d["ok"] != true || d["subscribed"] != false {
		t.Fatal(d)
	}
	if subs := listSubsForTask(t, d["task_id"].(string)); len(subs) != 0 {
		t.Fatal(subs)
	}
}

func TestMaybeAutoSubscribeSwallowsAddNotifySubFailure(t *testing.T) {
	toolWorkerEnv(t)
	t.Setenv("ICEBATH_SESSION_PLATFORM", "telegram")
	t.Setenv("ICEBATH_SESSION_CHAT_ID", "chat-42")
	prev := addNotifySubFn
	t.Cleanup(func() { addNotifySubFn = prev })
	addNotifySubFn = func(context.Context, state.SQLiteConn, NotifySubOptions) error {
		return &RuntimeError{"simulated DB failure"}
	}
	d := runKanbanTool(t, "kanban_create", `{"title": "auto-sub tolerates add_notify_sub failure", "assignee": "peer"}`)
	if d["ok"] != true || d["subscribed"] != false {
		t.Fatal(d)
	}
}
