//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Ready daemon of a home with two sibling profiles, the owner token fixed and the model unreachable
func nativeHTTPDaemon(t *testing.T, tmp string) (string, map[string]any, *gatewayChild) {
	t.Helper()
	h, user := filepath.Join(tmp, "state"), filepath.Join(tmp, "user")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(user, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"other", "current"} {
		sibling := filepath.Join(h, "profiles", name)
		if err := os.MkdirAll(sibling, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSONConfig(t, filepath.Join(sibling, "config.yaml"), map[string]any{
			"model": map[string]any{"provider": "custom", "default": "sibling-" + name}})
	}
	writeJSONConfig(t, filepath.Join(h, "config.yaml"), map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": false},
		"model":     map[string]any{"provider": "custom", "default": "native-http-fixture", "base_url": "http://127.0.0.1:1/v1"},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
	})
	env := gatewayChildEnv(h, user, "ICEBATH_DASHBOARD_SESSION_TOKEN=normal-http-owner")
	daemon := startGatewayChild(t, filepath.Join(tmp, "gateway.log"), env, gateway.GatewayRunModuleEntry)
	return h, daemon.waitReady(t, h, 40*time.Second), daemon
}

// Writes the value as the JSON text of a config file
func writeJSONConfig(t *testing.T, path string, value map[string]any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Fresh grant of the purpose from the control socket, the error of a refused mint
func nativeTicket(h string, descriptor map[string]any, purpose string, overrides map[string]any) (string, error) {
	resolved, err := filepath.EvalSymlinks(h)
	if err != nil {
		return "", err
	}
	params := map[string]any{"profile_id": resolved, "instance_id": descriptor["instance_id"], "purpose": purpose}
	for k, v := range overrides {
		params[k] = v
	}
	grant, err := gatewayControl(h, "session-ticket", params)
	if err != nil {
		return "", err
	}
	ticket, _ := grant["ticket"].(string)
	return ticket, nil
}

// Status of a request carrying a fresh native grant beside the extra headers
func (c *originClient) granted(h string, descriptor map[string]any, method, path string, extra http.Header, body string) int {
	c.t.Helper()
	header := http.Header{nativeTicketHeader: {mustNativeTicket(c.t, h, descriptor, "native-http", nil)}}
	for name, values := range extra {
		header[name] = values
	}
	status, _ := c.call(method, path, header, body)
	return status
}

func mustNativeTicket(t *testing.T, h string, descriptor map[string]any, purpose string, overrides map[string]any) string {
	t.Helper()
	ticket, err := nativeTicket(h, descriptor, purpose, overrides)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestNativeHTTPGrantsAreSingleUseAndBoundToDaemon(t *testing.T) {
	tmp := t.TempDir()
	h, descriptor, daemon := nativeHTTPDaemon(t, tmp)
	origin, _ := descriptor["api_origin"].(string)
	client := newOriginClient(t, origin)
	status, _ := client.call("GET", "/api/config", nil, "")
	wantStatus(t, "no credential", status, 401)
	normal := http.Header{"Authorization": {"Bearer normal-http-owner"}}
	status, _ = client.call("GET", "/api/config", normal, "")
	wantStatus(t, "owner token", status, 200)
	if _, body := client.call("GET", "/api/config?profile=other", normal, ""); !strings.Contains(body, "sibling-other") {
		t.Fatalf("sibling config = %s", body)
	}
	siblingConfig := filepath.Join(h, "profiles", "other", "config.yaml")
	siblingBefore, err := os.ReadFile(siblingConfig)
	if err != nil {
		t.Fatal(err)
	}
	first := mustNativeTicket(t, h, descriptor, "native-http", nil)
	status, body := client.call("GET", "/api/config", http.Header{nativeTicketHeader: {first}}, "")
	wantStatus(t, "first grant "+body, status, 200)
	if !strings.Contains(body, "native-http-fixture") {
		t.Fatalf("config = %s", body)
	}
	status, _ = client.call("GET", "/api/config", http.Header{nativeTicketHeader: {first}}, "")
	wantStatus(t, "replayed grant", status, 401)
	status, _ = client.call("GET", "/api/config", http.Header{"Authorization": normal["Authorization"], nativeTicketHeader: {"invalid"}}, "")
	wantStatus(t, "owner token with an invalid grant", status, 401)
	for _, foreign := range []string{"https://evil.example", "", "null"} {
		wantStatus(t, "origin "+foreign, client.granted(h, descriptor, "GET", "/api/config", http.Header{"Origin": {foreign}}, ""), 401)
	}
	status, _ = client.call("GET", "/api/config", http.Header{nativeTicketHeader: {
		mustNativeTicket(t, h, descriptor, "native-http", nil), mustNativeTicket(t, h, descriptor, "native-http", nil)}}, "")
	wantStatus(t, "two grant headers", status, 401)
	for _, purpose := range []string{"interactive", "exposure", "worker-adoption"} {
		status, _ = client.call("GET", "/api/config", http.Header{nativeTicketHeader: {mustNativeTicket(t, h, descriptor, purpose, nil)}}, "")
		wantStatus(t, "purpose "+purpose, status, 401)
	}
	wsURL := strings.Replace(origin, "http:", "ws:", 1) + "/api/ws"
	protocols := []string{gateway.CanonicalGatewayProtocol, gateway.GatewayWSTicketPrefix + mustNativeTicket(t, h, descriptor, "native-http", nil)}
	if ws, response, err := (&websocket.Dialer{Subprotocols: protocols, HandshakeTimeout: 10 * time.Second}).Dial(wsURL, nil); err == nil {
		ws.Close()
		t.Fatal("a native HTTP grant opened the session socket")
	} else if !errors.Is(err, websocket.ErrBadHandshake) || response == nil {
		t.Fatalf("session socket dial = %v", err)
	}
	for _, overrides := range []map[string]any{{"instance_id": "stale"}, {"profile_id": filepath.Join(tmp, "other")}} {
		if _, err := nativeTicket(h, descriptor, "native-http", overrides); err == nil || !strings.Contains(err.Error(), "PermissionError") {
			t.Fatalf("mint with %v = %v", overrides, err)
		}
	}
	for _, path := range []string{"/api/config?profile=other", "/api/config?profile=current&profile=other",
		"/api/profiles/other/soul", "/api/sessions?profile=current"} {
		wantStatus(t, path, client.granted(h, descriptor, "GET", path, nil, ""), 403)
	}
	foreignEdit, _ := json.Marshal(map[string]any{"profile": "other", "config": map[string]any{}})
	wantStatus(t, "PUT foreign config", client.granted(h, descriptor, "PUT", "/api/config",
		http.Header{"Content-Type": {"application/json"}}, string(foreignEdit)), 403)
	for _, contentType := range []string{"application/json", "application/problem+json", ""} {
		wantStatus(t, "PUT foreign config as "+contentType, client.granted(h, descriptor, "PUT", "/api/config",
			http.Header{"Content-Type": {contentType}}, string(foreignEdit)), 403)
	}
	for _, path := range []string{"/api/profiles/sessions", "/api/profiles/sessions/sidebar", "/api/profiles/projects/tree", "/api/cron/jobs"} {
		wantStatus(t, path, client.granted(h, descriptor, "GET", path, nil, ""), 403)
	}
	if after, err := os.ReadFile(siblingConfig); err != nil || string(after) != string(siblingBefore) {
		t.Fatalf("sibling config changed: %s, %v", after, err)
	}
	wantStatus(t, "own profile config", client.granted(h, descriptor, "GET", "/api/config?profile=default", nil, ""), 200)
	wantStatus(t, "own sidebar", client.granted(h, descriptor, "GET", "/api/profiles/sessions/sidebar?recents_profile=default", nil, ""), 200)
	wantStatus(t, "own project tree", client.granted(h, descriptor, "GET", "/api/profiles/projects/tree?profile=default", nil, ""), 200)
	scan := `{"ids": ["x"]}`
	jsonHeader := http.Header{"Content-Type": {"application/json"}}
	wantStatus(t, "own pull request scan", client.granted(h, descriptor, "POST", "/api/profiles/sessions/pull-requests?profile=default", jsonHeader, scan), 200)
	for _, scope := range []string{"", "?profile=all", "?profile=other", "?profile=default&profile=other"} {
		wantStatus(t, "project tree "+scope, client.granted(h, descriptor, "GET", "/api/profiles/projects/tree"+scope, nil, ""), 403)
		wantStatus(t, "pull request scan "+scope, client.granted(h, descriptor, "POST", "/api/profiles/sessions/pull-requests"+scope, jsonHeader, scan), 403)
	}
	wantStatus(t, "current profile config", client.granted(h, descriptor, "GET", "/api/config?profile=current", nil, ""), 200)
	wantStatus(t, "profiles", client.granted(h, descriptor, "GET", "/api/profiles", nil, ""), 200)
	update, _ := json.Marshal(map[string]any{"profile": "current", "config": map[string]any{"display": map[string]any{"skin": "ares"}}})
	status, body = client.call("PUT", "/api/config", http.Header{"Content-Type": {"application/json"},
		nativeTicketHeader: {mustNativeTicket(t, h, descriptor, "native-http", nil)}}, string(update))
	wantStatus(t, "PUT current config "+body, status, 200)
	_, body = client.call("GET", "/api/config", http.Header{nativeTicketHeader: {mustNativeTicket(t, h, descriptor, "native-http", nil)}}, "")
	var served struct {
		Display map[string]any `json:"display"`
	}
	if err := json.Unmarshal([]byte(body), &served); err != nil || served.Display["skin"] != "ares" {
		t.Fatalf("config after the update = %s, %v", body, err)
	}
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Display map[string]any `yaml:"display"`
	}
	if err := yaml.Unmarshal(raw, &stored); err != nil || stored.Display["skin"] != "ares" {
		t.Fatalf("stored config = %s, %v", raw, err)
	}
	queryTicket := mustNativeTicket(t, h, descriptor, "native-http", nil)
	status, _ = client.call("GET", "/api/config?ticket="+queryTicket, nil, "")
	wantStatus(t, "grant in the query", status, 401)
	status, _ = client.call("GET", "/api/config", http.Header{"Cookie": {nativeTicketHeader + "=" + queryTicket}}, "")
	wantStatus(t, "grant in a cookie", status, 401)
	stale := mustNativeTicket(t, h, descriptor, "native-http", nil)
	oldInstance := descriptor["instance_id"]
	identified, err := gatewayControl(h, "identify", nil)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := json.Marshal(identified); strings.Contains(strings.ToLower(string(text)), "ticket") {
		t.Fatalf("identify carries a ticket: %s", text)
	}
	_, _ = daemon.interrupt(20 * time.Second)
	h, descriptor, _ = nativeHTTPDaemon(t, tmp)
	origin, _ = descriptor["api_origin"].(string)
	client = newOriginClient(t, origin)
	if descriptor["instance_id"] == oldInstance {
		t.Fatalf("instance_id %v survived the restart", oldInstance)
	}
	status, _ = client.call("GET", "/api/config", http.Header{nativeTicketHeader: {stale}}, "")
	wantStatus(t, "grant of the previous daemon", status, 401)
	wantStatus(t, "grant of the new daemon", client.granted(h, descriptor, "GET", "/api/config", nil, ""), 200)
	t.Log(`{"native_http": "ordinary daemon config and profiles HTTP passed", "replay_restart_purpose_origin_profile": "rejected"}`)
}

func TestNativeHTTPPrincipalSatisfiesRouteLocalTokenPolicy(t *testing.T) {
	h, descriptor, _ := nativeHTTPDaemon(t, t.TempDir())
	origin, _ := descriptor["api_origin"].(string)
	client := newOriginClient(t, origin)
	status, body := client.call("GET", "/api/dashboard/plugins/hub", http.Header{nativeTicketHeader: {mustNativeTicket(t, h, descriptor, "native-http", nil)}}, "")
	wantStatus(t, "plugins hub "+body, status, 200)
	status, body = client.call("DELETE", "/api/providers/oauth/__not_a_real_provider__",
		http.Header{nativeTicketHeader: {mustNativeTicket(t, h, descriptor, "native-http", nil)}}, "")
	wantStatus(t, "oauth delete "+body, status, 400)
	status, _ = client.call("DELETE", "/api/providers/oauth/__not_a_real_provider__", nil, "")
	wantStatus(t, "oauth delete without a credential", status, 401)
}
