package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

type relayFakeHandle struct {
	name string
	seq  int
}

type relayFakePush struct {
	name     string
	metadata map[string]any
	parent   any
	input    map[string]any
	seq      int
}

// Scriptable stand in for the native binding recording every push, pop and flush
type relayFake struct {
	mu         sync.Mutex
	seq        int
	pushes     []relayFakePush
	pops       []*relayFakeHandle
	flushed    int
	stack      []*relayFakeHandle
	strict     bool
	wedgePop   *pythread.Event
	wedgeFlush *pythread.Event
	relay      *NemoRelay
}

func newRelayFake(wedgePop, wedgeFlush *pythread.Event) *relayFake {
	f := &relayFake{wedgePop: wedgePop, wedgeFlush: wedgeFlush}
	f.relay = &NemoRelay{
		Scope:         &NemoRelayScope{Push: f.push, Pop: f.pop},
		ScopeType:     NemoRelayScopeTypes{Agent: "agent", Function: "function"},
		GetScopeStack: func(context.Context) (any, error) { return nil, nil },
		Subscribers:   &NemoRelaySubscribers{FlushAsync: f.flush},
	}
	return f
}

func (f *relayFake) push(_ context.Context, name string, _ any, kwargs map[string]any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	metadata, _ := kwargs["metadata"].(map[string]any)
	input, _ := kwargs["input"].(map[string]any)
	cp := map[string]any{}
	for k, v := range metadata {
		cp[k] = v
	}
	in := map[string]any{}
	for k, v := range input {
		in[k] = v
	}
	f.pushes = append(f.pushes, relayFakePush{name: name, metadata: cp, parent: kwargs["handle"], input: in, seq: f.seq})
	h := &relayFakeHandle{name: name, seq: f.seq}
	f.stack = append(f.stack, h)
	return h, nil
}

func (f *relayFake) pop(_ context.Context, handle any, _ map[string]any) (any, error) {
	if f.wedgePop != nil {
		f.wedgePop.Wait(-1)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h := handle.(*relayFakeHandle)
	if f.strict {
		if len(f.stack) == 0 || f.stack[len(f.stack)-1] != h {
			return nil, &RelayError{Class: "RuntimeError", Message: "scope handle is not at the top of the stack"}
		}
	}
	for i := len(f.stack) - 1; i >= 0; i-- {
		if f.stack[i] == h {
			f.stack = append(f.stack[:i], f.stack[i+1:]...)
			break
		}
	}
	f.pops = append(f.pops, h)
	return nil, nil
}

func (f *relayFake) flush(context.Context) (any, error) {
	if f.wedgeFlush != nil {
		f.wedgeFlush.Wait(-1)
	}
	f.mu.Lock()
	f.flushed++
	f.mu.Unlock()
	return nil, nil
}

func (f *relayFake) sessionPushes() []relayFakePush {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []relayFakePush
	for _, p := range f.pushes {
		if p.name == RelaySessionScope {
			out = append(out, p)
		}
	}
	return out
}

func (f *relayFake) snapshot() ([]relayFakePush, []*relayFakeHandle, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relayFakePush(nil), f.pushes...), append([]*relayFakeHandle(nil), f.pops...), f.flushed
}

type relayStubRegistry struct{ host RelayHost }

func (r relayStubRegistry) ForProfile(context.Context, string, bool) RelayHost { return r.host }

type relayLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *relayLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *relayLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Fast scope timeout, empty segments config and a captured log for one relay test
func relayTestEnv(t *testing.T, segments map[string]any) *relayLogSink {
	t.Helper()
	timeout, loader, logger := RelayScopeOpTimeout, relayLoadUserConfigEffective, relayLogger
	sink := &relayLogSink{}
	RelayScopeOpTimeout = 1.0
	relayLoadUserConfigEffective = func(context.Context) (map[string]any, error) {
		if segments == nil {
			return map[string]any{}, nil
		}
		return map[string]any{"gateway": map[string]any{"telemetry": map[string]any{"session_segments": segments}}}, nil
	}
	ResetRelaySegmentsConfigForTests()
	relayLogger = func() *slog.Logger {
		return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})).With("logger", "agent.relay_runtime")
	}
	t.Cleanup(func() {
		RelayScopeOpTimeout, relayLoadUserConfigEffective, relayLogger = timeout, loader, logger
		ResetRelaySegmentsConfigForTests()
	})
	return sink
}

// Runtime over the fake that teardown unwedges and shuts down
func relayTestRuntime(t *testing.T, f *relayFake) *RelayRuntime {
	t.Helper()
	rt, err := NewRelayRuntime(context.Background(), f.relay, "/tmp/test-profile")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, ev := range []*pythread.Event{f.wedgePop, f.wedgeFlush} {
			if ev != nil {
				ev.Set()
			}
		}
		rt.Shutdown()
	})
	return rt
}

func relayAcquire(c *RelaySessionCoordinator, rt *RelayRuntime, sessionID string, sessionCwd, turnCwd *string) *ConversationLease {
	c.Registry = relayStubRegistry{host: rt}
	c.prepareSession = func(context.Context, *RelayRuntime, map[string]any) {}
	return c.AcquireConversation(context.Background(), RelayConversation{ProfileKey: rt.ProfileKey, SessionID: sessionID,
		Platform: "test", SessionCwd: sessionCwd, TurnCwd: turnCwd})
}

func relayRunTurn(t *testing.T, c *RelaySessionCoordinator, ctx context.Context, lease *ConversationLease, turnID string) *RelayTurnContext {
	t.Helper()
	turn, err := c.BeginTurn(ctx, lease, turnID, "task-"+turnID, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.EndTurn(ctx, turn, "success")
	return turn
}

// Whether fn returns within timeout seconds
func relayReturnsWithin(fn func(), timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func TestPopRelayScopeOmitsUnsupportedMetadataKwarg(t *testing.T) {
	var calls []map[string]any
	relay := &NemoRelay{Scope: &NemoRelayScope{
		Pop: func(_ context.Context, handle any, kwargs map[string]any) (any, error) {
			calls = append(calls, map[string]any{"handle": handle, "kwargs": kwargs})
			return nil, nil
		},
		PopParameters: func() ([]RelayParameter, error) { return []RelayParameter{{Name: "handle"}, {Name: "output"}}, nil },
	}}
	handle := []any{"scope", "icebath.turn", 1}
	if _, err := PopRelayScope(context.Background(), relay, handle, map[string]any{"outcome": "success"},
		map[string]any{RelayRuntimeSchemaKey: RelayRuntimeSchemaVersion}, nil); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"handle": handle, "kwargs": map[string]any{"output": map[string]any{"outcome": "success"}}}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
}

func TestPopRelayScopeForwardsMetadataWhenSupported(t *testing.T) {
	var calls []map[string]any
	relay := &NemoRelay{Scope: &NemoRelayScope{
		Pop: func(_ context.Context, handle any, kwargs map[string]any) (any, error) {
			call := map[string]any{"output": kwargs["output"], "metadata": kwargs["metadata"], "timestamp": kwargs["timestamp"]}
			calls = append(calls, map[string]any{"handle": handle, "kwargs": call})
			return nil, nil
		},
		PopParameters: func() ([]RelayParameter, error) {
			return []RelayParameter{{Name: "handle"}, {Name: "output"}, {Name: "metadata"}, {Name: "timestamp"}}, nil
		},
	}}
	handle := []any{"scope", "icebath.turn", 2}
	metadata := map[string]any{RelayRuntimeInstanceKey: "abc"}
	if _, err := PopRelayScope(context.Background(), relay, handle, map[string]any{"outcome": "error"}, metadata, nil); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"handle": handle, "kwargs": map[string]any{"output": map[string]any{"outcome": "error"}, "metadata": metadata, "timestamp": nil}}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
}

func TestEndTurnFinalizationSurvivesPopWithoutMetadata(t *testing.T) {
	sink := relayTestEnv(t, nil)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	f := newRelayFake(nil, nil)
	f.strict = true
	original := f.relay.Scope.Pop
	f.relay.Scope.PopParameters = func() ([]RelayParameter, error) {
		return []RelayParameter{{Name: "handle"}, {Name: "output"}, {Name: "metadata"}, {Name: "timestamp"}}, nil
	}
	f.relay.Scope.GetHandle = func(context.Context) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.stack) == 0 {
			return nil, nil
		}
		return f.stack[len(f.stack)-1], nil
	}
	loader := loadNemoRelay
	loadNemoRelay = func() (*NemoRelay, error) { return f.relay, nil }
	ResetRelayRuntimeForTests()
	t.Cleanup(func() {
		ResetRelayRuntimeForTests()
		loadNemoRelay = loader
	})
	ctx := context.Background()
	lease := RelayCoordinator.AcquireConversation(ctx, RelayConversation{ProfileKey: CurrentRelayProfileKey(ctx), SessionID: "session-78993", Platform: "cli"})
	turn, err := RelayCoordinator.BeginTurn(ctx, lease, "turn-1", "task-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Host.RetainManagedExecution("test.relay_scope_pop"); err != nil {
		t.Fatal(err)
	}
	host := lease.LiveRuntime()
	if host == nil || turn.Handle == nil {
		t.Fatalf("no live runtime %v %v", host, turn.Handle)
	}
	f.relay.Scope.PopParameters = func() ([]RelayParameter, error) {
		return []RelayParameter{{Name: "handle"}, {Name: "output"}, {Name: "timestamp"}}, nil
	}
	f.relay.Scope.Pop = func(ctx context.Context, handle any, kwargs map[string]any) (any, error) {
		if _, ok := kwargs["metadata"]; ok {
			return nil, errors.New("pop_without_metadata() got an unexpected keyword argument 'metadata'")
		}
		return original(ctx, handle, kwargs)
	}
	logical, err := host.RunInSession(ctx, lease.Session, func(c context.Context) (any, error) {
		return f.relay.Scope.Push(c, "logical-llm", "custom", map[string]any{"handle": turn.Handle, "input": map[string]any{}, "metadata": map[string]any{"icebath.test": true}})
	}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn.LogicalLLMCalls.Set("api-1", logical)
	RelayCoordinator.EndTurn(ctx, turn, "success")
	joined := sink.String()
	for _, bad := range []string{"unexpected keyword argument 'metadata'", "turn finalization failed", "logical LLM finalization failed"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("log mentions %q\n%s", bad, joined)
		}
	}
	if turn.LogicalLLMCalls.Len() != 0 || !turn.Closed() {
		t.Fatalf("logical calls %v closed %v", turn.LogicalLLMCalls.Keys(), turn.Closed())
	}
	lease.Host.ReleaseManagedExecution("test.relay_scope_pop")
	RelayCoordinator.ReleaseConversation(lease)
}

func TestEndTurnReturnsWhenNativePopWedges(t *testing.T) {
	relayTestEnv(t, nil)
	c := NewRelaySessionCoordinator(RelayHosts)
	rt := relayTestRuntime(t, newRelayFake(&pythread.Event{}, nil))
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	if lease.Session == nil {
		t.Fatal("fake session must initialize")
	}
	turn, err := c.BeginTurn(context.Background(), lease, "t1", "task1", nil)
	if err != nil || turn.Handle == nil {
		t.Fatalf("turn scope must push on fake relay %v", err)
	}
	if !relayReturnsWithin(func() { c.EndTurn(context.Background(), turn, "success") }, 5*time.Second) {
		t.Fatal("end_turn must return even when the native scope.pop wedges")
	}
}

func TestCloseSessionReturnsWhenNativePopWedges(t *testing.T) {
	relayTestEnv(t, nil)
	c := NewRelaySessionCoordinator(RelayHosts)
	rt := relayTestRuntime(t, newRelayFake(&pythread.Event{}, nil))
	if relayAcquire(c, rt, "sess-1", nil, nil).Session == nil {
		t.Fatal("fake session must initialize")
	}
	if !relayReturnsWithin(func() { rt.CloseSession(context.Background(), map[string]any{"session_id": "sess-1"}) }, 5*time.Second) {
		t.Fatal("close_session must return even when the native scope.pop wedges")
	}
}

func TestCloseSessionReturnsWhenSubscriberFlushWedges(t *testing.T) {
	relayTestEnv(t, nil)
	c := NewRelaySessionCoordinator(RelayHosts)
	rt := relayTestRuntime(t, newRelayFake(nil, &pythread.Event{}))
	if relayAcquire(c, rt, "sess-1", nil, nil).Session == nil {
		t.Fatal("fake session must initialize")
	}
	if !relayReturnsWithin(func() { rt.CloseSession(context.Background(), map[string]any{"session_id": "sess-1"}) }, 5*time.Second) {
		t.Fatal("close_session must return even when subscribers.flush wedges")
	}
}

func TestFinishLogicalCallsReturnsAndRetainsPrefix(t *testing.T) {
	relayTestEnv(t, nil)
	c := NewRelaySessionCoordinator(RelayHosts)
	rt := relayTestRuntime(t, newRelayFake(&pythread.Event{}, nil))
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	turn, err := c.BeginTurn(context.Background(), lease, "t1", "task1", nil)
	if err != nil {
		t.Fatal(err)
	}
	turn.LogicalLLMMu.Lock()
	turn.LogicalLLMCalls.Set("req-1", &relayFakeHandle{name: "llm-1"})
	turn.LogicalLLMCalls.Set("req-2", &relayFakeHandle{name: "llm-2"})
	turn.LogicalLLMMu.Unlock()
	if !relayReturnsWithin(func() { c.FinishLogicalCalls(context.Background(), turn, "success") }, 5*time.Second) {
		t.Fatal("finish_logical_calls must return even when native pops wedge")
	}
	turn.LogicalLLMMu.Lock()
	defer turn.LogicalLLMMu.Unlock()
	if turn.LogicalLLMCalls.Len() == 0 {
		t.Fatal("wedged logical scopes must be retained for diagnostics, not silently dropped")
	}
}

func TestFullTurnLifecycleHealthy(t *testing.T) {
	relayTestEnv(t, nil)
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	ctx := context.Background()
	turn, err := c.BeginTurn(ctx, lease, "t1", "task1", nil)
	if err != nil || turn.Handle == nil {
		t.Fatalf("turn %v", err)
	}
	c.FinishLogicalCalls(ctx, turn, "success")
	c.EndTurn(ctx, turn, "success")
	c.ReleaseConversation(lease)
	rt.CloseSession(ctx, map[string]any{"session_id": "sess-1"})
	pushes, pops, flushed := f.snapshot()
	turnPushes, turnPopped := 0, false
	for _, p := range pushes {
		if p.name == RelayTurnScope {
			turnPushes++
		}
	}
	for _, p := range pops {
		turnPopped = turnPopped || p.name == RelayTurnScope
	}
	if turnPushes != 1 || !turnPopped || flushed != 0 {
		t.Fatalf("turn pushes %d popped %v flushed %d", turnPushes, turnPopped, flushed)
	}
}

func TestPushRuntimeErrorDoesNotDoublePush(t *testing.T) {
	relayTestEnv(t, nil)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	original := f.relay.Scope.Push
	calls := 0
	f.relay.Scope.Push = func(context.Context, string, any, map[string]any) (any, error) {
		calls++
		return nil, &RelayError{Class: "RuntimeError", Message: "scope push failed"}
	}
	ctx := context.Background()
	if _, err := rt.EnsureSession(ctx, map[string]any{"session_id": "sess-rt"}, RelaySessionOptions{}); err == nil || err.Error() != "scope push failed" {
		t.Fatalf("err %v", err)
	}
	session, _ := rt.sessions.Get("sess-rt")
	if calls != 1 || session.Handle != nil || session.Context != nil {
		t.Fatalf("calls %d handle %v context %v", calls, session.Handle, session.Context)
	}
	f.relay.Scope.Push = original
	if _, err := rt.EnsureSession(ctx, map[string]any{"session_id": "sess-rt"}, RelaySessionOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.sessionPushes()) != 1 || session.CurrentHandle() == nil {
		t.Fatalf("pushes %d", len(f.sessionPushes()))
	}
}

type relayRefusingExecutor struct{}

func (relayRefusingExecutor) Submit(context.Context, func(context.Context) (any, error)) (relayFuture, error) {
	return nil, ErrPoolShutdown
}

func TestExecutorRefusalStillUsesSyncFallback(t *testing.T) {
	relayTestEnv(t, nil)
	executor := relayScopeOpExecutor
	relayScopeOpExecutor = func() relayExecutor { return relayRefusingExecutor{} }
	t.Cleanup(func() { relayScopeOpExecutor = executor })
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	if _, err := rt.EnsureSession(context.Background(), map[string]any{"session_id": "sess-ref"}, RelaySessionOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.sessionPushes()) != 1 {
		t.Fatalf("pushes %d", len(f.sessionPushes()))
	}
}

func TestTurnMetadataIncludesRequestMetadataWithoutOverridingRuntimeFields(t *testing.T) {
	relayTestEnv(t, nil)
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	ctx := context.Background()
	turn, err := c.BeginTurn(ctx, lease, "t1", "task1", map[string]any{"request_id": "req-123",
		"context": map[string]any{"tenant": "example"}, RelayRuntimeInstanceKey: "caller-supplied"})
	if err != nil {
		t.Fatal(err)
	}
	pushes, _, _ := f.snapshot()
	var md map[string]any
	for _, p := range pushes {
		if p.name == RelayTurnScope {
			md = p.metadata
		}
	}
	if md["request_id"] != "req-123" || !reflect.DeepEqual(md["context"], map[string]any{"tenant": "example"}) || md[RelayRuntimeInstanceKey] != rt.RuntimeID {
		t.Fatalf("metadata %v", md)
	}
	c.EndTurn(ctx, turn, "success")
}

func TestDefaultsNeverRotateAcrossManyTurnsAndCompactions(t *testing.T) {
	relayTestEnv(t, nil)
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	ctx := context.Background()
	if lease.Session == nil {
		t.Fatal("no session")
	}
	c.NotifySessionCompacted(ctx, rt.ProfileKey, "sess-1", "")
	for i := range 5 {
		relayRunTurn(t, c, ctx, lease, "t"+strconv.Itoa(i))
	}
	if n := len(f.sessionPushes()); n != 1 {
		t.Fatalf("defaults off must never rotate the session scope, %d pushes", n)
	}
}

func TestDistinctSessionAndTurnCwdsSurviveSegmentRotation(t *testing.T) {
	relayTestEnv(t, nil)
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	ctx := context.Background()
	str := func(s string) *string { return &s }
	check := func(session, turn map[string]any) {
		t.Helper()
		pushes, _, _ := f.snapshot()
		sp := f.sessionPushes()
		if !reflect.DeepEqual(sp[len(sp)-1].input, session) || !reflect.DeepEqual(pushes[len(pushes)-1].input, turn) {
			t.Fatalf("session input %v turn input %v", sp[len(sp)-1].input, pushes[len(pushes)-1].input)
		}
	}
	lease := relayAcquire(c, rt, "sess-1", str("/workspace/session"), str("/workspace/task"))
	turn, _ := c.BeginTurn(ctx, lease, "t1", "task1", nil)
	check(map[string]any{"cwd": "/workspace/session"}, map[string]any{"cwd": "/workspace/task"})
	c.EndTurn(ctx, turn, "success")
	lease = relayAcquire(c, rt, "sess-1", str("/workspace/moved"), str("/workspace/next-task"))
	rt.RotateSessionScope(ctx, lease.Session, "compaction")
	turn, _ = c.BeginTurn(ctx, lease, "t2", "task2", nil)
	check(map[string]any{"cwd": "/workspace/moved"}, map[string]any{"cwd": "/workspace/next-task"})
	c.EndTurn(ctx, turn, "success")
	lease = relayAcquire(c, rt, "sess-1", str(""), str(""))
	rt.RotateSessionScope(ctx, lease.Session, "compaction")
	turn, _ = c.BeginTurn(ctx, lease, "t3", "task3", nil)
	check(map[string]any{}, map[string]any{})
	c.EndTurn(ctx, turn, "success")
}

func TestCompactionRotatesAtNextBeginTurnNotImmediately(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": true, "max_turns": 0})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	ctx := context.Background()
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	original := lease.Session.CurrentHandle()
	c.NotifySessionCompacted(ctx, rt.ProfileKey, "sess-1", "")
	if _, pops, _ := f.snapshot(); len(f.sessionPushes()) != 1 || len(pops) != 0 {
		t.Fatal("compaction only flags, the scope stack stays untouched")
	}
	turn, _ := c.BeginTurn(ctx, lease, "t1", "task1", nil)
	pushes, pops, _ := f.snapshot()
	if len(f.sessionPushes()) != 2 {
		t.Fatal("rotation must happen at the next begin_turn")
	}
	oldPopped := false
	for _, p := range pops {
		oldPopped = oldPopped || p.seq == 1
	}
	if !oldPopped || lease.Session.CurrentHandle() == original {
		t.Fatalf("old segment popped %v", oldPopped)
	}
	var turnPush relayFakePush
	for _, p := range pushes {
		if p.name == RelayTurnScope {
			turnPush = p
		}
	}
	if turnPush.parent != lease.Session.CurrentHandle() {
		t.Fatal("the turn scope parents to the new segment handle")
	}
	c.EndTurn(ctx, turn, "success")
}

func TestSegmentMetadataOnRotatedScope(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": true, "max_turns": 0})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	ctx := context.Background()
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	c.NotifySessionCompacted(ctx, rt.ProfileKey, "sess-1", "")
	relayRunTurn(t, c, ctx, lease, "t1")
	sp := f.sessionPushes()
	md := sp[len(sp)-1].metadata
	if md["icebath.session.segment"] != 1 || md["icebath.session.segment_reason"] != "compaction" {
		t.Fatalf("metadata %v", md)
	}
}

func TestUnknownSessionCompactionIsNoop(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": true, "max_turns": 0})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	relayAcquire(c, rt, "sess-1", nil, nil)
	c.NotifySessionCompacted(context.Background(), rt.ProfileKey, "never-seen", "")
	if len(f.sessionPushes()) != 1 {
		t.Fatal("unknown session compaction rotated a scope")
	}
}

func TestRotatingCompactionClosesOldSessionScope(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": true, "max_turns": 0})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	relayAcquire(c, rt, "parent-1", nil, nil)
	if _, pops, _ := f.snapshot(); len(pops) != 0 {
		t.Fatal("pops before compaction")
	}
	c.NotifySessionCompacted(context.Background(), rt.ProfileKey, "child-1", "parent-1")
	if _, pops, flushed := f.snapshot(); len(pops) != 1 || flushed != 0 {
		t.Fatalf("rotating compaction must close the old session scope without flushing, pops %d flushed %d", len(pops), flushed)
	}
}

func TestRotatingCompactionMidTurnDefersCloseToEndTurn(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": true, "max_turns": 0})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	ctx := context.Background()
	lease := relayAcquire(c, rt, "parent-1", nil, nil)
	turn, _ := c.BeginTurn(ctx, lease, "t1", "task1", nil)
	c.NotifySessionCompacted(ctx, rt.ProfileKey, "child-1", "parent-1")
	if _, pops, _ := f.snapshot(); len(pops) != 0 {
		t.Fatal("old-session close must defer while its turn is live")
	}
	c.EndTurn(ctx, turn, "success")
	_, pops, _ := f.snapshot()
	if len(pops) != 2 || pops[0].name != RelayTurnScope || pops[1].name != RelaySessionScope || rt.GetSession("parent-1") != nil {
		t.Fatalf("end_turn must consume the deferred close turn first, pops %v", pops)
	}
}

func TestRotatingCompactionNoopWhenDisabled(t *testing.T) {
	relayTestEnv(t, nil)
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	relayAcquire(c, rt, "parent-1", nil, nil)
	c.NotifySessionCompacted(context.Background(), rt.ProfileKey, "child-1", "parent-1")
	if _, pops, _ := f.snapshot(); len(pops) != 0 {
		t.Fatal("defaults off: rotating compaction is a no-op")
	}
}

func TestMaxTurnsRotatesAfterCap(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": false, "max_turns": 2})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	ctx := context.Background()
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	for i := range 5 {
		relayRunTurn(t, c, ctx, lease, "t"+strconv.Itoa(i))
	}
	sp := f.sessionPushes()
	if len(sp) != 3 || sp[len(sp)-1].metadata["icebath.session.segment_reason"] != "max_turns" {
		t.Fatalf("cap of 2 over 5 turns gives 2 rotations, %d pushes", len(sp))
	}
}

func TestZeroCapMeansUnlimited(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": false, "max_turns": 0})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	ctx := context.Background()
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	for i := range 4 {
		relayRunTurn(t, c, ctx, lease, "t"+strconv.Itoa(i))
	}
	if len(f.sessionPushes()) != 1 {
		t.Fatal("zero cap rotated")
	}
}

func TestNeverRotatesMidTurn(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": true, "max_turns": 0})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	ctx := context.Background()
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	turn, _ := c.BeginTurn(ctx, lease, "t1", "task1", nil)
	c.NotifySessionCompacted(ctx, rt.ProfileKey, "sess-1", "")
	if len(f.sessionPushes()) != 1 {
		t.Fatal("no rotation while a turn is live")
	}
	c.EndTurn(ctx, turn, "success")
	if len(f.sessionPushes()) != 1 {
		t.Fatal("end_turn does not rotate either")
	}
	turn2, _ := c.BeginTurn(ctx, lease, "t2", "task2", nil)
	if len(f.sessionPushes()) != 2 {
		t.Fatal("the next begin_turn rotates")
	}
	c.EndTurn(ctx, turn2, "success")
}

func TestWedgedRotationIsBoundedAndAgentContinues(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": true, "max_turns": 0})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(&pythread.Event{}, nil)
	rt := relayTestRuntime(t, f)
	ctx := context.Background()
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	c.NotifySessionCompacted(ctx, rt.ProfileKey, "sess-1", "")
	var turn *RelayTurnContext
	if !relayReturnsWithin(func() { turn, _ = c.BeginTurn(ctx, lease, "t1", "task1", nil) }, 5*time.Second) {
		t.Fatal("begin_turn must return even when the rotation pop wedges")
	}
	c.EndTurn(ctx, turn, "success")
}

func TestSubagentChildrenParentToNewSegmentAfterRotation(t *testing.T) {
	relayTestEnv(t, map[string]any{"on_compaction": true, "max_turns": 0})
	c := NewRelaySessionCoordinator(RelayHosts)
	f := newRelayFake(nil, nil)
	rt := relayTestRuntime(t, f)
	ctx := context.Background()
	lease := relayAcquire(c, rt, "sess-1", nil, nil)
	c.NotifySessionCompacted(ctx, rt.ProfileKey, "sess-1", "")
	relayRunTurn(t, c, ctx, lease, "t1")
	newHandle := lease.Session.CurrentHandle()
	child, err := rt.RegisterSubagent(ctx, map[string]any{"parent_session_id": "sess-1", "child_session_id": "child-1"}, RelaySessionOptions{})
	if err != nil || child == nil {
		t.Fatalf("child %v %v", child, err)
	}
	var childPush relayFakePush
	for _, p := range f.sessionPushes() {
		if p.parent != nil {
			childPush = p
		}
	}
	if childPush.parent != newHandle {
		t.Fatal("post-rotation children must parent to the new segment handle")
	}
}

func TestRelayRuntimeNeverImportsGatewayRun(t *testing.T) {
	for _, v := range []string{"TERMINAL_CWD", "ICEBATH_QUIET", "_ICEBATH_GATEWAY"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	ResetRelaySegmentsConfigForTests()
	t.Cleanup(ResetRelaySegmentsConfigForTests)
	relaySegmentsConfigLazy.get(context.Background())
	relaySegmentsConfigLazy.get(context.Background())
	leaked := map[string]string{}
	for _, v := range []string{"TERMINAL_CWD", "ICEBATH_QUIET", "_ICEBATH_GATEWAY"} {
		if val, ok := os.LookupEnv(v); ok {
			leaked[v] = val
		}
	}
	fset := token.NewFileSet()
	for _, name := range []string{"relay_runtime.go", "relay_coordinator.go", "relay_plugin_cutover.go"} {
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if strings.Contains(imp.Path.Value, "/pkg/gateway") {
				t.Fatalf("%s imports %s", name, imp.Path.Value)
			}
		}
	}
	if len(leaked) > 0 {
		t.Fatal(fmt.Sprint("leaked env: ", leaked))
	}
}
