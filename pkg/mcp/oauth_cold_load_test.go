package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Token store for srv under a fresh isolated home
func coldStorage(t *testing.T) (*TokenStorage, string) {
	t.Helper()
	dir := freshManager(t)
	return NewTokenStorage(context.Background(), "srv", ""), filepath.Join(dir, "mcp-tokens", "srv.json")
}

func TestOAuthColdLoadSetTokensPersistsAbsoluteExpiresAt(t *testing.T) {
	s, path := coldStorage(t)
	before := oauthNow()
	if err := s.SetTokens(tokenPair("a", "r", int64Ptr(3600))); err != nil {
		t.Fatal(err)
	}
	after := oauthNow()
	at, ok := readJSONFile(t, path)["expires_at"].(float64)
	if !ok || at < before+3600 || at > after+3600 {
		t.Fatalf("expires_at %v", readJSONFile(t, path)["expires_at"])
	}
}

func TestOAuthColdLoadSetTokensWithoutExpiresInOmitsExpiresAt(t *testing.T) {
	s, path := coldStorage(t)
	if err := s.SetTokens(tokenPair("a", "r", nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := readJSONFile(t, path)["expires_at"]; ok {
		t.Fatal("expires_at written without expires_in")
	}
}

func TestOAuthColdLoadGetTokensUsesExpiresAtForRemainingTTL(t *testing.T) {
	s, _ := coldStorage(t)
	if err := s.SetTokens(tokenPair("a", "r", int64Ptr(3600))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	got, err := NewTokenStorage(context.Background(), "srv", "").GetTokens()
	if err != nil || got == nil || got.ExpiresIn == nil || *got.ExpiresIn <= 3500 || *got.ExpiresIn > 3600 {
		t.Fatalf("reloaded %+v %v", got, err)
	}
}

func TestOAuthColdLoadGetTokensReturnsZeroTTLForExpiredToken(t *testing.T) {
	s, path := coldStorage(t)
	writeTokenFile(t, filepath.Dir(filepath.Dir(path)), "srv.json", map[string]any{"access_token": "a", "token_type": "Bearer",
		"expires_in": 3600, "expires_at": oauthNow() - 60})
	got, err := s.GetTokens()
	if err != nil || got == nil || got.ExpiresIn == nil || *got.ExpiresIn != 0 {
		t.Fatalf("reloaded %+v %v", got, err)
	}
}

func TestOAuthColdLoadLegacyFileWithoutExpiresAtIsLoadable(t *testing.T) {
	s, path := coldStorage(t)
	writeTokenFile(t, filepath.Dir(filepath.Dir(path)), "srv.json", map[string]any{"access_token": "a", "token_type": "Bearer",
		"expires_in": 3600, "refresh_token": "r"})
	fresh, err := s.GetTokens()
	if err != nil || fresh == nil || fresh.ExpiresIn == nil || *fresh.ExpiresIn <= 3500 {
		t.Fatalf("fresh legacy file %+v %v", fresh, err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTokens()
	if err != nil || got == nil || got.ExpiresIn == nil || *got.ExpiresIn != 0 {
		t.Fatalf("stale legacy file %+v %v", got, err)
	}
}

// Manager provider for srv whose auth requests answer through route, with tokens and a client already on disk
func coldProvider(t *testing.T, serverURL string, tokens *OAuthToken, route func(r *http.Request, body string) (int, any, http.Header)) (*OAuthProvider, *standIn) {
	t.Helper()
	s := NewTokenStorage(context.Background(), "srv", "")
	if tokens != nil {
		if err := s.SetTokens(tokens); err != nil {
			t.Fatal(err)
		}
	}
	info, err := validateClientInfo(map[string]any{"client_id": "test-client", "redirect_uris": []any{"http://127.0.0.1:12345/callback"},
		"grant_types": []any{"authorization_code", "refresh_token"}, "response_types": []any{"code"}, "token_endpoint_auth_method": "none"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetClientInfo(info); err != nil {
		t.Fatal(err)
	}
	p := managerProvider(t, context.Background(), "srv", serverURL, nil)
	si := newStandIn(route)
	p.client.Transport = si
	return p, si
}

// Route answering every request with 404
func notFound(*http.Request, string) (int, any, http.Header) { return 404, nil, nil }

func TestOAuthColdLoadInitializeSeedsTokenExpiryTimeFromStoredTokens(t *testing.T) {
	freshManager(t)
	p, _ := coldProvider(t, "https://example.com/mcp", tokenPair("a", "r", int64Ptr(7200)), notFound)
	if err := p.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := oauthNow()
	if p.tokenExpiry == nil || *p.tokenExpiry <= now+7000 || *p.tokenExpiry > now+7200+5 {
		t.Fatalf("token expiry %v", p.tokenExpiry)
	}
}

func TestOAuthColdLoadInitializeMarksZeroTTLColdLoadedTokenInvalid(t *testing.T) {
	_, path := coldStorage(t)
	p, _ := coldProvider(t, "https://example.com/mcp", tokenPair("expired-access", "refresh-token", int64Ptr(3600)), notFound)
	fixed := oauthNow()
	persisted := readJSONFile(t, path)
	persisted["expires_at"] = fixed - 60
	data, _ := json.Marshal(persisted)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := oauthNow
	oauthNow = func() float64 { return fixed }
	t.Cleanup(func() { oauthNow = prev })
	if err := p.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.currentTokens == nil || p.currentTokens.ExpiresIn == nil || *p.currentTokens.ExpiresIn != 0 || p.isTokenValid() {
		t.Fatalf("tokens %+v expiry %v", p.currentTokens, p.tokenExpiry)
	}
}

func TestOAuthColdLoadInitializePrefetchesOAuthMetadataWhenMissing(t *testing.T) {
	freshManager(t)
	var agents []string
	p, _ := coldProvider(t, "https://mcp.example.com", tokenPair("a", "r", int64Ptr(3600)), func(r *http.Request, _ string) (int, any, http.Header) {
		agents = append(agents, r.Header.Get("User-Agent"))
		u := r.URL.String()
		switch {
		case strings.HasSuffix(u, "/.well-known/oauth-protected-resource"):
			return 200, map[string]any{"resource": "https://mcp.example.com", "authorization_servers": []any{"https://auth.example.com"},
				"scopes_supported": []any{"read", "write"}, "bearer_methods_supported": []any{"header"}}, nil
		case strings.HasSuffix(u, "/.well-known/oauth-authorization-server"):
			return 200, map[string]any{"issuer": "https://auth.example.com", "authorization_endpoint": "https://auth.example.com/oauth/authorize",
				"token_endpoint": "https://auth.example.com/oauth/token", "registration_endpoint": "https://auth.example.com/oauth/register",
				"response_types_supported": []any{"code"}, "grant_types_supported": []any{"authorization_code", "refresh_token"},
				"code_challenge_methods_supported": []any{"S256"}, "token_endpoint_auth_methods_supported": []any{"none"},
				"scopes_supported": []any{"read", "write"}}, nil
		}
		return 404, nil, nil
	})
	if err := p.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.protectedResourceMetadata == nil || p.oauthMetadata == nil || p.oauthMetadata.TokenEndpoint != "https://auth.example.com/oauth/token" {
		t.Fatalf("prm %+v asm %+v", p.protectedResourceMetadata, p.oauthMetadata)
	}
	if len(agents) == 0 {
		t.Fatal("no discovery requests")
	}
	for _, a := range agents {
		if a != defaultAuthRequestUserAgent() {
			t.Fatalf("user agents %v", agents)
		}
	}
}
