package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Provider over seeded tokens for a fake server, registered or not, with no-op redirect and callback handlers
func makeDiscoveryFlow(t *testing.T, fs *oauthFakeServer, registered bool) (*OAuthProvider, *http.Request) {
	t.Helper()
	oauthHome(t)
	s := NewTokenStorage(context.Background(), "srv", "")
	tok := bearer("old_access", "old_refresh")
	tok.ExpiresIn = int64Ptr(3600)
	if err := s.SetTokens(tok); err != nil {
		t.Fatal(err)
	}
	if registered {
		info, err := validateClientInfo(map[string]any{"client_id": "test-client", "redirect_uris": []any{"http://127.0.0.1:12345/callback"},
			"grant_types": []any{"authorization_code", "refresh_token"}, "response_types": []any{"code"}, "token_endpoint_auth_method": "none"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetClientInfo(info); err != nil {
			t.Fatal(err)
		}
	}
	md, err := validateClientMetadata(map[string]any{"redirect_uris": []any{"http://127.0.0.1:12345/callback"}, "client_name": "Icebath Agent"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := newOAuthProvider(oauthProviderOptions{ServerURL: fs.URL + "/mcp", ClientMetadata: md, Storage: s,
		RedirectHandler: func(context.Context, string) error { return nil },
		CallbackHandler: func(context.Context) (*AuthorizationCodeResult, error) {
			return &AuthorizationCodeResult{Code: "code"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", fs.URL+"/mcp", nil)
	if err := p.Prepare(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return p, req
}

func TestOAuthDiscoveryRequestGetsDefaultUserAgent(t *testing.T) {
	fs := newOAuthFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(404) })
	p, req := makeDiscoveryFlow(t, fs, true)
	_, _ = p.HandleUnauthorized(context.Background(), req, challenge401(req, `Bearer resource_metadata="`+fs.URL+`/.well-known/oauth-protected-resource"`))
	reqs := fs.requests()
	if len(reqs) == 0 || !strings.Contains(reqs[0].URL, ".well-known") || reqs[0].Header.Get("User-Agent") != defaultAuthRequestUserAgent() {
		t.Fatalf("first discovery request %+v", reqs)
	}
}

func TestOAuthDiscoveryCallersMCPRequestIsLeftUntouched(t *testing.T) {
	fs := newOAuthFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(404) })
	_, req := makeDiscoveryFlow(t, fs, true)
	if ua := req.Header.Get("User-Agent"); ua != "" || req.Header.Get("Authorization") != "Bearer old_access" {
		t.Fatalf("caller request headers %v", req.Header)
	}
}

func TestOAuthDiscoveryRegistrationFailureAfterFailedDiscoveryLeadsWithDiscovery(t *testing.T) {
	fs := newOAuthFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		switch {
		case strings.Contains(r.URL.Path, "oauth-authorization-server") || strings.Contains(r.URL.Path, "openid-configuration"):
			w.WriteHeader(403)
			_, _ = w.Write([]byte("Forbidden"))
		case r.Method == "POST":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
		default:
			w.WriteHeader(404)
		}
	})
	p, req := makeDiscoveryFlow(t, fs, false)
	_, err := p.HandleUnauthorized(context.Background(), req, challenge401(req, "Bearer"))
	var re *OAuthRegistrationError
	if !errors.As(err, &re) {
		t.Fatalf("error %T %v", err, err)
	}
	msg := re.Error()
	if !strings.Contains(msg, "403") || !strings.Contains(msg, "404") || strings.Index(msg, "403") > strings.Index(msg, "404") {
		t.Fatalf("message %q", msg)
	}
	if h, ok := HumanizeOAuthRegistrationError("srv", msg, "https://example.com/mcp"); ok {
		t.Fatalf("humanized %q", h)
	}
}
