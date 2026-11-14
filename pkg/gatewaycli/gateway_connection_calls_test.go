package gatewaycli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestKeyedResumeFallsBackPlainOnlyWhenTheKeyDoesNotApply(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, refusal string
		rpc, typed    bool
		wantCalls     int
		wantErr       string
	}{
		{"alias key not taken", "admission_conflict", true, false, 2, ""},
		{"read only viewer", "permission_denied", true, false, 2, ""},
		{"typed key conflict", "admission_conflict", true, true, 1, gateway.ResumeKeyMismatch},
		{"typed key denied", "permission_denied", true, true, 1, "permission_denied"},
		{"other refusal", "session_busy", true, false, 1, "session_busy"},
		{"transport error", "admission_conflict", false, false, 1, "admission_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := NewGatewayConnection(nil)
			conn.SetLaunchKey("launch-key", tc.typed)
			var calls []map[string]any
			call := func(_ context.Context, method string, params map[string]any) (any, error) {
				calls = append(calls, params)
				if _, keyed := params["api_key"]; keyed {
					if tc.rpc {
						return nil, &gateway.GatewayRPCError{Msg: tc.refusal}
					}
					return nil, &gateway.GatewayClientError{Msg: tc.refusal}
				}
				return storeSnapshot("local-old"), nil
			}
			snapshot, err := conn.Resume(ctx, call, "local-old", tc.typed)
			if len(calls) != tc.wantCalls || !reflect.DeepEqual(calls[0], map[string]any{"session_id": "local-old", "api_key": "launch-key"}) {
				t.Fatalf("calls %v", calls)
			}
			if tc.wantErr == "" {
				if err != nil || snapshot == nil || !reflect.DeepEqual(calls[1], map[string]any{"session_id": "local-old"}) {
					t.Fatalf("snapshot %v err %v calls %v", snapshot, err, calls)
				}
				return
			}
			wantClientError(t, err, tc.wantErr)
		})
	}
}

func eventFrame(kind, sessionID string, payload map[string]any) *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "method", "event", "params",
		pyjson.ObjectOf("type", kind, "session_id", sessionID, "payload", payload))
}

func nextEvent(t *testing.T, conn *GatewayConnection) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame, err := conn.Events.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestEventFramesUpdateTheSessionCacheAndSideAnswersReachTheirWaiter(t *testing.T) {
	db, client := storeFixture(t)
	conn := db.Connection
	mustOpen(t, db, storeCLI("local-old", true), nil)
	cached := conn.State("local-old")
	var mu sync.Mutex
	var usages, cancels []map[string]any
	cached.Lock()
	onUsage := SessionListener(func(_ context.Context, usage map[string]any) {
		mu.Lock()
		usages = append(usages, usage)
		mu.Unlock()
	})
	onCancel := SessionListener(func(_ context.Context, payload map[string]any) {
		mu.Lock()
		cancels = append(cancels, payload)
		mu.Unlock()
	})
	cached.UsageListeners = append(cached.UsageListeners, &onUsage)
	cached.RequestListeners = append(cached.RequestListeners, &onCancel)
	seq := cached.PendingSeq
	cached.Unlock()
	client.events.PutNowait(eventFrame("btw.complete", "local-old", map[string]any{"task_id": "task-1", "text": "side"}))
	client.events.PutNowait(eventFrame("session.info", "local-old", map[string]any{"session_id": "ignored", "model": "next-model",
		"title": "Renamed", "pending": []any{"adm-1"}}))
	client.events.PutNowait(eventFrame("todo.updated", "local-old", map[string]any{"todos": []any{"a"}, "revision": 4}))
	client.events.PutNowait(eventFrame("session.usage", "local-old", map[string]any{"api_calls": 2, "usage": map[string]any{"nested": true}}))
	client.events.PutNowait(eventFrame("session.usage", "local-old", map[string]any{"usage": map[string]any{"nested": true}}))
	client.events.PutNowait(eventFrame("request.cancel", "local-old", map[string]any{"request_id": "r1"}))
	client.events.PutNowait(eventFrame("session.replay_gap", "local-old", nil))
	for range 6 {
		nextEvent(t, conn)
	}
	if !conn.Events.Empty() {
		t.Fatal("the side answer reached the turn queue")
	}
	side, ok := conn.SideWaiter("task-1").GetNowait()
	if !ok || !reflect.DeepEqual(side, map[string]any{"task_id": "task-1", "text": "side"}) {
		t.Fatalf("side answer %v", side)
	}
	if !cached.RunsRoute("next-model", "") || cached.RunsRoute("other-model", "") {
		t.Fatal("the cached route does not match its own model")
	}
	conn.DropSideWaiter("task-1")
	if !conn.SideWaiter("task-1").Empty() {
		t.Fatal("a dropped waiter kept its answer")
	}
	cached.Lock()
	defer cached.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if cached.Info["model"] != "next-model" || cached.Info["session_id"] != nil || cached.Title != "Renamed" ||
		!reflect.DeepEqual(cached.Pending, []any{"adm-1"}) || cached.PendingSeq != seq+1 {
		t.Fatalf("info %v title %q pending %v seq %d", cached.Info, cached.Title, cached.Pending, cached.PendingSeq)
	}
	if !reflect.DeepEqual(cached.PlanState, map[string]any{"todos": []any{"a"}, "revision": 4}) || cached.Gaps != 1 {
		t.Fatalf("plan %v gaps %d", cached.PlanState, cached.Gaps)
	}
	if len(usages) != 2 || usages[0]["api_calls"] != 2 || usages[1]["nested"] != true || !reflect.DeepEqual(cached.Usage, usages[1]) {
		t.Fatalf("usages %v", usages)
	}
	if len(cancels) != 1 || cancels[0]["request_id"] != "r1" {
		t.Fatalf("cancels %v", cancels)
	}
}

func TestReplyAnswersAServerRequestOnTheLiveSocket(t *testing.T) {
	db, client := storeFixture(t)
	if !db.Connection.Reply(7, map[string]any{"password": "pw"}) {
		t.Fatal("reply refused on a live socket")
	}
	client.mu.Lock()
	frames := append([]string{}, client.frames...)
	client.mu.Unlock()
	decoded, err := pyjson.Loads(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	wantEqual(t, pyjson.Plain(decoded), map[string]any{"jsonrpc": "2.0", "id": 7, "result": map[string]any{"password": "pw"}})
	if NewGatewayConnection(nil).Reply(7, nil) {
		t.Fatal("reply claimed a send with no socket")
	}
}

func TestACallGivesUpWhenAbandonedOrUnanswered(t *testing.T) {
	ctx := context.Background()
	db, client := storeFixture(t)
	conn := db.Connection
	_, err := conn.CallWith(ctx, "status", nil, CallOptions{Abandon: func() bool { return true }})
	var stopped *CallStopped
	if !errors.As(err, &stopped) || stopped.Method != "status" || len(client.of("status")) != 0 {
		t.Fatalf("error %v", err)
	}
	release := make(chan struct{})
	defer close(release)
	client.setRPC(func(ctx context.Context, method string, params map[string]any) (any, error) {
		client.record(method, params)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, os.ErrDeadlineExceeded
	})
	polls := 0
	_, err = conn.CallWith(ctx, "slow", nil, CallOptions{Abandon: func() bool { polls++; return polls > 2 }})
	if !errors.As(err, &stopped) || len(client.of("slow")) != 1 {
		t.Fatalf("error %v", err)
	}
	wantClientError(t, err, "slow")
	client.setRPC(func(context.Context, string, map[string]any) (any, error) { return nil, os.ErrDeadlineExceeded })
	_, err = conn.Call(ctx, "late", nil)
	wantClientError(t, err, "Gateway did not answer late")
}

func TestMoveCliWorkspaceMovesTheGatewaySessionBeforeThisProcess(t *testing.T) {
	ctx := context.Background()
	db, client := storeFixture(t)
	client.mu.Lock()
	client.known["local-s"] = true
	client.mu.Unlock()
	tree := filepath.Join(t.TempDir(), "moved")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("TERMINAL_CWD", "")
	info := map[string]string{"path": tree, "branch": "icebath/moved", "repo_root": filepath.Dir(tree)}
	if err := MoveCLIWorkspace(ctx, WorkspaceCLI{SessionDB: db, GatewaySessionOpen: true, SessionID: "local-s"}, info); err != nil {
		t.Fatal(err)
	}
	mutates := client.of("session.mutate")
	if len(mutates) != 1 || mutates[0]["operation"] != "workspace" || mutates[0]["session_id"] != "local-s" ||
		!reflect.DeepEqual(mutates[0]["payload"], map[string]any{"cwd": tree}) {
		t.Fatalf("mutates %v", mutates)
	}
	wantEqual(t, mutates[0]["expected_generation"], 0)
	cwd, _ := os.Getwd()
	if resolved(t, cwd) != resolved(t, tree) || os.Getenv("TERMINAL_CWD") != tree {
		t.Fatalf("cwd %s TERMINAL_CWD %s", cwd, os.Getenv("TERMINAL_CWD"))
	}
	other := t.TempDir()
	refused := client.rpc
	client.setRPC(func(ctx context.Context, method string, params map[string]any) (any, error) {
		if method == "session.mutate" {
			return nil, &gateway.GatewayRPCError{Msg: "session_busy"}
		}
		return refused(ctx, method, params)
	})
	err := MoveCLIWorkspace(ctx, WorkspaceCLI{SessionDB: db, GatewaySessionOpen: true, SessionID: "local-s"}, map[string]string{"path": other})
	wantClientError(t, err, "session_busy")
	if os.Getenv("TERMINAL_CWD") != tree || strings.Contains(mustGetwd(t), filepath.Base(other)) {
		t.Fatal("a refused move still moved this process")
	}
	if err := MoveCLIWorkspace(ctx, WorkspaceCLI{SessionDB: nil, SessionID: "local-s"}, map[string]string{"path": other}); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TERMINAL_CWD") != other {
		t.Fatalf("TERMINAL_CWD %s", os.Getenv("TERMINAL_CWD"))
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return cwd
}
