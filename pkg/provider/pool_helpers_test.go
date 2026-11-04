package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var seedableEnvVars = []string{
	"OPENROUTER_API_KEY", "OPENROUTER_API_KEY_2", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	"DEEPSEEK_API_KEY", "DEEPSEEK_API_KEY_2", "DEEPSEEK_API_KEY_3", "DEEPSEEK_API_KEY_5", "OPENROUTER_API_KEY_3", "OPENROUTER_API_KEY_5",
	"ICEBATH_CODEX_BASE_URL", "XAI_API_KEY", "NOUS_API_KEY",
}

// Isolated icebath home with the host's provider env and Claude Code login kept out
func newTestHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, "icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, h)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("HOME", filepath.Join(root, "userhome"))
	for _, v := range seedableEnvVars {
		t.Setenv(v, "")
	}
	home.ResetKeyCache()
	config.ResetCache()
	return h
}

func testEnv(t *testing.T, h string) PoolEnv {
	t.Helper()
	return PoolEnv{Store: NewStore(h), Secrets: config.NewSecrets(h), Config: ConfigLoader(h), Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
}

func writeAuthStore(t *testing.T, h string, payload map[string]any) {
	t.Helper()
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAuthStore(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func poolRows(t *testing.T, h, provider string) []map[string]any {
	t.Helper()
	pool, _ := readAuthStore(t, h)["credential_pool"].(map[string]any)
	raw, _ := pool[provider].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func rowByID(rows []map[string]any, id string) map[string]any {
	for _, r := range rows {
		if r["id"] == id {
			return r
		}
	}
	return nil
}

func jwtWithClaims(claims map[string]any) string {
	part := func(payload map[string]any) string {
		raw, _ := json.Marshal(payload)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return part(map[string]any{"alg": "none", "typ": "JWT"}) + "." + part(claims) + ".sig"
}

func isoIn(seconds float64) string {
	return time.Now().Add(time.Duration(seconds * float64(time.Second))).UTC().Format(time.RFC3339)
}

func row(fields map[string]any) map[string]any { return fields }

func mustSelect(t *testing.T, pool *Pool, model string) PooledCredential {
	t.Helper()
	entry, ok := pool.Select(model)
	if !ok {
		t.Fatal("expected a selected entry")
	}
	return entry
}

func statusesOf(pool *Pool) map[string]string {
	out := map[string]string{}
	for _, e := range pool.Entries() {
		out[e.ID] = e.LastStatus
	}
	return out
}

func entryByID(pool *Pool, id string) (PooledCredential, bool) {
	for _, e := range pool.Entries() {
		if e.ID == id {
			return e, true
		}
	}
	return PooledCredential{}, false
}

func writeEnvFile(t *testing.T, h, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeConfigYAML(t *testing.T, h, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

func writeClaudeCodeCredentials(t *testing.T, access, refresh string, expiresAt int64) {
	t.Helper()
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": access, "refreshToken": refresh, "expiresAt": expiresAt}})
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Collects log messages so tests can count throttled lines
type recordingHandler struct {
	mu       sync.Mutex
	messages []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) count(message string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.messages {
		if m == message {
			n++
		}
	}
	return n
}

func (h *recordingHandler) containing(fragment string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.messages {
		if strings.Contains(m, fragment) {
			out = append(out, m)
		}
	}
	return out
}

func newRecordingLogger(h *recordingHandler) *slog.Logger { return slog.New(h) }
