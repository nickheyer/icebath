package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// test_spawn_tree_records.py test_spawn_tree_list_survives_non_dict_snapshot
func TestSpawnTreeListSurvivesNonDictSnapshot(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	d := spawnTreeSessionDir("sess-x")
	if err := os.WriteFile(filepath.Join(d, "bad.json"), []byte(`"not a snapshot"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "good.json"), []byte(`{"session_id": "sess-x", "label": "ok"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	envelope := lookupMethod("spawn_tree.list")(context.Background(), 1, map[string]any{"session_id": "sess-x"})
	if envelope["error"] != nil {
		t.Fatalf("envelope %v", envelope)
	}
	entries := envelope["result"].(map[string]any)["entries"].([]map[string]any)
	labels := map[any]bool{}
	for _, entry := range entries {
		labels[entry["label"]] = true
	}
	if !labels["ok"] || len(entries) != 2 {
		t.Fatalf("entries %v", entries)
	}
}

// test_spawn_tree_records.py test_spawn_tree_load_rejects_non_dict_snapshot
func TestSpawnTreeLoadRejectsNonDictSnapshot(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	bad := filepath.Join(spawnTreeSessionDir("sess-y"), "bad.json")
	if err := os.WriteFile(bad, []byte("[1, 2]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if envelope := lookupMethod("spawn_tree.load")(context.Background(), 1, map[string]any{"path": bad}); envelope["error"] == nil {
		t.Fatalf("envelope %v", envelope)
	}
}

// test_legacy_events_since_replay_epoch.py test_legacy_events_since_dispatch_refuses_other_unknown_keys
func TestLegacyEventsSinceDispatchRefusesOtherUnknownKeys(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tuigateway.ResetReplayState()
	t.Cleanup(tuigateway.ResetReplayState)
	response := handleRequest(context.Background(), map[string]any{"id": "r0", "method": "session.events.since",
		"params": map[string]any{"session_id": "s1", "last_seen": 3, "replay_epoch": "proc-1", "bogus": 1}})
	message := rpcErrorOf(t, response)["message"].(string)
	if !strings.Contains(message, "bogus") || strings.Contains(message, "replay_epoch") {
		t.Fatalf("response %v", response)
	}
}

// test_legacy_events_since_replay_epoch.py test_legacy_events_since_replays_missed_events_and_open_requests
func TestLegacyEventsSinceReplaysMissedEventsAndOpenRequests(t *testing.T) {
	for name, params := range map[string]map[string]any{
		"with-replay-epoch":    {"session_id": "s-legacy", "last_seen": 1, "replay_epoch": "proc-1"},
		"without-replay-epoch": {"session_id": "s-legacy", "last_seen": 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			tuigateway.ResetReplayState()
			t.Cleanup(tuigateway.ResetReplayState)
			for _, n := range []int{1, 2} {
				frame := pyjson.NewObject()
				frame.Set("jsonrpc", "2.0")
				frame.Set("method", "event")
				eventParams := pyjson.NewObject()
				eventParams.Set("type", "message.delta")
				eventParams.Set("session_id", "s-legacy")
				payload := pyjson.NewObject()
				payload.Set("n", n)
				eventParams.Set("payload", payload)
				frame.Set("params", eventParams)
				tuigateway.StampEvent(frame)
			}
			question := pyjson.NewObject()
			question.Set("questions", []any{map[string]any{"question": "Ship it?", "choices": []any{"yes", "no"}}})
			cancel, err := tuigateway.SendAsync("clarify", "s-legacy", question, func(*pyjson.Object) {})
			if err != nil {
				t.Fatal(err)
			}
			open := tuigateway.OpenRequests("s-legacy")
			if len(open) != 1 {
				t.Fatalf("open %v", open)
			}
			requestID, _ := open[0].Get("id")
			response := handleRequest(context.Background(), map[string]any{"id": "r1", "method": "session.events.since", "params": params})
			cancel("test done")
			if response["error"] != nil {
				t.Fatalf("response %v", response)
			}
			wire, err := pyjson.Dumps(response["result"])
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := pyjson.Loads(wire)
			if err != nil {
				t.Fatal(err)
			}
			plain := pyjson.Plain(decoded).(map[string]any)
			var ns []any
			for _, event := range plain["events"].([]any) {
				ns = append(ns, event.(map[string]any)["payload"].(map[string]any)["n"])
			}
			if !pyjson.Equal(ns, []any{2}) {
				t.Fatalf("events %v", plain["events"])
			}
			var opened [][2]any
			for _, entry := range plain["open_requests"].([]any) {
				opened = append(opened, [2]any{entry.(map[string]any)["id"], entry.(map[string]any)["method"]})
			}
			if len(opened) != 1 || opened[0] != [2]any{requestID, "clarify"} {
				t.Fatalf("open_requests %v", opened)
			}
		})
	}
}

// test_billing_rpc.py test_keep_going_on_credits_rejects_a_non_bool_without_calling_the_portal
func TestKeepGoingOnCreditsRejectsANonBoolWithoutCallingThePortal(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, params := range []map[string]any{{}, {"enabled": "true"}, {"enabled": 1}} {
		res := lookupMethod("billing.keep_going_on_credits")(context.Background(), 1, params)["result"].(map[string]any)
		if res["ok"] != false || res["error"] != "invalid_request" {
			t.Fatalf("params %v res %v", params, res)
		}
	}
}

// test_protocol.py test_approval_respond_4001_when_nothing_resolves
func TestApprovalRespond4001WhenNothingResolves(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	response := handleRequest(context.Background(), map[string]any{"id": "r-nope", "method": "approval.respond",
		"params": map[string]any{"session_id": "nope", "request_id": "req-x", "choice": "once"}})
	if code := rpcErrorOf(t, response)["code"]; code != 4001 {
		t.Fatalf("response %v", response)
	}
}
