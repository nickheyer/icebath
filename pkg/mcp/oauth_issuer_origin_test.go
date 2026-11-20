package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ioResource   = "https://res.example/mcp"
	ioASOrigin   = "https://as.example"
	ioAdvertised = ioASOrigin + "/mcp-issuer"
	ioPathDoc    = "/.well-known/oauth-authorization-server/mcp-issuer"
)

func TestOAuthOriginIssuedMetadataShimDoesNotReadNonDiscoveryResponse(t *testing.T) {
	for _, raw := range []string{ioResource, ioASOrigin + "/proxy/.well-known/oauth-authorization-server",
		ioASOrigin + "/.well-known/oauth-authorization-server-extra", ioResource + "?next=/.well-known/oauth-authorization-server",
		ioResource + "#/.well-known/openid-configuration"} {
		p := &OAuthProvider{authServerURL: ioAdvertised}
		req, _ := http.NewRequest("GET", raw, nil)
		r := &flowResponse{StatusCode: 200, Header: http.Header{}, Body: []byte(`{"issuer": "https://as.example"}`), Request: req}
		if got := p.acceptOriginIssuedMetadata(r); got != r || p.oauthMetadata != nil {
			t.Fatalf("%s was consumed as OAuth metadata", raw)
		}
	}
}

// Authorization server metadata naming issuer with every endpoint on the AS origin
func ioASM(issuer string) map[string]any {
	return map[string]any{"issuer": issuer, "authorization_endpoint": ioASOrigin + "/authorize", "token_endpoint": ioASOrigin + "/token",
		"registration_endpoint": ioASOrigin + "/register", "response_types_supported": []any{"code"},
		"code_challenge_methods_supported": []any{"S256"}, "authorization_response_iss_parameter_supported": true}
}

// Resource plus authorization server stand in serving issuerDoc by path
func ioStandIn(issuerDoc map[string]map[string]any) *standIn {
	return newStandIn(func(r *http.Request, _ string) (int, any, http.Header) {
		u := r.URL.String()
		switch {
		case u == ioResource:
			if r.Header.Get("Authorization") == "Bearer AT-1" {
				return 200, map[string]any{"ok": true}, nil
			}
			return 401, map[string]any{}, http.Header{"Www-Authenticate": {`Bearer resource_metadata="https://res.example/.well-known/oauth-protected-resource"`}}
		case u == "https://res.example/.well-known/oauth-protected-resource":
			return 200, map[string]any{"resource": ioResource, "authorization_servers": []any{ioAdvertised}}, nil
		case strings.HasPrefix(u, ioASOrigin) && issuerDoc[r.URL.Path] != nil:
			return 200, issuerDoc[r.URL.Path], nil
		case u == ioASOrigin+"/register":
			return 201, map[string]any{"client_id": "dcr-1", "redirect_uris": []any{"http://127.0.0.1:1/cb"}, "token_endpoint_auth_method": "none",
				"grant_types": []any{"authorization_code", "refresh_token"}, "response_types": []any{"code"}}, nil
		case u == ioASOrigin+"/token":
			return 200, map[string]any{"access_token": "AT-1", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "RT-1"}, nil
		}
		return 404, map[string]any{}, nil
	})
}

// Runs a browser flow against the stand in, returning the resource response, stand in, authorize URL and provider
func ioRunFlow(t *testing.T, issuerDoc map[string]map[string]any) (*http.Response, *standIn, string, *OAuthProvider, error) {
	t.Helper()
	oauthHome(t)
	var authorizeURL, state string
	md, err := validateClientMetadata(map[string]any{"redirect_uris": []any{"http://127.0.0.1:1/cb"}, "client_name": "Icebath Agent"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := newOAuthProvider(oauthProviderOptions{ServerURL: ioResource, ClientMetadata: md, Storage: NewTokenStorage(context.Background(), "srv", ""),
		RedirectHandler: func(_ context.Context, u string) error {
			authorizeURL = u
			parsed, _ := url.Parse(u)
			state = parsed.Query().Get("state")
			return nil
		},
		CallbackHandler: func(context.Context) (*AuthorizationCodeResult, error) {
			return &AuthorizationCodeResult{Code: "code-1", State: state, Iss: ioASOrigin}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	si := ioStandIn(issuerDoc)
	p.client.Transport = si
	resp, err := authedDo(context.Background(), p, si, "GET", ioResource)
	return resp, si, authorizeURL, p, err
}

func TestOAuthOriginIssuedDocumentOfPathScopedServerCompletesTheFlow(t *testing.T) {
	resp, si, authorizeURL, p, err := ioRunFlow(t, map[string]map[string]any{ioPathDoc: ioASM(ioASOrigin)})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("response %v err %v hits %v", resp, err, si.allHits())
	}
	if !strings.HasPrefix(authorizeURL, ioASOrigin+"/authorize?") || !si.hit("POST", ioASOrigin+"/register") || !si.hit("POST", ioASOrigin+"/token") {
		t.Fatalf("authorize %q hits %v", authorizeURL, si.allHits())
	}
	if strings.TrimRight(p.oauthMetadata.Issuer, "/") != ioASOrigin {
		t.Fatalf("issuer %q", p.oauthMetadata.Issuer)
	}
	dir := filepath.Dir(p.storage.TokensPath())
	if got := readJSONFile(t, filepath.Join(dir, "srv.client.json"))["issuer"]; got != ioAdvertised {
		t.Fatalf("client issuer %v", got)
	}
	if got := readJSONFile(t, filepath.Join(dir, "srv.json"))["access_token"]; got != "AT-1" {
		t.Fatalf("access token %v", got)
	}
}

func TestOAuthOtherIssuerShapesAreStillRejected(t *testing.T) {
	for name, doc := range map[string]map[string]map[string]any{
		"different-origin":   {ioPathDoc: ioASM("https://other.example")},
		"root-document-only": {"/.well-known/oauth-authorization-server": ioASM(ioASOrigin)},
		"different-path":     {ioPathDoc: ioASM(ioASOrigin + "/other-path")},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, p, err := ioRunFlow(t, doc)
			var fe *OAuthFlowError
			if !errors.As(err, &fe) {
				t.Fatalf("error %T %v", err, err)
			}
			if exists(p.storage.TokensPath()) {
				t.Fatal("tokens persisted for a rejected issuer")
			}
		})
	}
}
