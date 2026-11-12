//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Endpoint of the profile the Desktop discovers by running gateway ensure under that profile
func desktopEnsure(t *testing.T, env []string, profile string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ensure := exec.CommandContext(ctx, os.Args[0], "--profile", profile, "gateway", "ensure", "--json", "--timeout", "30")
	ensure.Env = append(append([]string{}, env...), runMainEnv+"=1")
	var stdout, stderr strings.Builder
	ensure.Stdout, ensure.Stderr = &stdout, &stderr
	runErr := ensure.Run()
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &payload); err != nil || runErr != nil || payload["state"] != "ready" {
		t.Fatalf("gateway ensure for %s = %v %v %s\n%s", profile, runErr, err, stdout.String(), tail(stderr.String(), 500))
	}
	endpoint, _ := payload["endpoint"].(map[string]any)
	return endpoint
}

// Endpoint of a discovery payload as the client reads it
func endpointOf(payload map[string]any) *gateway.GatewayEndpoint {
	text := func(key string) string { value, _ := payload[key].(string); return value }
	endpoint := &gateway.GatewayEndpoint{ProfileID: text("profile_id"), InstanceID: text("instance_id"), APIOrigin: text("api_origin"),
		Supervisor: text("supervisor"), ControlHome: text("control_home"), CodeSHA: text("code_sha"), Capabilities: map[string]bool{}}
	if epoch, isNumber := payload["authority_epoch"].(float64); isNumber {
		endpoint.AuthorityEpoch = big.NewInt(int64(epoch))
	}
	if protocol, isNumber := payload["runtime_protocol"].(float64); isNumber {
		endpoint.RuntimeProtocol = int(protocol)
	}
	capabilities, _ := payload["capabilities"].([]any)
	for _, raw := range capabilities {
		if name, isText := raw.(string); isText {
			endpoint.Capabilities[name] = true
		}
	}
	return endpoint
}

// Ticket of the purpose minted through the endpoint's own control home, the Desktop client path
func endpointTicket(t *testing.T, payload map[string]any, purpose string) string {
	t.Helper()
	endpoint := endpointOf(payload)
	ticket, err := gateway.SessionTicket(endpoint.ProfileID, endpoint, purpose)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

// Session created on the endpoint over the canonical socket, the socket returned still open
func createViaWS(t *testing.T, payload map[string]any, requestID string) (string, *websocket.Conn) {
	t.Helper()
	origin, _ := payload["api_origin"].(string)
	protocols := []string{gateway.CanonicalGatewayProtocol, gateway.GatewayWSTicketPrefix + endpointTicket(t, payload, "interactive")}
	ws, _, err := (&websocket.Dialer{Subprotocols: protocols, HandshakeTimeout: 10 * time.Second}).Dial(wsOrigin(origin), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	if err := ws.WriteJSON(map[string]any{"id": 1, "method": "session.create", "params": map[string]any{"request_id": requestID, "source": "gui"}}); err != nil {
		t.Fatal(err)
	}
	if err := ws.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		var response map[string]any
		if err := ws.ReadJSON(&response); err != nil {
			t.Fatal(err)
		}
		if response["id"] == float64(1) {
			result, ok := response["result"].(map[string]any)
			if !ok {
				t.Fatalf("session.create = %v", response)
			}
			sid, _ := result["session_id"].(string)
			return sid, ws
		}
	}
}

// Snapshot then PATCH with identity and profile in both query and body, what the Desktop sidebar sends
func desktopSidebarPatch(t *testing.T, client *originClient, endpoint map[string]any, profile, sid string, change map[string]any, requestID string) (int, map[string]any, string) {
	t.Helper()
	status, body := client.call("GET", fmt.Sprintf("/api/sessions/%s/mutation-snapshot?profile=%s", sid, profile),
		http.Header{nativeTicketHeader: {endpointTicket(t, endpoint, "native-http")}}, "")
	wantStatus(t, "mutation snapshot "+body, status, 200)
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		t.Fatal(err)
	}
	edit := map[string]any{}
	for k, v := range change {
		edit[k] = v
	}
	edit["request_id"], edit["expected_revision"], edit["expected_generation"] = requestID, snapshot["runtime_revision"], snapshot["runtime_generation"]
	edit["profile"] = profile
	raw, _ := json.Marshal(edit)
	status, body = client.call("PATCH", fmt.Sprintf("/api/sessions/%s?profile=%s", sid, profile),
		http.Header{nativeTicketHeader: {endpointTicket(t, endpoint, "native-http")}, "Content-Type": {"application/json"}}, string(raw))
	var reply map[string]any
	_ = json.Unmarshal([]byte(body), &reply)
	return status, reply, body
}

// One integer column of the first row of a read only query of a state database
func scalarInt(t *testing.T, dbPath, query string, args ...any) (int64, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value int64
	err = db.QueryRowContext(context.Background(), query, args...).Scan(&value)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return value, true
}

func TestColdProfileArchiveUnarchiveWhileAnotherProfileIsAttached(t *testing.T) {
	tmp, h, user := gatewayCaseHomes(t, map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": true},
		"model":     map[string]any{"provider": "custom", "default": "launch-model", "base_url": "http://127.0.0.1:1/v1"},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
	})
	for _, name := range []string{"warm", "cold"} {
		secondary := filepath.Join(h, "profiles", name)
		if err := os.MkdirAll(secondary, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSONConfig(t, filepath.Join(secondary, "config.yaml"), map[string]any{
			"model": map[string]any{"provider": "custom", "default": name + "-model", "base_url": "http://127.0.0.1:1/v1"}})
	}
	env := gatewayChildEnv(h, user)
	daemon := startGatewayChild(t, filepath.Join(tmp, "gateway.log"), env, gateway.GatewayRunModuleEntry)
	descriptor := daemon.waitReady(t, h, 60*time.Second)
	origin, _ := descriptor["api_origin"].(string)
	client := newOriginClient(t, origin)
	resolve := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	homeResolved, coldHome, warmHome := resolve(h), resolve(filepath.Join(h, "profiles", "cold")), resolve(filepath.Join(h, "profiles", "warm"))
	var served []any
	for _, raw := range descriptor["served_profiles"].([]any) {
		if profile, isDict := raw.(map[string]any); isDict {
			served = append(served, profile["profile_id"])
		}
	}
	for _, want := range []string{homeResolved, coldHome, warmHome} {
		if !slices.Contains(served, any(want)) {
			t.Fatalf("served = %v", descriptor)
		}
	}
	endpoints := map[string]map[string]any{}
	for _, name := range []string{"default", "warm", "cold"} {
		endpoints[name] = desktopEnsure(t, env, name)
		if endpoints[name]["instance_id"] != descriptor["instance_id"] {
			t.Fatalf("%s endpoint = %v", name, endpoints[name])
		}
	}
	if endpoints["cold"]["profile_id"] != coldHome || endpoints["cold"]["control_home"] != homeResolved || endpoints["default"]["control_home"] != nil {
		t.Fatalf("endpoints = %v", endpoints)
	}
	coldSID, coldWS := createViaWS(t, endpoints["cold"], "cold-create")
	coldWS.Close()
	_, warmWS := createViaWS(t, endpoints["warm"], "warm-create")
	status, archived, body := desktopSidebarPatch(t, client, endpoints["cold"], "cold", coldSID, map[string]any{"archived": true}, "archive")
	wantStatus(t, "archive "+body, status, 200)
	readRow := func() map[string]any {
		_, body := client.call("GET", fmt.Sprintf("/api/sessions/%s?profile=cold", coldSID),
			http.Header{nativeTicketHeader: {endpointTicket(t, endpoints["cold"], "native-http")}}, "")
		var row map[string]any
		if err := json.Unmarshal([]byte(body), &row); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return row
	}
	if row := readRow(); row["archived"] != float64(1) || row["profile"] != "cold" {
		t.Fatalf("archived row = %v", row)
	}
	status, restored, body := desktopSidebarPatch(t, client, endpoints["cold"], "cold", coldSID, map[string]any{"archived": false}, "unarchive")
	wantStatus(t, "unarchive "+body, status, 200)
	if row := readRow(); row["archived"] != float64(0) || row["profile"] != "cold" {
		t.Fatalf("restored row = %v", row)
	}
	warmWS.Close()
	if archived["archived"] != true || restored["archived"] != false {
		t.Fatalf("archive %v unarchive %v", archived, restored)
	}
	if before, after := archived["revision"].(float64), restored["revision"].(float64); after != before+1 {
		t.Fatalf("revisions %v then %v", before, after)
	}
	if value, found := scalarInt(t, filepath.Join(coldHome, "state.db"), "SELECT archived FROM sessions WHERE id=?", coldSID); !found || value != 0 {
		t.Fatalf("cold store archived = %d %v", value, found)
	}
	if count, _ := scalarInt(t, filepath.Join(h, "state.db"), "SELECT COUNT(*) FROM sessions WHERE id=?", coldSID); count != 0 {
		t.Fatalf("launch store holds %d cold rows", count)
	}
	status, body = client.call("GET", fmt.Sprintf("/api/sessions/%s/mutation-snapshot?profile=cold", coldSID),
		http.Header{nativeTicketHeader: {endpointTicket(t, endpoints["default"], "native-http")}}, "")
	wantStatus(t, "default grant on the cold profile "+body, status, 403)
}
