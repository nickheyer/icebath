package agent

import (
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// The _StubAgent of test_subagent_steer.py, accepting or refusing steers or exploding on them
type steerStubAgent struct {
	mu      sync.Mutex
	accept  bool
	boom    bool
	steered []string
}

func newSteerStub(accept, boom bool) *steerStubAgent {
	return &steerStubAgent{accept: accept, boom: boom}
}

func (a *steerStubAgent) Steer(text string) bool {
	if a.boom {
		panic("steer exploded")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steered = append(a.steered, text)
	return a.accept
}

func (a *steerStubAgent) texts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.steered...)
}

// The _with_registered helper, a running record at depth one under the root parent
func withRegisteredSteerAgent(sid string, agent any, ownerSessionID any, ownerTransport, ownerSessionRecord any) {
	registerSubagent(map[string]any{
		"subagent_id": sid, "parent_id": "root", "depth": 1, "goal": "test goal", "status": "running", "agent": agent,
		"owner_session_id": ownerSessionID, "owner_transport": ownerTransport, "owner_session_record": ownerSessionRecord,
	})
}

func steerPlain(sid, text string) bool { return SteerSubagent(sid, text, nil, nil, nil) }

func TestSteerReachesTheLiveChild(t *testing.T) {
	agent := newSteerStub(true, false)
	withRegisteredSteerAgent("sid-steer-1", agent, nil, nil, nil)
	defer unregisterSubagent("sid-steer-1", nil)
	if !steerPlain("sid-steer-1", "focus on pricing instead") || strings.Join(agent.texts(), "|") != "focus on pricing instead" {
		t.Fatalf("steered %v", agent.texts())
	}
}

func TestUnknownSubagentIsFalseNotAnError(t *testing.T) {
	if steerPlain("sid-not-registered", "hello") {
		t.Fatal("unknown subagent steered")
	}
}

func TestEmptyTextIsRefusedWithoutALookup(t *testing.T) {
	agent := newSteerStub(true, false)
	withRegisteredSteerAgent("sid-steer-2", agent, nil, nil, nil)
	defer unregisterSubagent("sid-steer-2", nil)
	if steerPlain("sid-steer-2", "   ") || len(agent.texts()) != 0 {
		t.Fatalf("blank steer accepted %v", agent.texts())
	}
}

func TestRecordWithoutLiveAgentIsFalse(t *testing.T) {
	registerSubagent(map[string]any{"subagent_id": "sid-steer-3", "status": "running", "agent": nil})
	defer unregisterSubagent("sid-steer-3", nil)
	if steerPlain("sid-steer-3", "hello") {
		t.Fatal("dead record steered")
	}
}

func TestAgentRejectionPropagatesAsFalse(t *testing.T) {
	agent := newSteerStub(false, false)
	withRegisteredSteerAgent("sid-steer-4", agent, nil, nil, nil)
	defer unregisterSubagent("sid-steer-4", nil)
	if steerPlain("sid-steer-4", "hello") {
		t.Fatal("rejected steer reported true")
	}
}

func TestExceptionInSteerDegradesToFalse(t *testing.T) {
	agent := newSteerStub(true, true)
	withRegisteredSteerAgent("sid-steer-5", agent, nil, nil, nil)
	defer unregisterSubagent("sid-steer-5", nil)
	if steerPlain("sid-steer-5", "hello") {
		t.Fatal("exploding steer reported true")
	}
}

func TestStaleAgentTeardownCannotUnregisterRecycledId(t *testing.T) {
	oldAgent, replacement := newSteerStub(true, false), newSteerStub(true, false)
	withRegisteredSteerAgent("sid-recycled-teardown", oldAgent, "old-owner", nil, nil)
	withRegisteredSteerAgent("sid-recycled-teardown", replacement, "new-owner", nil, nil)
	defer unregisterSubagent("sid-recycled-teardown", replacement)
	unregisterSubagent("sid-recycled-teardown", oldAgent)
	if !steerPlain("sid-recycled-teardown", "replacement remains live") {
		t.Fatal("replacement not steerable after the stale teardown")
	}
	if len(oldAgent.texts()) != 0 || strings.Join(replacement.texts(), "|") != "replacement remains live" {
		t.Fatalf("old %v replacement %v", oldAgent.texts(), replacement.texts())
	}
}

type steerOwnerTransport struct{ name string }

func TestStatusSnapshotNeverLeaksOwnerOrLifecycleMetadata(t *testing.T) {
	agent := newSteerStub(true, false)
	ownerTransport := &steerOwnerTransport{name: "transport"}
	ownerSessionRecord := map[string]any{"session_key": "private-owner"}
	withRegisteredSteerAgent("sid-private-metadata", agent, "private-owner", ownerTransport, ownerSessionRecord)
	defer unregisterSubagent("sid-private-metadata", agent)
	var snapshot map[string]any
	for _, item := range ListActiveSubagents() {
		if item["subagent_id"] == "sid-private-metadata" {
			snapshot = item
		}
	}
	if snapshot == nil || snapshot["status"] != "running" {
		t.Fatalf("snapshot %v", snapshot)
	}
	for _, key := range []string{"agent", "owner_session_id", "owner_transport", "owner_session_record", "accepting_steer"} {
		if _, has := snapshot[key]; has {
			t.Fatalf("snapshot leaks %s", key)
		}
	}
	if strings.Contains(pyrepr.Repr(snapshot), "private-owner") {
		t.Fatalf("snapshot leaks the owner %v", snapshot)
	}
	for _, v := range snapshot {
		if v == any(ownerTransport) || pyrepr.Is(v, ownerSessionRecord) {
			t.Fatalf("snapshot leaks an authority object %v", snapshot)
		}
	}
}
