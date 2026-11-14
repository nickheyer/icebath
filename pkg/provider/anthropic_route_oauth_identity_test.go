package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	routeOAuthToken   = "sk-ant-oat01-keycmd-token"
	routeConsoleKey   = "sk-ant-api03-console-key"
	routeUserScopeKey = "sk-ant-usr01-user-scoped-key"
)

func TestRouteIdentityRequiresNativeHostAndOauthShape(t *testing.T) {
	ctx := context.Background()
	calls := 0
	keyCmd := func(context.Context) (string, error) {
		calls++
		return routeOAuthToken, nil
	}
	raising := func(context.Context) (string, error) { return "", errors.New("no token") }
	checks := []struct {
		base, credential, provider string
		source                     func(context.Context) (string, error)
		want                       bool
	}{
		{"https://api.anthropic.com", "", "", keyCmd, true},
		{"https://api.anthropic.com/v1", routeOAuthToken, "", nil, true},
		{"", routeOAuthToken, "", nil, true},
		{"https://proxy.example.com", routeOAuthToken, "anthropic", nil, true},
		{"https://api.minimax.io/anthropic", "", "", keyCmd, false},
		{"https://api.anthropic.com", routeConsoleKey, "", nil, false},
		{"https://api.anthropic.com", routeUserScopeKey, "", nil, false},
		{"https://notapi.anthropic.com.evil.example", "", "", keyCmd, false},
		{"https://api.anthropic.com", "", "", raising, false},
	}
	for i, c := range checks {
		if got := AnthropicRouteIsOAuth(ctx, c.base, c.credential, c.source, c.provider); got != c.want {
			t.Fatalf("check %d %q: got %v want %v", i, c.base, got, c.want)
		}
	}
	if calls != 1 {
		t.Fatalf("the third party route must never materialise the callable, calls=%d", calls)
	}
}

// Request headers the built SDK client sends on its first call
func routeIdentityHeaders(t *testing.T, client *anthropicSDKClient) map[string]string {
	t.Helper()
	headers, _, err := client.inner.requestHeaders(context.Background(), client.betas, nil)
	if err != nil {
		t.Fatal(err)
	}
	return headers
}

func TestKeyCmdOauthRouteBuildsClaudeCodeIdentityOnMainAndAuxClients(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	keyCmd := func(context.Context) (string, error) { return routeOAuthToken, nil }
	native, err := buildAnthropicClient(ctx, keyCmd, "https://api.anthropic.com", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	headers := routeIdentityHeaders(t, native)
	ua, _ := auxHeaderValue(headers, "user-agent")
	app, _ := auxHeaderValue(headers, "x-app")
	if !strings.HasPrefix(ua, "claude-code/") || app != "cli" {
		t.Fatalf("native route lost the Claude Code identity: %v", headers)
	}
	betas, _ := auxHeaderValue(headers, "anthropic-beta")
	for _, beta := range anthropicOAuthOnlyBetas {
		if !strings.Contains(betas, beta) {
			t.Fatalf("missing OAuth beta %s in %q", beta, betas)
		}
	}
	thirdParty, err := buildAnthropicClient(ctx, keyCmd, "https://api.minimax.io/anthropic", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	tpHeaders := routeIdentityHeaders(t, thirdParty)
	tpUA, _ := auxHeaderValue(tpHeaders, "user-agent")
	if _, hasApp := auxHeaderValue(tpHeaders, "x-app"); strings.HasPrefix(tpUA, "claude-code/") || hasApp {
		t.Fatalf("third party route carries the Claude Code identity: %v", tpHeaders)
	}
	tpBetas, _ := auxHeaderValue(tpHeaders, "anthropic-beta")
	for _, beta := range anthropicOAuthOnlyBetas {
		if strings.Contains(tpBetas, beta) {
			t.Fatalf("third party route carries OAuth beta %s", beta)
		}
	}

	oldRuntime, oldModel := auxResolveCustomRuntimeFn, auxReadMainModel
	t.Cleanup(func() { auxResolveCustomRuntimeFn, auxReadMainModel = oldRuntime, oldModel })
	auxReadMainModel = func(context.Context) string { return "claude-opus-5" }
	for _, c := range []struct {
		base string
		want bool
	}{{"https://api.anthropic.com", true}, {"https://api.minimax.io/anthropic", false}} {
		auxResolveCustomRuntimeFn = func(context.Context) (string, any, string, error) {
			return c.base, keyCmd, "anthropic_messages", nil
		}
		client, _, err := auxTryCustomEndpoint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		anth, ok := client.(*AnthropicAuxiliaryClient)
		if !ok {
			t.Fatalf("%s: expected AnthropicAuxiliaryClient, got %T", c.base, client)
		}
		if anth.adapter.isOAuth != c.want {
			t.Fatalf("%s: is_oauth %v want %v", c.base, anth.adapter.isOAuth, c.want)
		}
	}
}
