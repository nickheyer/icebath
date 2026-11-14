package provider

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestIsAnthropicOAuthToken(t *testing.T) {
	for key, want := range map[string]bool{"sk-ant-oat01-x": true, "eyJhbGciOi": true, "cc-abc": true, "sk-ant-api03-x": false, "sk-ant-usr-x": false, "sk-ant-svc-x": false, "sk-ant-admin-x": false, "sk-ant-future-x": true, "": false, "plain": false} {
		if IsAnthropicOAuthToken(key) != want {
			t.Fatalf("%q: want %t", key, want)
		}
	}
}

func TestAnthropicRouteIsOAuth(t *testing.T) {
	if !AnthropicRouteIsOAuth(context.Background(), "", "sk-ant-oat01-x", nil, "") || !AnthropicRouteIsOAuth(context.Background(), "https://api.anthropic.com/v1", "sk-ant-oat01-x", nil, "") {
		t.Fatal("the native host carries the identity")
	}
	if AnthropicRouteIsOAuth(context.Background(), "https://proxy.example/v1", "sk-ant-oat01-x", nil, "") {
		t.Fatal("third party hosts never do")
	}
	if !AnthropicRouteIsOAuth(context.Background(), "https://proxy.example/v1", "sk-ant-oat01-x", nil, "anthropic") {
		t.Fatal("the anthropic provider qualifies")
	}
	minted := func(context.Context) (string, error) { return "sk-ant-oat01-minted", nil }
	if !AnthropicRouteIsOAuth(context.Background(), "", "", minted, "") {
		t.Fatal("a token source is materialized once for the shape test")
	}
	if AnthropicRouteIsOAuth(context.Background(), "", "sk-ant-api03-x", nil, "anthropic") {
		t.Fatal("a Console API key is never OAuth")
	}
}

func TestClaudeCodeTokenValidity(t *testing.T) {
	future := float64(time.Now().UnixMilli() + 3_600_000)
	if !IsClaudeCodeTokenValid(map[string]any{"accessToken": "a", "expiresAt": future}) || IsClaudeCodeTokenValid(map[string]any{"accessToken": "a", "expiresAt": float64(1000)}) {
		t.Fatal("expiry with a 60s buffer")
	}
	if !IsClaudeCodeTokenValid(map[string]any{"accessToken": "a"}) || IsClaudeCodeTokenValid(map[string]any{}) {
		t.Fatal("a managed key without expiry is valid when present")
	}
}

func TestReadClaudeCodeCredentialsHonoursAdoptOptOut(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeClaudeCodeCredentials(t, "tok", "rt", time.Now().UnixMilli()+3_600_000)
	if creds := ReadClaudeCodeCredentials(env); creds == nil || creds["accessToken"] != "tok" || creds["source"] != "claude_code_credentials_file" {
		t.Fatalf("file login expected: %v", creds)
	}
	writeConfigYAML(t, h, "auth:\n  adopt_external_logins: false\n")
	if ReadClaudeCodeCredentials(env) != nil {
		t.Fatal("the opt out hides the borrowed login")
	}
}

func TestSpentRotationRegistryAndSidecar(t *testing.T) {
	ResetSpentRotationsForTests()
	t.Cleanup(ResetSpentRotationsForTests)
	h := newTestHome(t)
	env := testEnv(t, h)
	source := SpentRotationSourcePath(env, "hermes_pkce")
	if source != HermesOAuthFile(env) || SpentRotationSourcePath(env, "manual") != "" {
		t.Fatal("only singleton sources have a sidecar")
	}
	MarkRotationConsumedUncommitted(source, "spent-rt", "")
	if !IsRotationConsumedUncommitted("spent-rt", source) || IsRotationConsumedUncommitted("fresh-rt", source) {
		t.Fatal("process local verdict")
	}
	ResetSpentRotationsForTests()
	if !IsRotationConsumedUncommitted("spent-rt", source) {
		t.Fatal("the sidecar lets a fresh process see the verdict")
	}
}

func TestResolveAnthropicTokenOrder(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-key")
	if ResolveAnthropicToken(env, "") != "sk-ant-api03-key" {
		t.Fatal("an explicit API key wins over discovered OAuth")
	}
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-oat01-static")
	if ResolveAnthropicToken(env, "") != "sk-ant-oat01-static" {
		t.Fatal("the env OAuth token comes first")
	}
	t.Setenv("ANTHROPIC_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	writeClaudeCodeCredentials(t, "sk-ant-oat01-cc", "rt", time.Now().UnixMilli()+3_600_000)
	if ResolveAnthropicToken(env, "") != "sk-ant-oat01-cc" {
		t.Fatal("the borrowed login is the last resort")
	}
}

func TestMergeKeychainCredentialPayloadKeepsMetadata(t *testing.T) {
	merged := mergeKeychainCredentialPayload(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "old", "refreshToken": "old-r", "subscriptionType": "max", "scopes": []any{"user:inference"}}, "other": 1}, "new", "new-r", 42)
	oauth := merged["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != "new" || oauth["refreshToken"] != "new-r" || oauth["expiresAt"] != int64(42) || oauth["subscriptionType"] != "max" || merged["other"] != 1 {
		t.Fatalf("got %v", merged)
	}
	argv, line := keychainMirrorCommand(`acc"ount`, merged)
	if argv[0] != "security" || argv[1] != "-i" || !hasPrefix(line, `add-generic-password -U -a "acc\"ount" -s "Claude Code-credentials" -X `) || contains(line, "new-r") {
		t.Fatalf("the secret rides hex encoded on stdin: %q", line)
	}
}

func TestDecodeKeychainAttr(t *testing.T) {
	if decodeKeychainAttr(keychainAttrRe.FindStringSubmatch(`    "acct"<blob>="user@example.com"`)) != "user@example.com" {
		t.Fatal("quoted attribute")
	}
	if decodeKeychainAttr(keychainAttrRe.FindStringSubmatch(`    "acct"<blob>=0x7573657240657861 "user@exa"`)) != "user@exa" {
		t.Fatal("hex attribute")
	}
	if decodeKeychainAttr(nil) != "" {
		t.Fatal("missing attribute")
	}
}

func TestOpenBrowserURLRunsThePlatformOpener(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := OpenBrowserURL("https://example.invalid/device"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("opener lookup on an empty PATH = %v", err)
	}
}
