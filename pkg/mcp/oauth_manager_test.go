package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Interactive provider built by a fresh manager for a server
func managerProvider(t *testing.T, ctx context.Context, name, serverURL string, oauthCfg map[string]any) *OAuthProvider {
	t.Helper()
	p, err := getManager().getOrBuildProvider(ForceInteractiveOAuth(ctx), name, serverURL, oauthCfg, HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func freshManager(t *testing.T) string {
	t.Helper()
	dir := oauthHome(t)
	resetOAuthManager()
	t.Cleanup(resetOAuthManager)
	return dir
}

func TestOAuthManagerIsolatesSameNamedServersByProfileHome(t *testing.T) {
	freshManager(t)
	ctxA := home.WithOverride(context.Background(), t.TempDir())
	ctxB := home.WithOverride(context.Background(), t.TempDir())
	pa := managerProvider(t, ctxA, "srv", "https://mcp.example.com/mcp", nil)
	pb := managerProvider(t, ctxB, "srv", "https://mcp.example.com/mcp", nil)
	if pa == pb || managerProvider(t, ctxA, "srv", "https://mcp.example.com/mcp", nil) != pa {
		t.Fatal("providers not keyed by home")
	}
	if getManager().evict(ctxA, "srv", "") == nil {
		t.Fatal("evict found nothing")
	}
	if _, p := getManager().lookup(ctxB, "srv", ""); p != pb {
		t.Fatal("evicting one profile dropped the other")
	}
	if managerProvider(t, ctxA, "srv", "https://other.example.com/mcp", nil) == pa {
		t.Fatal("URL change kept the cached provider")
	}
}

func TestOAuthManagerRestoreEntryPreservesNewerConcurrentEntry(t *testing.T) {
	freshManager(t)
	ctx := context.Background()
	managerProvider(t, ctx, "srv", "https://mcp.example.com/mcp", nil)
	old := getManager().evict(ctx, "srv", "")
	newer := managerProvider(t, ctx, "srv", "https://mcp.example.com/mcp", nil)
	getManager().restoreEntry(ctx, "srv", old, "")
	if _, p := getManager().lookup(ctx, "srv", ""); p != newer {
		t.Fatal("restore clobbered the newer entry")
	}
	getManager().evict(ctx, "srv", "")
	getManager().restoreEntry(ctx, "srv", old, "")
	if e, _ := getManager().lookup(ctx, "srv", ""); e != old {
		t.Fatal("restore did not put the entry back")
	}
}

func TestOAuthManagerRefusesNonInteractiveFirstLogin(t *testing.T) {
	freshManager(t)
	_, err := getManager().getOrBuildProvider(SuppressInteractiveOAuth(context.Background()), "srv", "https://mcp.example.com/mcp", nil, HTTPOptions{})
	var ni *OAuthNonInteractiveError
	if !errors.As(err, &ni) || !strings.Contains(err.Error(), "icebath mcp login srv") {
		t.Fatalf("error %v", err)
	}
}

func TestOAuthManagerFirstObservation401StillAllowsInPlaceRefresh(t *testing.T) {
	dir := freshManager(t)
	ctx := context.Background()
	p := managerProvider(t, ctx, "srv", "https://mcp.example.com/mcp", nil)
	p.currentTokens, p.clientInfo = bearer("a", "r"), &OAuthClientInfo{ClientID: "c"}
	writeTokenFile(t, dir, "srv.json", map[string]any{"access_token": "a", "refresh_token": "r", "token_type": "Bearer"})
	ok, err := getManager().handle401(ctx, "srv", "a")
	if err != nil || !ok {
		t.Fatalf("handle401 %v %v", ok, err)
	}
}

func TestOAuthManagerExternalLoginAfterAbsentTokensFileForcesReload(t *testing.T) {
	dir := freshManager(t)
	ctx := context.Background()
	p := managerProvider(t, ctx, "srv", "https://mcp.example.com/mcp", nil)
	if changed, _ := getManager().invalidateIfDiskChanged(ctx, "srv", ""); changed {
		t.Fatal("absent file reported a change")
	}
	p.initialized = true
	writeTokenFile(t, dir, "srv.json", map[string]any{"access_token": "ext", "token_type": "Bearer"})
	if changed, err := getManager().invalidateIfDiskChanged(ctx, "srv", ""); !changed || err != nil || p.initialized {
		t.Fatalf("external login not reloaded %v %v", changed, err)
	}
	if changed, _ := getManager().invalidateIfDiskChanged(ctx, "srv", ""); changed {
		t.Fatal("unchanged file reported a change")
	}
}

func TestOAuthManagerHandle401DedupsConcurrentRecoveries(t *testing.T) {
	freshManager(t)
	ctx := context.Background()
	p := managerProvider(t, ctx, "srv", "https://mcp.example.com/mcp", nil)
	p.currentTokens, p.clientInfo = bearer("a", "r"), &OAuthClientInfo{ClientID: "c"}
	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = getManager().handle401(ctx, "srv", "a")
		}()
	}
	wg.Wait()
	for _, r := range results {
		if !r {
			t.Fatalf("results %v", results)
		}
	}
	e, _ := getManager().lookup(ctx, "srv", "")
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pending401) != 0 {
		t.Fatal("dedup slot not cleared")
	}
	if ok, _ := getManager().handle401(ctx, "missing", "a"); ok {
		t.Fatal("unknown server recovered")
	}
}

func tokenEndpointReply(status int, rawURL, body string) *flowResponse {
	req, _ := http.NewRequest(http.MethodPost, rawURL, nil)
	return &flowResponse{StatusCode: status, Body: []byte(body), Request: req}
}

func TestOAuthManagerPoisonsOnlyAnInvalidClientAtTheTokenEndpoint(t *testing.T) {
	freshManager(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, url, body string
		status          int
		oauth           map[string]any
		poisoned        bool
	}{
		{"poison", "https://auth.example/token/?x=1", `{"error":"invalid_client"}`, 400, nil, true},
		{"metadata", "https://auth.example/token", `{"error":"invalid_client_metadata"}`, 400, nil, false},
		{"elsewhere", "https://auth.example/register", `{"error":"invalid_client"}`, 401, nil, false},
		{"status", "https://auth.example/token", `{"error":"invalid_client"}`, 500, nil, false},
		{"preregistered", "https://auth.example/token", `{"error":"invalid_client"}`, 401, map[string]any{"client_id": "cfg"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := managerProvider(t, ctx, "srv-"+tc.name, "https://mcp.example.com/mcp", tc.oauth)
			p.oauthMetadata = &OAuthMetadata{Issuer: "https://auth.example/", TokenEndpoint: "https://auth.example/token"}
			p.clientInfo, p.initialized = &OAuthClientInfo{ClientID: "dcr"}, true
			if err := p.storage.SetClientInfo(ctx, p.clientInfo); err != nil {
				t.Fatal(err)
			}
			p.hooks.inspectResponse(ctx, p, tokenEndpointReply(tc.status, tc.url, tc.body))
			if gone := p.clientInfo == nil && !p.initialized && !exists(p.storage.ClientInfoPath()); gone != tc.poisoned {
				t.Fatalf("poisoned %v want %v", gone, tc.poisoned)
			}
		})
	}
}

func TestOAuthManagerRejectedDocumentStopsBeingPresented(t *testing.T) {
	freshManager(t)
	ctx := context.Background()
	p := managerProvider(t, ctx, "srv", "https://mcp.example.com/mcp", nil)
	p.oauthMetadata = &OAuthMetadata{Issuer: "https://auth.example/", TokenEndpoint: "https://auth.example/token"}
	p.clientMetadataURL = cimdClientMetadataURL
	p.clientInfo = &OAuthClientInfo{ClientID: cimdClientMetadataURL}
	p.hooks.inspectResponse(ctx, p, tokenEndpointReply(401, "https://auth.example/token", `{"error":"invalid_client"}`))
	if p.clientMetadataURL != "" || !p.storage.CIMDRejected() || p.clientInfo != nil {
		t.Fatal("rejected CIMD document still presented")
	}
	dcr := managerProvider(t, ctx, "dcr", "https://mcp.example.com/mcp", nil)
	dcr.oauthMetadata, dcr.clientMetadataURL = p.oauthMetadata, cimdClientMetadataURL
	dcr.clientInfo = &OAuthClientInfo{ClientID: "dcr-client"}
	dcr.hooks.inspectResponse(ctx, dcr, tokenEndpointReply(401, "https://auth.example/token", `{"error":"invalid_client"}`))
	if dcr.clientMetadataURL != cimdClientMetadataURL || dcr.storage.CIMDRejected() {
		t.Fatal("rejected DCR client retired the CIMD document")
	}
}

func TestOAuthManagerRefusesTheFabricatedRegistration(t *testing.T) {
	freshManager(t)
	ctx := context.Background()
	p := managerProvider(t, ctx, "gmail", "https://gmailmcp.googleapis.com/mcp/v1", nil)
	post := func(u string) *http.Request { r, _ := http.NewRequest(http.MethodPost, u, nil); return r }
	if err := p.hooks.checkOutgoing(ctx, p, post("https://gmailmcp.googleapis.com/register")); err != nil {
		t.Fatalf("undiscovered metadata refused %v", err)
	}
	p.oauthMetadata = &OAuthMetadata{Issuer: "https://accounts.google.com/", TokenEndpoint: "https://oauth2.googleapis.com/token"}
	err := p.hooks.checkOutgoing(ctx, p, post("https://GMAILMCP.googleapis.com:443/register"))
	var regErr *OAuthRegistrationError
	if !errors.As(err, &regErr) || !strings.Contains(err.Error(), "client_id") || !strings.Contains(err.Error(), "icebath mcp login gmail") {
		t.Fatalf("fabricated registration sent %v", err)
	}
	get, _ := http.NewRequest(http.MethodGet, "https://gmailmcp.googleapis.com/register", nil)
	if p.hooks.checkOutgoing(ctx, p, get) != nil || p.hooks.checkOutgoing(ctx, p, post("https://gmailmcp.googleapis.com/token")) != nil {
		t.Fatal("unrelated request refused")
	}
	p.oauthMetadata.RegistrationEndpoint = strPtr("https://oauth2.googleapis.com/register")
	if p.hooks.checkOutgoing(ctx, p, post("https://gmailmcp.googleapis.com/register")) != nil {
		t.Fatal("advertised registration refused")
	}
}

func TestOAuthManagerAsyncAuthFlowPersistsOnCompletion(t *testing.T) {
	freshManager(t)
	ctx := context.Background()
	p := managerProvider(t, ctx, "srv", "https://mcp.example.com/mcp", nil)
	p.oauthMetadata = &OAuthMetadata{Issuer: "https://auth.example/", TokenEndpoint: "https://auth.example/token",
		AuthorizationEndpoint: strPtr("https://auth.example/authorize"), ResponseTypesSupported: []string{"code"}}
	p.hooks.flowFinished(ctx, p)
	if md := p.storage.LoadOAuthMetadata(); md == nil || md.TokenEndpoint != "https://auth.example/token" {
		t.Fatalf("metadata %v", md)
	}
}

func TestOAuthManagerSeedsExpiryAndPrefetchesMetadataOnColdLoad(t *testing.T) {
	freshManager(t)
	ctx := context.Background()
	p := managerProvider(t, ctx, "srv", "https://mcp.example.com/mcp", nil)
	s := newStandIn(func(r *http.Request, _ string) (int, any, http.Header) {
		switch {
		case strings.Contains(r.URL.Path, "oauth-protected-resource"):
			return 200, map[string]any{"resource": "https://mcp.example.com/mcp", "authorization_servers": []string{"https://auth.example"}}, nil
		case r.URL.Host == "auth.example" && strings.Contains(r.URL.Path, "oauth-authorization-server"):
			return 200, map[string]any{"issuer": "https://auth.example", "authorization_endpoint": "https://auth.example/authorize",
				"token_endpoint": "https://auth.example/oauth/token"}, nil
		}
		return 404, nil, nil
	})
	p.client.Transport = s
	p.currentTokens = &OAuthToken{AccessToken: "a", TokenType: "Bearer", ExpiresIn: int64Ptr(0)}
	p.hooks.afterInitialize(ctx, p)
	if p.isTokenValid() {
		t.Fatal("zero TTL token treated as valid")
	}
	if p.oauthMetadata == nil || p.oauthMetadata.TokenEndpoint != "https://auth.example/oauth/token" || p.authServerURL != "https://auth.example" {
		t.Fatalf("prefetch %v %v", p.oauthMetadata, s.allHits())
	}
	if md := p.storage.LoadOAuthMetadata(); md == nil || md.TokenEndpoint != "https://auth.example/oauth/token" {
		t.Fatal("prefetched metadata not persisted")
	}
}

func TestOAuthManagerForwardsTheDocumentURL(t *testing.T) {
	privateCIMDPorts(t)
	resetOAuthManager()
	t.Cleanup(resetOAuthManager)
	p := managerProvider(t, context.Background(), "srv", "https://mcp.example.com/mcp", map[string]any{})
	if p.clientMetadataURL != cimdClientMetadataURL {
		t.Fatalf("client metadata url %q", p.clientMetadataURL)
	}
}

func TestOAuthManagerRejectedDocumentStaysRejectedAfterARestart(t *testing.T) {
	privateCIMDPorts(t)
	resetOAuthManager()
	t.Cleanup(resetOAuthManager)
	ctx := context.Background()
	p := managerProvider(t, ctx, "srv", "https://mcp.example.com", map[string]any{})
	if p.clientMetadataURL != cimdClientMetadataURL {
		t.Fatalf("document not presented %q", p.clientMetadataURL)
	}
	p.oauthMetadata = &OAuthMetadata{TokenEndpoint: idpTokenEndpoint}
	p.clientInfo, p.initialized = &OAuthClientInfo{ClientID: cimdClientMetadataURL}, true
	p.hooks.inspectResponse(ctx, p, tokenEndpointReply(400, idpTokenEndpoint, `{"error":"invalid_client"}`))
	s := NewTokenStorage(ctx, "srv", "")
	if !s.CIMDRejected() {
		t.Fatal("rejection marker not on disk")
	}
	if _, _, ok := maybeUseCIMD(ctx, map[string]any{}, s); ok {
		t.Fatal("rejected document presented again")
	}
	resetOAuthManager()
	if again := managerProvider(t, ctx, "srv", "https://mcp.example.com", map[string]any{}); again.clientMetadataURL != "" {
		t.Fatalf("restarted manager presents %q", again.clientMetadataURL)
	}
}
