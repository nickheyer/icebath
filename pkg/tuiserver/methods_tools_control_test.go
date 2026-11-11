package tuiserver

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session id and payload of every session.control.update frame the recorder saw
func controlUpdates(r *frameRecorder) [][2]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][2]any
	for _, frame := range r.frames {
		raw, _ := frame.Get("params")
		params, isObj := raw.(*pyjson.Object)
		if !isObj {
			continue
		}
		if kind, _ := params.Get("type"); kind != "session.control.update" {
			continue
		}
		sid, _ := params.Get("session_id")
		payload, _ := params.Get("payload")
		out = append(out, [2]any{sid, payload})
	}
	return out
}

// Result member of a control response, failing on an error answer
func controlResult(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	result, isDict := resp["result"].(map[string]any)
	if !isDict {
		t.Fatalf("no result in %v", resp)
	}
	return result
}

// One named control slot of a control response
func controlSlot(t *testing.T, resp map[string]any, name string) map[string]any {
	t.Helper()
	control, _ := controlResult(t, resp)["control"].(map[string]any)
	slot, _ := control[name].(map[string]any)
	return slot
}

func TestGoalPauseResumeClearMutateRealPersistedState(t *testing.T) {
	sid, key := controlSession(t)
	saveGoal(key, func(st *goals.GoalState) { st.TurnsUsed = 8 })
	ctx := context.Background()
	paused := controlCall(ctx, "session.control", map[string]any{"session_id": sid, "action": "goal.pause"})
	if status := controlSlot(t, paused, "goal")["status"]; status != "paused" {
		t.Fatalf("paused status %v in %v", status, paused)
	}
	resumed := controlCall(ctx, "session.control", map[string]any{"session_id": sid, "action": "goal.resume"})
	dispatch, _ := controlResult(t, resumed)["dispatch"].(map[string]any)
	if dispatch["type"] != "send" || !truthy(dispatch["message"]) || !truthy(dispatch["notice"]) || dispatch["display"] != "/goal resume" {
		t.Fatalf("resume dispatch %v", dispatch)
	}
	if status := controlSlot(t, resumed, "goal")["status"]; status != "active" {
		t.Fatalf("resumed status %v", status)
	}
	cleared := controlCall(ctx, "session.control", map[string]any{"session_id": sid, "action": "goal.clear"})
	if control, _ := controlResult(t, cleared)["control"].(map[string]any); control["goal"] != nil {
		t.Fatalf("goal after clear %v", control["goal"])
	}
}

func TestLoopPauseResumeStopMutateRealPersistedState(t *testing.T) {
	sid, key := controlSession(t)
	saveLoop(key, nil)
	ctx := context.Background()
	paused := controlCall(ctx, "session.control", map[string]any{"session_id": sid, "action": "loop.pause"})
	if status := controlSlot(t, paused, "loop")["status"]; status != "paused" {
		t.Fatalf("paused status %v in %v", status, paused)
	}
	resumed := controlCall(ctx, "session.control", map[string]any{"session_id": sid, "action": "loop.resume"})
	if status := controlSlot(t, resumed, "loop")["status"]; status != "active" {
		t.Fatalf("resumed status %v", status)
	}
	stopped := controlCall(ctx, "session.control", map[string]any{"session_id": sid, "action": "loop.stop"})
	if control, _ := controlResult(t, stopped)["control"].(map[string]any); control["loop"] != nil {
		t.Fatalf("loop after stop %v", control["loop"])
	}
}

func TestGoalUnwaitClearsTheRealBarrierThroughSharedCommand(t *testing.T) {
	sid, key := controlSession(t)
	saveGoal(key, nil)
	ctx := context.Background()
	if _, err := goals.NewGoalManager(ctx, key, 0).WaitForSeconds(60, "backoff", 0); err != nil {
		t.Fatal(err)
	}
	resp := controlCall(ctx, "session.control", map[string]any{"session_id": sid, "action": "goal.unwait"})
	if _, failed := resp["error"]; failed {
		t.Fatalf("error %v", resp)
	}
	if waiting := goals.NewGoalManager(ctx, key, 0).State().WaitingUntil; waiting != 0.0 {
		t.Fatalf("waiting_until %v", waiting)
	}
}

func TestControlActionPublishesExactlyOneUpdateMatchingTheResponse(t *testing.T) {
	sid, key := controlSession(t)
	saveGoal(key, nil)
	recorder := &frameRecorder{}
	ctx := tuigateway.BindTransport(context.Background(), recorder)
	resp := controlCall(ctx, "session.control", map[string]any{"session_id": sid, "action": "goal.pause"})
	updates := controlUpdates(recorder)
	want := [][2]any{{sid, map[string]any{"control": controlResult(t, resp)["control"]}}}
	if !reflect.DeepEqual(updates, want) {
		t.Fatalf("updates %v, want %v", updates, want)
	}
}

func TestTypedSlashViaCommandDispatchPublishesTheNewState(t *testing.T) {
	for _, tc := range []struct{ name, arg string }{{"goal", "pause"}, {"loop", "pause"}} {
		t.Run(tc.name, func(t *testing.T) {
			sid, key := controlSession(t)
			ctx := context.Background()
			if tc.name == "goal" {
				saveGoal(key, nil)
			} else {
				interval := int64(300)
				if _, err := agent.NewLoopManager(ctx, key).Set(ctx, "poll CI", &interval, 0, "", nil); err != nil {
					t.Fatal(err)
				}
			}
			recorder := &frameRecorder{}
			bound := tuigateway.BindTransport(ctx, recorder)
			resp := controlCall(bound, "command.dispatch", map[string]any{"session_id": sid, "name": tc.name, "arg": tc.arg})
			if _, has := resp["result"]; !has {
				t.Fatalf("no result in %v", resp)
			}
			updates := controlUpdates(recorder)
			if len(updates) != 1 || updates[0][0] != sid {
				t.Fatalf("updates %v", updates)
			}
			control, _ := updates[0][1].(map[string]any)["control"].(map[string]any)
			if slot, _ := control[tc.name].(map[string]any); slot["status"] != "paused" {
				t.Fatalf("%s slot %v", tc.name, control[tc.name])
			}
		})
	}
}

func TestUnknownDispatchPublishesNothing(t *testing.T) {
	sid, _ := controlSession(t)
	recorder := &frameRecorder{}
	ctx := tuigateway.BindTransport(context.Background(), recorder)
	resp := controlCall(ctx, "command.dispatch", map[string]any{"session_id": sid, "name": "definitely-not-a-command", "arg": ""})
	if _, failed := resp["error"]; !failed {
		t.Fatalf("no error in %v", resp)
	}
	if updates := controlUpdates(recorder); len(updates) != 0 {
		t.Fatalf("updates %v", updates)
	}
}
