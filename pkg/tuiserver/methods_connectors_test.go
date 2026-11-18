package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

const connectorRPCSID = "op-rpc-session"

// Owner and stranger transports with a live TUI session the owner steers
func connectorOwned(t *testing.T) (*frameRecorder, *frameRecorder, *tuihost.Session) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tools.LiveResetForTests()
	owner, stranger := &frameRecorder{}, &frameRecorder{}
	session := tuihost.NewSession(map[string]any{"transport": owner, "agent": nil, "session_key": connectorRPCSID,
		"history": []any{}, "history_lock": &sync.Mutex{}, "history_version": 0, "running": false, "attached_images": []any{},
		"source": "tui"})
	tuihost.SetSessionRecord(connectorRPCSID, session)
	savedHome := icebathHome
	icebathHome = home.Process()
	t.Cleanup(func() {
		icebathHome = savedHome
		tuihost.SetSessionRecord(connectorRPCSID, nil)
		tools.LiveResetForTests()
	})
	return owner, stranger, session
}

// Frames of a recorder decoded into plain values
func recordedFrames(r *frameRecorder) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, 0, len(r.frames))
	for _, frame := range r.frames {
		out = append(out, pyjson.Plain(frame).(map[string]any))
	}
	return out
}

// Session RPC through the real dispatcher, a pool handler's reply read back from the client transport
func connectorRPCCall(t *testing.T, client *frameRecorder, method string, params map[string]any) map[string]any {
	t.Helper()
	merged := map[string]any{"owner": map[string]any{"type": "session", "session_id": connectorRPCSID}}
	for key, value := range params {
		merged[key] = value
	}
	before := client.count()
	response := Dispatch(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": merged}, client)
	if response != nil {
		return pyjson.Plain(response).(map[string]any)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		frames := recordedFrames(client)
		for i := len(frames) - 1; i >= before; i-- {
			if id, _ := frames[i]["id"].(int); id == 7 {
				return frames[i]
			}
			if id, _ := frames[i]["id"].(float64); id == 7 {
				return frames[i]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no reply to %s", method)
	return nil
}

// Event payloads of one kind the client received, waiting until at least expect arrived
func connectorEvents(client *frameRecorder, kind string, expect int) []map[string]any {
	collect := func() []map[string]any {
		var found []map[string]any
		for _, frame := range recordedFrames(client) {
			params, _ := frame["params"].(map[string]any)
			if frame["method"] == "event" && params["type"] == kind {
				found = append(found, params)
			}
		}
		return found
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if found := collect(); len(found) >= expect {
			return found
		}
		time.Sleep(10 * time.Millisecond)
	}
	return collect()
}

// Two pending MCP authorize targets opened as the session's live operation
func openConnectorOp(t *testing.T) *tools.ConnectionOperation {
	t.Helper()
	operation := tools.NewConnectionOperation([]*tools.ConnTarget{tools.NewConnTarget("gmail", "mcp", "authorize"),
		tools.NewConnTarget("notion", "mcp", "authorize")}, connectorRPCSID, "", nil)
	if err := tools.LiveOpen(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	return operation
}

// Transition that must be legal for the case to mean anything
func mustTransition(t *testing.T, operation *tools.ConnectionOperation, name string, to tools.TargetState, actor tools.ConnActor, fields tools.TransitionFields) {
	t.Helper()
	if _, err := operation.Transition(name, to, actor, fields); err != nil {
		t.Fatal(err)
	}
}

func strPtr(s string) *string { return &s }

func rpcCode(response map[string]any) int {
	failure, _ := response["error"].(map[string]any)
	switch code := failure["code"].(type) {
	case float64:
		return int(code)
	case int:
		return code
	case int64:
		return int(code)
	}
	return 0
}

// OAuth attempt of the fake MCP backend, pending until the case approves it
type fakeMCPAttempt struct {
	mu     sync.Mutex
	url    string
	status string
}

func (a *fakeMCPAttempt) AuthURL() string            { return a.url }
func (a *fakeMCPAttempt) Detail() string             { return "" }
func (a *fakeMCPAttempt) Flow() *mcp.ClientOAuthFlow { return nil }

// Approved attempts carry a discovery error so no server is dialed
func (a *fakeMCPAttempt) Poll() mcp.AttemptPoll {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == "approved" {
		return mcp.AttemptPoll{Status: a.status, DiscoveryError: "tools are listed once the server is reachable"}
	}
	return mcp.AttemptPoll{Status: a.status}
}

// MCP backend whose OAuth starts are recorded by server name, a start failing with the reason set for its name
type fakeMCPBackend struct {
	mu       sync.Mutex
	base     string
	starts   []string
	failures map[string]string
	attempts map[string]*fakeMCPAttempt
}

func newFakeMCPBackend(base string) *fakeMCPBackend {
	return &fakeMCPBackend{base: base, failures: map[string]string{}, attempts: map[string]*fakeMCPAttempt{}}
}

func (b *fakeMCPBackend) RequiredEnv(context.Context, string) ([]*pyjson.Object, error) {
	return nil, nil
}
func (b *fakeMCPBackend) InstallsWithOAuth(context.Context, string) (bool, error) { return true, nil }
func (b *fakeMCPBackend) Install(context.Context, string, *pyjson.Object) ([]string, error) {
	return nil, errors.New("install is not driven by these cases")
}
func (b *fakeMCPBackend) Enable(context.Context, string) error { return nil }
func (b *fakeMCPBackend) StartInstallOAuth(ctx context.Context, name string, _ *pyjson.Object) (mcp.ConnectorAttempt, error) {
	return b.StartOAuth(ctx, name)
}

// Link numbered by the server's start count, or the failure set for the server
func (b *fakeMCPBackend) StartOAuth(_ context.Context, name string) (mcp.ConnectorAttempt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts = append(b.starts, name)
	if reason := b.failures[name]; reason != "" {
		return nil, errors.New(reason)
	}
	count := 0
	for _, started := range b.starts {
		if started == name {
			count++
		}
	}
	attempt := &fakeMCPAttempt{url: fmt.Sprintf("%s%s/%d", b.base, name, count), status: "pending"}
	b.attempts[name] = attempt
	return attempt, nil
}

// Server names of every OAuth start so far
func (b *fakeMCPBackend) started() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.starts)
}

// Sets or clears the failure of the next starts of one server
func (b *fakeMCPBackend) fail(name, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures[name] = reason
}

// Approves the server's open attempt
func (b *fakeMCPBackend) setConnected(name string) {
	b.mu.Lock()
	attempt := b.attempts[name]
	b.mu.Unlock()
	attempt.mu.Lock()
	attempt.status = "approved"
	attempt.mu.Unlock()
}

// Configures gmail and notion as MCP servers of the case home so authorize accepts them
func configureConnectorServers(t *testing.T) {
	t.Helper()
	body := "mcp_servers:\n  gmail:\n    url: https://gmail.example/mcp\n  notion:\n    url: https://notion.example/mcp\n"
	if err := os.WriteFile(filepath.Join(home.Process(), "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Card authorize of gmail and notion on the RPC session, returned once its card is drawn and settled at cleanup
func driveConnectorOp(t *testing.T, backend *fakeMCPBackend) *tools.ConnectionOperation {
	t.Helper()
	configureConnectorServers(t)
	drawn, finished := make(chan struct{}), make(chan struct{})
	ctx, env := setSessionContext(context.Background(), connectorRPCSID, nil, "")
	go func() {
		defer close(finished)
		defer clearSessionContext(env)
		tools.ManageConnections(ctx, env, map[string]any{"action": "authorize", "connectors": []any{"gmail", "notion"}},
			tools.ManageOptions{MCPBackend: backend, SessionID: connectorRPCSID, Callback: func(*pyjson.Object) string {
				close(drawn)
				return ""
			}})
	}()
	select {
	case <-drawn:
	case <-finished:
		t.Fatal("the operation ended before its card was drawn")
	case <-time.After(5 * time.Second):
		t.Fatal("no card drawn")
	}
	operation := tools.LiveCurrent(connectorRPCSID, "")
	t.Cleanup(func() {
		operation.Settle(tools.SettleContinue)
		operation.Wake.Set()
		<-finished
	})
	return operation
}

// test_connector_operation_rpc.py test_operation_status_returns_the_live_snapshot_with_the_server_deadline
func TestOperationStatusReturnsTheLiveSnapshotWithTheServerDeadline(t *testing.T) {
	owner, _, _ := connectorOwned(t)
	operation := openConnectorOp(t)
	mustTransition(t, operation, "gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{ConnectURL: strPtr("https://l/gmail")})
	reply := connectorRPCCall(t, owner, "connectors.operation.status", map[string]any{"op_id": operation.OpID})
	result, isDict := reply["result"].(map[string]any)
	if !isDict {
		t.Fatalf("reply %v", reply)
	}
	if result["op_id"] != operation.OpID || result["deadline_at"] != operation.DeadlineAt {
		t.Fatalf("result %v", result)
	}
	if result["settled"] != false || result["settled_by"] != nil {
		t.Fatalf("settled fields %v", result)
	}
	by := map[string]map[string]any{}
	for _, item := range result["targets"].([]any) {
		target := item.(map[string]any)
		by[target["name"].(string)] = target
	}
	if by["gmail"]["state"] != "initiated" || by["gmail"]["connect_url"] != "https://l/gmail" || by["notion"]["state"] != "pending" {
		t.Fatalf("targets %v", by)
	}
}

// test_connector_operation_rpc.py test_operation_status_is_owner_only_and_validates_params
func TestOperationStatusIsOwnerOnlyAndValidatesParams(t *testing.T) {
	owner, stranger, _ := connectorOwned(t)
	operation := openConnectorOp(t)
	if code := rpcCode(connectorRPCCall(t, stranger, "connectors.operation.status", map[string]any{"op_id": operation.OpID})); code != 4001 {
		t.Fatalf("stranger code %v", code)
	}
	if code := rpcCode(connectorRPCCall(t, owner, "connectors.operation.status", nil)); code != 4000 {
		t.Fatalf("missing op_id code %v", code)
	}
	if code := rpcCode(connectorRPCCall(t, owner, "connectors.operation.status", map[string]any{"op_id": "nope"})); code != 4004 {
		t.Fatalf("unknown op code %v", code)
	}
}

// test_connector_operation_rpc.py test_respond_drives_the_live_operation_and_emits_update
func TestRespondDrivesTheLiveOperationAndEmitsUpdate(t *testing.T) {
	owner, stranger, _ := connectorOwned(t)
	operation := openConnectorOp(t)
	mustTransition(t, operation, "gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{})
	answer := map[string]any{"targets": []any{map[string]any{"name": "notion", "status": "skipped"}}}
	foreign := connectorRPCCall(t, stranger, "connection.respond", map[string]any{"op_id": operation.OpID, "result": answer})
	if code := rpcCode(foreign); code != 4001 {
		t.Fatalf("foreign %v", foreign)
	}
	if state := operation.StateOf(operation.Target("notion")); state != tools.TargetPending {
		t.Fatalf("notion moved to %s by a stranger", state)
	}
	reply := connectorRPCCall(t, owner, "connection.respond", map[string]any{"op_id": operation.OpID, "result": answer})
	if _, isResult := reply["result"]; !isResult {
		t.Fatalf("reply %v", reply)
	}
	if state := operation.StateOf(operation.Target("notion")); state != tools.TargetSkipped {
		t.Fatalf("notion state %s", state)
	}
	if !operation.Wake.IsSet() {
		t.Fatal("wake not set")
	}
	updates := connectorEvents(owner, "connection.update", 2)
	if len(updates) == 0 {
		t.Fatal("no connection.update")
	}
	last := updates[len(updates)-1]["payload"].(map[string]any)
	if last["target"] != "notion" || last["to"] != "skipped" || last["actor"] != "user" || last["op_id"] != operation.OpID {
		t.Fatalf("last update %v", last)
	}
}

// test_connector_operation_rpc.py test_respond_cannot_claim_an_outcome_for_any_target
func TestRespondCannotClaimAnOutcomeForAnyTarget(t *testing.T) {
	owner, _, _ := connectorOwned(t)
	operation := openConnectorOp(t)
	mustTransition(t, operation, "gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{})
	reply := connectorRPCCall(t, owner, "connection.respond", map[string]any{"op_id": operation.OpID, "result": map[string]any{
		"targets": []any{map[string]any{"name": "gmail", "status": "connected"}, map[string]any{"name": "notion", "status": "failed"}}}})
	if code := rpcCode(reply); code != 4000 {
		t.Fatalf("reply %v", reply)
	}
	if operation.StateOf(operation.Target("gmail")) != tools.TargetInitiated || operation.StateOf(operation.Target("notion")) != tools.TargetPending {
		t.Fatal("a refused claim moved a target")
	}
}

// test_connector_operation_rpc.py test_respond_continue_settles_and_emits_the_settlement_update
func TestRespondContinueSettlesAndEmitsTheSettlementUpdate(t *testing.T) {
	owner, _, _ := connectorOwned(t)
	operation := openConnectorOp(t)
	reply := connectorRPCCall(t, owner, "connection.respond", map[string]any{"op_id": operation.OpID, "result": map[string]any{"settled_by": "continue"}})
	if _, isResult := reply["result"]; !isResult {
		t.Fatalf("reply %v", reply)
	}
	if !operation.Settled() || operation.SettledBy() != "continue" {
		t.Fatalf("settled %v by %v", operation.Settled(), operation.SettledBy())
	}
	var settled []map[string]any
	for _, update := range connectorEvents(owner, "connection.update", 1) {
		if payload := update["payload"].(map[string]any); payload["settled"] == true {
			settled = append(settled, payload)
		}
	}
	if len(settled) == 0 || settled[len(settled)-1]["settled_by"] != "continue" {
		t.Fatalf("settlement updates %v", settled)
	}
}

// test_connector_operation_rpc.py test_pending_connection_on_resume_comes_from_the_live_registry
func TestPendingConnectionOnResumeComesFromTheLiveRegistry(t *testing.T) {
	connectorOwned(t)
	operation := openConnectorOp(t)
	payload := pendingConnectionRequestPayload(connectorRPCSID)
	if payload["op_id"] != operation.OpID || payload["deadline_at"] != operation.DeadlineAt {
		t.Fatalf("payload %v", payload)
	}
	tools.LiveClose(operation)
	if payload := pendingConnectionRequestPayload(connectorRPCSID); payload != nil {
		t.Fatalf("closed operation still pending %v", payload)
	}
}

// test_connector_operation_rpc.py test_panel_connect_reissues_only_a_dead_link
func TestPanelConnectReissuesOnlyADeadLink(t *testing.T) {
	owner, _, _ := connectorOwned(t)
	backend := newFakeMCPBackend("https://l/")
	backend.fail("notion", "vendor: nope")
	operation := driveConnectorOp(t, backend)
	if operation.StateOf(operation.Target("gmail")) != tools.TargetInitiated || operation.StateOf(operation.Target("notion")) != tools.TargetFailed {
		t.Fatalf("gmail %s notion %s", operation.StateOf(operation.Target("gmail")), operation.StateOf(operation.Target("notion")))
	}
	before := len(backend.started())
	refused := connectorRPCCall(t, owner, "connectors.connect", map[string]any{"connectors": []any{"gmail"}, "reconnect": true})
	if code := rpcCode(refused); code != 4002 || len(backend.started()) != before {
		t.Fatalf("refused %v starts %v", refused, backend.started())
	}
	if url := operation.Target("gmail").ConnectURL; url != "https://l/gmail/1" {
		t.Fatalf("gmail link %q", url)
	}
	backend.fail("notion", "")
	reply := connectorRPCCall(t, owner, "connectors.connect", map[string]any{"connectors": []any{"notion"}, "reconnect": true})
	if _, isResult := reply["result"]; !isResult {
		t.Fatalf("reply %v", reply)
	}
	if starts := backend.started()[before:]; strings.Join(starts, ",") != "notion" {
		t.Fatalf("starts %v", starts)
	}
	target := operation.Target("notion")
	if operation.StateOf(target) != tools.TargetInitiated || target.ConnectURL != "https://l/notion/2" {
		t.Fatalf("notion %s %q", operation.StateOf(target), target.ConnectURL)
	}
}

// test_connector_operation_rpc.py test_a_failed_reissue_leaves_the_row_failed_with_no_link
func TestAFailedReissueLeavesTheRowFailedWithNoLink(t *testing.T) {
	owner, _, _ := connectorOwned(t)
	backend := newFakeMCPBackend("https://l/")
	backend.fail("notion", "vendor: nope")
	operation := driveConnectorOp(t, backend)
	backend.fail("notion", "vendor: still no")
	connectorRPCCall(t, owner, "connectors.connect", map[string]any{"connectors": []any{"notion"}, "reconnect": true})
	target := operation.Target("notion")
	if operation.StateOf(target) != tools.TargetFailed || target.ConnectURL != "" || target.Detail != "vendor: still no" {
		t.Fatalf("notion %s %q %q", operation.StateOf(target), target.ConnectURL, target.Detail)
	}
}

const connectorE2ESID = "connector-operation-e2e"

// Owner and stranger transports of a TUI session in a fresh home
func connectorE2ESession(t *testing.T) (*frameRecorder, *frameRecorder) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tools.LiveResetForTests()
	owner, stranger := &frameRecorder{}, &frameRecorder{}
	session := tuihost.NewSession(map[string]any{"transport": owner, "agent": nil, "session_key": connectorE2ESID,
		"history": []any{}, "history_lock": &sync.Mutex{}, "history_version": 0, "running": false, "attached_images": []any{},
		"source": "tui"})
	tuihost.SetSessionRecord(connectorE2ESID, session)
	t.Cleanup(func() {
		tuihost.SetSessionRecord(connectorE2ESID, nil)
		tools.LiveResetForTests()
	})
	return owner, stranger
}

// Session RPC of the e2e session through the real dispatcher
func connectorE2ERPC(t *testing.T, client *frameRecorder, method string, params map[string]any) map[string]any {
	t.Helper()
	merged := map[string]any{"owner": map[string]any{"type": "session", "session_id": connectorE2ESID}}
	for key, value := range params {
		merged[key] = value
	}
	before := client.count()
	if response := Dispatch(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": merged}, client); response != nil {
		return pyjson.Plain(response).(map[string]any)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frames := recordedFrames(client)
		for i := len(frames) - 1; i >= before; i-- {
			if id, _ := frames[i]["id"].(int); id == 7 {
				return frames[i]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no reply to %s", method)
	return nil
}

// Event payloads of one kind matching a predicate, waiting up to five seconds for the first
func connectorE2EEvents(client *frameRecorder, kind string, match func(map[string]any) bool) []map[string]any {
	collect := func() []map[string]any {
		var found []map[string]any
		for _, frame := range recordedFrames(client) {
			params, _ := frame["params"].(map[string]any)
			if frame["method"] != "event" || params["type"] != kind {
				continue
			}
			payload, _ := params["payload"].(map[string]any)
			if payload == nil {
				payload = map[string]any{}
			}
			if match(payload) {
				found = append(found, payload)
			}
		}
		return found
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if found := collect(); len(found) > 0 {
			return found
		}
		time.Sleep(10 * time.Millisecond)
	}
	return collect()
}

func targetStates(targets any) map[string]any {
	out := map[string]any{}
	for _, item := range targets.([]any) {
		target := item.(map[string]any)
		out[target["name"].(string)] = target["state"]
	}
	return out
}

// test_connector_operation_e2e.py connect settles through the callback response
func TestConnectSettlesThroughCallbackResponse(t *testing.T) {
	owner, _ := connectorE2ESession(t)
	configureConnectorServers(t)
	backend := newFakeMCPBackend("https://connect.example/")
	savedTick := tools.WatchIntervalSeconds
	tools.WatchIntervalSeconds = 0.05
	t.Cleanup(func() { tools.WatchIntervalSeconds = savedTick })
	var raw string
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ctx, env := setSessionContext(context.Background(), connectorE2ESID, nil, "")
		defer clearSessionContext(env)
		cbs := agentCbs(ctx, connectorE2ESID)
		raw = tools.ManageConnections(ctx, env, map[string]any{"action": "authorize", "connectors": []any{"gmail", "notion"}},
			tools.ManageOptions{MCPBackend: backend, SessionID: "20260914_rotated_agent_id",
				Callback: func(payload *pyjson.Object) string {
					cbs.Connection(pyjson.Plain(payload).(map[string]any))
					return ""
				}})
	}()
	requests := connectorE2EEvents(owner, "connection.request", func(map[string]any) bool { return true })
	if len(requests) != 1 {
		t.Fatalf("connection.request events %v", requests)
	}
	request := requests[0]
	if request["op_id"] == "" || len(request["targets"].([]any)) != 2 || request["deadline_at"].(float64) <= float64(time.Now().Unix()) {
		t.Fatalf("request %v", request)
	}
	statusReply := connectorE2ERPC(t, owner, "connectors.operation.status", map[string]any{"op_id": request["op_id"]})
	initiated, isResult := statusReply["result"].(map[string]any)
	if !isResult {
		t.Fatalf("status %v", statusReply)
	}
	if states := targetStates(initiated["targets"]); states["gmail"] != "initiated" || states["notion"] != "initiated" {
		t.Fatalf("states %v", states)
	}
	for _, item := range initiated["targets"].([]any) {
		target := item.(map[string]any)
		if target["connect_url"] != "https://connect.example/"+target["name"].(string)+"/1" {
			t.Fatalf("target %v", target)
		}
	}
	backend.setConnected("gmail")
	if len(connectorE2EEvents(owner, "connection.update", func(p map[string]any) bool { return p["target"] == "gmail" && p["to"] == "connected" })) == 0 {
		t.Fatal("no gmail connected update")
	}
	response := connectorE2ERPC(t, owner, "connection.respond", map[string]any{"op_id": request["op_id"],
		"result": map[string]any{"targets": []any{map[string]any{"name": "notion", "status": "skipped"}}}})
	if result, _ := response["result"].(map[string]any); len(result) != 2 || result["status"] != "ok" || result["settled"] != true {
		t.Fatalf("respond %v", response)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("connection.respond must release the callback bridge")
	}
	var settled map[string]any
	if err := json.Unmarshal([]byte(raw), &settled); err != nil {
		t.Fatalf("%v in %s", err, raw)
	}
	if settled["status"] != "settled" || settled["settled_by"] != "all_resolved" {
		t.Fatalf("settled %v", settled)
	}
	if states := targetStates(settled["targets"]); states["gmail"] != "connected" || states["notion"] != "skipped" {
		t.Fatalf("settled states %v", states)
	}
	if strings.Contains(raw, "connect_url") {
		t.Fatalf("settled result carries a link %s", raw)
	}
	if tools.LiveCurrent(connectorE2ESID, "") != nil {
		t.Fatal("settled operation still current")
	}
	if code := rpcCode(connectorE2ERPC(t, owner, "connectors.operation.status", map[string]any{"op_id": request["op_id"]})); code != 4004 {
		t.Fatalf("closed status code %v", code)
	}
	finals := connectorE2EEvents(owner, "connection.update", func(p map[string]any) bool { return p["settled"] == true })
	if len(finals) == 0 {
		t.Fatal("no settled update")
	}
	if states := targetStates(finals[len(finals)-1]["targets"]); states["gmail"] != "connected" || states["notion"] != "skipped" {
		t.Fatalf("final update states %v", states)
	}
}

// test_connector_operation_e2e.py test_cli_connect_returns_urls_without_emitting_a_card
func TestCliConnectReturnsUrlsWithoutEmittingACard(t *testing.T) {
	owner, _ := connectorE2ESession(t)
	configureConnectorServers(t)
	backend := newFakeMCPBackend("https://connect.example/")
	env := &tools.Env{Home: home.Process(), Values: &sync.Map{}}
	tools.SetSessionVars(env, tools.SessionVars{Platform: "cli", SessionKey: connectorE2ESID, SessionID: connectorE2ESID})
	raw := tools.ManageConnections(tools.WithToolEnv(context.Background(), env), env,
		map[string]any{"action": "authorize", "connectors": []any{"gmail", "notion"}},
		tools.ManageOptions{MCPBackend: backend, SessionID: connectorE2ESID})
	tools.ClearSessionVars(env)
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("%v in %s", err, raw)
	}
	if result["status"] != "initiated" {
		t.Fatalf("result %v", result)
	}
	for _, item := range result["targets"].([]any) {
		if url, _ := item.(map[string]any)["connect_url"].(string); url == "" {
			t.Fatalf("target without a link %v", item)
		}
	}
	for _, frame := range recordedFrames(owner) {
		if params, _ := frame["params"].(map[string]any); params["type"] == "connection.request" {
			t.Fatalf("a card was emitted %v", frame)
		}
	}
	if tools.LiveCurrent(connectorE2ESID, "") != nil {
		t.Fatal("a no-card connect registered an operation")
	}
}

// test_connector_operation_e2e.py test_connection_respond_ignores_outcome_claims_and_rejects_strangers
func TestConnectionRespondIgnoresOutcomeClaimsAndRejectsStrangers(t *testing.T) {
	owner, stranger := connectorE2ESession(t)
	operation := tools.NewConnectionOperation([]*tools.ConnTarget{tools.NewConnTarget("gmail", "mcp", "authorize")}, connectorE2ESID, "", nil)
	if err := tools.LiveOpen(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	mustTransition(t, operation, "gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{})
	refused := connectorE2ERPC(t, owner, "connection.respond", map[string]any{"op_id": operation.OpID,
		"result": map[string]any{"targets": []any{map[string]any{"name": "gmail", "status": "connected"}}}})
	if code := rpcCode(refused); code != 4000 {
		t.Fatalf("refused %v", refused)
	}
	if operation.StateOf(operation.Target("gmail")) != tools.TargetInitiated {
		t.Fatal("a claimed outcome moved gmail")
	}
	foreign := connectorE2ERPC(t, stranger, "connection.respond", map[string]any{"op_id": operation.OpID,
		"result": map[string]any{"targets": []any{map[string]any{"name": "gmail", "status": "skipped"}}}})
	if code := rpcCode(foreign); code != 4001 {
		t.Fatalf("foreign %v", foreign)
	}
}
