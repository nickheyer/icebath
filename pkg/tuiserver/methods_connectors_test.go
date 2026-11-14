package tuiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const connectorRPCSID = "op-rpc-session"

// Owner and stranger transports with a live desktop session the owner steers and no guest mint
func connectorOwned(t *testing.T) (*frameRecorder, *frameRecorder, *tuigateway.Session) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tools.LiveResetForTests()
	savedEnsure := tools.EnsureGuestIdentity
	tools.EnsureGuestIdentity = func(*tools.Env) string { return "" }
	owner, stranger := &frameRecorder{}, &frameRecorder{}
	session := tuigateway.NewSession(map[string]any{"transport": owner, "agent": nil, "session_key": connectorRPCSID,
		"history": []any{}, "history_lock": &sync.Mutex{}, "history_version": 0, "running": false, "attached_images": []any{},
		"source": "desktop"})
	tuigateway.SetSessionRecord(connectorRPCSID, session)
	savedHome := hermesHome
	hermesHome = home.Process()
	t.Cleanup(func() {
		hermesHome = savedHome
		tuigateway.SetSessionRecord(connectorRPCSID, nil)
		tools.EnsureGuestIdentity = savedEnsure
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

// Two pending connector targets opened as the session's live operation
func openConnectorOp(t *testing.T) *tools.ConnectionOperation {
	t.Helper()
	operation := tools.NewConnectionOperation([]*tools.ConnTarget{tools.NewConnTarget("gmail", "connector", "connect"),
		tools.NewConnTarget("notion", "connector", "connect")}, connectorRPCSID, "", nil)
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

// Loopback connector gateway and Portal routes, each answering a status and JSON body
type connectorBackend struct {
	mu          sync.Mutex
	mints       []map[string]any
	agents      []string
	connections func(names []string, body map[string]any) []any
	list        func() []any
	account     func(id string) (int, any)
	portal      func(method, path string, body map[string]any) (int, any)
}

// Guest identity, bearer and loopback gateway and Portal that make connectors available in the case home
func connectorBackendDouble(t *testing.T, b *connectorBackend) *connectorBackend {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		status, reply := http.StatusOK, any(nil)
		b.mu.Lock()
		b.agents = append(b.agents, r.Header.Get("User-Agent"))
		b.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			if b.portal == nil {
				http.NotFound(w, r)
				return
			}
			status, reply = b.portal(r.Method, r.URL.Path, body)
		case strings.HasSuffix(r.URL.Path, "/v1/connectors/connections"):
			var names []string
			for _, item := range body["connectors"].([]any) {
				names = append(names, item.(string))
			}
			b.mu.Lock()
			b.mints = append(b.mints, map[string]any{"names": names, "reinitiate": body["reinitiate"], "alias": body["alias"],
				"connection_id": body["connectionId"]})
			b.mu.Unlock()
			reply = map[string]any{"results": b.connections(names, body)}
		case strings.Contains(r.URL.Path, "/v1/connectors/accounts/"):
			if b.account == nil {
				status, reply = http.StatusNotFound, map[string]any{"error": map[string]any{"code": "connection_not_found"}}
			} else {
				status, reply = b.account(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			}
		case strings.HasSuffix(r.URL.Path, "/v1/connectors") && r.Method == http.MethodGet:
			items := []any{}
			if b.list != nil {
				items = b.list()
			}
			reply = map[string]any{"items": items}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CONNECTOR_GATEWAY_URL", srv.URL)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "guest-bearer")
	if _, err := provider.NewStore(home.Process()).SaveActiveProviderState("nous", map[string]any{"auth_method": "anonymous",
		"account_tier": "anonymous", "anon_token": "anon_t", "portal_base_url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	return b
}

// Recorded mints of the double
func (b *connectorBackend) minted() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]map[string]any(nil), b.mints...)
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
	if code := rpcCode(reply); code != 4002 {
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
	operation := openConnectorOp(t)
	mustTransition(t, operation, "gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{ConnectURL: strPtr("https://l/gmail/1")})
	mustTransition(t, operation, "notion", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{ConnectURL: strPtr("https://l/notion/1")})
	mustTransition(t, operation, "notion", tools.TargetFailed, tools.ActorBackendWatcher, tools.TransitionFields{Detail: strPtr("vendor: nope")})
	backend := connectorBackendDouble(t, &connectorBackend{connections: func(names []string, _ map[string]any) []any {
		var results []any
		for _, name := range names {
			results = append(results, map[string]any{"connector": name, "status": "initiated", "connectUrl": "https://l/" + name + "/2"})
		}
		return results
	}})
	refused := connectorRPCCall(t, owner, "connectors.connect", map[string]any{"connectors": []any{"gmail"}})
	if code := rpcCode(refused); code != 4002 || len(backend.minted()) != 0 {
		t.Fatalf("refused %v mints %v", refused, backend.minted())
	}
	if url := operation.Target("gmail").ConnectURL; url != "https://l/gmail/1" {
		t.Fatalf("gmail link %q", url)
	}
	reply := connectorRPCCall(t, owner, "connectors.connect", map[string]any{"connectors": []any{"notion"}})
	if _, isResult := reply["result"]; !isResult {
		t.Fatalf("reply %v", reply)
	}
	if mints := backend.minted(); len(mints) != 1 || strings.Join(mints[0]["names"].([]string), ",") != "notion" || mints[0]["reinitiate"] != true {
		t.Fatalf("mints %v", mints)
	}
	target := operation.Target("notion")
	if operation.StateOf(target) != tools.TargetInitiated || target.ConnectURL != "https://l/notion/2" {
		t.Fatalf("notion %s %q", operation.StateOf(target), target.ConnectURL)
	}
}

// test_connector_operation_rpc.py test_a_failed_reissue_leaves_the_row_failed_with_no_link
func TestAFailedReissueLeavesTheRowFailedWithNoLink(t *testing.T) {
	for _, dead := range []tools.TargetState{tools.TargetFailed, tools.TargetExpired} {
		t.Run(string(dead), func(t *testing.T) {
			owner, _, _ := connectorOwned(t)
			operation := openConnectorOp(t)
			mustTransition(t, operation, "notion", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{ConnectURL: strPtr("https://l/notion/1")})
			actor := tools.ActorBackendWatcher
			if dead == tools.TargetExpired {
				actor = tools.ActorClock
			}
			mustTransition(t, operation, "notion", dead, actor, tools.TransitionFields{Detail: strPtr("vendor: nope")})
			connectorBackendDouble(t, &connectorBackend{connections: func(names []string, _ map[string]any) []any {
				var results []any
				for _, name := range names {
					results = append(results, map[string]any{"connector": name, "status": "failed", "statusReason": "vendor: still no"})
				}
				return results
			}})
			connectorRPCCall(t, owner, "connectors.connect", map[string]any{"connectors": []any{"notion"}})
			target := operation.Target("notion")
			if operation.StateOf(target) != tools.TargetFailed || target.ConnectURL != "" || target.Detail != "vendor: still no" {
				t.Fatalf("notion %s %q %q", operation.StateOf(target), target.ConnectURL, target.Detail)
			}
		})
	}
}

const connectorE2ESID = "connector-operation-e2e"

// Tool gateway client boundary listing, minting and reading accounts, the only fake of the e2e cases
type fakeConnectorClient struct {
	mu        sync.Mutex
	connected map[string]bool
}

func (c *fakeConnectorClient) ListConnectors(context.Context, time.Duration) ([]map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return []map[string]any{{"connector": "gmail", "enabled": true, "connected": c.connected["gmail"]},
		{"connector": "notion", "enabled": true, "connected": c.connected["notion"]}}, nil
}

func (c *fakeConnectorClient) Connections(_ context.Context, req tools.ConnectionsRequest) (map[string]any, error) {
	results := []any{}
	for _, name := range req.Connectors {
		results = append(results, map[string]any{"connector": name, "status": "initiated", "connect_url": "https://connect.example/" + name,
			"connection_id": "ca_" + name, "reinitiated": req.Reinitiate})
	}
	return map[string]any{"results": results}, nil
}

func (c *fakeConnectorClient) AccountStatus(_ context.Context, connectionID string, _ time.Duration) (map[string]any, error) {
	name := connectionID[strings.Index(connectionID, "ca_")+3:]
	c.mu.Lock()
	active := c.connected[name]
	c.mu.Unlock()
	status := "pending"
	if active {
		status = "active"
	}
	return map[string]any{"connectionId": connectionID, "connector": name, "status": status, "label": name + "_a", "active": active,
		"createdAt": "2026-09-14T10:00:00.000Z", "updatedAt": "2026-09-14T10:00:00.000Z"}, nil
}

func (c *fakeConnectorClient) setConnected(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected[name] = true
}

// Owner and stranger transports of a desktop session in a fresh home
func connectorE2ESession(t *testing.T) (*frameRecorder, *frameRecorder) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tools.LiveResetForTests()
	owner, stranger := &frameRecorder{}, &frameRecorder{}
	session := tuigateway.NewSession(map[string]any{"transport": owner, "agent": nil, "session_key": connectorE2ESID,
		"history": []any{}, "history_lock": &sync.Mutex{}, "history_version": 0, "running": false, "attached_images": []any{},
		"source": "desktop"})
	tuigateway.SetSessionRecord(connectorE2ESID, session)
	t.Cleanup(func() {
		tuigateway.SetSessionRecord(connectorE2ESID, nil)
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

// test_connector_operation_e2e.py test_desktop_connect_settles_through_callback_response
func TestDesktopConnectSettlesThroughCallbackResponse(t *testing.T) {
	owner, _ := connectorE2ESession(t)
	client := &fakeConnectorClient{connected: map[string]bool{}}
	savedTick := tools.ManagedWatchTickSeconds
	tools.ManagedWatchTickSeconds = 0.05
	t.Cleanup(func() { tools.ManagedWatchTickSeconds = savedTick })
	var raw string
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ctx, env := setSessionContext(context.Background(), connectorE2ESID, nil, "")
		defer clearSessionContext(env)
		cbs := agentCbs(ctx, connectorE2ESID)
		raw = tools.ManageConnections(ctx, env, map[string]any{"action": "connect", "connectors": []any{"gmail", "notion"}},
			tools.ManageOptions{ClientFactory: func() (tools.ManagedClient, error) { return client, nil },
				SessionID: "20260914_rotated_agent_id",
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
		if target["connect_url"] != "https://connect.example/"+target["name"].(string) {
			t.Fatalf("target %v", target)
		}
	}
	client.setConnected("gmail")
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
	client := &fakeConnectorClient{connected: map[string]bool{}}
	env := &tools.Env{Home: home.Process(), Values: &sync.Map{}}
	tools.SetSessionVars(env, tools.SessionVars{Platform: "cli", SessionKey: connectorE2ESID, SessionID: connectorE2ESID})
	raw := tools.ManageConnections(tools.WithToolEnv(context.Background(), env), env,
		map[string]any{"action": "connect", "connectors": []any{"gmail", "notion"}},
		tools.ManageOptions{ClientFactory: func() (tools.ManagedClient, error) { return client, nil }, SessionID: connectorE2ESID})
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
	operation := tools.NewConnectionOperation([]*tools.ConnTarget{tools.NewConnTarget("gmail", "connector", "connect")}, connectorE2ESID, "", nil)
	if err := tools.LiveOpen(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	mustTransition(t, operation, "gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{})
	refused := connectorE2ERPC(t, owner, "connection.respond", map[string]any{"op_id": operation.OpID,
		"result": map[string]any{"targets": []any{map[string]any{"name": "gmail", "status": "connected"}}}})
	if code := rpcCode(refused); code != 4002 {
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

// Guest auth state of one home whose held connectors bearer names that home to the double
func ticketGuestHome(t *testing.T, h, token, portal string) {
	t.Helper()
	if _, err := provider.NewStore(h).SaveActiveProviderState("nous", map[string]any{"auth_method": "anonymous",
		"account_tier": "anonymous", "anon_token": "anon_" + token, "access_token": token,
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "portal_base_url": portal}); err != nil {
		t.Fatal(err)
	}
}

func TestSecondaryTicketRunsConnectorsAndBillingInItsOwnProfile(t *testing.T) {
	launch, bee := wsLegacyHomes(t)
	var mu sync.Mutex
	var seen [][2]string
	homes := map[string]string{"Bearer launch-token": "launch", "Bearer bee-token": "bee"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var label string
		var reply any
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/connectors"):
			label, reply = "list", map[string]any{"items": []any{}}
		case strings.Contains(r.URL.Path, "charge"):
			label, reply = "charge", map[string]any{"chargeId": "c"}
		default:
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		seen = append(seen, [2]string{label, homes[r.Header.Get("Authorization")]})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ICEBATH_SHARED_AUTH_DIR", t.TempDir())
	t.Setenv("CONNECTOR_GATEWAY_URL", srv.URL)
	if err := os.WriteFile(filepath.Join(bee, ".env"), []byte("CONNECTOR_GATEWAY_URL="+srv.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ticketGuestHome(t, launch, "launch-token", srv.URL)
	ticketGuestHome(t, bee, "bee-token", srv.URL)
	calls := []struct {
		method string
		params map[string]any
	}{{"connectors.list", map[string]any{"owner": map[string]any{"type": "account"}}}, {"billing.charge", map[string]any{"amount_usd": 5}}}
	for _, call := range calls {
		wsLegacyCall(t, context.Background(), wsLegacyActor(bee), call.method, call.params)
	}
	mu.Lock()
	labels, homesSeen := map[string]bool{}, map[string]bool{}
	for _, row := range seen {
		labels[row[0]], homesSeen[row[1]] = true, true
	}
	if len(labels) != 2 || !labels["list"] || !labels["charge"] || len(homesSeen) != 1 || !homesSeen["bee"] {
		t.Fatalf("seen %v", seen)
	}
	seen = nil
	mu.Unlock()
	wsLegacyCall(t, context.Background(), wsLegacyActor(launch), "billing.charge", map[string]any{"amount_usd": 5})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0][1] != "launch" {
		t.Fatalf("launch ticket seen %v", seen)
	}
}
