package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Fresh home with no live operations and no guest mint, the autouse gate of the accounts cases
func accountsGate(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tools.LiveResetForTests()
	savedEnsure, savedHome := tools.EnsureGuestIdentity, hermesHome
	tools.EnsureGuestIdentity = func(*tools.Env) string { return "" }
	hermesHome = home.Process()
	t.Cleanup(func() {
		tools.EnsureGuestIdentity, hermesHome = savedEnsure, savedHome
		tools.LiveResetForTests()
	})
}

// RPC through the real dispatcher on a transport collecting a pool handler's reply
func accountsRPC(t *testing.T, transport *frameRecorder, method string, params map[string]any) map[string]any {
	t.Helper()
	if transport == nil {
		transport = &frameRecorder{}
	}
	transport.mu.Lock()
	transport.frames = nil
	transport.mu.Unlock()
	if reply := Dispatch(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, transport); reply != nil {
		return pyjson.Plain(reply).(map[string]any)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range recordedFrames(transport) {
			if id, _ := frame["id"].(int); id == 1 {
				return frame
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no reply")
	return nil
}

// Live TUI session of sid on a transport the case owns
func accountsSession(t *testing.T, sid string) *frameRecorder {
	t.Helper()
	transport := &frameRecorder{}
	tuigateway.SetSessionRecord(sid, tuigateway.NewSession(map[string]any{"transport": transport, "agent": nil, "session_key": sid,
		"history": []any{}, "history_lock": &sync.Mutex{}, "history_version": 0, "running": false, "attached_images": []any{},
		"source": "tui"}))
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
	return transport
}

// test_connector_accounts_rpc.py test_a_session_retry_for_another_account_does_not_re_mint_the_open_one
func TestASessionRetryForAnotherAccountDoesNotReMintTheOpenOne(t *testing.T) {
	accountsGate(t)
	transport := accountsSession(t, "alias-sid")
	backend := connectorBackendDouble(t, &connectorBackend{connections: func([]string, map[string]any) []any { return []any{} }})
	named := tools.NewConnTarget("gmail", "connector", "connect")
	named.Alias = strPtr("home")
	operation := tools.NewConnectionOperation([]*tools.ConnTarget{named}, "alias-sid", "", nil)
	if err := tools.LiveOpen(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	mustTransition(t, operation, "gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{ConnectURL: strPtr("https://l/1")})
	mustTransition(t, operation, "gmail", tools.TargetFailed, tools.ActorBackendWatcher, tools.TransitionFields{Detail: strPtr("expired")})
	owner := map[string]any{"type": "session", "session_id": "alias-sid"}
	reply := accountsRPC(t, transport, "connectors.connect", map[string]any{"owner": owner, "connectors": []any{"gmail"}, "alias": "work"})
	if code := rpcCode(reply); code != 4004 {
		t.Fatalf("alias retry %v", reply)
	}
	tools.LiveClose(operation)
	repair := tools.NewConnTarget("gmail", "connector", "reconnect")
	repair.RepairID = strPtr("ca_home")
	unnamed := tools.NewConnectionOperation([]*tools.ConnTarget{repair}, "alias-sid", "", nil)
	if err := tools.LiveOpen(context.Background(), unnamed); err != nil {
		t.Fatal(err)
	}
	mustTransition(t, unnamed, "gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{ConnectURL: strPtr("https://l/2")})
	mustTransition(t, unnamed, "gmail", tools.TargetFailed, tools.ActorBackendWatcher, tools.TransitionFields{Detail: strPtr("expired")})
	reply = accountsRPC(t, transport, "connectors.connect", map[string]any{"owner": owner, "connectors": []any{"gmail"},
		"reconnect": true, "connection_id": "ca_work"})
	if code := rpcCode(reply); code != 4004 {
		t.Fatalf("id retry %v", reply)
	}
	if mints := backend.minted(); len(mints) != 0 {
		t.Fatalf("mints %v", mints)
	}
}

// test_connector_accounts_rpc.py test_a_session_retry_with_the_open_accounts_id_reissues_it
func TestASessionRetryWithTheOpenAccountsIdReissuesIt(t *testing.T) {
	accountsGate(t)
	transport := accountsSession(t, "retry-sid")
	backend := connectorBackendDouble(t, &connectorBackend{connections: func(names []string, _ map[string]any) []any {
		var results []any
		for _, name := range names {
			results = append(results, map[string]any{"connector": name, "status": "initiated", "connectUrl": "https://l/2", "connectionId": "ca_work2"})
		}
		return results
	}})
	work := tools.NewConnTarget("gmail", "connector", "reconnect")
	work.Alias = strPtr("work")
	operation := tools.NewConnectionOperation([]*tools.ConnTarget{work}, "retry-sid", "", nil)
	if err := tools.LiveOpen(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	mustTransition(t, operation, "gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{ConnectURL: strPtr("https://l/1"),
		ConnectionID: strPtr("ca_work")})
	mustTransition(t, operation, "gmail", tools.TargetFailed, tools.ActorBackendWatcher, tools.TransitionFields{Detail: strPtr("expired")})
	reply := accountsRPC(t, transport, "connectors.connect", map[string]any{"owner": map[string]any{"type": "session", "session_id": "retry-sid"},
		"connectors": []any{"gmail"}, "reconnect": true, "connection_id": "ca_work"})
	if _, isResult := reply["result"]; !isResult {
		t.Fatalf("reply %v", reply)
	}
	if mints := backend.minted(); len(mints) != 1 || strings.Join(mints[0]["names"].([]string), ",") != "gmail" || mints[0]["connection_id"] != "ca_work" {
		t.Fatalf("mints %v", mints)
	}
}

// User agents of every request the double served
func (b *connectorBackend) userAgents() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.agents...)
}

// Mints of the double once at least one arrived, waiting up to two seconds
func waitMinted(t *testing.T, b *connectorBackend) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(b.minted()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return b.minted()
}

// Gateway double minting an initiated link for every requested connector under one account id
func initiatedConnections(id string) func([]string, map[string]any) []any {
	return func(names []string, _ map[string]any) []any {
		var results []any
		for _, name := range names {
			results = append(results, map[string]any{"connector": name, "status": "initiated", "connectionId": id,
				"connectUrl": "https://connect.example/" + id})
		}
		return results
	}
}

// Portal account row of the wire shape
func portalAccount(id, connector, status string, alias any, disabled bool) map[string]any {
	return map[string]any{"connectionId": id, "connector": connector, "status": status, "statusReason": nil,
		"label": connector + "-label", "alias": alias, "active": status == "active", "disabled": disabled,
		"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z"}
}

// test_connector_accounts_rpc.py test_account_connect_with_an_alias_mints_that_account
func TestAccountConnectWithAnAliasMintsThatAccount(t *testing.T) {
	accountsGate(t)
	backend := connectorBackendDouble(t, &connectorBackend{connections: initiatedConnections("ca_1")})
	reply := accountsRPC(t, nil, "connectors.connect", map[string]any{"owner": map[string]any{"type": "account"},
		"connectors": []any{"gmail"}, "alias": "work"})
	result, isResult := reply["result"].(map[string]any)
	if !isResult {
		t.Fatalf("reply %v", reply)
	}
	mints := waitMinted(t, backend)
	if len(mints) != 1 || strings.Join(mints[0]["names"].([]string), ",") != "gmail" || truthy(mints[0]["reinitiate"]) || mints[0]["alias"] != "work" {
		t.Fatalf("mints %v", mints)
	}
	var targets [][2]any
	for _, item := range result["targets"].([]any) {
		target := item.(map[string]any)
		targets = append(targets, [2]any{target["name"], target["alias"]})
	}
	if len(targets) != 1 || targets[0] != [2]any{"gmail", "work"} {
		t.Fatalf("targets %v", targets)
	}
}

// test_connector_accounts_rpc.py test_rename_to_a_name_another_account_holds_is_alias_taken
func TestRenameToANameAnotherAccountHoldsIsAliasTaken(t *testing.T) {
	accountsGate(t)
	connectorBackendDouble(t, &connectorBackend{portal: func(method, path string, _ map[string]any) (int, any) {
		if method == "PATCH" && path == "/api/v1/connectors/accounts/ca_1" {
			return 409, map[string]any{"error": map[string]any{"code": "alias_taken", "message": "alias taken"}}
		}
		return 404, map[string]any{}
	}})
	reply := accountsRPC(t, nil, "connectors.accounts.rename", map[string]any{"connection_id": "ca_1", "alias": "home"})
	failure, _ := reply["error"].(map[string]any)
	data, _ := failure["data"].(map[string]any)
	if rpcCode(reply) != 4090 || data["reason"] != "ALIAS_TAKEN" {
		t.Fatalf("reply %v", reply)
	}
}

func TestAccountConnectNamesTheConnectorsPageInitiatorAndTheTUISurface(t *testing.T) {
	accountsGate(t)
	backend := connectorBackendDouble(t, &connectorBackend{connections: initiatedConnections("ca_1")})
	reply := accountsRPC(t, nil, "connectors.connect", map[string]any{"owner": map[string]any{"type": "account"}, "connectors": []any{"gmail"}})
	if _, isResult := reply["result"]; !isResult {
		t.Fatalf("reply %v", reply)
	}
	if mints := waitMinted(t, backend); len(mints) != 1 || mints[0]["initiator"] != "desktop_page" {
		t.Fatalf("mints %v", mints)
	}
	for _, agent := range backend.userAgents() {
		if !strings.Contains(agent, " (tui;") {
			t.Fatalf("user agent %q does not name the tui surface", agent)
		}
	}
}

func TestAccountsListsTheMembersAccountsFilteredByConnector(t *testing.T) {
	accountsGate(t)
	connectorBackendDouble(t, &connectorBackend{portal: func(method, path string, _ map[string]any) (int, any) {
		if method == "GET" && path == "/api/v1/connectors/accounts" {
			return 200, map[string]any{"accounts": []any{portalAccount("ca_1", "gmail", "active", "work", false),
				portalAccount("ca_2", "notion", "expired", nil, false), portalAccount("ca_3", "gmail", "revoked", "old", true)}}
		}
		return 404, map[string]any{}
	}})
	all := accountsRPC(t, nil, "connectors.accounts", map[string]any{})
	rows, _ := pyjson.Plain(all["result"]).(map[string]any)["accounts"].([]any)
	if len(rows) != 3 {
		t.Fatalf("accounts %v", all)
	}
	first := rows[0].(map[string]any)
	want := map[string]any{"connection_id": "ca_1", "connector": "gmail", "status": "active", "status_reason": nil, "label": "gmail-label",
		"alias": "work", "active": true, "disabled": false, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"}
	if !pyjson.Equal(first, want) {
		t.Fatalf("row %v", first)
	}
	gmail := accountsRPC(t, nil, "connectors.accounts", map[string]any{"connector": "gmail"})
	var ids []any
	for _, row := range pyjson.Plain(gmail["result"]).(map[string]any)["accounts"].([]any) {
		ids = append(ids, row.(map[string]any)["connection_id"])
	}
	if !pyjson.Equal(ids, []any{"ca_1", "ca_3"}) {
		t.Fatalf("filtered ids %v", ids)
	}
	if code := rpcCode(accountsRPC(t, nil, "connectors.accounts", map[string]any{"connector": "Not A Slug"})); code != 4000 {
		t.Fatalf("invalid filter code %d", code)
	}
}

func TestRenameNamesTheAccountAndAnswersItsRow(t *testing.T) {
	accountsGate(t)
	var sent map[string]any
	connectorBackendDouble(t, &connectorBackend{portal: func(method, path string, body map[string]any) (int, any) {
		if method == "PATCH" && path == "/api/v1/connectors/accounts/ca_1" {
			sent = body
			return 200, portalAccount("ca_1", "gmail", "active", body["alias"], false)
		}
		return 404, map[string]any{}
	}})
	reply := accountsRPC(t, nil, "connectors.accounts.rename", map[string]any{"connection_id": "ca_1", "alias": "office"})
	row, _ := pyjson.Plain(reply["result"]).(map[string]any)
	if row["connection_id"] != "ca_1" || row["alias"] != "office" || !pyjson.Equal(sent, map[string]any{"alias": "office"}) {
		t.Fatalf("reply %v sent %v", reply, sent)
	}
	if code := rpcCode(accountsRPC(t, nil, "connectors.accounts.rename", map[string]any{"connection_id": "ca_1", "alias": "Bad Name"})); code != 4000 {
		t.Fatalf("invalid alias code %d", code)
	}
}

func TestRenameAndRemoveMapPortalFailuresToTheirRefusals(t *testing.T) {
	accountsGate(t)
	connectorBackendDouble(t, &connectorBackend{portal: func(method, path string, _ map[string]any) (int, any) {
		switch path {
		case "/api/v1/connectors/accounts/gone":
			return 404, map[string]any{"error": map[string]any{"code": "connection_not_found"}}
		case "/api/v1/connectors/accounts/bad":
			return 400, map[string]any{"error": map[string]any{"code": "invalid_connection_id"}}
		case "/api/v1/connectors/accounts/denied":
			return 401, map[string]any{"error": map[string]any{"code": "invalid_token"}}
		case "/api/v1/connectors/accounts/org":
			return 400, map[string]any{"error": map[string]any{"code": "org_required"}}
		}
		return 500, map[string]any{"error": map[string]any{"code": "boom"}}
	}})
	reason := func(reply map[string]any) any {
		failure, _ := reply["error"].(map[string]any)
		data, _ := failure["data"].(map[string]any)
		return data["reason"]
	}
	for _, c := range []struct {
		method, id string
		code       int
		reason     string
	}{
		{"connectors.accounts.rename", "gone", 4041, "CONNECTION_NOT_FOUND"},
		{"connectors.accounts.rename", "bad", 4000, "INVALID_PARAMS"},
		{"connectors.accounts.rename", "denied", 4032, "NEEDS_NOUS_AUTH"},
		{"connectors.accounts.rename", "org", 4090, "ORG_REQUIRED"},
		{"connectors.accounts.rename", "else", 5034, "ACCOUNTS_UNAVAILABLE"},
		{"connectors.accounts.remove", "gone", 4041, "CONNECTION_NOT_FOUND"},
		{"connectors.accounts.remove", "bad", 4000, "INVALID_PARAMS"},
		{"connectors.accounts.remove", "denied", 4032, "NEEDS_NOUS_AUTH"},
		{"connectors.accounts.remove", "org", 4090, "ORG_REQUIRED"},
		{"connectors.accounts.remove", "else", 5034, "ACCOUNTS_UNAVAILABLE"},
	} {
		params := map[string]any{"connection_id": c.id}
		if c.method == "connectors.accounts.rename" {
			params["alias"] = "home"
		}
		reply := accountsRPC(t, nil, c.method, params)
		if rpcCode(reply) != c.code || reason(reply) != c.reason {
			t.Errorf("%s %s answered %v", c.method, c.id, reply)
		}
	}
}

func TestRemoveDeletesTheAccountAndAnswersTheRemovedRow(t *testing.T) {
	accountsGate(t)
	var deleted []string
	connectorBackendDouble(t, &connectorBackend{portal: func(method, path string, _ map[string]any) (int, any) {
		if method == "DELETE" && strings.HasPrefix(path, "/api/v1/connectors/accounts/") {
			id := strings.TrimPrefix(path, "/api/v1/connectors/accounts/")
			deleted = append(deleted, id)
			return 200, map[string]any{"connectionId": id, "connector": "gmail", "status": "removed"}
		}
		return 404, map[string]any{}
	}})
	reply := accountsRPC(t, nil, "connectors.accounts.remove", map[string]any{"connection_id": "ca_1"})
	if !pyjson.Equal(reply["result"], map[string]any{"connection_id": "ca_1", "connector": "gmail", "status": "removed"}) || len(deleted) != 1 || deleted[0] != "ca_1" {
		t.Fatalf("reply %v deleted %v", reply, deleted)
	}
	if code := rpcCode(accountsRPC(t, nil, "connectors.accounts.remove", map[string]any{"connection_id": ""})); code != 4000 {
		t.Fatalf("empty id code %d", code)
	}
}

func TestAccountMethodsRefuseWhileConnectorsAreUnavailable(t *testing.T) {
	accountsGate(t)
	connectorBackendDouble(t, &connectorBackend{connections: initiatedConnections("ca_1")})
	if err := os.WriteFile(filepath.Join(home.Process(), "config.yaml"), []byte("tools:\n  connectors: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for method, params := range map[string]map[string]any{"connectors.accounts": {}, "connectors.accounts.rename": {"connection_id": "ca_1", "alias": "home"},
		"connectors.accounts.remove": {"connection_id": "ca_1"}, "connectors.connect": {"owner": map[string]any{"type": "account"}, "connectors": []any{"gmail"}},
		"connectors.operation.status": {"owner": map[string]any{"type": "account"}, "op_id": "op_x"}} {
		reply := accountsRPC(t, nil, method, params)
		failure, _ := reply["error"].(map[string]any)
		data, _ := failure["data"].(map[string]any)
		if rpcCode(reply) != 4031 || data["reason"] != "CONNECTORS_UNAVAILABLE" {
			t.Errorf("%s answered %v", method, reply)
		}
	}
	if reply := accountsRPC(t, nil, "connectors.list", map[string]any{"owner": map[string]any{"type": "account"}}); !pyjson.Equal(reply["result"],
		map[string]any{"available": false, "connectors": []any{}}) {
		t.Fatalf("list %v", reply)
	}
}

func TestAccountOperationIsReadAndAnsweredByItsOpID(t *testing.T) {
	accountsGate(t)
	connectorBackendDouble(t, &connectorBackend{connections: initiatedConnections("ca_1")})
	owner := map[string]any{"type": "account"}
	reply := accountsRPC(t, nil, "connectors.connect", map[string]any{"owner": owner, "connectors": []any{"gmail"}, "alias": "work"})
	result, _ := reply["result"].(map[string]any)
	opID, _ := result["op_id"].(string)
	if opID == "" {
		t.Fatalf("connect %v", reply)
	}
	status := accountsRPC(t, nil, "connectors.operation.status", map[string]any{"owner": owner, "op_id": opID})
	snapshot, _ := status["result"].(map[string]any)
	if snapshot["op_id"] != opID || snapshot["settled"] != false {
		t.Fatalf("status %v", status)
	}
	if code := rpcCode(accountsRPC(t, nil, "connectors.operation.status", map[string]any{"owner": owner, "op_id": "op_unknown"})); code != 4004 {
		t.Fatalf("unknown op code %d", code)
	}
	if woke := accountsRPC(t, nil, "connectors.operation.wake", map[string]any{"owner": owner, "op_id": opID}); !pyjson.Equal(woke["result"], map[string]any{"status": "ok"}) {
		t.Fatalf("wake %v", woke)
	}
	answered := accountsRPC(t, nil, "connection.respond", map[string]any{"owner": owner, "op_id": opID,
		"result": map[string]any{"targets": []any{map[string]any{"name": "gmail", "status": "skipped"}}}})
	if !pyjson.Equal(answered["result"], map[string]any{"status": "ok", "settled": true}) {
		t.Fatalf("respond %v", answered)
	}
	if code := rpcCode(accountsRPC(t, nil, "connectors.operation.status", map[string]any{"owner": owner, "op_id": opID})); code != 4004 {
		t.Fatalf("settled op code %d", code)
	}
}

func TestAnOpenAccountOperationIsFoundAgainNotReMinted(t *testing.T) {
	accountsGate(t)
	backend := connectorBackendDouble(t, &connectorBackend{connections: initiatedConnections("ca_1")})
	owner := map[string]any{"type": "account"}
	first := accountsRPC(t, nil, "connectors.connect", map[string]any{"owner": owner, "connectors": []any{"gmail"}, "alias": "work"})
	again := accountsRPC(t, nil, "connectors.connect", map[string]any{"owner": owner, "connectors": []any{"gmail"}, "alias": "work"})
	firstOp, _ := first["result"].(map[string]any)
	againOp, _ := again["result"].(map[string]any)
	if firstOp["op_id"] == nil || firstOp["op_id"] != againOp["op_id"] {
		t.Fatalf("first %v again %v", first, again)
	}
	if mints := waitMinted(t, backend); len(mints) != 1 {
		t.Fatalf("mints %v", mints)
	}
}
