package tuiserver

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One emitted session event
type emitted struct {
	event, sid string
	payload    map[string]any
}

// Recorder of every emitted session event
type emitLog struct {
	sync.Mutex
	list []emitted
}

func (l *emitLog) all() []emitted {
	l.Lock()
	defer l.Unlock()
	return append([]emitted(nil), l.list...)
}

func (l *emitLog) clear() {
	l.Lock()
	l.list = nil
	l.Unlock()
}

func (l *emitLog) eventsFor(sid string) []emitted {
	out := []emitted{}
	for _, e := range l.all() {
		if e.sid == sid {
			out = append(out, e)
		}
	}
	return out
}

// Empty mirror registries, a recorded emitter and a registry cleanup of every sid a case adds
func childMirrorFixture(t *testing.T) *emitLog {
	t.Helper()
	log := &emitLog{}
	prev := emitEvent
	emitEvent = func(ctx context.Context, event, sid string, payload map[string]any) bool {
		log.Lock()
		log.list = append(log.list, emitted{event, sid, payload})
		log.Unlock()
		return true
	}
	resetMirrors := func() {
		childMirrorsMu.Lock()
		childMirrors = map[childMirrorKey]*childMirror{}
		childMirrorsMu.Unlock()
		activeChildRuns.Lock()
		activeChildRuns.at = map[childMirrorKey]float64{}
		activeChildRuns.Unlock()
	}
	resetMirrors()
	t.Cleanup(func() {
		emitEvent = prev
		for _, sid := range []string{"parent-sid", "live-1", "live-2"} {
			tuigateway.SetSessionRecord(sid, nil)
		}
		resetMirrors()
	})
	return log
}

// Drives the progress callback the way the delegate relay does on a live parent turn
func relay(eventType string, payload map[string]any) {
	if tuigateway.SessionRecord("parent-sid") == nil {
		tuigateway.SetSessionRecord("parent-sid", tuigateway.NewSession(map[string]any{"session_key": "parent"}))
	}
	var name, preview any
	if v, ok := payload["tool_name"]; ok {
		name = v
		delete(payload, "tool_name")
	}
	if v, ok := payload["preview"]; ok {
		preview = v
		delete(payload, "preview")
	}
	kw := map[string]any{"goal": "research X", "task_count": 1, "task_index": 0}
	for k, v := range payload {
		kw[k] = v
	}
	onToolProgress(context.Background(), "parent-sid", eventType, name, preview, kw)
}

func mirrorCount() int {
	childMirrorsMu.Lock()
	defer childMirrorsMu.Unlock()
	return len(childMirrors)
}

func liveChild(sid string, agentValue any) {
	tuigateway.SetSessionRecord(sid, tuigateway.NewSession(map[string]any{"session_key": "child-1", "agent": agentValue}))
}

func eventNames(list []emitted) []string {
	out := []string{}
	for _, e := range list {
		out = append(out, e.event)
	}
	return out
}

func TestNoLiveChildSessionNoMirror(t *testing.T) {
	log := childMirrorFixture(t)
	relay("subagent.tool", map[string]any{"tool_name": "terminal", "child_session_id": "child-1"})
	got := log.all()
	if len(got) != 1 || got[0].event != "subagent.tool" || got[0].sid != "parent-sid" || mirrorCount() != 0 {
		t.Fatalf("emits = %v, mirrors = %d", got, mirrorCount())
	}
}

func TestLiveChildSessionGetsNativeStream(t *testing.T) {
	log := childMirrorFixture(t)
	liveChild("live-1", nil)
	relay("subagent.tool", map[string]any{"tool_name": "terminal", "preview": "ls", "child_session_id": "child-1"})
	relay("subagent.thinking", map[string]any{"preview": "hmm", "child_session_id": "child-1"})
	relay("subagent.tool", map[string]any{"tool_name": "read_file", "child_session_id": "child-1"})
	relay("subagent.complete", map[string]any{"child_session_id": "child-1", "status": "completed", "summary": "done deal"})
	child := log.eventsFor("live-1")
	want := []string{"message.start", "tool.start", "reasoning.delta", "tool.complete", "tool.start", "tool.complete", "message.complete"}
	if !reflect.DeepEqual(eventNames(child), want) {
		t.Fatalf("child events = %v", eventNames(child))
	}
	first := child[1].payload
	if first["name"] != "terminal" || !strings.HasPrefix(first["tool_id"].(string), "submirror:child-1:") {
		t.Fatalf("first tool = %v", first)
	}
	if !reflect.DeepEqual(child[2].payload, map[string]any{"text": "hmm"}) || child[3].payload["tool_id"] != first["tool_id"] ||
		!reflect.DeepEqual(child[6].payload, map[string]any{"text": "done deal"}) {
		t.Fatalf("child payloads = %v", child)
	}
	parent := eventNames(log.eventsFor("parent-sid"))
	if !reflect.DeepEqual(parent, []string{"subagent.tool", "subagent.thinking", "subagent.tool", "subagent.complete"}) || mirrorCount() != 0 {
		t.Fatalf("parent events = %v, mirrors = %d", parent, mirrorCount())
	}
}

func TestWindowClosedMidrunDropsStateThenFreshTurnOnReopen(t *testing.T) {
	log := childMirrorFixture(t)
	liveChild("live-1", nil)
	relay("subagent.tool", map[string]any{"tool_name": "terminal", "child_session_id": "child-1"})
	childMirrorsMu.Lock()
	_, present := childMirrors[childMirrorKey{"", "child-1"}]
	childMirrorsMu.Unlock()
	if !present {
		t.Fatal("mirror state missing")
	}
	tuigateway.SetSessionRecord("live-1", nil)
	tuigateway.SetSessionRecord("parent-sid", nil)
	relay("subagent.tool", map[string]any{"tool_name": "read_file", "child_session_id": "child-1"})
	if mirrorCount() != 0 {
		t.Fatal("mirror state kept after the window closed")
	}
	log.clear()
	liveChild("live-2", nil)
	relay("subagent.tool", map[string]any{"tool_name": "web_search", "child_session_id": "child-1"})
	if got := eventNames(log.eventsFor("live-2")); !reflect.DeepEqual(got, []string{"message.start", "tool.start"}) {
		t.Fatalf("live-2 events = %v", got)
	}
}

func TestUpgradedChildSessionNotMirrored(t *testing.T) {
	log := childMirrorFixture(t)
	liveChild("live-1", struct{}{})
	relay("subagent.tool", map[string]any{"tool_name": "terminal", "child_session_id": "child-1"})
	got := log.all()
	activeChildRuns.Lock()
	_, running := activeChildRuns.at[childMirrorKey{"", "child-1"}]
	activeChildRuns.Unlock()
	if len(got) != 1 || got[0].event != "subagent.tool" || mirrorCount() != 0 || !running {
		t.Fatalf("emits = %v, mirrors = %d, running = %v", got, mirrorCount(), running)
	}
}

func TestStaleChildRunNotReportedActive(t *testing.T) {
	childMirrorFixture(t)
	activeChildRuns.Lock()
	activeChildRuns.at[childMirrorKey{"", "child-1"}] = 0
	activeChildRuns.Unlock()
	if childRunActive("child-1", nil) {
		t.Fatal("ancient run reported active")
	}
	relay("subagent.tool", map[string]any{"tool_name": "terminal", "child_session_id": "child-1"})
	if !childRunActive("child-1", nil) {
		t.Fatal("fresh run not reported active")
	}
}

func TestActiveChildRunsRegistryTracksLiveness(t *testing.T) {
	childMirrorFixture(t)
	tracked := func() bool {
		activeChildRuns.Lock()
		defer activeChildRuns.Unlock()
		_, ok := activeChildRuns.at[childMirrorKey{"", "child-1"}]
		return ok
	}
	relay("subagent.start", map[string]any{"preview": "go", "child_session_id": "child-1"})
	if !tracked() {
		t.Fatal("start not tracked")
	}
	relay("subagent.tool", map[string]any{"tool_name": "terminal", "child_session_id": "child-1"})
	if !tracked() {
		t.Fatal("tool not tracked")
	}
	relay("subagent.complete", map[string]any{"child_session_id": "child-1", "status": "completed", "summary": "ok"})
	if tracked() {
		t.Fatal("completion kept the run")
	}
}

func TestStartMirrorsAsImmediateHeaderLine(t *testing.T) {
	log := childMirrorFixture(t)
	liveChild("live-1", nil)
	relay("subagent.start", map[string]any{"preview": "starting child branch", "child_session_id": "child-1"})
	relay("subagent.progress", map[string]any{"preview": "step 1/3", "child_session_id": "child-1"})
	child := log.eventsFor("live-1")
	if len(child) != 2 || child[0].event != "message.start" || child[0].payload != nil || child[1].event != "message.delta" ||
		!reflect.DeepEqual(child[1].payload, map[string]any{"text": "starting child branch\n"}) {
		t.Fatalf("child events = %v", child)
	}
}

func TestTextMirrorsAsMessageDelta(t *testing.T) {
	log := childMirrorFixture(t)
	liveChild("live-1", nil)
	relay("subagent.text", map[string]any{"preview": "Here is ", "child_session_id": "child-1"})
	relay("subagent.text", map[string]any{"preview": "the answer.", "child_session_id": "child-1"})
	child := log.eventsFor("live-1")
	if len(child) != 3 || child[0].event != "message.start" || child[0].payload != nil ||
		!reflect.DeepEqual(child[1], emitted{"message.delta", "live-1", map[string]any{"text": "Here is "}}) ||
		!reflect.DeepEqual(child[2], emitted{"message.delta", "live-1", map[string]any{"text": "the answer."}}) {
		t.Fatalf("child events = %v", child)
	}
}
