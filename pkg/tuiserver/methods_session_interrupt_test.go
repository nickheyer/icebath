package tuiserver

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const removedProvider = "Unknown provider 'custom:llama-local'."

// Live session record of a stop case, removed when the test ends
func stopRecord(t *testing.T, state map[string]any) *tuigateway.Session {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	sid := "stop-" + t.Name()
	values := map[string]any{"agent": nil, "agent_ready": &pythread.Event{}, "agent_error": nil, "history": []any{},
		"history_lock": &sync.Mutex{}, "running": false, "session_key": sid}
	for k, v := range state {
		values[k] = v
	}
	session := tuigateway.NewSession(values)
	tuigateway.SetSessionRecord(sid, session)
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
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

// test_free_tier_task_done.py test_a_correction_rpc_during_the_task_disqualifies_it
func TestACorrectionRpcDuringTheTaskDisqualifiesIt(t *testing.T) {
	for _, c := range []struct{ name, method, text string }{
		{"steer", "session.steer", "also check my calendar"}, {"redirect", "session.redirect", "actually only Monday"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := freeTierHome(t)
			a := freeTierAgent(t, true)
			session := freeTierSession(a, nil)
			tuigateway.SetSessionRecord("sid", session)
			defer tuigateway.SetSessionRecord("sid", nil)
			resp := handleRequest(ctx, map[string]any{"id": "1", "method": c.method, "params": map[string]any{"session_id": "sid", "text": c.text}})
			if _, answered := resp["result"]; !answered {
				t.Fatalf("response %v", resp)
			}
			noteTaskDone(ctx, session, cleanTaskResult(), a, "")
			if _, pending := offerRecorded(t, ctx); pending {
				t.Fatal("a corrected task armed the offer")
			}
		})
	}
}
