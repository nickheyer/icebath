package agent

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
)

type relayCompactionHandle struct{ name string }

// Binding double recording every scope event the compaction marks emit
type relayCompactionFake struct {
	mu     sync.Mutex
	events []map[string]any
	event  func(ctx context.Context, name string, kwargs map[string]any) (any, error)
	relay  *tools.NemoRelay
}

func newRelayCompactionFake() *relayCompactionFake {
	f := &relayCompactionFake{}
	f.event = func(_ context.Context, name string, kwargs map[string]any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		record := map[string]any{"name": name}
		for k, v := range kwargs {
			record[k] = v
		}
		f.events = append(f.events, record)
		return nil, nil
	}
	f.relay = &tools.NemoRelay{
		Scope: &tools.NemoRelayScope{
			Push: func(_ context.Context, name string, _ any, _ map[string]any) (any, error) {
				return &relayCompactionHandle{name: name}, nil
			},
			Pop: func(context.Context, any, map[string]any) (any, error) { return nil, nil },
			Event: func(ctx context.Context, name string, kwargs map[string]any) (any, error) {
				return f.event(ctx, name, kwargs)
			},
		},
		ScopeType:     tools.NemoRelayScopeTypes{Agent: "agent", Function: "function"},
		GetScopeStack: func(context.Context) (any, error) { return nil, nil },
		Subscribers:   &tools.NemoRelaySubscribers{FlushAsync: func(context.Context) (any, error) { return nil, nil }},
	}
	return f
}

func (f *relayCompactionFake) recorded() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.events...)
}

// The relay fixture, a host for the active profile reachable through the host registry
func relayCompactionHost(t *testing.T) (*relayCompactionFake, context.Context, *tools.RelayRuntime) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "profile"))
	f := newRelayCompactionFake()
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) { return f.relay, nil })
	tools.ResetRelayRuntimeForTests()
	relayCompactionStalled.Store(false)
	ctx := pyctx.With(context.Background(), pyctx.New())
	host := tools.GetRelayRuntime(ctx, true, tools.CurrentRelayProfileKey(ctx))
	if host == nil {
		t.Fatal("no runtime")
	}
	t.Cleanup(func() {
		relayCompactionStalled.Store(false)
		tools.ResetRelayRuntimeForTests()
		tools.RegisterNemoRelayBinding(nil)
	})
	return f, ctx, host
}

func relayCompactionAcquire(ctx context.Context, host *tools.RelayRuntime, sessionID, platform string) *tools.ConversationLease {
	return tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: host.ProfileKey, SessionID: sessionID, Platform: platform})
}

func relayCompactionBegin(t *testing.T, ctx context.Context, lease *tools.ConversationLease, turnID string) *tools.RelayTurnContext {
	t.Helper()
	turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, turnID, "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func relayCompactionCommitted(sessionID string) map[string]any {
	return CompactionAttemptPayload(map[string]any{"session_id": sessionID, "commit_status": "committed", "route": "icebath", "trigger_source": "pre_api"}, nil)
}

func relayCompactionEmit(t *testing.T, ctx context.Context, sessionID, name string, data map[string]any, turnSessionID string) bool {
	t.Helper()
	ok, err := EmitCompactionMark(ctx, sessionID, name, data, turnSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestRelayCompactionMarkInsideATurnIsParentedToThatTurn(t *testing.T) {
	f, ctx, host := relayCompactionHost(t)
	lease := relayCompactionAcquire(ctx, host, "s1", "cli")
	turn := relayCompactionBegin(t, ctx, lease, "t1")
	payload := relayCompactionCommitted("s1")
	ok := relayCompactionEmit(t, ctx, "s1", CompactionEventName(payload), payload, "")
	tools.RelayCoordinator.EndTurn(ctx, turn, "success")
	if !ok {
		t.Fatal("mark not emitted inside the turn")
	}
	events := f.recorded()
	if len(events) != 1 {
		t.Fatalf("events %v", events)
	}
	event := events[0]
	if event["name"] != "compaction" || !tools.RelayIdentical(event["handle"], turn.Handle) || !reflect.DeepEqual(event["data"], payload) || !reflect.DeepEqual(event["data_schema"], CompactionDataSchema) {
		t.Fatalf("event %v", event)
	}
	if event["metadata"].(map[string]any)[tools.RelayRuntimeInstanceKey] != host.RuntimeID {
		t.Fatalf("metadata %v", event["metadata"])
	}
}

func TestRelayCompactionMarkOutsideATurnUsesTheSessionScope(t *testing.T) {
	f, ctx, host := relayCompactionHost(t)
	lease := relayCompactionAcquire(ctx, host, "hygiene", "telegram")
	payload := CompactionAttemptPayload(map[string]any{"session_id": "hygiene", "commit_status": "blocked"}, nil)
	if !relayCompactionEmit(t, ctx, "hygiene", CompactionEventName(payload), payload, "") {
		t.Fatal("mark not emitted on the session scope")
	}
	events := f.recorded()
	if len(events) != 1 || events[0]["name"] != "compaction.attempt" || !tools.RelayIdentical(events[0]["handle"], lease.Session.Handle) {
		t.Fatalf("events %v", events)
	}
}

func TestRelayCompactionMarkForAnotherSessionDoesNotLandOnTheCurrentTurn(t *testing.T) {
	f, ctx, host := relayCompactionHost(t)
	other := relayCompactionAcquire(ctx, host, "other", "cli")
	lease := relayCompactionAcquire(ctx, host, "s1", "cli")
	turn := relayCompactionBegin(t, ctx, lease, "t1")
	relayCompactionEmit(t, ctx, "other", "compaction", relayCompactionCommitted("other"), "")
	tools.RelayCoordinator.EndTurn(ctx, turn, "success")
	events := f.recorded()
	if len(events) != 1 || !tools.RelayIdentical(events[0]["handle"], other.Session.Handle) {
		t.Fatalf("events %v", events)
	}
}

func TestRelayCompactionNoRelaySessionSkipsQuietly(t *testing.T) {
	f, ctx, _ := relayCompactionHost(t)
	if relayCompactionEmit(t, ctx, "never-opened", "compaction", relayCompactionCommitted("never-opened"), "") ||
		relayCompactionEmit(t, ctx, "", "compaction", relayCompactionCommitted(""), "") {
		t.Fatal("mark emitted without a session")
	}
	if len(f.recorded()) != 0 {
		t.Fatalf("events %v", f.recorded())
	}
}

func TestRelayCompactionNoRelayHostSkipsQuietly(t *testing.T) {
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "profile"))
	tools.ResetRelayRuntimeForTests()
	t.Cleanup(tools.ResetRelayRuntimeForTests)
	ctx := pyctx.With(context.Background(), pyctx.New())
	if relayCompactionEmit(t, ctx, "s1", "compaction", relayCompactionCommitted("s1"), "") {
		t.Fatal("mark emitted without a host")
	}
}

func TestRelayCompactionStalledRelayEventDoesNotHoldTheCaller(t *testing.T) {
	f, ctx, host := relayCompactionHost(t)
	relayCompactionAcquire(ctx, host, "s1", "cli")
	release := make(chan struct{})
	f.event = func(context.Context, string, map[string]any) (any, error) {
		<-release
		return nil, nil
	}
	prev := tools.RelayScopeOpTimeout
	tools.RelayScopeOpTimeout = 2.0
	defer func() { tools.RelayScopeOpTimeout = prev }()
	defer close(release)
	finished := make(chan error, 1)
	go func() {
		_, err := EmitCompactionMark(ctx, "s1", "compaction", relayCompactionCommitted("s1"), "")
		finished <- err
	}()
	var err error
	select {
	case err = <-finished:
	case <-time.After(4 * time.Second):
		t.Fatal("stalled publish held the caller")
	}
	var te *tools.RelayTimeoutError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "Relay scope operation exceeded") {
		t.Fatalf("err = %v", err)
	}
	started := time.Now()
	if relayCompactionEmit(t, ctx, "s1", "compaction", relayCompactionCommitted("s1"), "") {
		t.Fatal("breaker did not trip")
	}
	if time.Since(started) >= 500*time.Millisecond {
		t.Fatal("tripped breaker still waited on Relay")
	}
}

func TestRelayCompactionATurnRelayDoesNotInstrumentRecordsNoMark(t *testing.T) {
	f, ctx, host := relayCompactionHost(t)
	lease := relayCompactionAcquire(ctx, host, "s1", "cli")
	live := relayCompactionBegin(t, ctx, lease, "live")
	fork := relayCompactionBegin(t, ctx, lease, "fork")
	enabled := fork.RelayEnabled()
	emitted := relayCompactionEmit(t, ctx, "s1", "compaction", relayCompactionCommitted("s1"), "")
	tools.RelayCoordinator.EndTurn(ctx, fork, "success")
	tools.RelayCoordinator.EndTurn(ctx, live, "success")
	if enabled || emitted || len(f.recorded()) != 0 {
		t.Fatalf("fork enabled=%v emitted=%v events=%v", enabled, emitted, f.recorded())
	}
}

func TestRelayCompactionATurnFallbackLandsOnlyOnTheTurnTheAgentStartedIn(t *testing.T) {
	f, ctx, host := relayCompactionHost(t)
	lease := relayCompactionAcquire(ctx, host, "s1", "cli")
	turn := relayCompactionBegin(t, ctx, lease, "t1")
	other := relayCompactionEmit(t, ctx, "child", "compaction", relayCompactionCommitted("child"), "other")
	own := relayCompactionEmit(t, ctx, "child", "compaction", relayCompactionCommitted("child"), "s1")
	tools.RelayCoordinator.EndTurn(ctx, turn, "success")
	if other || !own {
		t.Fatalf("fallback other=%v own=%v", other, own)
	}
	events := f.recorded()
	if len(events) != 1 || !tools.RelayIdentical(events[0]["handle"], turn.Handle) || events[0]["data"].(map[string]any)["session_id"] != "child" {
		t.Fatalf("events %v", events)
	}
}
