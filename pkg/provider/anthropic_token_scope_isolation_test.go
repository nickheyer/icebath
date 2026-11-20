package provider

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Multiplexing pool env over a temp home with an empty pool and no borrowed login, reading secrets through ctx
func anthropicScopeEnv(t *testing.T, ctx context.Context) PoolEnv {
	t.Helper()
	resetMultiplex(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	h := newTestHome(t)
	env := testEnv(t, h)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	env.Ctx = ctx
	SetMultiplexActive(true)
	return env
}

// Removes the variables for the test, the previous values restored afterwards
func unsetForTest(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func TestApiKeyScopeIsolationScopedApiKeyUsedOverEnviron(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-WRONG-OTHER-PROFILE")
	unsetForTest(t, "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN")
	ctx := WithSecretScope(context.Background(), map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api-CORRECT-PROFILE"}, "")
	if got := ResolveAnthropicToken(anthropicScopeEnv(t, ctx), ""); got != "sk-ant-api-CORRECT-PROFILE" {
		t.Fatalf("resolve_anthropic_token() returned %q (from os.environ) instead of 'sk-ant-api-CORRECT-PROFILE' (from profile scope)", got)
	}
}

func TestApiKeyScopeIsolationTwoProfilesReturnDifferentKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-SHARED-ENVIRON-WRONG")
	unsetForTest(t, "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN")
	env := anthropicScopeEnv(t, context.Background())
	env.Ctx = WithSecretScope(context.Background(), map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api-PROFILE-A"}, "")
	resultA := ResolveAnthropicToken(env, "")
	env.Ctx = WithSecretScope(context.Background(), map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api-PROFILE-B"}, "")
	resultB := ResolveAnthropicToken(env, "")
	if resultA != "sk-ant-api-PROFILE-A" {
		t.Fatalf("Profile A got %q — expected its own key.", resultA)
	}
	if resultB != "sk-ant-api-PROFILE-B" {
		t.Fatalf("Profile B got %q — expected its own key.", resultB)
	}
	if resultA == resultB {
		t.Fatalf("Both profiles resolved to %q. Credential isolation is broken — both read from os.environ.", resultA)
	}
}

func TestOAuthTokenLeakageFromEnvironAnthropicTokenEnvDoesNotShadowScopedApiKey(t *testing.T) {
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-oat-LEAKED-PROFILE-B")
	unsetForTest(t, "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY")
	ctx := WithSecretScope(context.Background(), map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api-PROFILE-A"}, "")
	got := ResolveAnthropicToken(anthropicScopeEnv(t, ctx), "")
	if got == "sk-ant-oat-LEAKED-PROFILE-B" {
		t.Fatal("Profile B's OAuth token leaked from os.environ[ANTHROPIC_TOKEN]. Profile A received the wrong credential.")
	}
	if got != "sk-ant-api-PROFILE-A" {
		t.Fatalf("Expected Profile A's API key but got %q.", got)
	}
}

func TestOAuthTokenLeakageFromEnvironAnthropicTokenInScopeIsUsed(t *testing.T) {
	unsetForTest(t, "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY")
	ctx := WithSecretScope(context.Background(), map[string]string{"ANTHROPIC_TOKEN": "sk-ant-oat-PROFILE-A-OWN-OAUTH"}, "")
	if got := ResolveAnthropicToken(anthropicScopeEnv(t, ctx), ""); got != "sk-ant-oat-PROFILE-A-OWN-OAUTH" {
		t.Fatalf("Profile A's own OAuth token (in scope) was not returned; got %q.", got)
	}
}

func TestClaudeCodeOAuthTokenLeakageCcOauthEnvDoesNotShadowScopedApiKey(t *testing.T) {
	unsetForTest(t, "ANTHROPIC_TOKEN", "ANTHROPIC_API_KEY")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-CC-LEAKED-ENVIRON")
	ctx := WithSecretScope(context.Background(), map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api-PROFILE-X"}, "")
	got := ResolveAnthropicToken(anthropicScopeEnv(t, ctx), "")
	if got == "sk-ant-oat-CC-LEAKED-ENVIRON" {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN leaked from os.environ. Profile X's Anthropic call used the wrong credential.")
	}
	if got != "sk-ant-api-PROFILE-X" {
		t.Fatalf("Expected Profile X's API key but got %q.", got)
	}
}

func TestCronSchedulerUnscopedCallUnscopedCallInMultiplexModeFailsClosed(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-PROCESS-LEVEL-SHOULD-NOT-LEAK")
	unsetForTest(t, "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN")
	env := anthropicScopeEnv(t, context.Background())
	defer func() {
		recovered := recover()
		unscoped, ok := recovered.(*UnscopedSecretError)
		if !ok {
			t.Fatalf("UnscopedSecretError expected, got %v", recovered)
		}
		if !strings.Contains(unscoped.Error(), "ANTHROPIC") {
			t.Fatalf("error names no Anthropic secret: %s", unscoped.Error())
		}
	}()
	ResolveAnthropicToken(env, "")
	t.Fatal("an unscoped resolve under multiplexing returned instead of failing closed")
}
