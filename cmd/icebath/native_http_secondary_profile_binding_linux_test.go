//go:build linux

package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestSecondaryNativeTicketImplicitSelectorsTouchOnlyTheSecondaryHome(t *testing.T) {
	tmp, h, user := gatewayCaseHomes(t, map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": true},
		"model":     map[string]any{"provider": "custom", "default": "launch-model", "base_url": "http://127.0.0.1:1/v1"},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
	})
	beta := filepath.Join(h, "profiles", "beta")
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONConfig(t, filepath.Join(beta, "config.yaml"), map[string]any{"model": map[string]any{"provider": "custom", "default": "beta-model"}})
	launchBefore, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	env := gatewayChildEnv(h, user, "ICEBATH_DASHBOARD_SESSION_TOKEN=normal-http-owner")
	daemon := startGatewayChild(t, filepath.Join(tmp, "gateway.log"), env, gateway.GatewayRunModuleEntry)
	descriptor := daemon.waitReady(t, h, 60*time.Second)
	resolvedHome, _ := filepath.EvalSymlinks(h)
	resolvedBeta, _ := filepath.EvalSymlinks(beta)
	var served []any
	for _, raw := range descriptor["served_profiles"].([]any) {
		if profile, isDict := raw.(map[string]any); isDict {
			served = append(served, profile["profile_id"])
		}
	}
	if !slices.Contains(served, any(resolvedBeta)) || !slices.Contains(served, any(resolvedHome)) {
		t.Fatalf("served = %v", descriptor)
	}
	ticket := func() http.Header {
		grant, err := gatewayControl(h, "session-ticket", map[string]any{"profile_id": resolvedBeta,
			"instance_id": descriptor["instance_id"], "purpose": "native-http"})
		if err != nil {
			t.Fatal(err)
		}
		value, _ := grant["ticket"].(string)
		return http.Header{nativeTicketHeader: {value}}
	}
	origin, _ := descriptor["api_origin"].(string)
	client := newOriginClient(t, origin)
	for _, query := range []url.Values{{}, {"profile": {""}}, {"profile": {"current"}}} {
		path := "/api/config"
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
		status, body := client.call("GET", path, ticket(), "")
		if status != 200 || !strings.Contains(body, "beta-model") || strings.Contains(body, "launch-model") {
			t.Fatalf("GET %s = %d %s", path, status, body)
		}
	}
	for _, selector := range []map[string]any{{}, {"profile": "current"}} {
		edit := map[string]any{"config": map[string]any{"display": map[string]any{"skin": "ares"}}}
		for k, v := range selector {
			edit[k] = v
		}
		raw, _ := json.Marshal(edit)
		header := ticket()
		header.Set("Content-Type", "application/json")
		status, body := client.call("PUT", "/api/config", header, string(raw))
		wantStatus(t, "PUT "+string(raw)+" "+body, status, 200)
	}
	raw, err := os.ReadFile(filepath.Join(beta, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Display map[string]any `yaml:"display"`
	}
	if err := yaml.Unmarshal(raw, &stored); err != nil || stored.Display["skin"] != "ares" {
		t.Fatalf("beta config = %s, %v", raw, err)
	}
	if after, err := os.ReadFile(filepath.Join(h, "config.yaml")); err != nil || string(after) != string(launchBefore) {
		t.Fatalf("launch config changed: %s, %v", after, err)
	}
}
