package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func anthCredFile() string { return filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json") }

func anthReadCredFile(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(anthCredFile())
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

type anthFuncTransport func(*http.Request) (*http.Response, error)

func (f anthFuncTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// OAuth token endpoint double answering every refresh with the given payload and counting calls
func anthTokenEndpoint(env *PoolEnv, payload map[string]any, fail bool) *int {
	calls := 0
	env.HTTP = &http.Client{Transport: anthFuncTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if fail {
			return nil, errors.New("network error")
		}
		return jsonResponse(r, 200, payload, nil), nil
	})}
	return &calls
}

func TestAnthropicReadClaudeCodeCredentials(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeClaudeCodeCredentials(t, "sk-ant-oat01-token", "sk-ant-oat01-refresh", time.Now().UnixMilli()+3_600_000)
	creds := ReadClaudeCodeCredentials(env)
	if creds == nil || creds["accessToken"] != "sk-ant-oat01-token" || creds["refreshToken"] != "sk-ant-oat01-refresh" || creds["source"] != "claude_code_credentials_file" {
		t.Fatalf("credentials wrong: %v", creds)
	}
}

func TestAnthropicReadClaudeCodeCredentialsIgnoresPrimaryAPIKey(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	for _, dir := range []string{os.Getenv("HOME"), os.Getenv("CLAUDE_CONFIG_DIR")} {
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"primaryApiKey": "sk-ant-api03-primary"}`), 0o600)
	}
	if creds := ReadClaudeCodeCredentials(env); creds != nil {
		t.Fatalf("primaryApiKey must not resolve: %v", creds)
	}
}

func TestAnthropicIsClaudeCodeTokenValid(t *testing.T) {
	now := time.Now().UnixMilli()
	if !IsClaudeCodeTokenValid(map[string]any{"accessToken": "tok", "expiresAt": now + 3_600_000}) {
		t.Fatal("valid token")
	}
	if IsClaudeCodeTokenValid(map[string]any{"accessToken": "tok", "expiresAt": now - 3_600_000}) {
		t.Fatal("expired token")
	}
	if !IsClaudeCodeTokenValid(map[string]any{"accessToken": "tok", "expiresAt": 0}) {
		t.Fatal("no expiry but a token is valid")
	}
}

func TestAnthropicResolveTokenEnvOrder(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-mykey")
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-oat01-mytoken")
	if got := ResolveAnthropicToken(env, ""); got != "sk-ant-oat01-mytoken" {
		t.Fatalf("OAuth env token wins over the api key: %q", got)
	}
	t.Setenv("ANTHROPIC_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant...ykey")
	if got := ResolveAnthropicToken(env, ""); got != "sk-ant...ykey" {
		t.Fatalf("api key fallback: %q", got)
	}
	writeClaudeCodeCredentials(t, "cc-auto-token", "refresh", time.Now().UnixMilli()+3_600_000)
	if got := ResolveAnthropicToken(env, ""); got != "sk-ant...ykey" {
		t.Fatalf("api key wins over discovered Claude Code credentials: %q", got)
	}
}

func TestAnthropicPrefersRefreshableClaudeCodeCredentialsOverStaticToken(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-oat01-static-token")
	writeClaudeCodeCredentials(t, "cc-auto-token", "refresh-token", time.Now().UnixMilli()+3_600_000)
	if got := ResolveAnthropicToken(env, ""); got != "cc-auto-token" {
		t.Fatalf("refreshable credentials must win: %q", got)
	}
}

func TestAnthropicRefreshOAuthToken(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	calls := anthTokenEndpoint(&env, map[string]any{"access_token": "new-token-abc", "refresh_token": "new-refresh-456", "expires_in": 7200}, false)
	if got := refreshClaudeCodeOAuthToken(env, map[string]any{"accessToken": "expired", "refreshToken": "", "expiresAt": 0}); got != "" || *calls != 0 {
		t.Fatalf("no refresh token must not refresh: %q %d", got, *calls)
	}
	got := refreshClaudeCodeOAuthToken(env, map[string]any{"accessToken": "old-token", "refreshToken": "refresh-123", "expiresAt": time.Now().UnixMilli() - 3_600_000})
	if got != "new-token-abc" {
		t.Fatalf("refresh result %q", got)
	}
	oauth := anthReadCredFile(t)["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != "new-token-abc" || oauth["refreshToken"] != "new-refresh-456" {
		t.Fatalf("refreshed credentials not written: %v", oauth)
	}
}

func TestAnthropicFailedRefreshReturnsEmpty(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	anthTokenEndpoint(&env, nil, true)
	if got := refreshClaudeCodeOAuthToken(env, map[string]any{"accessToken": "old", "refreshToken": "refresh-123", "expiresAt": 0}); got != "" {
		t.Fatalf("failed refresh must return nothing: %q", got)
	}
}

func TestAnthropicWriteClaudeCodeCredentials(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	if err := WriteClaudeCodeCredentials(env, "tok", "ref", 12345, nil, ""); err != nil {
		t.Fatal(err)
	}
	oauth := anthReadCredFile(t)["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != "tok" || oauth["refreshToken"] != "ref" || oauth["expiresAt"] != float64(12345) {
		t.Fatalf("written credentials wrong: %v", oauth)
	}
	info, err := os.Stat(anthCredFile())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode must be 0600: %v %v", info.Mode(), err)
	}
	if err := os.WriteFile(anthCredFile(), []byte(`{"otherField": "keep-me"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteClaudeCodeCredentials(env, "new-tok", "new-ref", 99999, nil, ""); err != nil {
		t.Fatal(err)
	}
	data := anthReadCredFile(t)
	if data["otherField"] != "keep-me" || data["claudeAiOauth"].(map[string]any)["accessToken"] != "new-tok" {
		t.Fatalf("existing fields must survive: %v", data)
	}
}

func TestAnthropicStaticEnvOAuthTokenDoesNotBlockRefreshableClaudeCreds(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-oat01-expired-env-token")
	writeClaudeCodeCredentials(t, "expired-claude-creds-token", "valid-refresh", time.Now().UnixMilli()-3_600_000)
	anthTokenEndpoint(&env, map[string]any{"access_token": "refreshed-token", "refresh_token": "next-refresh", "expires_in": 3600}, false)
	if got := ResolveAnthropicToken(env, ""); got != "refreshed-token" {
		t.Fatalf("expired Claude Code credentials must refresh past the static env token: %q", got)
	}
}

// Fake claude CLI on PATH whose setup-token run leaves a marker file
func anthFakeClaudeCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := "#!/bin/sh\n[ \"$1\" = setup-token ] && : > '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func TestAnthropicRunOAuthSetupToken(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	marker := anthFakeClaudeCLI(t)
	writeClaudeCodeCredentials(t, "from-cred-file", "refresh", time.Now().UnixMilli()+3_600_000)
	token, err := RunOAuthSetupToken(env)
	if err != nil || token != "from-cred-file" {
		t.Fatalf("setup token from credentials file: %q %v", token, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("claude setup-token was not run")
	}
}

func TestAnthropicRunOAuthSetupTokenNoCreds(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	anthFakeClaudeCLI(t)
	if token, err := RunOAuthSetupToken(env); err != nil || token != "" {
		t.Fatalf("no credentials must yield no token: %q %v", token, err)
	}
}
