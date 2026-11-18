package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
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
			bound := tuihost.BindTransport(ctx, recorder)
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
	ctx := tuihost.BindTransport(context.Background(), recorder)
	resp := controlCall(ctx, "command.dispatch", map[string]any{"session_id": sid, "name": "definitely-not-a-command", "arg": ""})
	if _, failed := resp["error"]; !failed {
		t.Fatalf("no error in %v", resp)
	}
	if updates := controlUpdates(recorder); len(updates) != 0 {
		t.Fatalf("updates %v", updates)
	}
}
