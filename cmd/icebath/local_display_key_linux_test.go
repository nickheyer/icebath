//go:build linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestImpeccableSlashExecHandsTheTUIItsSkillTurnAndRefusesMidTurn(t *testing.T) {
	hold, holding := make(chan struct{}), make(chan struct{}, 1)
	peer := &modelPeer{}
	peer.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("HOLD_THE_TURN")) {
			holding <- struct{}{}
			<-hold
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		peer.serve(w, r)
	}))
	t.Cleanup(peer.server.Close)
	released := false
	release := func() {
		if !released {
			released = true
			close(hold)
		}
	}
	t.Cleanup(release)
	modelURL := peer.server.URL + "/v1"
	tmp, h, user := gatewayCaseHomes(t, map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": false},
		"model":     map[string]any{"provider": "custom", "default": "impeccable-model", "base_url": modelURL},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
		"plugins":   map[string]any{"enabled": []any{"impeccable"}},
	})
	work := filepath.Join(tmp, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	pluginDir := filepath.Join(h, "plugins", "impeccable")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte("name: impeccable\nkind: standalone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "plugins", ".install-metadata.json"), []byte(`{"impeccable": {"source": "builtin:impeccable"}}`), 0o600); err != nil {
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
	created := sessionResult(t, ws, "session.create", map[string]any{"request_id": "impeccable", "source": "tui", "toolsets": []any{}, "cwd": work})
	sid, _ := created["session_id"].(string)
	catalog := sessionResult(t, ws, "commands.catalog", map[string]any{"session_id": sid})
	subs, _ := catalog["sub"].(map[string]any)
	if listed, _ := subs["/impeccable"].([]any); len(listed) != 24 || listed[0] != "craft" || !strings.Contains(fmt.Sprint(catalog["pairs"]), "[/impeccable ") {
		t.Fatalf("the catalog does not offer /impeccable and its subcommands: %v %v", subs["/impeccable"], catalog["pairs"])
	}
	turnOf := func(command, want string) string {
		t.Helper()
		turn := sessionResult(t, ws, "slash.exec", map[string]any{"session_id": sid, "command": command})
		message, _ := turn["message"].(string)
		display, _ := turn["display"].(string)
		if turn["type"] != "skill" || turn["name"] != "impeccable" || !strings.HasPrefix(display, "/impeccable") ||
			!strings.HasPrefix(message, `[IMPORTANT: The user has invoked the "impeccable`) || !strings.Contains(message, want) {
			t.Fatalf("%s answered %v", command, turn)
		}
		return message
	}
	if taught := turnOf("impeccable teach the checkout flow", "init the checkout flow"); strings.Contains(taught, "teach the checkout flow") {
		t.Fatalf("teach was not routed to init: %s", taught)
	}
	if admitted := sessionResult(t, ws, "prompt.submit", map[string]any{"session_id": sid, "input_id": "held", "text": "HOLD_THE_TURN"}); admitted["status"] != "queued" {
		t.Fatalf("prompt.submit = %v", admitted)
	}
	select {
	case <-holding:
	case <-time.After(60 * time.Second):
		t.Fatalf("the held turn never reached the model\n%s", homeFailureLines(h))
	}
	busy := sessionResult(t, ws, "slash.exec", map[string]any{"session_id": sid, "command": "impeccable audit the header"})
	if output, _ := busy["output"].(string); busy["type"] != "exec" || !strings.Contains(output, "`/impeccable` can't run mid-turn") {
		t.Fatalf("mid-turn /impeccable answered %v", busy)
	}
	release()
	message := ""
	for deadline := time.Now().Add(30 * time.Second); message == ""; {
		if reply := sessionRPC(t, ws, "slash.exec", map[string]any{"session_id": sid, "command": "impeccable audit the header"}); reply["result"] != nil {
			if result, _ := reply["result"].(map[string]any); result["type"] == "skill" {
				message = turnOf("impeccable audit the header", "audit the header")
			}
		}
		if message == "" && time.Now().After(deadline) {
			t.Fatalf("the session never went idle\n%s", homeFailureLines(h))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if admitted := sessionResult(t, ws, "prompt.submit", map[string]any{"session_id": sid, "input_id": "impeccable-turn", "text": message}); admitted["status"] != "queued" {
		t.Fatalf("prompt.submit = %v", admitted)
	}
	for deadline := time.Now().Add(60 * time.Second); ; {
		if modelReceived(peer, message) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the model never received the impeccable turn\n%s", homeFailureLines(h))
		}
		time.Sleep(50 * time.Millisecond)
	}
	ws.Close()
	if ok, err := child.interrupt(20 * time.Second); !ok {
		t.Fatalf("the gateway did not stop on SIGINT: %v\n%s", err, child.output())
	}
}

// Whether a chat request the peer served carried the text in its last user message
func modelReceived(peer *modelPeer, text string) bool {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for _, request := range peer.requests {
		messages, _ := request["messages"].([]any)
		for i := len(messages) - 1; i >= 0; i-- {
			if m, isDict := messages[i].(map[string]any); isDict && m["role"] == "user" {
				var parts []string
				switch content := m["content"].(type) {
				case string:
					parts = append(parts, content)
				case []any:
					for _, part := range content {
						if p, isPart := part.(map[string]any); isPart {
							if s, isText := p["text"].(string); isText {
								parts = append(parts, s)
							}
						}
					}
				}
				if strings.Contains(strings.Join(parts, "\n"), text) {
					return true
				}
				break
			}
		}
	}
	return false
}
