package agent

import (
	"log/slog"
	"strings"
	"testing"
)

// Empties the session in flight registry for the rest of the test
func clearInflightRegistry(t *testing.T) {
	t.Helper()
	clearIt := func() {
		inflightTurnsLock.Lock()
		clear(inflightTurnsBySession)
		inflightTurnsLock.Unlock()
	}
	clearIt()
	t.Cleanup(clearIt)
}

func TestCleanSerialTurnsNoWarning(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	clearInflightRegistry(t)
	records := captureLogger(t, "agent.agent_runtime_helpers", slog.LevelWarn)
	a := &AIAgent{sessionID: "s1"}
	if prev := noteTurnStart(a, "s1:t1:aaaa"); prev != "" {
		t.Fatalf("prev %q", prev)
	}
	noteTurnPersisted(a)
	if prev := noteTurnStart(a, "s1:t2:bbbb"); prev != "" {
		t.Fatalf("prev %q", prev)
	}
	noteTurnPersisted(a)
	if r := records(); len(r) != 0 {
		t.Fatalf("records %v", r)
	}
}

func TestCrossAgentSameSessionOverlapWarns(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	clearInflightRegistry(t)
	records := captureLogger(t, "agent.agent_runtime_helpers", slog.LevelWarn)
	a, b := &AIAgent{sessionID: "s1"}, &AIAgent{sessionID: "s1"}
	if prev := noteTurnStart(a, "s1:t1:aaaa"); prev != "" {
		t.Fatalf("prev %q", prev)
	}
	if prev := noteTurnStart(b, "s1:t2:bbbb"); prev != "s1:t1:aaaa" {
		t.Fatalf("prev %q", prev)
	}
	r := records()
	if len(r) != 1 || !strings.Contains(r[0].msg, "s1:t1:aaaa") || !strings.Contains(r[0].msg, "s1:t2:bbbb") || !strings.Contains(r[0].msg, "s1") {
		t.Fatalf("records %v", r)
	}
}

func TestDistinctSessionsNeverCrossWarn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	clearInflightRegistry(t)
	records := captureLogger(t, "agent.agent_runtime_helpers", slog.LevelWarn)
	a, b := &AIAgent{sessionID: "s1"}, &AIAgent{sessionID: "s2"}
	noteTurnStart(a, "s1:t1:aaaa")
	if prev := noteTurnStart(b, "s2:t2:bbbb"); prev != "" {
		t.Fatalf("prev %q", prev)
	}
	if r := records(); len(r) != 0 {
		t.Fatalf("records %v", r)
	}
}

func TestPersistDisabledForkNeitherRegistersNorWarns(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	clearInflightRegistry(t)
	records := captureLogger(t, "agent.agent_runtime_helpers", slog.LevelWarn)
	parent, fork := &AIAgent{sessionID: "s1"}, &AIAgent{sessionID: "s1", persistDisabled: true}
	noteTurnStart(parent, "s1:t1:aaaa")
	if prev := noteTurnStart(fork, "s1:tr:ffff"); prev != "" {
		t.Fatalf("fork prev %q", prev)
	}
	noteTurnPersisted(fork)
	noteTurnPersisted(parent)
	noteTurnStart(fork, "s1:tr:gggg")
	if prev := noteTurnStart(parent, "s1:t2:bbbb"); prev != "" {
		t.Fatalf("parent prev %q", prev)
	}
	if r := records(); len(r) != 0 {
		t.Fatalf("records %v", r)
	}
}
