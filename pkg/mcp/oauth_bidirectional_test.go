package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	bidiServerURL = "https://example.com/mcp"
	bidiPRMURL    = "https://example.com/.well-known/oauth-protected-resource"
	bidiASMURL    = "https://auth.example.com/.well-known/oauth-authorization-server"
	bidiChallenge = `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`
)

// Manager provider for srv at the example server holding the given tokens and a registered client
func bidiProvider(t *testing.T, tokens *OAuthToken) *OAuthProvider {
	t.Helper()
	freshManager(t)
	s := NewTokenStorage(context.Background(), "srv", "")
	if tokens != nil {
		if err := s.SetTokens(tokens); err != nil {
			t.Fatal(err)
		}
		info, err := validateClientInfo(map[string]any{"client_id": "test-client", "redirect_uris": []any{"http://127.0.0.1:12345/callback"},
			"grant_types": []any{"authorization_code", "refresh_token"}, "response_types": []any{"code"}, "token_endpoint_auth_method": "none"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetClientInfo(info); err != nil {
			t.Fatal(err)
		}
	}
	return managerProvider(t, context.Background(), "srv", bidiServerURL, nil)
}

// Discovery answers shared by the flow tests, with asm as the authorization server metadata document
func bidiDiscovery(r *http.Request, asm map[string]any) (int, any, http.Header, bool) {
	switch r.URL.String() {
	case bidiServerURL:
		return 401, nil, http.Header{"Www-Authenticate": {bidiChallenge}}, true
	case bidiPRMURL:
		return 200, map[string]any{"authorization_servers": []any{"https://auth.example.com"}, "resource": bidiServerURL,
			"bearer_methods_supported": []any{"header"}}, nil, true
	case bidiASMURL:
		return 200, asm, nil, true
	}
	return 0, nil, nil, false
}

// Authorization server metadata without a registration endpoint unless one is given
func bidiASM(registration string) map[string]any {
	m := map[string]any{"issuer": "https://auth.example.com", "authorization_endpoint": "https://auth.example.com/authorize",
		"token_endpoint": "https://auth.example.com/token", "response_types_supported": []any{"code"},
		"code_challenge_methods_supported": []any{"S256"}}
	if registration != "" {
		m["registration_endpoint"] = registration
	}
	return m
}

func TestOAuthBidirectionalForwards401IntoDiscovery(t *testing.T) {
	p := bidiProvider(t, tokenPair("old_access", "old_refresh", int64Ptr(3600)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	challenged := false
	s := newStandIn(func(r *http.Request, _ string) (int, any, http.Header) {
		if r.URL.String() == bidiServerURL {
			challenged = true
			return 401, nil, http.Header{"Www-Authenticate": {bidiChallenge}}
		}
		if challenged {
			cancel()
		}
		return 404, nil, nil
	})
	p.client.Transport = s
	_, err := authedDo(ctx, p, s, http.MethodPost, bidiServerURL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("flow did not stop at the discovery request %v", err)
	}
	hits := s.allHits()
	at := len(hits) - 2
	if at < 0 || hits[at] != [2]string{"POST", bidiServerURL} || hits[at+1] != [2]string{"GET", bidiPRMURL} {
		t.Fatalf("hits %v", hits)
	}
}

func TestOAuthBidirectionalLongLivedResourceRequestDoesNotBlockConcurrentPost(t *testing.T) {
	p := bidiProvider(t, tokenPair("access-token", "refresh-token", int64Ptr(3600)))
	getSent, releaseGet := make(chan struct{}), make(chan struct{})
	auths := make(chan [2]string, 4)
	gets := 0
	s := newStandIn(func(r *http.Request, _ string) (int, any, http.Header) {
		if r.URL.String() != bidiServerURL {
			return 404, nil, nil
		}
		auths <- [2]string{r.Method, r.Header.Get("Authorization")}
		if r.Method == http.MethodPost {
			return 200, nil, nil
		}
		gets++
		if gets > 1 {
			return 200, nil, nil
		}
		close(getSent)
		<-releaseGet
		return 401, nil, http.Header{"Www-Authenticate": {bidiChallenge}}
	})
	p.client.Transport = s
	getDone := make(chan error, 1)
	go func() {
		resp, err := authedDo(context.Background(), p, s, http.MethodGet, bidiServerURL)
		if err == nil && resp.StatusCode != 200 {
			err = errors.New(resp.Status)
		}
		getDone <- err
	}()
	<-getSent
	postCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := authedDo(postCtx, p, s, http.MethodPost, bidiServerURL)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("concurrent POST blocked %v", err)
	}
	if got := <-auths; got != [2]string{"GET", "Bearer access-token"} {
		t.Fatalf("GET carried %v", got)
	}
	if got := <-auths; got != [2]string{"POST", "Bearer access-token"} {
		t.Fatalf("POST carried %v", got)
	}
	if err := p.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.currentTokens = tokenPair("new-access-token", "refresh-token", int64Ptr(3600))
	p.updateTokenExpiry(p.currentTokens)
	p.unlock()
	before := len(s.allHits())
	close(releaseGet)
	if err := <-getDone; err != nil {
		t.Fatal(err)
	}
	if got := <-auths; got != [2]string{"GET", "Bearer new-access-token"} {
		t.Fatalf("retried GET carried %v", got)
	}
	if after := s.allHits()[before:]; len(after) != 1 || after[0] != [2]string{"GET", bidiServerURL} {
		t.Fatalf("stale 401 started a flow %v", after)
	}
}

func TestOAuthBidirectionalFabricatedDCRRegistrationRejectedBeforeNetwork(t *testing.T) {
	p := bidiProvider(t, nil)
	s := newStandIn(func(r *http.Request, _ string) (int, any, http.Header) {
		if status, v, h, ok := bidiDiscovery(r, bidiASM("")); ok {
			return status, v, h
		}
		return 404, nil, nil
	})
	p.client.Transport = s
	_, err := authedDo(context.Background(), p, s, http.MethodPost, bidiServerURL)
	var regErr *OAuthRegistrationError
	if !errors.As(err, &regErr) {
		t.Fatalf("err %v", err)
	}
	for _, want := range []string{"does not support automatic client registration", "https://example.com/register", "config.yaml", "icebath mcp login srv"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%q missing from %q", want, err.Error())
		}
	}
	if s.hit(http.MethodPost, "https://example.com/register") {
		t.Fatal("fabricated registration reached the network")
	}
	if hits := s.allHits(); len(hits) != 3 || hits[1] != [2]string{"GET", bidiPRMURL} || hits[2] != [2]string{"GET", bidiASMURL} {
		t.Fatalf("hits %v", hits)
	}
}

func TestOAuthBidirectionalAdvertisedRegistrationEndpointPassesThrough(t *testing.T) {
	p := bidiProvider(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newStandIn(func(r *http.Request, _ string) (int, any, http.Header) {
		if status, v, h, ok := bidiDiscovery(r, bidiASM("https://example.com/register")); ok {
			return status, v, h
		}
		cancel()
		return 404, nil, nil
	})
	p.client.Transport = s
	if _, err := authedDo(ctx, p, s, http.MethodPost, bidiServerURL); err == nil {
		t.Fatal("flow went past the aborted registration")
	}
	hits := s.allHits()
	if len(hits) != 4 || hits[3] != [2]string{"POST", "https://example.com/register"} {
		t.Fatalf("hits %v", hits)
	}
}

func TestOAuthBidirectionalGuardIgnoresNonFabricatedPostPaths(t *testing.T) {
	p := bidiProvider(t, nil)
	ctx := context.Background()
	req := func(method, u string) *http.Request { r, _ := http.NewRequest(method, u, nil); return r }
	if p.oauthMetadata != nil {
		t.Fatal("metadata already discovered")
	}
	if err := p.hooks.checkOutgoing(ctx, p, req("POST", "https://example.com/register")); err != nil {
		t.Fatalf("refused before discovery %v", err)
	}
	md, err := validateOAuthMetadata(map[string]any{"issuer": "https://auth.example.com",
		"authorization_endpoint": "https://auth.example.com/authorize", "token_endpoint": "https://auth.example.com/token"}, false)
	if err != nil {
		t.Fatal(err)
	}
	p.oauthMetadata = md
	for _, c := range [][2]string{{"POST", "https://example.com/foo/register"}, {"POST", "https://example.com/register/"},
		{"POST", "https://example.com/registers"}, {"POST", "https://other.example.com/register"},
		{"POST", "https://example.com:8443/register"}, {"GET", "https://example.com/register"}} {
		if err := p.hooks.checkOutgoing(ctx, p, req(c[0], c[1])); err != nil {
			t.Fatalf("%v refused %v", c, err)
		}
	}
	var regErr *OAuthRegistrationError
	if err := p.hooks.checkOutgoing(ctx, p, req("POST", "https://example.com:443/register")); !errors.As(err, &regErr) {
		t.Fatalf("fabricated registration passed %v", err)
	}
}
