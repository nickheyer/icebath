package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
)

const (
	googleResource = "https://gmailmcp.example/mcp"
	googleIssuer   = "https://accounts.google.com"
	otherAS        = "https://as.example"
)

// Authorization server metadata document for an issuer with extra members merged in
func googleASMDoc(issuer string, extra map[string]any) map[string]any {
	doc := map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/o/oauth2/v2/auth", "token_endpoint": issuer + "/token",
		"registration_endpoint": issuer + "/register", "response_types_supported": []any{"code"},
		"code_challenge_methods_supported": []any{"S256"}, "grant_types_supported": []any{"authorization_code", "refresh_token"}}
	for k, v := range extra {
		doc[k] = v
	}
	return doc
}

// Resource, discovery, registration and token answers of a server whose authorization server is issuer
func googleStandIn(issuer string, authorizationServers []any) *standIn {
	return newStandIn(func(r *http.Request, _ string) (int, any, http.Header) {
		u := r.URL.String()
		parts, _ := url.Parse(issuer)
		base := parts.Scheme + "://" + parts.Host
		switch {
		case u == googleResource:
			if r.Header.Get("Authorization") == "Bearer AT-1" {
				return 200, map[string]any{"ok": true}, nil
			}
			return 401, map[string]any{}, http.Header{"Www-Authenticate": {`Bearer resource_metadata="https://gmailmcp.example/.well-known/oauth-protected-resource"`}}
		case u == "https://gmailmcp.example/.well-known/oauth-protected-resource":
			return 200, map[string]any{"resource": googleResource, "authorization_servers": authorizationServers}, nil
		case r.URL.Scheme+"://"+r.URL.Host == base && r.URL.Path == "/.well-known/oauth-authorization-server":
			return 200, googleASMDoc(issuer, nil), nil
		case u == base+"/register":
			return 201, map[string]any{"client_id": "dcr-1", "redirect_uris": []any{"http://127.0.0.1:1/cb"}, "token_endpoint_auth_method": "none",
				"grant_types": []any{"authorization_code", "refresh_token"}, "response_types": []any{"code"}}, nil
		case u == base+"/token":
			return 200, map[string]any{"access_token": "AT-1", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "RT-1"}, nil
		}
		return 404, map[string]any{}, nil
	})
}

// Runs a browser login against the stand-in, returning the resource status, the authorization URL and the provider
func runGoogleBrowserFlow(t *testing.T, homeDir, issuer string, authorizationServers []any, scope string) (int, string, *OAuthProvider) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", homeDir)
	resetOAuthManager()
	md, err := validateClientMetadata(map[string]any{"redirect_uris": []any{"http://127.0.0.1:1/cb"}, "client_name": "Icebath Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if scope != "" {
		md.Scope = strPtr(scope)
	}
	var authorizeURL, state string
	p, err := newOAuthProvider(oauthProviderOptions{ServerURL: googleResource, ClientMetadata: md, OAuthFlow: "browser",
		Storage: NewTokenStorage(context.Background(), "srv", ""),
		RedirectHandler: func(_ context.Context, u string) error {
			authorizeURL = u
			parts, _ := url.Parse(u)
			state = parts.Query().Get("state")
			return nil
		},
		CallbackHandler: func(context.Context) (*AuthorizationCodeResult, error) {
			return &AuthorizationCodeResult{Code: "code-1", State: state}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	p.hooks = &managerHooks{name: "srv"}
	s := googleStandIn(issuer, authorizationServers)
	p.client.Transport = s
	resp, err := authedDo(context.Background(), p, s, http.MethodGet, googleResource)
	if err != nil {
		t.Fatalf("flow %v hits %v", err, s.allHits())
	}
	resp.Body.Close()
	return resp.StatusCode, authorizeURL, p
}

// Values of a query key in a URL
func queryValues(t *testing.T, rawURL, key string) []string {
	t.Helper()
	parts, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return parts.Query()[key]
}

func TestOAuthGoogleAuthorizationURLAsksForOfflineAccess(t *testing.T) {
	dir := oauthHome(t)
	t.Cleanup(resetOAuthManager)
	status, authorizeURL, p := runGoogleBrowserFlow(t, dir, googleIssuer, []any{googleIssuer}, "email offline_access")
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	if got := queryValues(t, authorizeURL, "access_type"); len(got) != 1 || got[0] != "offline" {
		t.Fatalf("access_type %v in %s", got, authorizeURL)
	}
	if got := queryValues(t, authorizeURL, "prompt"); len(got) != 1 || got[0] != "consent" {
		t.Fatalf("prompt %v in %s", got, authorizeURL)
	}
	if !exists(filepath.Join(dir, "mcp-tokens", "srv.json")) {
		t.Fatal("tokens not persisted")
	}
	var forwarded string
	p.redirectHandler = func(_ context.Context, u string) error { forwarded = u; return nil }
	p.offlineAccessWrapped = false
	p.requestGoogleOfflineAccess()
	p.requestGoogleOfflineAccess()
	if err := p.redirectHandler(context.Background(), "https://accounts.google.com/o/oauth2/v2/auth?state=s"); err != nil {
		t.Fatal(err)
	}
	if got := queryValues(t, forwarded, "access_type"); len(got) != 1 || !p.offlineAccessWrapped {
		t.Fatalf("rewrapped handler forwarded %s", forwarded)
	}

	status, authorizeURL, _ = runGoogleBrowserFlow(t, filepath.Join(dir, "other"), otherAS, []any{otherAS}, "")
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	if len(queryValues(t, authorizeURL, "access_type"))+len(queryValues(t, authorizeURL, "prompt")) != 0 {
		t.Fatalf("non Google authorization URL %s", authorizeURL)
	}

	probe := &OAuthProvider{}
	for _, lookalike := range []string{"", otherAS, "https://evil.example/accounts.google.com", "https://accounts.google.com.evil.example"} {
		probe.oauthMetadata = nil
		if lookalike != "" {
			probe.oauthMetadata = &OAuthMetadata{Issuer: lookalike}
		}
		if got := probe.googleOfflineAccessParams(); len(got) != 0 {
			t.Fatalf("%q gave %v", lookalike, got)
		}
	}
	probe.oauthMetadata = &OAuthMetadata{Issuer: googleIssuer}
	if got := probe.googleOfflineAccessParams(); len(got) != 1 || got[0] != [2]string{"access_type", "offline"} {
		t.Fatalf("google gave %v", got)
	}
}

func TestOAuthGoogleDeviceFlowNormalizesIssuerAndAsksForOfflineAccess(t *testing.T) {
	oauthHome(t)
	ctx := context.Background()
	deviceGrants := []any{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"}
	for _, c := range []struct {
		document, advertised string
		accepted             bool
	}{{googleIssuer, googleIssuer + "/", true}, {googleIssuer + "/", googleIssuer + "/", true}, {"https://evil.example", googleIssuer + "/", false}} {
		doc := googleASMDoc(c.document, map[string]any{"device_authorization_endpoint": googleIssuer + "/device/code", "grant_types_supported": deviceGrants})
		client := &http.Client{Transport: newStandIn(func(*http.Request, string) (int, any, http.Header) { return 200, doc, nil })}
		md, err := deviceMetadata(ctx, client, googleResource, c.advertised)
		var flowErr *OAuthFlowError
		if c.accepted && (err != nil || md == nil || !md.IsDevice()) {
			t.Fatalf("%v rejected %v", c, err)
		}
		if !c.accepted && !errors.As(err, &flowErr) {
			t.Fatalf("%v accepted %v", c, err)
		}
	}
	for _, c := range []struct {
		issuer         string
		expectsOffline bool
	}{{googleIssuer, true}, {otherAS, false}} {
		var deviceForm url.Values
		client := &http.Client{Transport: newStandIn(func(r *http.Request, body string) (int, any, http.Header) {
			if r.URL.Path == "/device/code" {
				deviceForm, _ = url.ParseQuery(body)
				return 200, map[string]any{"device_code": "DC-1", "user_code": "UC-1", "verification_uri": c.issuer + "/activate",
					"interval": 0.05, "expires_in": 60}, nil
			}
			return 200, map[string]any{"access_token": "AT-1", "token_type": "Bearer", "expires_in": 3600}, nil
		})}
		md, err := validateClientMetadata(map[string]any{"redirect_uris": []any{"http://127.0.0.1:1/cb"}, "client_name": "Icebath Agent"})
		if err != nil {
			t.Fatal(err)
		}
		p, err := newOAuthProvider(oauthProviderOptions{ServerURL: googleResource, ClientMetadata: md, Storage: NewTokenStorage(ctx, "srv", "")})
		if err != nil {
			t.Fatal(err)
		}
		p.hooks = &managerHooks{name: "srv"}
		if p.oauthMetadata, err = validateOAuthMetadata(googleASMDoc(c.issuer, map[string]any{"device_authorization_endpoint": c.issuer + "/device/code"}), true); err != nil {
			t.Fatal(err)
		}
		if p.clientInfo, err = validateClientInfo(map[string]any{"client_id": "cid-1", "redirect_uris": []any{"http://127.0.0.1:1/cb"},
			"token_endpoint_auth_method": "none"}); err != nil {
			t.Fatal(err)
		}
		tok, err := deviceAuthorize(ctx, client, p, map[string]any{"timeout": 30})
		testStderr.Reset()
		if err != nil || tok == nil || tok.AccessToken != "AT-1" {
			t.Fatalf("%s token %+v %v", c.issuer, tok, err)
		}
		if _, has := deviceForm["access_type"]; has != c.expectsOffline {
			t.Fatalf("%s device form %v", c.issuer, deviceForm)
		}
	}
}
