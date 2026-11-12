package tuiserver

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Owner record and transport of the snapshot cases with the subagent and delegation registries emptied
func subagentSnapshotRuntime(t *testing.T) (*tuigateway.Session, *frameRecorder) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	agent.ClearActiveSubagents()
	tools.ResetAsyncDelegationForTests()
	transport := &frameRecorder{}
	owner := tuigateway.NewSession(map[string]any{"session_key": "parent", "history": []any{}, "transport": transport})
	tuigateway.SetSessionRecord("ui-owner", owner)
	t.Cleanup(func() {
		tuigateway.SetSessionRecord("ui-owner", nil)
		agent.ClearActiveSubagents()
		tools.ResetAsyncDelegationForTests()
	})
	return owner, transport
}

// One RPC of the owner session through the real dispatcher on the given transport
func subagentSnapshotCall(via tuigateway.Transport, method string, params map[string]any) map[string]any {
	merged := map[string]any{"session_id": "ui-owner"}
	for key, value := range params {
		merged[key] = value
	}
	return pyjson.Plain(Dispatch(context.Background(), map[string]any{"id": 1, "method": method, "params": merged}, via)).(map[string]any)
}

// Result of one owner RPC, failing the case on an error reply
func subagentSnapshotResult(t *testing.T, via tuigateway.Transport, method string, params map[string]any) map[string]any {
	t.Helper()
	reply := subagentSnapshotCall(via, method, params)
	result, isDict := reply["result"].(map[string]any)
	if !isDict {
		t.Fatalf("%s answered %v", method, reply)
	}
	return result
}

func TestSnapshotProjectsOnlyThisSessionsRuntimeRecords(t *testing.T) {
	owner, transport := subagentSnapshotRuntime(t)
	ctx := context.Background()
	release, finished := make(chan struct{}), make(chan struct{})
	handoff, model := "private handoff", "test"
	env := &tools.Env{Home: home.Process(), Values: &sync.Map{}}
	dispatch, err := tools.DispatchAsyncDelegationBatch(ctx, env, tools.AsyncDispatch{Goals: []string{"owned task"}, Context: &handoff,
		Role: "leaf", Model: &model, SessionKey: "parent", OriginUISessionID: "ui-owner",
		Runner: func(context.Context, *tools.Env) (map[string]any, error) {
			defer close(finished)
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				t.Error("the runner was never released")
			}
			return map[string]any{"results": []any{}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	did, _ := dispatch["delegation_id"].(string)
	child := agent.NewSubagentChildForTests("child", 1, did, "test", "")
	agent.RegisterChild(ctx, child, nil, "owned task", "ui-owner", transport, owner)
	foreign := agent.NewSubagentChildForTests("foreign", 1, "", "test", "")
	agent.RegisterChild(ctx, foreign, nil, "foreign secret", "other", transport, tuigateway.NewSession(map[string]any{}))
	progress := agent.BuildChildProgressCallback(0, "owned task",
		agent.NewProgressParentForTests(func(string, string, string, map[string]any, map[string]any) {}), "child")
	agent.FireToolProgress(progress, "tool.started", "read_file")
	agent.FireToolProgress(progress, "tool.completed", "read_file")
	defer func() {
		agent.UnregisterSubagent("child", nil)
		agent.UnregisterSubagent("foreign", nil)
		close(release)
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("the runner never finished")
		}
		for deadline := time.Now().Add(10 * time.Second); tools.AsyncDelegationActiveCount() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}()
	snapshot := subagentSnapshotResult(t, transport, "subagent.list", nil)
	subagents, _ := snapshot["subagents"].([]any)
	var ids []any
	for _, row := range subagents {
		ids = append(ids, row.(map[string]any)["subagent_id"])
	}
	if !pyjson.Equal(ids, []any{"child"}) {
		t.Fatalf("subagents %v", snapshot)
	}
	first := subagents[0].(map[string]any)
	if first["last_tool"] != "read_file" || !pyjson.Equal(first["tool_count"], 1) || !pyjson.Equal(snapshot["delegations"], []any{}) {
		t.Fatalf("snapshot %v", snapshot)
	}
	wire, _ := json.Marshal(snapshot)
	for _, secret := range []string{"private handoff", "foreign secret", "owner_transport", "session_key"} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("%q leaked in %s", secret, wire)
		}
	}
	if _, failed := subagentSnapshotCall(&frameRecorder{}, "subagent.list", nil)["error"]; !failed {
		t.Fatal("a foreign transport read the roster")
	}
	if _, failed := subagentSnapshotCall(transport, "subagent.list", map[string]any{"session_id": "missing"})["error"]; !failed {
		t.Fatal("a missing session read the roster")
	}
	tuigateway.SetSessionRecord("ui-owner", tuigateway.NewSession(owner.Fields()))
	if got := subagentSnapshotResult(t, transport, "subagent.list", nil)["subagents"]; !pyjson.Equal(got, []any{}) {
		t.Fatalf("a replaced record still owns %v", got)
	}
}

func TestLiveTailAndSteerShareExactOwnerAndEndWithChild(t *testing.T) {
	owner, transport := subagentSnapshotRuntime(t)
	ctx := context.Background()
	writer := agent.NewLiveTranscriptWriter(ctx, "deleg-rpc", 0, "owned task")
	child := agent.NewSubagentChildForTests("child", 1, "", "test", writer.Path())
	agent.RegisterChild(ctx, child, nil, "owned task", "ui-owner", transport, owner)
	defer agent.UnregisterSubagent("child", nil)
	tail := func(via tuigateway.Transport) map[string]any {
		return subagentSnapshotResult(t, via, "subagent.tail", map[string]any{"subagent_id": "child"})
	}
	writer.Event("tool", strings.Repeat("x", 20000))
	writer.ToolResult("read_file", "first result")
	first := tail(transport)
	text, _ := first["text"].(string)
	if first["available"] != true || first["truncated"] != true || len(text) > 16384 || !strings.Contains(text, "first result") {
		t.Fatalf("tail %v (%d bytes)", first["available"], len(text))
	}
	writer.ToolResult("read_file", "new live output")
	if text, _ := tail(transport)["text"].(string); !strings.Contains(text, "new live output") {
		t.Fatalf("tail missed the new output")
	}
	queued := subagentSnapshotResult(t, transport, "subagent.steer", map[string]any{"subagent_id": "child", "text": "change course"})
	if _, delivered := queued["delivered"]; queued["status"] != "queued" || delivered {
		t.Fatalf("steer %v", queued)
	}
	if got := agent.CloseSubagentSteering("child", child); got != "change course" {
		t.Fatalf("closed steering answered %q", got)
	}
	if got := subagentSnapshotResult(t, transport, "subagent.steer", map[string]any{"subagent_id": "child", "text": "too late"}); got["status"] != "rejected" {
		t.Fatalf("late steer %v", got)
	}
	if _, failed := subagentSnapshotCall(&frameRecorder{}, "subagent.tail", map[string]any{"subagent_id": "child"})["error"]; !failed {
		t.Fatal("a foreign transport read the tail")
	}
	tuigateway.SetSessionRecord("ui-owner", tuigateway.NewSession(owner.Fields()))
	if got := tail(transport); got["available"] != false {
		t.Fatalf("a replaced record read the tail %v", got["available"])
	}
	tuigateway.SetSessionRecord("ui-owner", owner)
	agent.UnregisterSubagent("child", nil)
	if got := tail(transport); !pyjson.Equal(got, map[string]any{"subagent_id": "child", "available": false, "text": "", "truncated": false}) {
		t.Fatalf("retired child tail %v", got)
	}
}

func TestInterruptRequiresExactLiveOwnerButDirectHelperStaysLegacy(t *testing.T) {
	owner, transport := subagentSnapshotRuntime(t)
	child := agent.NewSubagentChildForTests("child", 1, "", "test", "")
	agent.RegisterChild(context.Background(), child, nil, "owned", "ui-owner", transport, owner)
	defer agent.UnregisterSubagent("child", nil)
	refusals := []struct {
		via    tuigateway.Transport
		params map[string]any
	}{{transport, map[string]any{"session_id": ""}}, {transport, map[string]any{"session_id": "missing"}}, {&frameRecorder{}, nil}}
	for _, refusal := range refusals {
		params := map[string]any{"subagent_id": "child"}
		for key, value := range refusal.params {
			params[key] = value
		}
		reply := subagentSnapshotCall(refusal.via, "subagent.interrupt", params)
		if _, failed := reply["error"]; !failed && reply["result"].(map[string]any)["found"] != false {
			t.Fatalf("refused interrupt answered %v", reply)
		}
		if child.IsInterrupted() {
			t.Fatalf("a refused interrupt %v stopped the child", refusal.params)
		}
	}
	tuigateway.SetSessionRecord("foreign", tuigateway.NewSession(owner.Fields()))
	t.Cleanup(func() { tuigateway.SetSessionRecord("foreign", nil) })
	if got := subagentSnapshotResult(t, transport, "subagent.interrupt", map[string]any{"session_id": "foreign", "subagent_id": "child"}); got["found"] != false {
		t.Fatalf("foreign record interrupt %v", got)
	}
	tuigateway.SetSessionRecord("ui-owner", tuigateway.NewSession(owner.Fields()))
	if got := subagentSnapshotResult(t, transport, "subagent.interrupt", map[string]any{"subagent_id": "child"}); got["found"] != false || child.IsInterrupted() {
		t.Fatalf("replaced record interrupt %v", got)
	}
	tuigateway.SetSessionRecord("ui-owner", owner)
	if got := subagentSnapshotResult(t, transport, "subagent.interrupt", map[string]any{"subagent_id": "child"}); got["found"] != true || !child.IsInterrupted() {
		t.Fatalf("owner interrupt %v", got)
	}
	child.ClearInterrupt()
	if !agent.InterruptSubagent("child") || !child.IsInterrupted() {
		t.Fatal("the direct helper did not stop the child")
	}
	agent.UnregisterSubagent("child", nil)
	if got := subagentSnapshotResult(t, transport, "subagent.interrupt", map[string]any{"subagent_id": "child"}); got["found"] != false {
		t.Fatalf("retired child interrupt %v", got)
	}
}

// Subagent ids of the roster one transport reads for the owner session
func subagentSnapshotIDs(t *testing.T, via tuigateway.Transport) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	rows, _ := subagentSnapshotResult(t, via, "subagent.list", nil)["subagents"].([]any)
	for _, row := range rows {
		id, _ := row.(map[string]any)["subagent_id"].(string)
		ids[id] = true
	}
	return ids
}

func TestReattachPreservesChildControlsIncludingLateRegistration(t *testing.T) {
	owner, old := subagentSnapshotRuntime(t)
	ctx := context.Background()
	fresh := &frameRecorder{}
	transcript := filepath.Join(t.TempDir(), "child.txt")
	if err := os.WriteFile(transcript, []byte("live child output"), 0o600); err != nil {
		t.Fatal(err)
	}
	children := map[string]*agent.AIAgent{}
	register := func(sid string) {
		children[sid] = agent.NewSubagentChildForTests(sid, 1, "", "test", transcript)
		agent.RegisterChild(ctx, children[sid], nil, "owned", "ui-owner", old, owner)
	}
	defer func() {
		agent.UnregisterSubagent("before", nil)
		agent.UnregisterSubagent("after", nil)
	}()
	register("before")
	historyLock := &sync.Mutex{}
	owner.Set("transport", tuigateway.DetachedWSTransport())
	owner.Set("history_lock", historyLock)
	rebind := func(transport tuigateway.Transport, refuseCheck bool) {
		sessionResumeLock.Lock()
		historyLock.Lock()
		defer sessionResumeLock.Unlock()
		defer historyLock.Unlock()
		if refuseCheck {
			if refusal := reattachRefusal(1, "ui-owner", owner); refusal != nil {
				t.Fatalf("reattach refused %v", refusal)
			}
		}
		rebindLiveTransport(ctx, "ui-owner", owner, transport)
	}
	rebind(fresh, true)
	register("after")
	both := map[string]bool{"before": true, "after": true}
	if got := subagentSnapshotIDs(t, fresh); !maps.Equal(got, both) {
		t.Fatalf("roster after reattach %v", got)
	}
	popup := &frameRecorder{}
	rebind(popup, false)
	for _, peer := range []tuigateway.Transport{fresh, popup} {
		if got := subagentSnapshotIDs(t, peer); !maps.Equal(got, both) {
			t.Fatalf("peer roster %v", got)
		}
		if got := subagentSnapshotResult(t, peer, "subagent.tail", map[string]any{"subagent_id": "before"})["text"]; got != "live child output" {
			t.Fatalf("peer tail %v", got)
		}
	}
	if closed, ended := closeSessionsForTransport(ctx, popup, "ws_disconnect"); closed != 0 || ended != 0 {
		t.Fatalf("closing the popup closed %d ended %d", closed, ended)
	}
	if !tuigateway.SessionTransportContains(owner, fresh) || tuigateway.SessionTransportContains(owner, popup) {
		t.Fatal("the survivor did not keep control")
	}
	if got := subagentSnapshotIDs(t, fresh); !maps.Equal(got, both) {
		t.Fatalf("survivor roster %v", got)
	}
	for _, sid := range []string{"before", "after"} {
		if got := subagentSnapshotResult(t, fresh, "subagent.tail", map[string]any{"subagent_id": sid})["text"]; got != "live child output" {
			t.Fatalf("%s tail %v", sid, got)
		}
		if got := subagentSnapshotResult(t, fresh, "subagent.steer", map[string]any{"subagent_id": sid, "text": sid})["status"]; got != "queued" {
			t.Fatalf("%s steer %v", sid, got)
		}
		if got := subagentSnapshotResult(t, fresh, "subagent.interrupt", map[string]any{"subagent_id": sid})["found"]; got != true {
			t.Fatalf("%s interrupt %v", sid, got)
		}
	}
	for _, method := range []string{"list", "tail", "steer", "interrupt"} {
		denied := subagentSnapshotCall(old, "subagent."+method, map[string]any{"subagent_id": "before", "text": "old"})
		if _, failed := denied["error"]; !failed && denied["result"].(map[string]any)["status"] != "rejected" {
			t.Fatalf("old transport %s answered %v", method, denied)
		}
	}
	for _, sid := range []string{"before", "after"} {
		if !children[sid].IsInterrupted() {
			t.Fatalf("%s was not stopped", sid)
		}
		if got := agent.CloseSubagentSteering(sid, children[sid]); got != sid {
			t.Fatalf("%s steering held %q", sid, got)
		}
	}
}

func TestReattachDoesNotAdoptForeignOrRetiredGenerations(t *testing.T) {
	owner, old := subagentSnapshotRuntime(t)
	ctx := context.Background()
	fresh := &frameRecorder{}
	children := map[string]*agent.AIAgent{}
	for _, row := range []struct {
		sid, sessionID string
		record         *tuigateway.Session
	}{{"foreign", "other", owner}, {"retired", "ui-owner", tuigateway.NewSession(owner.Fields())}} {
		children[row.sid] = agent.NewSubagentChildForTests(row.sid, 1, "", "test", "")
		agent.RegisterChild(ctx, children[row.sid], nil, "private", row.sessionID, old, row.record)
	}
	defer func() {
		agent.UnregisterSubagent("foreign", nil)
		agent.UnregisterSubagent("retired", nil)
	}()
	func() {
		sessionResumeLock.Lock()
		defer sessionResumeLock.Unlock()
		if code := rpcCode(reattachRefusal(1, "ui-owner", tuigateway.NewSession(owner.Fields()))); code != 4007 {
			t.Fatalf("copied record refusal code %d", code)
		}
		owner.Set("_client_gone_interrupt_requested", true)
		if code := rpcCode(reattachRefusal(1, "ui-owner", owner)); code != 4009 {
			t.Fatalf("client gone refusal code %d", code)
		}
		owner.Pop("_client_gone_interrupt_requested")
		rebindLiveTransport(ctx, "ui-owner", owner, fresh)
	}()
	if got := subagentSnapshotResult(t, fresh, "subagent.list", nil)["subagents"]; !pyjson.Equal(got, []any{}) {
		t.Fatalf("roster adopted %v", got)
	}
	for _, sid := range []string{"foreign", "retired"} {
		if got := subagentSnapshotResult(t, fresh, "subagent.tail", map[string]any{"subagent_id": sid})["available"]; got != false {
			t.Fatalf("%s tail available %v", sid, got)
		}
		if got := subagentSnapshotResult(t, fresh, "subagent.steer", map[string]any{"subagent_id": sid, "text": "deny"})["status"]; got != "rejected" {
			t.Fatalf("%s steer %v", sid, got)
		}
		if got := subagentSnapshotResult(t, fresh, "subagent.interrupt", map[string]any{"subagent_id": sid})["found"]; got != false {
			t.Fatalf("%s interrupt %v", sid, got)
		}
	}
	for sid, child := range children {
		if child.IsInterrupted() || agent.CloseSubagentSteering(sid, child) != "" {
			t.Fatalf("%s was steered or stopped", sid)
		}
	}
}

func TestAnyAttachPathCarriesSubagentAuthorityWithoutRegistrySync(t *testing.T) {
	owner, old := subagentSnapshotRuntime(t)
	fresh := &frameRecorder{}
	child := agent.NewSubagentChildForTests("child", 1, "", "test", "")
	agent.RegisterChild(context.Background(), child, nil, "owned", "ui-owner", old, owner)
	defer agent.UnregisterSubagent("child", nil)
	owner.Set("transport", tuigateway.DetachedWSTransport())
	if !tuigateway.AttachSessionTransport(owner, fresh) {
		t.Fatal("the attach was refused")
	}
	if got := subagentSnapshotIDs(t, fresh); !maps.Equal(got, map[string]bool{"child": true}) {
		t.Fatalf("roster %v", got)
	}
	if got := subagentSnapshotResult(t, fresh, "subagent.steer", map[string]any{"subagent_id": "child", "text": "go"})["status"]; got != "queued" {
		t.Fatalf("steer %v", got)
	}
	if got := subagentSnapshotResult(t, fresh, "subagent.interrupt", map[string]any{"subagent_id": "child"})["found"]; got != true {
		t.Fatalf("interrupt %v", got)
	}
	for _, method := range []string{"list", "steer", "interrupt"} {
		denied := subagentSnapshotCall(old, "subagent."+method, map[string]any{"subagent_id": "child", "text": "stale"})
		if _, failed := denied["error"]; !failed && denied["result"].(map[string]any)["status"] != "rejected" {
			t.Fatalf("detached transport %s answered %v", method, denied)
		}
	}
	if !child.IsInterrupted() || agent.CloseSubagentSteering("child", child) != "go" {
		t.Fatal("the attached transport did not steer and stop the child exactly once")
	}
}
