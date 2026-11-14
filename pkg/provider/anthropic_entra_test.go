package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func anthTokenRoute(s *anthServer, baseURL string, mint func(context.Context) (string, error)) Route {
	return Route{Provider: "azure-foundry", Model: "claude-sonnet-4-20250514", BaseURL: baseURL, Mode: ModeAnthropicMessages, Transport: s,
		TokenSource: mint, Headers: map[string]string{"Authorization": "Bearer stale", "api-key": "leaked-placeholder"}}
}

func anthAuthOf(h http.Header) string {
	return fmt.Sprintf("auth=%q api-key=%q x-api-key=%q", h.Get("Authorization"), h.Get("Api-Key"), h.Get("X-Api-Key"))
}

func TestAnthropicEntraRouteMintsAFreshBearerPerRequest(t *testing.T) {
	ok := anthReplyJSON(200, anthMessage("end_turn", anthText("hi")))
	s := newAnthServer(t, ok, ok)
	var n atomic.Int32
	mint := TagCredentialSource(func(context.Context) (string, error) { return fmt.Sprintf("jwt-%d", n.Add(1)), nil }, CredentialSourceEntraID).Func()
	c := s.client(anthTokenRoute(s, "https://res.services.ai.azure.com/anthropic", mint))
	for i := 0; i < 2; i++ {
		if _, err := c.Complete(t.Context(), anthUserRequest("claude-sonnet-4-20250514", "hi")); err != nil {
			t.Fatal(err)
		}
	}
	reqs := s.requests()
	for i, want := range []string{"Bearer jwt-1", "Bearer jwt-2"} {
		if got := anthAuthOf(reqs[i].Header); got != fmt.Sprintf("auth=%q api-key=\"\" x-api-key=\"\"", want) {
			t.Fatalf("request %d: %s", i, got)
		}
	}
}

func TestAnthropicEntraEmptyMintSendsWithoutAuthHeaders(t *testing.T) {
	s := newAnthServer(t, anthReplyJSON(401, map[string]any{"type": "error", "error": map[string]any{"type": "authentication_error", "message": "missing auth"}}))
	mint := func(context.Context) (string, error) { return "", nil }
	c := s.client(anthTokenRoute(s, "https://res.services.ai.azure.com/anthropic", mint))
	_, err := c.Complete(t.Context(), anthUserRequest("claude-sonnet-4-20250514", "hi"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("Azure 401 not surfaced: %v", err)
	}
	if reqs := s.requests(); len(reqs) != 1 || anthAuthOf(reqs[0].Header) != `auth="" api-key="" x-api-key=""` {
		t.Fatalf("empty mint must reach Azure with no auth header %v", reqs)
	}
}

func TestAnthropicEntraMintErrorNeverSendsTheRequest(t *testing.T) {
	s := newAnthServer(t)
	mint := func(context.Context) (string, error) { return "", errors.New("credential chain exhausted") }
	c := s.client(anthTokenRoute(s, "https://res.services.ai.azure.com/anthropic", mint))
	_, err := c.Complete(t.Context(), anthUserRequest("claude-sonnet-4-20250514", "hi"))
	if err == nil || !strings.Contains(err.Error(), "credential chain exhausted") {
		t.Fatalf("mint error lost: %v", err)
	}
	if reqs := s.requests(); len(reqs) != 0 {
		t.Fatalf("request sent after a raising mint %v", reqs)
	}
}

func TestAnthropicNativeKeyCmdRouteChoosesHeaderPerMint(t *testing.T) {
	ok := anthReplyJSON(200, anthMessage("end_turn", anthText("hi")))
	s := newAnthServer(t, ok, ok)
	c := s.client(anthTokenRoute(s, "https://api.anthropic.com", func(context.Context) (string, error) { return "sk-ant-api03-console", nil }))
	if _, err := c.Complete(t.Context(), anthUserRequest("claude-sonnet-4-20250514", "hi")); err != nil {
		t.Fatal(err)
	}
	if got := anthAuthOf(s.last().Header); got != `auth="" api-key="" x-api-key="sk-ant-api03-console"` {
		t.Fatalf("console key: %s", got)
	}
	c = s.client(anthTokenRoute(s, "https://api.anthropic.com", func(context.Context) (string, error) { return "sk-ant-oat01-sub", nil }))
	if _, err := c.Complete(t.Context(), anthUserRequest("claude-sonnet-4-20250514", "hi")); err != nil {
		t.Fatal(err)
	}
	h := s.last().Header
	if anthAuthOf(h) != `auth="Bearer sk-ant-oat01-sub" api-key="" x-api-key=""` || h.Get("x-app") != "cli" || !strings.HasPrefix(h.Get("User-Agent"), "claude-code/") {
		t.Fatalf("oauth mint lost the Claude Code identity: %s %v", anthAuthOf(h), h)
	}
}

func TestEveryTaggedMinterKeepsItsSourceOnTheRoute(t *testing.T) {
	entra := BuildEntraTokenProvider(nil, ScopeAIAzureDefault, true)
	if CredentialSourceOf(entra) != CredentialSourceEntraID || CredentialLabel(entra) != "Microsoft Entra ID" {
		t.Fatalf("entra provider tag %q %q", CredentialSourceOf(entra), CredentialLabel(entra))
	}
	cfg := map[string]any{"model": map[string]any{"provider": "azure-foundry", "base_url": "https://res.services.ai.azure.com/anthropic",
		"api_mode": "anthropic_messages", "auth_mode": "entra_id", "default": "claude-sonnet-4-5"}}
	rt := mustResolve(t, testResolver(t, cfg, &fakeCreds{}, ""), "azure-foundry", "", "", "")
	if route := rt.Route(); CredentialSourceOf(route.TokenSource) != CredentialSourceEntraID || CredentialLabel(route.TokenSource) != "Microsoft Entra ID" {
		t.Fatalf("azure foundry route lost the Entra tag %q", CredentialSourceOf(route.TokenSource))
	}

	vertexIsolate(t)
	var calls int32
	srv := vertexTokenServer(t, &calls, 3600)
	h := t.TempDir()
	sa := filepath.Join(h, "sa.json")
	vertexWriteSA(t, sa, srv.URL, "proj", nil)
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("VERTEX_CREDENTIALS_PATH="+sa+"\nVERTEX_REGION=europe-west4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, _, ok := vertexRuntimeConfig(WithDeferredTokenMint(context.Background()), map[string]any{}, nil, h)
	if !ok || CredentialSourceOf(src) != CredentialSourceVertexOAuth || CredentialLabel(src) != "Google OAuth (Vertex AI)" || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("vertex minter tag %q label %q calls %d", CredentialSourceOf(src), CredentialLabel(src), calls)
	}

	keyCmd := map[string]any{"providers": map[string]any{"gw": map[string]any{"api": "https://gw.example/v1", "key_cmd": printTokenCmd("minted-token")}}}
	rt = mustResolve(t, testResolver(t, keyCmd, &fakeCreds{}, ""), "custom:gw", "", "", "")
	if route := rt.Route(); CredentialSourceOf(route.TokenSource) != CredentialSourceKeyCmd || CredentialLabel(route.TokenSource) != "key_cmd token (command)" {
		t.Fatalf("key_cmd route tag %q", CredentialSourceOf(route.TokenSource))
	}
	if tok, err := rt.TokenSource(context.Background()); err != nil || tok != "minted-token" {
		t.Fatalf("tagged key_cmd still mints %q %v", tok, err)
	}

	minimax := BuildMinimaxOAuthTokenProvider(PoolEnv{})
	if CredentialSourceOf(minimax) != CredentialSourceMinimaxOAuth || CredentialSourceOf(minimax.Func()) != CredentialSourceMinimaxOAuth {
		t.Fatal("minimax provider tag lost on its bare func")
	}
	bare := func(context.Context) (string, error) { return "x", nil }
	if CredentialSourceOf(bare) != "" || CredentialLabel(bare) != "Token provider" {
		t.Fatalf("untagged func must stay neutral %q %q", CredentialSourceOf(bare), CredentialLabel(bare))
	}
}
