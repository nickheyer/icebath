package tuiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Transport recording every frame written to it
type frameRecorder struct {
	mu     sync.Mutex
	frames []*pyjson.Object
}

func (r *frameRecorder) Write(obj *pyjson.Object) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, obj)
	return true
}

func (r *frameRecorder) Close() {}

func (r *frameRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

// Live session with an isolated control store, its goal loop and heartbeat cleared afterwards
func controlSession(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	goals.ClearSessionDBCache()
	sid := fmt.Sprintf("sid-control-%d", time.Now().UnixNano())
	key := fmt.Sprintf("control-%d", time.Now().UnixNano())
	session := tuihost.NewSession(map[string]any{"session_key": key, "history": []any{}, "history_version": 0,
		"running": false, "attached_images": []any{}, "cols": 120, "agent": nil, "created_at": float64(time.Now().Unix())})
	tuihost.SetSessionRecord(sid, session)
	t.Cleanup(func() {
		ctx := context.Background()
		goals.NewGoalManager(ctx, key, 0).Clear()
		agent.NewLoopManager(ctx, key).Clear(ctx)
		agent.NewHeartbeatManager(ctx, key).Clear()
		tuihost.SetSessionRecord(sid, nil)
		goals.ClearSessionDBCache()
	})
	return sid, key
}

// Calls a registered method the way the dispatcher does
func controlCall(ctx context.Context, method string, params map[string]any) map[string]any {
	return lookupMethod(method)(ctx, 91, params)
}

func readControl(t *testing.T, sid string) map[string]any {
	t.Helper()
	resp := controlCall(context.Background(), "session.control.read", map[string]any{"session_id": sid})
	result, _ := resp["result"].(map[string]any)
	control, isMap := result["control"].(map[string]any)
	if !isMap {
		t.Fatalf("read response %v", resp)
	}
	return control
}

// Replaces a registered method for the test
func swapMethod(t *testing.T, name string, fn Handler) {
	t.Helper()
	methodsMu.Lock()
	previous, had := methods[name]
	methods[name] = fn
	methodsMu.Unlock()
	t.Cleanup(func() {
		methodsMu.Lock()
		defer methodsMu.Unlock()
		if had {
			methods[name] = previous
		} else {
			delete(methods, name)
		}
	})
}

func saveGoal(key string, edit func(*goals.GoalState)) *goals.GoalState {
	st := &goals.GoalState{Goal: "Finish the TUI control card", Status: "active", TurnsUsed: 3, MaxTurns: 12,
		CreatedAt: 100.0, LastTurnAt: 200.0}
	if edit != nil {
		edit(st)
	}
	goals.SaveGoal(context.Background(), key, st)
	return st
}

func saveLoop(key string, edit func(*agent.SessionLoopState)) {
	st := &agent.SessionLoopState{Prompt: "Check the deployment", Status: "active", Mode: "interval", IntervalSeconds: 300,
		CurrentDelay: 300, CreatedAt: 100.0, NextDueAt: 400.0}
	if edit != nil {
		edit(st)
	}
	agent.SaveLoop(context.Background(), key, st)
}

func saveHeartbeat(key string, edit func(*agent.HeartbeatState)) {
	st := &agent.HeartbeatState{Prompt: "Check the deployment", IntervalSeconds: big.NewInt(600), Status: "active", CreatedAt: 100.0,
		LastFiredAt: 150.0, FireCount: 2}
	if edit != nil {
		edit(st)
	}
	agent.SaveHeartbeat(context.Background(), key, st)
}

// test_session_control.py TestStructuredRead test_methods_are_registered_and_empty_snapshot_is_stable
func TestMethodsAreRegisteredAndEmptySnapshotIsStable(t *testing.T) {
	sid, _ := controlSession(t)
	first, second := readControl(t, sid), readControl(t, sid)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("%v != %v", first, second)
	}
	want := map[string]any{"goal": nil, "loop": nil, "heartbeat": nil, "revision": "", "updated_at": 0}
	if fmt.Sprint(first) != fmt.Sprint(want) {
		t.Fatalf("empty snapshot %v", first)
	}
}

// test_session_control.py TestStructuredRead test_goal_contract_subgoals_and_gates_are_structured_and_sanitized
func TestGoalContractSubgoalsAndGatesAreStructuredAndSanitized(t *testing.T) {
	sid, key := controlSession(t)
	contract := goals.GoalContract{Outcome: "Card is correct", Verification: "Run focused tests"}
	exitCode := int64(1)
	saveGoal(key, func(st *goals.GoalState) {
		st.Contract = contract
		st.Subgoals = []string{"Keep command routing narrow", "Document event hydration seam"}
		st.Gates = []goals.GoalGate{{Command: "scripts/run_tests.sh tests/tui_gateway/test_session_control.py",
			TimeoutSeconds: 90, MaxRetries: 2, Attempts: 1, LastExitCode: &exitCode, LastOutputTail: "private output must stay private"}}
	})
	goal, _ := readControl(t, sid)["goal"].(map[string]any)
	if goal["title"] != "Finish the TUI control card" {
		t.Fatalf("title %v", goal["title"])
	}
	wantContract, _ := pyjson.Dumps(contract.ToDict())
	gotContract, _ := pyjson.Dumps(goal["contract"])
	if gotContract != wantContract {
		t.Fatalf("contract %s want %s", gotContract, wantContract)
	}
	if fmt.Sprint(goal["subgoals"]) != fmt.Sprint([]string{"Keep command routing narrow", "Document event hydration seam"}) {
		t.Fatalf("subgoals %v", goal["subgoals"])
	}
	gates, _ := json.Marshal(pyjson.Plain(goal["gates"]))
	wantGates, _ := json.Marshal([]map[string]any{{"command": "scripts/run_tests.sh tests/tui_gateway/test_session_control.py",
		"timeout_seconds": 90, "max_retries": 2, "attempts": 1, "last_exit_code": 1}})
	if string(gates) != string(wantGates) {
		t.Fatalf("gates %s", gates)
	}
	serialized, err := json.Marshal(goal)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"last_output_tail", "private output", "route", "session_id", "credential", "api_key"} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("%q leaked into %s", forbidden, serialized)
		}
	}
}

// test_session_control.py TestStructuredRead test_revision_is_stable_then_changes_for_each_visible_control
func TestRevisionIsStableThenChangesForEachVisibleControl(t *testing.T) {
	for _, kind := range []string{"goal", "loop", "heartbeat"} {
		t.Run(kind, func(t *testing.T) {
			sid, key := controlSession(t)
			switch kind {
			case "goal":
				saveGoal(key, nil)
			case "loop":
				saveLoop(key, nil)
			default:
				saveHeartbeat(key, nil)
			}
			before := readControl(t, sid)
			if revision, isText := before["revision"].(string); !isText || revision == "" {
				t.Fatalf("revision %v", before["revision"])
			}
			if again := readControl(t, sid); !reflect.DeepEqual(before, again) {
				t.Fatalf("%v != %v", before, again)
			}
			switch kind {
			case "goal":
				saveGoal(key, func(st *goals.GoalState) { st.Goal = "A visible replacement goal" })
			case "loop":
				saveLoop(key, func(st *agent.SessionLoopState) { st.Prompt = "A visibly changed loop prompt" })
			default:
				saveHeartbeat(key, func(st *agent.HeartbeatState) { st.Prompt = "A visibly changed heartbeat prompt" })
			}
			if readControl(t, sid)["revision"] == before["revision"] {
				t.Fatal("revision unchanged")
			}
		})
	}
}

// Transport stalling every frame of one session until released, recording the session of each frame written
type stallingTransport struct {
	stallSID string
	stalled  chan struct{}
	release  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	written  []string
}

func (s *stallingTransport) Write(obj *pyjson.Object) bool {
	params, _ := obj.Get("params")
	frameSID := ""
	if p, isObj := params.(*pyjson.Object); isObj {
		v, _ := p.Get("session_id")
		frameSID, _ = v.(string)
	}
	if frameSID == s.stallSID {
		s.once.Do(func() { close(s.stalled) })
		select {
		case <-s.release:
		case <-time.After(5 * time.Second):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, frameSID)
	return true
}

func (s *stallingTransport) Close() {}

// test_session_control.py TestUpdatePublication test_a_stalled_client_write_never_delays_another_sessions_publish
func TestAStalledClientWriteNeverDelaysAnotherSessionsPublish(t *testing.T) {
	sid, key := controlSession(t)
	saveGoal(key, nil)
	entry := tuihost.SessionRecord(sid)
	otherSID, otherKey := fmt.Sprintf("sid-control-other-%d", time.Now().UnixNano()), fmt.Sprintf("control-other-%d", time.Now().UnixNano())
	other := tuihost.NewSession(map[string]any{"session_key": otherKey, "history": []any{}, "history_version": 0,
		"running": false, "attached_images": []any{}, "cols": 120, "agent": nil})
	tuihost.SetSessionRecord(otherSID, other)
	saveGoal(otherKey, nil)
	transport := &stallingTransport{stallSID: sid, stalled: make(chan struct{}), release: make(chan struct{})}
	ctx := tuihost.BindTransport(context.Background(), transport)
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		publishSessionControlSnapshot(ctx, sid, entry, false, false)
	}()
	defer func() {
		close(transport.release)
		<-slowDone
		tuihost.SetSessionRecord(otherSID, nil)
		goals.NewGoalManager(context.Background(), otherKey, 0).Clear()
	}()
	select {
	case <-transport.stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the first publish never reached its write")
	}
	fastDone := make(chan struct{})
	go func() {
		defer close(fastDone)
		publishSessionControlSnapshot(ctx, otherSID, other, false, false)
	}()
	select {
	case <-fastDone:
	case <-time.After(2 * time.Second):
		t.Fatal("another session's stalled write blocked this publish")
	}
	transport.mu.Lock()
	written := append([]string(nil), transport.written...)
	transport.mu.Unlock()
	if !reflect.DeepEqual(written, []string{otherSID}) {
		t.Fatalf("written %v", written)
	}
}
