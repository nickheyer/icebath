package agent

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const activityOffLoopSession = "session-1"

// In process owner standing in for the worker socket, recording each operation and whether a loop ran on its sender
type activityOwnerReplies struct {
	mu        sync.Mutex
	persisted []any
	onLoop    []bool
	sending   bool
	reply     *pyjson.Object
}

func (o *activityOwnerReplies) connect(ctx context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sending = pyctx.RunningLoop(ctx) || tools.RelayHasRunningEventLoop(ctx)
}

func (o *activityOwnerReplies) send(requestID int64, message string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.onLoop = append(o.onLoop, o.sending)
	raw, err := pyjson.Loads(message)
	if err != nil {
		return err
	}
	params, _ := raw.(*pyjson.Object).GetOr("params", nil).(*pyjson.Object)
	o.persisted = append(o.persisted, params.GetOr("operation", nil))
	o.reply = pyjson.ObjectOf("jsonrpc", "2.0", "id", requestID, "result", pyjson.ObjectOf("ok", true, "value", nil))
	return nil
}

func (o *activityOwnerReplies) receive(time.Duration) (*pyjson.Object, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reply, nil
}

// The relay_turn fixture, a managed turn on session-1 over a binding whose pipelines run their callbacks on the Relay loop
func activityRelayTurn(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "profile"))
	t.Setenv("ICEBATH_KANBAN_TASK", "")
	f := newRelayCompactionFake()
	f.relay.LLMRequest = func(headers, content map[string]any) (any, error) {
		return &tools.RelayLLMRequest{Headers: headers, Content: content}, nil
	}
	f.relay.LLM = &tools.NemoRelayLLM{Execute: func(_ context.Context, _ string, request any, callback tools.RelayProviderCallback, _ map[string]any) (any, error) {
		return tools.RelayAwaitable(func(ctx context.Context) (any, error) { return callback(ctx, request) }), nil
	}}
	f.relay.Tools = &tools.NemoRelayTools{Execute: func(_ context.Context, _ string, args any, callback tools.RelayProviderCallback, _ map[string]any) (any, error) {
		return tools.RelayAwaitable(func(ctx context.Context) (any, error) { return callback(ctx, args) }), nil
	}}
	f.relay.ToolExecutionResult = func(result any) (*tools.RelayToolExecutionResult, error) {
		return &tools.RelayToolExecutionResult{Result: result}, nil
	}
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) { return f.relay, nil })
	tools.ResetRelayRuntimeForTests()
	ctx := pyctx.With(context.Background(), pyctx.New())
	lease := tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: activityOffLoopSession, Platform: "cli"})
	turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, "turn-1", "task-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Host.RetainManagedExecution("test.activity_off_loop"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lease.Host.ReleaseManagedExecution("test.activity_off_loop")
		tools.RelayCoordinator.EndTurn(ctx, turn, "success")
		tools.RelayCoordinator.ReleaseConversation(lease)
		tools.ResetRelayRuntimeForTests()
		tools.RegisterNemoRelayBinding(nil)
	})
	return ctx
}

// The worker_agent fixture, an agent whose session store is a worker store reaching the in process owner
func activityWorkerAgent(t *testing.T) (*AIAgent, *RuntimeSessionStore, *activityOwnerReplies) {
	t.Helper()
	owner := &activityOwnerReplies{}
	prev := workerRPCConnection
	workerRPCConnection = func(_ *WorkerRPC, ctx context.Context) (workerOwnerConn, error) {
		owner.connect(ctx)
		return owner, nil
	}
	t.Cleanup(func() { workerRPCConnection = prev })
	tmp := t.TempDir()
	rpc := mustNewWorkerRPC(t, filepath.Join(tmp, "home"))
	store := mustStore(t, rpc.Call, pyjson.ObjectOf("session_id", activityOffLoopSession), filepath.Join(tmp, "outbox"), 0)
	t.Cleanup(store.closeOwner)
	return &AIAgent{sessionID: activityOffLoopSession, sessionDB: store}, store, owner
}

// Asserts no on loop refusal was logged or stuck and every operation reached the owner off the loop
func assertPersistedOffLoop(t *testing.T, records func() []loggerRecord, store *RuntimeSessionStore, owner *activityOwnerReplies, operations ...any) {
	t.Helper()
	for _, r := range records() {
		if strings.Contains(r.msg, "synchronous_rpc_on_event_loop") {
			t.Fatalf("guard warning %q", r.msg)
		}
	}
	if f := store.Failure(); f != nil {
		t.Fatalf("store failure %q", *f)
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if !reflect.DeepEqual(owner.persisted, operations) {
		t.Fatalf("persisted %v, want %v", owner.persisted, operations)
	}
	if !reflect.DeepEqual(owner.onLoop, make([]bool, len(operations))) {
		t.Fatalf("on loop %v", owner.onLoop)
	}
}

// Runs one managed tool call on a pool goroutine holding a copy of the caller context, waiting at most 30 seconds
func activityToolOnPool(t *testing.T, ctx context.Context, name string, args map[string]any, tool func(ctx context.Context, args map[string]any) (any, error)) {
	t.Helper()
	done := make(chan error, 1)
	worker := pyctx.With(ctx, pyctx.CopyContext(ctx))
	go func() {
		_, _, err := tools.RelayExecuteTool(worker, name, args, tool, activityOffLoopSession, "call-1", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("tool call did not finish")
	}
}

func TestProviderCallbackHeartbeatPersistsOffRelayLoop(t *testing.T) {
	ctx := activityRelayTurn(t)
	agent, store, owner := activityWorkerAgent(t)
	records := captureLogger(t, "agent.runtime_session_store", slog.LevelWarn)
	var onLoop bool
	callback := func(c context.Context, _ map[string]any) (any, error) {
		onLoop = tools.RelayHasRunningEventLoop(c)
		agent.TouchActivity("waiting for provider response")
		return map[string]any{"id": "r", "object": "chat.completion", "choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}}, nil
	}
	session := activityOffLoopSession
	if _, err := provider.RelayExecute(ctx, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "model": "m"}, callback,
		provider.RelayExecuteOptions{Name: "prov", ModelName: "m", SessionID: &session, Metadata: map[string]any{"api_mode": "chat_completions", "api_request_id": "req-1"}}); err != nil {
		t.Fatal(err)
	}
	if !onLoop {
		t.Fatal("the provider callback did not run on the Relay loop")
	}
	assertPersistedOffLoop(t, records, store, owner, "session.activity")
}

func TestConcurrentToolCallbackHeartbeatPersistsOffRelayLoop(t *testing.T) {
	ctx := activityRelayTurn(t)
	agent, store, owner := activityWorkerAgent(t)
	records := captureLogger(t, "agent.runtime_session_store", slog.LevelWarn)
	var onLoop bool
	activityToolOnPool(t, ctx, "image_generate", map[string]any{"prompt": "x"}, func(c context.Context, _ map[string]any) (any, error) {
		onLoop = tools.RelayHasRunningEventLoop(c)
		agent.TouchActivity("executing tool: image_generate")
		return map[string]any{"ok": true}, nil
	})
	if !onLoop {
		t.Fatal("the tool callback did not run on the Relay loop")
	}
	assertPersistedOffLoop(t, records, store, owner, "session.activity")
}

func TestLaterTurnWritesStillLandAfterAnOnLoopHeartbeat(t *testing.T) {
	ctx := activityRelayTurn(t)
	agent, store, owner := activityWorkerAgent(t)
	records := captureLogger(t, "agent.runtime_session_store", slog.LevelWarn)
	if _, _, err := tools.RelayExecuteTool(ctx, "terminal", map[string]any{"command": "true"}, func(context.Context, map[string]any) (any, error) {
		agent.TouchActivity("executing tool: terminal")
		return map[string]any{"ok": true}, nil
	}, activityOffLoopSession, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearSessionActivityLabels(activityOffLoopSession); err != nil {
		t.Fatal(err)
	}
	assertPersistedOffLoop(t, records, store, owner, "session.activity", "session.activity_clear")
}

func TestLabelResetOnARunningLoopPersistsOffLoop(t *testing.T) {
	agent, store, owner := activityWorkerAgent(t)
	records := captureLogger(t, "agent.runtime_session_store", slog.LevelWarn)
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.resetActivityLabelsAfterTurn()
	}()
	<-done
	assertPersistedOffLoop(t, records, store, owner, "session.activity_clear")
}

// Error a managed tool callback returns when it does not run on the Relay loop
func requireRelayLoop(c context.Context) error {
	if !tools.RelayHasRunningEventLoop(c) {
		return errors.New("the tool callback did not run on the Relay loop")
	}
	return nil
}

func TestSyncAuxUsageInsideManagedToolPersistsOffRelayLoop(t *testing.T) {
	ctx := activityRelayTurn(t)
	_, store, owner := activityWorkerAgent(t)
	records := captureLogger(t, "agent.runtime_session_store", slog.LevelWarn)
	response := map[string]any{"model": "m", "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3},
		"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}}}
	tool := func(c context.Context, _ map[string]any) (any, error) {
		if err := requireRelayLoop(c); err != nil {
			return nil, err
		}
		provider.RecordAuxUsage(c, response, "vision", "prov", "")
		return map[string]any{"ok": true}, nil
	}
	accounted := provider.SetAccountingContext(ctx, auxUsageRecorderFor(store), activityOffLoopSession)
	if _, _, err := tools.RelayExecuteTool(accounted, "vision_analyze", map[string]any{"image_url": "x"}, tool, activityOffLoopSession, "", nil); err != nil {
		t.Fatal(err)
	}
	assertPersistedOffLoop(t, records, store, owner, "usage.auxiliary")
}

func TestBeginToolExecutionInsideRelayToolPersistsOffRelayLoop(t *testing.T) {
	ctx := activityRelayTurn(t)
	agent, store, owner := activityWorkerAgent(t)
	records := captureLogger(t, "agent.runtime_session_store", slog.LevelWarn)
	agent.quietMode, agent.toolProgressCallback, agent.toolStartCallback, agent.checkpointMgr = true, nil, nil, nil
	ref := &tools.ToolCallRef{Name: "image_generate", Args: map[string]any{"prompt": "x"}, TaskID: "task-1", CallID: "call-1", Trace: []map[string]any{}}
	activityToolOnPool(t, ctx, "image_generate", map[string]any{"prompt": "x"}, func(c context.Context, _ map[string]any) (any, error) {
		if err := requireRelayLoop(c); err != nil {
			return nil, err
		}
		agent.toolExecutor().BeginToolExecution(ref, 1)
		return map[string]any{"ok": true}, nil
	})
	assertPersistedOffLoop(t, records, store, owner, "session.activity")
}

func TestAsyncDelegationLedgerWritesInsideRelayToolPersistOffRelayLoop(t *testing.T) {
	ctx := activityRelayTurn(t)
	_, store, owner := activityWorkerAgent(t)
	records := captureLogger(t, "agent.runtime_session_store", slog.LevelWarn)
	ledger := &tools.WorkerDelegationLedger{Store: store}
	prev := tools.WorkerLedger
	tools.WorkerLedger = func() (*tools.WorkerDelegationLedger, error) { return ledger, nil }
	t.Cleanup(func() { tools.WorkerLedger = prev })
	tool := func(c context.Context, _ map[string]any) (any, error) {
		if err := requireRelayLoop(c); err != nil {
			return nil, err
		}
		if err := tools.PersistDispatch(nil, map[string]any{"delegation_id": "d-1", "parent_session_id": activityOffLoopSession,
			"goal": "g", "dispatched_at": 1.0}); err != nil {
			return nil, err
		}
		if _, err := ledger.Unscheduled("d-1"); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	if _, _, err := tools.RelayExecuteTool(ctx, "cronjob", map[string]any{"action": "run"}, tool, activityOffLoopSession, "", nil); err != nil {
		t.Fatal(err)
	}
	assertPersistedOffLoop(t, records, store, owner, "delegation.dispatch", "delegation.unscheduled")
}
