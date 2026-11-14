package tuiserver

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

type hostedRoomFakeCall struct {
	method string
	params map[string]any
}

// Fake server whose handlers record their params, with a session table and an approval reader
type hostedRoomFakeServer struct {
	mu       sync.Mutex
	methods  map[string]Handler
	sessions map[string]*tuigateway.Session
	calls    []hostedRoomFakeCall
	pending  func(ctx context.Context, sessionKey string) map[string]any
}

func newHostedRoomFakeServer() *hostedRoomFakeServer {
	f := &hostedRoomFakeServer{sessions: map[string]*tuigateway.Session{}}
	method := func(name string, result map[string]any) Handler {
		return func(_ context.Context, rid any, params map[string]any) map[string]any {
			f.mu.Lock()
			f.calls = append(f.calls, hostedRoomFakeCall{name, params})
			f.mu.Unlock()
			out := map[string]any{"id": rid}
			for key, value := range result {
				out[key] = value
			}
			return out
		}
	}
	f.methods = map[string]Handler{
		"session.list": method("session.list", map[string]any{"result": map[string]any{
			"sessions": []any{map[string]any{"id": "stored", "resolved_id": "tip", "title": "Group: room"}}}}),
		"session.create":    method("session.create", map[string]any{"result": map[string]any{"session_id": "runtime"}}),
		"session.resume":    method("session.resume", map[string]any{"result": map[string]any{"session_id": "runtime"}}),
		"session.history":   method("session.history", map[string]any{"result": map[string]any{"messages": []any{map[string]any{"role": "assistant"}}}}),
		"session.interrupt": method("session.interrupt", map[string]any{"result": map[string]any{"interrupted": true}}),
		"approval.respond":  method("approval.respond", map[string]any{"result": map[string]any{"resolved": 1}}),
		"prompt.submit":     method("prompt.submit", map[string]any{"result": map[string]any{"status": "streaming"}}),
	}
	f.pending = func(context.Context, string) map[string]any { return nil }
	return f
}

func (f *hostedRoomFakeServer) server() HostedRoomServer {
	return HostedRoomServer{
		Method: func(name string) Handler { return f.methods[name] },
		SessionRecord: func(sessionID string) *tuigateway.Session {
			if record := f.sessions[sessionID]; record != nil {
				return record
			}
			for _, candidate := range f.sessions {
				if key, _ := candidate.Get("session_key").(string); key == sessionID {
					return candidate
				}
			}
			return nil
		},
		PendingApproval: func(ctx context.Context, sessionKey string) map[string]any { return f.pending(ctx, sessionKey) },
		ProfileHome:     profileHome,
	}
}

func (f *hostedRoomFakeServer) paramsOf(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.method == method {
			return call.params
		}
	}
	return nil
}

func TestRoutesExactHiddenSessionAndInternalTaskProof(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	fake := newHostedRoomFakeServer()
	rpc := NewHostedRoomServerRPC(fake.server(), nil)
	task := gateway.TaskIdentity{RoomID: "room", TaskID: "task", ThreadID: "thread", TurnID: "turn"}
	called := 0
	callback := func(map[string]any) error { called++; return nil }
	member := "ops-member"

	resolved, err := rpc.ResolveExact(ctx, "ops", "Group: room", "bot_room")
	if err != nil || resolved["session_id"] != "tip" {
		t.Fatalf("resolve_exact = %v, %v", resolved, err)
	}
	created, err := rpc.Create(ctx, "ops", "Group: room", "bot_room")
	if err != nil || created["session_id"] != "runtime" {
		t.Fatalf("create = %v, %v", created, err)
	}
	if _, err := rpc.Submit(ctx, gateway.HostedSubmit{Profile: "ops", SessionID: "runtime", Prompt: "Do the work", Source: "bot_room",
		Task: task, ExecutionGeneration: 2, OnTerminal: callback, MemberID: &member}); err != nil {
		t.Fatal(err)
	}

	create := fake.paramsOf("session.create")
	submit := fake.paramsOf("prompt.submit")
	if create["hidden"] != true || create["room_plumbing"] != true || create["follow_profile_config"] != true || create["close_on_disconnect"] != false {
		t.Fatalf("create params = %v", create)
	}
	want := map[string]any{"room_id": "room", "task_id": "task", "thread_id": "thread", "turn_id": "turn",
		"execution_generation": int64(2), "member_id": "ops-member"}
	if !reflect.DeepEqual(submit["_hosted_task"], want) {
		t.Fatalf("_hosted_task = %#v", submit["_hosted_task"])
	}
	handed, ok := submit["_hosted_terminal_callback"].(func(map[string]any) error)
	if !ok || reflect.ValueOf(handed).Pointer() != reflect.ValueOf(callback).Pointer() {
		t.Fatalf("_hosted_terminal_callback is not the callback")
	}

	if _, err := rpc.Resume(ctx, "ops", "stored", "bot_room"); err != nil {
		t.Fatal(err)
	}
	if resume := fake.paramsOf("session.resume"); resume["source"] != "bot_room" {
		t.Fatalf("resume params = %v", resume)
	}
}

func TestInfoAndInterruptAreExactTaskScoped(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	fake := newHostedRoomFakeServer()
	fake.sessions["runtime"] = tuigateway.NewSession(map[string]any{
		"history_lock": &sync.Mutex{}, "running": true, "_hosted_room_task": map[string]any{"task_id": "task-a"}})
	rpc := NewHostedRoomServerRPC(fake.server(), nil)

	info, err := rpc.Info(ctx, "ops", "runtime", "bot_room")
	if err != nil || !reflect.DeepEqual(info, map[string]any{"active": true, "task_id": "task-a"}) {
		t.Fatalf("info = %v, %v", info, err)
	}
	if _, err := rpc.Interrupt(ctx, "ops", "runtime", "bot_room", "task-a"); err != nil {
		t.Fatal(err)
	}
	if params := fake.paramsOf("session.interrupt"); params["expected_hosted_task_id"] != "task-a" {
		t.Fatalf("interrupt params = %v", params)
	}
}

func TestLocalApprovalSnapshotAndResponseUseExactRequest(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	fake := newHostedRoomFakeServer()
	fake.pending = func(_ context.Context, sessionKey string) map[string]any {
		if sessionKey != "stored-session" {
			return nil
		}
		return map[string]any{"request_id": "approval-1", "command": "pytest -q tests/focused", "choices": []any{"once", "deny"}}
	}
	fake.sessions["runtime"] = tuigateway.NewSession(map[string]any{
		"history_lock": &sync.Mutex{}, "running": true, "session_key": "stored-session",
		"_hosted_room_task": map[string]any{"task_id": "task-a"}})
	rpc := NewHostedRoomServerRPC(fake.server(), nil)

	info, err := rpc.Info(ctx, "ops", "runtime", "bot_room")
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := info["pending_approval"].(map[string]any)
	if info["status"] != "waiting_for_approval" || pending["request_id"] != "approval-1" {
		t.Fatalf("info = %v", info)
	}
	approved, err := rpc.Approve(ctx, "runtime", "approval-1", "once")
	if err != nil || !reflect.DeepEqual(approved, map[string]any{"resolved": 1}) {
		t.Fatalf("approve = %v, %v", approved, err)
	}
	want := map[string]any{"session_id": "runtime", "request_id": "approval-1", "choice": "once", "all": false}
	if params := fake.paramsOf("approval.respond"); !reflect.DeepEqual(params, want) {
		t.Fatalf("approval.respond params = %v", params)
	}
}

func TestRpcErrorsAreTyped(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fake := newHostedRoomFakeServer()
	fake.methods["session.list"] = func(_ context.Context, rid any, _ map[string]any) map[string]any {
		return map[string]any{"id": rid, "error": map[string]any{"code": 4007, "message": "not found"}}
	}
	rpc := NewHostedRoomServerRPC(fake.server(), nil)

	_, err := rpc.ResolveExact(context.Background(), "ops", "Group: room", "bot_room")
	var typed *HostedRoomSessionError
	if !errors.As(err, &typed) || typed.Code != 4007 {
		t.Fatalf("resolve_exact error = %v", err)
	}
}

func TestPromptRejectionIsProvenNotAdmitted(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fake := newHostedRoomFakeServer()
	fake.methods["prompt.submit"] = func(_ context.Context, rid any, _ map[string]any) map[string]any {
		return map[string]any{"id": rid, "error": map[string]any{"code": 4121, "message": "session is already busy"}}
	}
	rpc := NewHostedRoomServerRPC(fake.server(), nil)
	member := "ops"

	_, err := rpc.Submit(context.Background(), gateway.HostedSubmit{Profile: "ops", SessionID: "runtime", Prompt: "Do the work", Source: "bot_room",
		Task: gateway.TaskIdentity{RoomID: "room", TaskID: "task", ThreadID: "thread", TurnID: "turn"}, ExecutionGeneration: 1,
		OnTerminal: func(map[string]any) error { return nil }, MemberID: &member})
	var typed *HostedRoomSessionError
	if !errors.As(err, &typed) || typed.Code != 4121 || !typed.NotAdmitted {
		t.Fatalf("submit error = %v", err)
	}
	var attempt *gateway.HostedAttemptError
	if !errors.As(err, &attempt) || !attempt.NotAdmitted {
		t.Fatalf("submit error carries no not admitted proof: %v", err)
	}
}
