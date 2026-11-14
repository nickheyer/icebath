//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestReasoningHideAndFooterReachTheLocalClientsConfigRead(t *testing.T) {
	peer := newModelPeer(t)
	modelURL := peer.server.URL + "/v1"
	tmp, h, user := gatewayCaseHomes(t, map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": false},
		"model":     map[string]any{"provider": "custom", "default": "display-model", "base_url": modelURL},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
		"display":   map[string]any{"show_reasoning": true, "platforms": map[string]any{"cli": map[string]any{"runtime_footer": map[string]any{"enabled": true}}}},
	})
	work := filepath.Join(tmp, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(h, "cache", "models_dev_cache.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(`{"icebath-test-offline": {"id": "icebath-test-offline", "env": [], "models": {}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	child := startGatewayChild(t, filepath.Join(h, "restart.log"),
		gatewayChildEnv(h, user, "OPENAI_API_KEY=loopback-only", "OPENAI_BASE_URL="+modelURL, runMainEnv+"=1"), "gateway", "run")
	descriptor := child.waitReady(t, h, 120*time.Second)
	bootHome = h
	ws, _ := dialSession(t, h, wsOrigin(descriptor["api_origin"].(string)),
		map[string]any{"profile_id": h, "instance_id": descriptor["instance_id"], "purpose": "interactive"})
	created := sessionResult(t, ws, "session.create", map[string]any{"request_id": "display", "source": "tui", "toolsets": []any{}, "cwd": work})
	sid, _ := created["session_id"].(string)
	for _, command := range []string{"reasoning hide", "footer off"} {
		sessionResult(t, ws, "slash.exec", map[string]any{"session_id": sid, "command": command})
	}
	full := sessionResult(t, ws, "config.get", map[string]any{"session_id": sid, "key": "full"})
	served, _ := full["config"].(map[string]any)
	display, _ := served["display"].(map[string]any)
	ws.Close()
	if ok, err := child.interrupt(20 * time.Second); !ok {
		t.Fatalf("the gateway did not stop on SIGINT: %v\n%s", err, child.output())
	}

	saved, err := config.ReadUserConfigRaw(context.Background(), filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	savedDisplay, _ := saved["display"].(map[string]any)
	platforms, _ := savedDisplay["platforms"].(map[string]any)
	cli, _ := platforms["cli"].(map[string]any)
	if cli["show_reasoning"] != false {
		t.Fatalf("local show_reasoning %v", saved)
	}
	if savedDisplay["show_reasoning"] != true {
		t.Fatalf("global show_reasoning %v", saved)
	}
	if footer, _ := cli["runtime_footer"].(map[string]any); footer["enabled"] != false {
		t.Fatalf("local runtime_footer %v", saved)
	}
	if _, global := savedDisplay["runtime_footer"]; global {
		t.Fatalf("the global footer key was written %v", saved)
	}
	if display["show_reasoning"] != false {
		t.Fatalf("served display %v", display)
	}
	if footer, _ := display["runtime_footer"].(map[string]any); footer["enabled"] != false {
		t.Fatalf("served runtime_footer %v", display)
	}
}
