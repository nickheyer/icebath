package tuiserver

import (
	"context"
	"net/http"
	"os"
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
	t.Setenv("ICEBATH_DESKTOP", "")
	os.Unsetenv("ICEBATH_DESKTOP")
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

// Live desktop session of sid on a transport the case owns
func accountsSession(t *testing.T, sid string) *frameRecorder {
	t.Helper()
	transport := &frameRecorder{}
	tuigateway.SetSessionRecord(sid, tuigateway.NewSession(map[string]any{"transport": transport, "agent": nil, "session_key": sid,
		"history": []any{}, "history_lock": &sync.Mutex{}, "history_version": 0, "running": false, "attached_images": []any{},
		"source": "desktop"}))
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
	return transport
}

// test_connector_accounts_rpc.py test_account_connect_with_an_alias_mints_that_account
func TestAccountConnectWithAnAliasMintsThatAccount(t *testing.T) {
	accountsGate(t)
	minted := make(chan struct{}, 1)
	backend := connectorBackendDouble(t, &connectorBackend{connections: func(names []string, _ map[string]any) []any {
		defer func() {
			select {
			case minted <- struct{}{}:
			default:
			}
		}()
		var results []any
		for _, name := range names {
			results = append(results, map[string]any{"connector": name, "status": "initiated", "connectionId": "ca_1",
				"connectUrl": "https://connect.example/1"})
		}
		return results
	}})
	reply := accountsRPC(t, nil, "connectors.connect", map[string]any{"owner": map[string]any{"type": "account"},
		"connectors": []any{"gmail"}, "alias": "work"})
	result, isResult := reply["result"].(map[string]any)
	if !isResult {
		t.Fatalf("reply %v", reply)
	}
	select {
	case <-minted:
	case <-time.After(2 * time.Second):
		t.Fatal("no mint")
	}
	mints := backend.minted()
	if len(mints) != 1 || strings.Join(mints[0]["names"].([]string), ",") != "gmail" || mints[0]["reinitiate"] != false || mints[0]["alias"] != "work" {
		t.Fatalf("mints %v", mints)
	}
	targets := result["targets"].([]any)
	if len(targets) != 1 || targets[0].(map[string]any)["name"] != "gmail" || targets[0].(map[string]any)["alias"] != "work" {
		t.Fatalf("targets %v", targets)
	}
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

// test_connector_accounts_rpc.py test_rename_to_a_name_another_account_holds_is_alias_taken
func TestRenameToANameAnotherAccountHoldsIsAliasTaken(t *testing.T) {
	accountsGate(t)
	connectorBackendDouble(t, &connectorBackend{portal: func(method, path string, _ map[string]any) (int, any) {
		if method == http.MethodPatch && strings.HasPrefix(path, "/api/v1/connectors/accounts/") {
			return http.StatusConflict, map[string]any{"error": map[string]any{"code": "alias_taken", "message": "alias taken"}}
		}
		return http.StatusNotFound, map[string]any{}
	}})
	reply := accountsRPC(t, nil, "connectors.accounts.rename", map[string]any{"connection_id": "ca_1", "alias": "home"})
	failure, _ := reply["error"].(map[string]any)
	data, _ := failure["data"].(map[string]any)
	if data["reason"] != "ALIAS_TAKEN" {
		t.Fatalf("reply %v", reply)
	}
}

// test_connector_account_surface.py test_account_reads_and_connects_name_the_desktop
func TestAccountReadsAndConnectsNameTheDesktop(t *testing.T) {
	accountsGate(t)
	backend := connectorBackendDouble(t, &connectorBackend{
		connections: func(names []string, _ map[string]any) []any {
			var results []any
			for _, name := range names {
				results = append(results, map[string]any{"connector": name, "status": "active"})
			}
			return results
		},
		portal: func(method, path string, _ map[string]any) (int, any) {
			if path == "/api/v1/connectors/gmail/tools" {
				return http.StatusNotFound, map[string]any{"error": map[string]any{"code": "connector_not_found", "message": "gone"}}
			}
			return http.StatusNotFound, map[string]any{}
		},
	})
	if code := rpcCode(accountsRPC(t, nil, "connectors.tools", map[string]any{"slug": "gmail", "refresh": false})); code != 4041 {
		t.Fatalf("tools code %v", code)
	}
	connected := accountsRPC(t, nil, "connectors.connect", map[string]any{"owner": map[string]any{"type": "account"}, "connectors": []any{"gmail"}})
	if _, isResult := connected["result"]; !isResult {
		t.Fatalf("connect %v", connected)
	}
	backend.mu.Lock()
	agents := append([]string(nil), backend.agents...)
	backend.mu.Unlock()
	var surfaces []string
	for _, agent := range agents {
		_, rest, _ := strings.Cut(agent, " (")
		surface, _, _ := strings.Cut(rest, ";")
		surfaces = append(surfaces, surface)
	}
	if len(surfaces) != 2 || surfaces[0] != "desktop" || surfaces[1] != "desktop" {
		t.Fatalf("surfaces %v of %v", surfaces, agents)
	}
	agent, err := tools.ConnectorUserAgent(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, rest, _ := strings.Cut(agent, " ("); !strings.HasPrefix(rest, "cli;") {
		t.Fatalf("request surface leaked out of the request %q", agent)
	}
}
