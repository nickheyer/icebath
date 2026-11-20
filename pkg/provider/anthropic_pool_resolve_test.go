package provider

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func anthWritePool(t *testing.T, h string, rows ...map[string]any) {
	t.Helper()
	list := make([]any, 0, len(rows))
	for _, r := range rows {
		list = append(list, r)
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"anthropic": list}})
}

func TestAnthropicDoesNotResolvePrimaryAPIKeyAsNativeToken(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	for _, dir := range []string{os.Getenv("HOME"), os.Getenv("CLAUDE_CONFIG_DIR")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"primaryApiKey":"sk-ant-api03-primary"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := ResolveAnthropicToken(env, ""); got != "" {
		t.Fatalf("primaryApiKey resolved as a token: %q", got)
	}
}

func TestAnthropicFallsBackToClaudeCodeCredentials(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeClaudeCodeCredentials(t, "cc-auto-token", "refresh", time.Now().UnixMilli()+3_600_000)
	if got := ResolveAnthropicToken(env, ""); got != "cc-auto-token" {
		t.Fatalf("got %q", got)
	}
}

func TestAnthropicFallsBackToCredentialPoolOAuth(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	anthWritePool(t, h, map[string]any{"id": "p1", "label": "pool", "auth_type": "oauth", "priority": 0, "source": "manual", "access_token": "pool-oauth-token"})
	if got := ResolveAnthropicToken(env, ""); got != "pool-oauth-token" {
		t.Fatalf("got %q", got)
	}
}

func TestAnthropicPoolEntryWithNullAccessTokenDoesNotCrash(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant...ykey")
	anthWritePool(t, h, map[string]any{"id": "p1", "label": "broken", "auth_type": "oauth", "priority": 0, "source": "manual", "access_token": nil})
	if got := ResolveAnthropicToken(env, ""); got != "sk-ant...ykey" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if got := resolveAnthropicPoolToken(env, true); got != "" {
		t.Fatalf("null token entry returned %q", got)
	}
}

func TestAnthropicPoolAPIKeyOnlyEntryIsNotReturnedAsToken(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	anthWritePool(t, h, map[string]any{"id": "p1", "label": "key", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-pool-apikey"})
	if got := ResolveAnthropicToken(env, ""); got != "" {
		t.Fatalf("api key pool entry returned as a token %q", got)
	}
}

func TestAnthropicPoolResolutionIsReadOnly(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	anthWritePool(t, h,
		map[string]any{"id": "p0", "label": "spent", "auth_type": "oauth", "priority": 0, "source": "manual", "access_token": "spent-token",
			"refresh_token": "spent-refresh", "expires_at": isoIn(-3600), "last_status": "exhausted", "last_status_at": float64(time.Now().Unix() - 10),
			"last_error_reset_at": float64(time.Now().Unix() - 5)},
		map[string]any{"id": "p1", "label": "pool", "auth_type": "oauth", "priority": 1, "source": "manual", "access_token": "pool-oauth-token"})
	path := filepath.Join(h, "auth.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	calls := anthTokenEndpoint(&env, map[string]any{"error": "must not refresh"}, true)
	got := ResolveAnthropicToken(env, "")
	after, _ := os.ReadFile(path)
	if *calls != 0 || !bytes.Equal(before, after) {
		t.Fatalf("resolve refreshed (%d calls) or wrote auth.json\nbefore %s\nafter %s", *calls, before, after)
	}
	if got != "spent-token" && got != "pool-oauth-token" {
		t.Fatalf("got %q", got)
	}
}

func TestAnthropicAutoRefreshOnExpiredClaudeCodeCreds(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeClaudeCodeCredentials(t, "expired-tok", "valid-refresh", time.Now().UnixMilli()-3_600_000)
	anthTokenEndpoint(&env, map[string]any{"access_token": "refreshed-token", "refresh_token": "next-refresh", "expires_in": 3600}, false)
	if got := ResolveAnthropicToken(env, ""); got != "refreshed-token" {
		t.Fatalf("got %q", got)
	}
}
