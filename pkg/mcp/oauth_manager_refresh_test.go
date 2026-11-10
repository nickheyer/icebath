package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const idpTokenEndpoint = "https://idp.example.com/oauth/token"

// Manager provider for srv with a discovered token endpoint and the flow marked initialized
func tokenEndpointProvider(t *testing.T, oauthCfg map[string]any, endpoint string) *OAuthProvider {
	t.Helper()
	resetOAuthManager()
	p := managerProvider(t, context.Background(), "srv", "https://mcp.example.com", oauthCfg)
	p.oauthMetadata = &OAuthMetadata{TokenEndpoint: endpoint}
	p.initialized = true
	return p
}

// Bearer pair with an access token, a refresh token and an optional lifetime
func tokenPair(access, refresh string, expiresIn *int64) *OAuthToken {
	t := bearer(access, refresh)
	t.ExpiresIn = expiresIn
	return t
}

// Provider holding an expired A1 R1 pair with real metadata and client info, ready to refresh
func fencedProvider(t *testing.T, endpoint string) *OAuthProvider {
	t.Helper()
	p := tokenEndpointProvider(t, nil, endpoint)
	md, err := validateOAuthMetadata(map[string]any{"issuer": "https://idp.example.com",
		"authorization_endpoint": "https://idp.example.com/authorize", "token_endpoint": endpoint}, false)
	if err != nil {
		t.Fatal(err)
	}
	info, err := validateClientInfo(map[string]any{"client_id": "client-id", "redirect_uris": []any{"http://localhost/cb"}})
	if err != nil {
		t.Fatal(err)
	}
	p.oauthMetadata, p.clientInfo = md, info
	p.currentTokens = tokenPair("A1", "R1", int64Ptr(3600))
	past := oauthNow() - 10
	p.tokenExpiry = &past
	return p
}

// Reply the token endpoint gave, as the flow hands it to a response handler
func tokenReply(status int, body string) *flowResponse {
	return tokenEndpointReply(status, idpTokenEndpoint, body)
}

// Stand-in answering every non POST with an empty JSON object and every POST through post
func refreshStandIn(post func(refresh string) (int, any)) *standIn {
	return newStandIn(func(r *http.Request, body string) (int, any, http.Header) {
		if r.Method != http.MethodPost {
			return 200, map[string]any{}, nil
		}
		q, _ := url.ParseQuery(body)
		status, v := post(q.Get("refresh_token"))
		return status, v, nil
	})
}

// Drives one MCP request through the provider with the stand-in on both the auth and the MCP wire
func driveFlow(ctx context.Context, p *OAuthProvider, s *standIn) error {
	p.client.Transport = s
	resp, err := authedDo(ctx, p, s, http.MethodGet, "https://mcp.example.com/mcp")
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestOAuthManagerBridgeForwardsRequestsAndPoisonsOnTokenEndpoint400(t *testing.T) {
	dir := freshManager(t)
	writeTokenFile(t, dir, "srv.client.json", `{"client_id": "dead"}`)
	p := tokenEndpointProvider(t, nil, idpTokenEndpoint)
	info, err := p.storage.GetClientInfo(context.Background())
	if err != nil || info == nil || info.ClientID != "dead" {
		t.Fatalf("client info %+v %v", info, err)
	}
	p.clientInfo = info
	p.currentTokens = tokenPair("A1", "R1", int64Ptr(3600))
	past := oauthNow() - 10
	p.tokenExpiry = &past
	s := refreshStandIn(func(string) (int, any) { return 400, `{"error":"invalid_client"}` })
	if err := driveFlow(context.Background(), p, s); err != nil {
		t.Fatal(err)
	}
	if !s.hit(http.MethodPost, idpTokenEndpoint) || !s.hit(http.MethodGet, "https://mcp.example.com/mcp") {
		t.Fatalf("hits %v", s.allHits())
	}
	if exists(filepath.Join(dir, "mcp-tokens", "srv.client.json")) || p.initialized || p.clientInfo != nil {
		t.Fatalf("poison not applied: initialized %v client %+v", p.initialized, p.clientInfo)
	}
}

func TestOAuthManagerProviderTokenExchangeIncludesDCRSecret(t *testing.T) {
	freshManager(t)
	p := managerProvider(t, context.Background(), "supabase", "https://mcp.supabase.com/mcp", nil)
	if len(p.clientMetadata.RedirectURIs) == 0 {
		t.Fatal("no redirect uris")
	}
	info, err := validateClientInfo(map[string]any{"client_id": "client-id", "client_secret": "secret",
		"redirect_uris": []any{p.clientMetadata.RedirectURIs[0]}, "token_endpoint_auth_method": "none"})
	if err != nil {
		t.Fatal(err)
	}
	p.clientInfo = info
	_, body, err := p.exchangeTokenAuthorizationCode(context.Background(), "auth-code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(string(body))
	if got := q["client_secret"]; len(got) != 1 || got[0] != "secret" {
		t.Fatalf("client_secret %v", got)
	}
	if p.clientInfo == nil || derefStr(p.clientInfo.TokenEndpointAuthMethod) != "client_secret_post" {
		t.Fatalf("auth method %+v", p.clientInfo)
	}
}

func TestOAuthManagerMalformed201TokenResponseDoesNotExposeBody(t *testing.T) {
	freshManager(t)
	p := tokenEndpointProvider(t, nil, idpTokenEndpoint)
	err := p.handleTokenResponse(context.Background(), tokenReply(201, `{"access_token": {"secret": "access-secret"}}`))
	var te *OAuthTokenError
	if !errors.As(err, &te) || err.Error() != "Invalid token response" || strings.Contains(err.Error(), "access-secret") {
		t.Fatalf("err %v", err)
	}
}

// Body that fails mid read with a message carrying token material
type failingBody struct{ msg string }

func (b failingBody) Read([]byte) (int, error) { return 0, errors.New(b.msg) }
func (b failingBody) Close() error             { return nil }

// Stand-in transport whose POST answers 201 with a body that fails to read
type readErrorTransport struct{ msg string }

func (rt readErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
	}
	if r.Method != http.MethodPost {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	}
	return &http.Response{StatusCode: 201, Header: http.Header{}, Body: failingBody{rt.msg}, Request: r}, nil
}

func TestOAuthManagerTokenReadErrorDoesNotExposeBody(t *testing.T) {
	freshManager(t)
	p := tokenEndpointProvider(t, nil, idpTokenEndpoint)
	p.client.Transport = readErrorTransport{"access-secret refresh-secret"}
	req, body, err := newFormRequest(context.Background(), idpTokenEndpoint, form{{"grant_type", "authorization_code"}}, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	var failures []string
	resp, sendErr := p.send(context.Background(), req, body, &failures)
	if sendErr == nil || resp == nil || resp.ReadErr == nil {
		t.Fatalf("read failure not surfaced %v %+v", sendErr, resp)
	}
	err = p.handleTokenResponse(context.Background(), resp)
	var te *OAuthTokenError
	if !errors.As(err, &te) || err.Error() != "Invalid token response" {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "access-secret") || strings.Contains(err.Error(), "refresh-secret") {
		t.Fatalf("leaked %v", err)
	}
}

func TestOAuthManagerMalformed201RefreshResponseClearsTokens(t *testing.T) {
	freshManager(t)
	p := tokenEndpointProvider(t, nil, idpTokenEndpoint)
	p.currentTokens = bearer("A1", "R1")
	logs := captureLogs(t)
	ok, err := p.handleRefreshResponse(context.Background(), tokenReply(201, `{"refresh_token": "refresh-secret"}`))
	if err != nil || ok || p.currentTokens != nil {
		t.Fatalf("ok %v err %v tokens %+v", ok, err, p.currentTokens)
	}
	if strings.Contains(logs.text(), "refresh-secret") {
		t.Fatalf("logged %q", logs.text())
	}
}

func TestOAuthManagerRefreshReadErrorClearsTokens(t *testing.T) {
	freshManager(t)
	p := fencedProvider(t, idpTokenEndpoint)
	if err := p.storage.SetTokens(context.Background(), p.currentTokens); err != nil {
		t.Fatal(err)
	}
	p.client.Transport = readErrorTransport{"body read failed"}
	req, err := http.NewRequest(http.MethodGet, "https://mcp.example.com/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Prepare(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if p.currentTokens != nil || p.initialized || req.Header.Get("Authorization") != "" || p.fence != nil {
		t.Fatalf("tokens %+v initialized %v auth %q", p.currentTokens, p.initialized, req.Header.Get("Authorization"))
	}
	direct := tokenEndpointProvider(t, nil, idpTokenEndpoint)
	direct.currentTokens = bearer("A1", "R1")
	ok, err := direct.handleRefreshResponse(context.Background(), &flowResponse{StatusCode: 201, ReadErr: errors.New("body read failed")})
	if err != nil || ok || direct.currentTokens != nil {
		t.Fatalf("ok %v err %v tokens %+v", ok, err, direct.currentTokens)
	}
}

func TestOAuthManagerRefreshResponseWithoutRefreshTokenKeepsStoredOne(t *testing.T) {
	dir := freshManager(t)
	p := tokenEndpointProvider(t, nil, idpTokenEndpoint)
	p.currentTokens = tokenPair("at-1", "rt-keep", int64Ptr(3600))
	p.currentTokens.Scope = strPtr("read")
	p.clientInfo = &OAuthClientInfo{ClientID: "cid"}
	ok, err := p.handleRefreshResponse(context.Background(), tokenReply(200, `{"access_token": "at-2", "token_type": "Bearer", "expires_in": 3600}`))
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	disk := readJSONFile(t, filepath.Join(dir, "mcp-tokens", "srv.json"))
	cur := p.currentTokens
	if cur.AccessToken != "at-2" || derefStr(cur.RefreshToken) != "rt-keep" || disk["refresh_token"] != "rt-keep" {
		t.Fatalf("refresh token %+v disk %v", cur, disk)
	}
	if derefStr(cur.Scope) != "read" || disk["scope"] != "read" || !p.canRefreshToken() {
		t.Fatalf("scope %v disk %v", cur.Scope, disk)
	}
}

func TestOAuthManagerRefreshResponseWithNewRefreshTokenRotates(t *testing.T) {
	dir := freshManager(t)
	p := tokenEndpointProvider(t, nil, idpTokenEndpoint)
	p.currentTokens = tokenPair("at-1", "rt-old", int64Ptr(3600))
	ok, err := p.handleRefreshResponse(context.Background(), tokenReply(200, `{"access_token": "at-2", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "rt-new"}`))
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	disk := readJSONFile(t, filepath.Join(dir, "mcp-tokens", "srv.json"))
	if derefStr(p.currentTokens.RefreshToken) != "rt-new" || disk["refresh_token"] != "rt-new" {
		t.Fatalf("tokens %+v disk %v", p.currentTokens, disk)
	}
}

func TestOAuthManagerRefresh400Recovery(t *testing.T) {
	for _, tc := range []struct {
		name      string
		disk      *OAuthToken
		recovered bool
	}{
		{"recovers_token_rotated_by_peer", tokenPair("A2", "R2", int64Ptr(3600)), true},
		{"rejects_disk_token_without_refresh_token", tokenPair("A2", "", int64Ptr(3600)), false},
		{"still_clears_when_disk_is_same_token", tokenPair("A1", "R1", int64Ptr(3600)), false},
		{"does_not_recover_expired_disk_token", tokenPair("A2", "R2", int64Ptr(-60)), false},
		{"does_not_recover_tokenless_disk_entry", tokenPair("", "R2", int64Ptr(3600)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freshManager(t)
			p := tokenEndpointProvider(t, nil, idpTokenEndpoint)
			p.currentTokens = tokenPair("A1", "R1", int64Ptr(3600))
			if err := p.storage.SetTokens(context.Background(), tc.disk); err != nil {
				t.Fatal(err)
			}
			ok, err := p.handleRefreshResponse(context.Background(), tokenReply(400, `{"error":"invalid_grant"}`))
			if err != nil || ok != tc.recovered {
				t.Fatalf("recovered %v err %v", ok, err)
			}
			if !tc.recovered {
				if p.currentTokens != nil {
					t.Fatalf("tokens kept %+v", p.currentTokens)
				}
				return
			}
			if p.currentTokens.AccessToken != "A2" || derefStr(p.currentTokens.RefreshToken) != "R2" {
				t.Fatalf("tokens %+v", p.currentTokens)
			}
		})
	}
}

func TestOAuthManagerRefresh400RecoveryNeverLogsTokenMaterial(t *testing.T) {
	freshManager(t)
	p := tokenEndpointProvider(t, nil, idpTokenEndpoint)
	p.currentTokens = tokenPair("access-secret", "refresh-secret", int64Ptr(3600))
	if err := p.storage.SetTokens(context.Background(), tokenPair("rotated-access-secret", "rotated-refresh-secret", int64Ptr(3600))); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	ok, err := p.handleRefreshResponse(context.Background(), tokenReply(400, `{"error":"invalid_grant"}`))
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if text := logs.text(); text == "" || strings.Contains(text, "refresh-secret") || strings.Contains(text, "access-secret") {
		t.Fatalf("logs %q", text)
	}
}

func TestOAuthManagerRefresh400RecoveryRejectsDiskPairFromAnotherIssuer(t *testing.T) {
	freshManager(t)
	p := fencedProvider(t, idpTokenEndpoint)
	p.storage.BindIssuer("https://other-idp.example")
	if err := p.storage.SetTokens(context.Background(), tokenPair("A2", "R2", int64Ptr(3600))); err != nil {
		t.Fatal(err)
	}
	ok, err := p.handleRefreshResponse(context.Background(), tokenReply(400, `{"error":"invalid_grant"}`))
	if err != nil || ok || p.currentTokens != nil {
		t.Fatalf("ok %v err %v tokens %+v", ok, err, p.currentTokens)
	}
	disk, err := p.storage.GetTokens()
	if err != nil || disk == nil || disk.RefreshToken != nil {
		t.Fatalf("disk %+v %v", disk, err)
	}
}

func TestOAuthManagerConcurrentRefreshPresentsSingleUseTokenExactlyOnce(t *testing.T) {
	freshManager(t)
	a := fencedProvider(t, idpTokenEndpoint)
	b := fencedProvider(t, idpTokenEndpoint)
	if a == b {
		t.Fatal("same provider")
	}
	if err := a.storage.SetTokens(context.Background(), tokenPair("A1", "R1", int64Ptr(3600))); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var presented []string
	post := func(refresh string) (int, any) {
		mu.Lock()
		defer mu.Unlock()
		presented = append(presented, refresh)
		if len(presented) == 1 && refresh == "R1" {
			return 200, map[string]any{"access_token": "A2", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "R2"}
		}
		return 400, map[string]any{"error": "invalid_grant"}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, p := range []*OAuthProvider{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = driveFlow(context.Background(), p, refreshStandIn(post))
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatal(errs)
	}
	if len(presented) != 1 || presented[0] != "R1" {
		t.Fatalf("presented %v", presented)
	}
	for _, p := range []*OAuthProvider{a, b} {
		if p.currentTokens.AccessToken != "A2" || derefStr(p.currentTokens.RefreshToken) != "R2" || p.fence != nil {
			t.Fatalf("tokens %+v fence %v", p.currentTokens, p.fence)
		}
	}
}

func TestOAuthManagerRefreshFailsClosedWhileAPeerHoldsTheFence(t *testing.T) {
	freshManager(t)
	p := fencedProvider(t, idpTokenEndpoint)
	if err := p.storage.SetTokens(context.Background(), tokenPair("A1", "R1", int64Ptr(3600))); err != nil {
		t.Fatal(err)
	}
	prev := refreshFenceTimeout
	refreshFenceTimeout = 200 * time.Millisecond
	t.Cleanup(func() { refreshFenceTimeout = prev })
	peer, err := acquireRefreshFence(context.Background(), p.storage.TokensPath(), prev)
	if err != nil {
		t.Fatal(err)
	}
	s := refreshStandIn(func(string) (int, any) { return 200, map[string]any{} })
	err = driveFlow(context.Background(), p, s)
	releaseRefreshFence(peer)
	var timeout *RefreshFenceTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("err %v", err)
	}
	if len(s.allHits()) != 0 || derefStr(p.currentTokens.RefreshToken) != "R1" || p.fence != nil {
		t.Fatalf("hits %v tokens %+v", s.allHits(), p.currentTokens)
	}
	disk, err := p.storage.GetTokens()
	if err != nil || derefStr(disk.RefreshToken) != "R1" {
		t.Fatalf("disk %+v %v", disk, err)
	}
}

func TestOAuthManagerRefreshAdoptsExpiredPeerPairAndPostsItsRefreshToken(t *testing.T) {
	freshManager(t)
	p := fencedProvider(t, idpTokenEndpoint)
	if err := p.storage.SetTokens(context.Background(), tokenPair("A2", "R2", int64Ptr(0))); err != nil {
		t.Fatal(err)
	}
	var presented []string
	s := refreshStandIn(func(refresh string) (int, any) {
		presented = append(presented, refresh)
		return 200, map[string]any{"access_token": "A3", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "R3"}
	})
	if err := driveFlow(context.Background(), p, s); err != nil {
		t.Fatal(err)
	}
	if len(presented) != 1 || presented[0] != "R2" || derefStr(p.currentTokens.RefreshToken) != "R3" {
		t.Fatalf("presented %v tokens %+v", presented, p.currentTokens)
	}
}

func TestOAuthManagerRefreshAdoptsPeerPairWithoutExpiryAndSkipsThePost(t *testing.T) {
	freshManager(t)
	p := fencedProvider(t, idpTokenEndpoint)
	if err := p.storage.SetTokens(context.Background(), tokenPair("A2", "R2", nil)); err != nil {
		t.Fatal(err)
	}
	posted := 0
	s := refreshStandIn(func(string) (int, any) { posted++; return 200, map[string]any{} })
	if err := driveFlow(context.Background(), p, s); err != nil {
		t.Fatal(err)
	}
	if posted != 0 || p.currentTokens.AccessToken != "A2" || derefStr(p.currentTokens.RefreshToken) != "R2" {
		t.Fatalf("posted %d tokens %+v", posted, p.currentTokens)
	}
}

func TestOAuthManagerRefreshRestartsFlowWhenDiskPairIsFromAnotherIssuer(t *testing.T) {
	freshManager(t)
	p := fencedProvider(t, idpTokenEndpoint)
	p.storage.BindIssuer("https://other-idp.example.com")
	if err := p.storage.SetTokens(context.Background(), tokenPair("A2", "R2", int64Ptr(3600))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.refreshTokenRequest(context.Background()); !errors.Is(err, errRefreshCompletedByPeer) {
		t.Fatalf("err %v", err)
	}
	if derefStr(p.currentTokens.RefreshToken) != "" || p.fence != nil {
		t.Fatalf("tokens %+v fence %v", p.currentTokens, p.fence)
	}
	disk, err := p.storage.GetTokens()
	if err != nil || disk.RefreshToken != nil {
		t.Fatalf("disk %+v %v", disk, err)
	}
}

func TestOAuthManagerRefreshFenceSurfacesNonContentionLockErrorsImmediately(t *testing.T) {
	dir := oauthHome(t)
	prev := fenceLock
	fenceLock = func(*os.File) (bool, error) { return false, errors.New("No locks available") }
	t.Cleanup(func() { fenceLock = prev })
	started := time.Now()
	_, err := acquireRefreshFence(context.Background(), filepath.Join(dir, "srv.json"), 5*time.Second)
	var timeout *RefreshFenceTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("err %v", err)
	}
	if time.Since(started) >= time.Second {
		t.Fatal("waited out the deadline")
	}
}
