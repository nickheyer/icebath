package tuiserver

import (
	"context"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Launch home under a fake user home with no profile created yet
func envRefsLaunchHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func TestNewBotConfigKeepsLaunchEnvRefsNotTheirValues(t *testing.T) {
	h := envRefsLaunchHome(t)
	secrets := map[string]string{"FAKE_TTS_KEY": "tts-FAKE-111", "FAKE_STT_KEY": "stt-FAKE-222", "FAKE_MCP_TOKEN": "mcp-FAKE-333"}
	for name, value := range secrets {
		t.Setenv(name, value)
	}
	launch := "tts:\n  provider: elevenlabs\n  elevenlabs:\n    api_key: ${FAKE_TTS_KEY}\n" +
		"stt:\n  provider: openai\n  openai:\n    api_key: ${FAKE_STT_KEY}\n" +
		"mcp_servers:\n  tracker:\n    command: tracker-mcp\n    env:\n      TRACKER_TOKEN: ${FAKE_MCP_TOKEN}\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(launch), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{"profiles.create", map[string]any{"name": "scout", "no_alias": true, "no_skills": true}},
		{"profiles.configure", map[string]any{"name": "scout", "enabled_mcp_servers": []any{"tracker"}}},
	} {
		reply := pyjson.Plain(lookupMethod(call.method)(context.Background(), 1, call.params)).(map[string]any)
		if _, failed := reply["error"]; failed {
			t.Fatalf("%s answered %v", call.method, reply["error"])
		}
	}
	written, err := os.ReadFile(filepath.Join(h, "profiles", "scout", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range secrets {
		if !strings.Contains(string(written), "${"+name+"}") {
			t.Fatalf("the ref ${%s} did not travel\n%s", name, written)
		}
		if strings.Contains(string(written), value) {
			t.Fatalf("the value of %s was written\n%s", name, written)
		}
	}
}
