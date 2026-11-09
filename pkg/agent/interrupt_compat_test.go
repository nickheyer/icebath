package agent

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

type modernInterruptAgent struct{ calls [][3]string }

func (m *modernInterruptAgent) HardInterrupt(message, toolReason string) {
	m.calls = append(m.calls, [3]string{"hard", message, toolReason})
}

func (m *modernInterruptAgent) Interrupt(message string) {
	m.calls = append(m.calls, [3]string{"soft", message, ""})
}

type legacyInterruptAgent struct{ calls [][2]string }

func (l *legacyInterruptAgent) Interrupt(message string) {
	l.calls = append(l.calls, [2]string{"legacy", message})
}

func strp(s string) *string { return &s }

func TestExplicitProducerPrefersFeatureDetectedHardInterrupt(t *testing.T) {
	agent := &modernInterruptAgent{}
	if !RequestHardInterruptOf(agent, strp("stop now"), nil) {
		t.Fatal("modern agent reported unsupported")
	}
	if !reflect.DeepEqual(agent.calls, [][3]string{{"hard", "stop now", ""}}) {
		t.Fatalf("calls %v", agent.calls)
	}
}

func TestSafeToolReasonOnlyReachesSupportingModernAgent(t *testing.T) {
	modern, legacy := &modernInterruptAgent{}, &legacyInterruptAgent{}
	if !RequestHardInterruptOf(modern, strp("private diagnostic"), strp("fixed category")) ||
		!RequestHardInterruptOf(legacy, strp("private diagnostic"), strp("fixed category")) {
		t.Fatal("producer reported unsupported")
	}
	if !reflect.DeepEqual(modern.calls, [][3]string{{"hard", "private diagnostic", "fixed category"}}) {
		t.Fatalf("modern calls %v", modern.calls)
	}
	if !reflect.DeepEqual(legacy.calls, [][2]string{{"legacy", "private diagnostic"}}) {
		t.Fatalf("legacy calls %v", legacy.calls)
	}
}

func TestExplicitProducerFallsBackToOldInterruptSignature(t *testing.T) {
	agent := &legacyInterruptAgent{}
	if !RequestHardInterruptOf(agent, strp("stop now"), nil) {
		t.Fatal("legacy agent reported unsupported")
	}
	if !reflect.DeepEqual(agent.calls, [][2]string{{"legacy", "stop now"}}) {
		t.Fatalf("calls %v", agent.calls)
	}
}

func TestExplicitProducerReportsUnsupportedAgent(t *testing.T) {
	if RequestHardInterruptOf(struct{}{}, strp("stop now"), nil) {
		t.Fatal("plain object reported supported")
	}
	if RequestHardInterrupt(nil, "stop now", "") {
		t.Fatal("nil agent reported supported")
	}
}

type interruptOnlyProxy struct {
	interrupts []string
	hard       int
}

func (p *interruptOnlyProxy) Interrupt(message string) { p.interrupts = append(p.interrupts, message) }

func TestDynamicProxyDoesNotFabricateHardInterruptSupport(t *testing.T) {
	agent := &interruptOnlyProxy{}
	if !RequestHardInterruptOf(agent, strp("stop now"), nil) {
		t.Fatal("proxy reported unsupported")
	}
	if !reflect.DeepEqual(agent.interrupts, []string{"stop now"}) || agent.hard != 0 {
		t.Fatalf("interrupts %v hard %d", agent.interrupts, agent.hard)
	}
}

type legacySubclassAgent struct {
	*AIAgent
	legacyCalls []string
}

func (l *legacySubclassAgent) Interrupt(message string) {
	l.legacyCalls = append(l.legacyCalls, message)
}

func TestInheritedHardInterruptBypassesLegacySubclassOverride(t *testing.T) {
	agent := &legacySubclassAgent{AIAgent: &AIAgent{
		hardInterruptRequested: &pythread.Event{}, pendingRedirectLock: &sync.Mutex{},
		toolWorkerThreads: map[int64]bool{}, toolWorkerThreadsLock: &sync.Mutex{},
		activeChildrenLock: &sync.Mutex{}, quietMode: true, apiMode: "test",
	}}
	if !RequestHardInterruptOf(agent, strp("stop now"), nil) {
		t.Fatal("subclass reported unsupported")
	}
	if len(agent.legacyCalls) != 0 || !agent.hardInterruptRequested.IsSet() || !agent.interruptRequested.Load() || agent.interruptMessage != "stop now" {
		t.Fatalf("legacy %v hard %v requested %v message %q", agent.legacyCalls, agent.hardInterruptRequested.IsSet(), agent.interruptRequested.Load(), agent.interruptMessage)
	}
}

type scopeStopAgent struct{ stops []string }

func (s *scopeStopAgent) HardInterrupt(message, toolReason string) {
	s.stops = append(s.stops, message)
}

func TestInterruptScopeCancelsAgentsThatStartAfterTheCancel(t *testing.T) {
	scope := NewInterruptScope()
	early, late, unscoped := &scopeStopAgent{}, &scopeStopAgent{}, &scopeStopAgent{}
	ctx := WithInterruptScope(context.Background(), scope)
	releaseEarly := TrackInInterruptScope(ctx, early)
	scope.CancelByHost("Console command cancelled")
	TrackInInterruptScope(ctx, late)()
	releaseEarly()
	releaseUnscoped := TrackInInterruptScope(context.Background(), unscoped)
	scope.CancelByHost("Console command cancelled")
	releaseUnscoped()
	want := []string{"Console command cancelled"}
	if !reflect.DeepEqual(early.stops, want) || !reflect.DeepEqual(late.stops, want) || len(unscoped.stops) != 0 {
		t.Fatalf("early %v late %v unscoped %v", early.stops, late.stops, unscoped.stops)
	}
}

func TestTuiSubagentInterruptIsAnExplicitHardStop(t *testing.T) {
	agent := &modernInterruptAgent{}
	subagentID := "sa-hard-stop-test"
	activeSubagentsMu.Lock()
	activeSubagents.set(subagentID, map[string]any{"agent": agent})
	activeSubagentsMu.Unlock()
	defer func() {
		activeSubagentsMu.Lock()
		activeSubagents.pop(subagentID)
		activeSubagentsMu.Unlock()
	}()
	if !InterruptSubagent(subagentID) {
		t.Fatal("running subagent reported missing")
	}
	if len(agent.calls) != 1 || agent.calls[0][0] != "hard" || !strings.Contains(agent.calls[0][1], subagentID) {
		t.Fatalf("calls %v", agent.calls)
	}
}
