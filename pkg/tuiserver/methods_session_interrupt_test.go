package tuiserver

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

const removedProvider = "Unknown provider 'custom:llama-local'."

// Live session record of a stop case, removed when the test ends
func stopRecord(t *testing.T, state map[string]any) *tuihost.Session {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	sid := "stop-" + t.Name()
	values := map[string]any{"agent": nil, "agent_ready": &pythread.Event{}, "agent_error": nil, "history": []any{},
		"history_lock": &sync.Mutex{}, "running": false, "session_key": sid}
	for k, v := range state {
		values[k] = v
	}
	session := tuihost.NewSession(values)
	tuihost.SetSessionRecord(sid, session)
	t.Cleanup(func() { tuihost.SetSessionRecord(sid, nil) })
	return session
}

func interruptStatus(t *testing.T) any {
	t.Helper()
	resp := lookupMethod("session.interrupt")(context.Background(), 1, map[string]any{"session_id": "stop-" + t.Name()})
	result, _ := resp["result"].(map[string]any)
	if result["status"] != "interrupted" {
		t.Fatalf("interrupt %v", resp)
	}
	return result["status"]
}

// test_interrupt_does_not_wait_for_agent.py test_stop_succeeds_after_the_agent_build_failed
func TestStopSucceedsAfterTheAgentBuildFailed(t *testing.T) {
	session := stopRecord(t, map[string]any{"agent_build_started": true, "agent_error": removedProvider})
	session.Get("agent_ready").(*pythread.Event).Set()
	interruptStatus(t)
}

// test_interrupt_does_not_wait_for_agent.py test_stop_frees_a_running_session_whose_build_failed
func TestStopFreesARunningSessionWhoseBuildFailed(t *testing.T) {
	session := stopRecord(t, map[string]any{"agent_build_started": true, "agent_error": removedProvider, "running": true})
	session.Get("agent_ready").(*pythread.Event).Set()
	interruptStatus(t)
	if session.Get("running") != false {
		t.Fatalf("running %v", session.Get("running"))
	}
}

// test_interrupt_does_not_wait_for_agent.py test_stop_cancels_a_turn_parked_on_an_unfinished_build
func TestStopCancelsATurnParkedOnAnUnfinishedBuild(t *testing.T) {
	release := make(chan struct{})
	thread := memory.SpawnContextThread(context.Background(), "parked-turn", func(context.Context) { <-release })
	thread.Start()
	t.Cleanup(func() { close(release) })
	session := stopRecord(t, map[string]any{"agent_build_started": true, "running": true, "_run_thread": thread})
	interruptStatus(t)
	if session.Get("_turn_cancel_requested") != true {
		t.Fatalf("cancel flag %v", session.Get("_turn_cancel_requested"))
	}
	if session.Get("agent_ready").(*pythread.Event).IsSet() {
		t.Fatal("the build event was set")
	}
}

// test_interrupt_does_not_wait_for_agent.py test_stop_does_not_build_an_agent_for_a_resumed_session
func TestStopDoesNotBuildAnAgentForAResumedSession(t *testing.T) {
	session := stopRecord(t, nil)
	interruptStatus(t)
	if session.Get("agent_build_started") != nil || session.Get("agent") != nil || session.Get("agent_ready").(*pythread.Event).IsSet() {
		t.Fatalf("stop started a build: started %v agent %v", session.Get("agent_build_started"), session.Get("agent"))
	}
}
