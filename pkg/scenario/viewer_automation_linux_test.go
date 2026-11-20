package scenario

import (
	"encoding/json"
	"testing"
)

// Manager state of one session.automation answer decoded from its JSON text, nil when the answer holds none
func viewerAutomationState(s *S, reply map[string]any) map[string]any {
	s.t.Helper()
	text, isText := Dig(reply, "result", "state").(string)
	if !isText {
		return nil
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(text), &state); err != nil {
		s.Failf("the state of %v is not JSON: %v", reply, err)
	}
	return state
}

// session.automation runs goal, loop and heartbeat manager calls on the session row and answers manager errors as values
func TestScenarioViewerAutomationGoalLoopHeartbeat(t *testing.T) {
	s := Start(t, Setup{})
	gw := s.Spawn("gateway", "run")
	gatewayRunning(s)
	w := attachGateway(s)
	created := w.Call("session.create", map[string]any{"request_id": "viewer-automation-1", "source": "cli", "cwd": s.Work})
	session, _ := Dig(created, "result", "session_id").(string)
	if session == "" {
		s.Failf("session.create answered %v", created)
	}
	call := func(kind, name string, args []any, kwargs map[string]any) map[string]any {
		params := map[string]any{"session_id": session, "kind": kind, "call": name}
		if args != nil {
			params["args"] = args
		}
		if kwargs != nil {
			params["kwargs"] = kwargs
		}
		return w.Call("session.automation", params)
	}

	s.SameJSON(Dig(call("goal", "state", nil, nil), "result"), `{"value": null, "state": null}`)
	s.SameJSON(Dig(call("goal", "add_subgoal", []any{"too early"}, nil), "result"),
		`{"error": {"type": "RuntimeError", "message": "no active goal"}, "state": null}`)

	set := call("goal", "set", []any{"  ship the port  "}, map[string]any{"max_turns": 7})
	state := viewerAutomationState(s, set)
	s.HasJSON(state, `{"goal": "ship the port", "status": "active", "turns_used": 0, "max_turns": 7, "subgoals": [], "gates": []}`)
	if Dig(set, "result", "value", "__state__") != Dig(set, "result", "state") {
		s.Failf("goal set must answer its state as the value too, got %v", set)
	}
	empty := call("goal", "set", []any{"   "}, nil)
	s.HasJSON(Dig(empty, "result"), `{"error": {"type": "ValueError", "message": "goal text is empty"}}`)
	s.HasJSON(viewerAutomationState(s, empty), `{"goal": "ship the port", "status": "active"}`)

	added := call("goal", "add_subgoal", []any{" write the scenario "}, nil)
	if Dig(added, "result", "value") != "write the scenario" {
		s.Failf("add_subgoal answered %v", added)
	}
	s.HasJSON(viewerAutomationState(s, added), `{"subgoals": ["write the scenario"]}`)
	s.HasJSON(Dig(call("goal", "remove_subgoal", []any{5}, nil), "result"),
		`{"error": {"type": "IndexError", "message": "index out of range (1..1)"}}`)
	s.HasJSON(Dig(call("goal", "add_gate", []any{"  "}, nil), "result"),
		`{"error": {"type": "ValueError", "message": "gate command is empty"}}`)
	gate := call("goal", "add_gate", []any{"true"}, map[string]any{"timeout_seconds": 30})
	s.SameJSON(Dig(gate, "result", "value"), `{"__gate__": {"command": "true", "timeout_seconds": 30, "max_retries": 3,
		"attempts": 0, "last_exit_code": null, "last_output_tail": ""}}`)

	status := call("goal", "command", []any{"status"}, nil)
	s.SameJSON(Dig(status, "result", "value"), `{"output": "⊙ Goal (active, 0/7 turns, 1 subgoal, 1 gate): ship the port",
		"prompt": null, "kickoff": false, "clear_pending": null, "error": false}`)
	paused := call("goal", "pause", nil, nil)
	s.HasJSON(viewerAutomationState(s, paused), `{"status": "paused", "paused_reason": "user-paused"}`)
	if Dig(call("goal", "command", []any{""}, nil), "result", "value", "output") != "⏸ Goal (paused, 0/7 turns, 1 subgoal, 1 gate — user-paused): ship the port" {
		s.Failf("the bare goal command did not print the paused status line")
	}
	removed := call("goal", "command", []any{"gate remove 9"}, nil)
	s.SameJSON(Dig(removed, "result", "value"), `{"output": "/goal gate remove: index out of range (1..1)", "prompt": null,
		"kickoff": false, "clear_pending": null, "error": true}`)
	if Dig(call("loop", "goal_blocks_tick", nil, nil), "result", "value") != false {
		s.Failf("a paused goal must not block a loop tick")
	}
	if Dig(call("goal", "resume", nil, map[string]any{"reset_budget": false}), "result", "value", "__state__") == nil {
		s.Failf("goal resume did not answer its state")
	}
	if Dig(call("loop", "goal_blocks_tick", nil, nil), "result", "value") != true {
		s.Failf("an active goal must block a loop tick")
	}
	s.SameJSON(Dig(call("goal", "clear", nil, nil), "result"), `{"value": null, "state": null}`)
	stored := s.Rows("state.db", "SELECT value FROM state_meta WHERE key LIKE ?", "goal:%")
	if len(stored) != 1 {
		s.Failf("state_meta holds %d goal rows of the session, want 1", len(stored))
	}
	var kept map[string]any
	text, _ := stored[0]["value"].(string)
	if err := json.Unmarshal([]byte(text), &kept); err != nil || kept["status"] != "cleared" {
		s.Failf("the stored goal row is %q, want status cleared", text)
	}

	s.HasJSON(Dig(call("loop", "set", []any{" "}, nil), "result"), `{"error": {"type": "ValueError", "message": "loop prompt is empty"}, "state": null}`)
	loop := call("loop", "set", []any{"check the build"}, map[string]any{"interval_seconds": 7200, "times": 2, "until": " green "})
	s.HasJSON(viewerAutomationState(s, loop), `{"prompt": "check the build", "mode": "interval", "interval_seconds": 7200, "times": 2,
		"until": "green", "status": "active"}`)
	paced := call("loop", "set", []any{"watch the queue"}, nil)
	s.HasJSON(viewerAutomationState(s, paced), `{"prompt": "watch the queue", "mode": "self_paced", "interval_seconds": 0, "times": 0}`)
	if Dig(call("loop", "clear", nil, nil), "result", "value") != true || Dig(call("loop", "clear", nil, nil), "result", "value") != false {
		s.Failf("loop clear must answer true once and false after it")
	}

	s.HasJSON(Dig(call("heartbeat", "set", []any{"look around", 30}, nil), "result"),
		`{"error": {"type": "ValueError", "message": "interval must be at least 60s"}, "state": null}`)
	s.HasJSON(Dig(call("heartbeat", "set", []any{"", 120}, nil), "result"),
		`{"error": {"type": "ValueError", "message": "heartbeat prompt is empty"}}`)
	beat := call("heartbeat", "set", []any{" look around "}, map[string]any{"interval_seconds": 120})
	s.HasJSON(viewerAutomationState(s, beat), `{"prompt": "look around", "interval_seconds": 120, "status": "active"}`)
	s.HasJSON(viewerAutomationState(s, call("heartbeat", "pause", nil, nil)), `{"status": "paused"}`)
	s.SameJSON(Dig(call("heartbeat", "clear", nil, nil), "result"), `{"value": true, "state": null}`)

	refused := []map[string]any{
		{"session_id": session, "kind": "calendar", "call": "state"},
		{"session_id": session, "kind": "goal", "call": "_save"},
		{"session_id": session, "kind": "loop", "call": "mark_done"},
		{"session_id": session, "kind": "goal", "call": "set", "args": "ship"},
		{"session_id": session, "kind": "goal", "call": "set", "args": []any{"ship"}, "kwargs": map[string]any{"_state": 1}},
		{"session_id": session, "kind": "goal", "call": "state", "extra": true},
	}
	for _, params := range refused {
		s.HasJSON(w.Call("session.automation", params), `{"error": {"data": {"reason": "invalid_params"}}}`)
	}
	last := call("goal", "state", nil, nil)
	if Dig(last, "result", "value") != nil {
		s.Failf("the state call answered the value %v", Dig(last, "result", "value"))
	}
	s.HasJSON(viewerAutomationState(s, last), `{"goal": "ship the port", "status": "cleared"}`)
	gw.Stop()
}
