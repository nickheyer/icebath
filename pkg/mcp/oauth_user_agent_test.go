package mcp

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOAuthUserAgentConfiguredIsReturned(t *testing.T) {
	if got := tokenRequestUserAgent(map[string]any{"user_agent": "My-MCP-Client/1.0"}); got != "My-MCP-Client/1.0" {
		t.Fatalf("got %q", got)
	}
}

func TestOAuthUserAgentUnsetValuesAreTreatedAsAbsent(t *testing.T) {
	for name, cfg := range map[string]map[string]any{"missing": {}, "null": {"user_agent": nil}, "empty": {"user_agent": ""},
		"whitespace-only": {"user_agent": "   "}, "non-string": {"user_agent": 7}} {
		if got := tokenRequestUserAgent(cfg); got != "" {
			t.Fatalf("%s gave %q", name, got)
		}
	}
}

func TestOAuthUserAgentIsStripped(t *testing.T) {
	if got := tokenRequestUserAgent(map[string]any{"user_agent": "  UA/2 "}); got != "UA/2" {
		t.Fatalf("got %q", got)
	}
}

// Builders the user agent cases run through, the direct auth builder and the manager
var userAgentBuilders = map[string]func(t *testing.T, cfg map[string]any) *OAuthProvider{
	"build_oauth_auth": func(t *testing.T, cfg map[string]any) *OAuthProvider {
		p, err := BuildOAuthAuth(ForceInteractiveOAuth(context.Background()), "srv", "https://mcp.example.com/mcp", cfg, HTTPOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return p
	},
	"oauth_manager": func(t *testing.T, cfg map[string]any) *OAuthProvider {
		resetOAuthManager()
		return managerProvider(t, context.Background(), "srv", "https://mcp.example.com/mcp", cfg)
	},
}

// Gives a provider the token endpoint, client and tokens both token requests need
func readyForTokenRequests(t *testing.T, p *OAuthProvider) {
	t.Helper()
	p.oauthMetadata = &OAuthMetadata{TokenEndpoint: idpTokenEndpoint}
	info, err := validateClientInfo(map[string]any{"client_id": "client-1", "redirect_uris": []any{"http://127.0.0.1:33333/callback"}})
	if err != nil {
		t.Fatal(err)
	}
	p.clientInfo = info
	p.currentTokens = bearer("at", "rt")
}

// User agents of the code exchange and the refresh request the provider builds
func tokenRequestAgents(t *testing.T, p *OAuthProvider) (exchange, refresh *http.Request) {
	t.Helper()
	exchange, _, err := p.exchangeTokenAuthorizationCode(context.Background(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	refresh, _, err = p.refreshTokenRequest(context.Background())
	p.releaseRefreshFence()
	if err != nil {
		t.Fatal(err)
	}
	return exchange, refresh
}

func TestOAuthUserAgentTokenRequestsCarryTheConfiguredUserAgent(t *testing.T) {
	for name, build := range userAgentBuilders {
		t.Run(name, func(t *testing.T) {
			freshManager(t)
			p := build(t, map[string]any{"user_agent": "My-MCP-Client/1.0"})
			readyForTokenRequests(t, p)
			exchange, refresh := tokenRequestAgents(t, p)
			if exchange.Header.Get("User-Agent") != "My-MCP-Client/1.0" || refresh.Header.Get("User-Agent") != "My-MCP-Client/1.0" {
				t.Fatalf("exchange %q refresh %q", exchange.Header.Get("User-Agent"), refresh.Header.Get("User-Agent"))
			}
		})
	}
}

func TestOAuthUserAgentUnconfiguredFallsBackToTheDefault(t *testing.T) {
	for name, build := range userAgentBuilders {
		t.Run(name, func(t *testing.T) {
			freshManager(t)
			p := build(t, map[string]any{})
			readyForTokenRequests(t, p)
			exchange, refresh := tokenRequestAgents(t, p)
			want := defaultAuthRequestUserAgent()
			if exchange.Header.Get("User-Agent") != want || refresh.Header.Get("User-Agent") != want {
				t.Fatalf("exchange %q refresh %q", exchange.Header.Get("User-Agent"), refresh.Header.Get("User-Agent"))
			}
		})
	}
}

func TestOAuthUserAgentDoesNotDisturbTokenAuthPreparation(t *testing.T) {
	freshManager(t)
	p := userAgentBuilders["build_oauth_auth"](t, map[string]any{"user_agent": "UA/1", "client_id": "pre", "client_secret": "shh",
		"token_endpoint_auth_method": "client_secret_basic"})
	readyForTokenRequests(t, p)
	info, err := validateClientInfo(map[string]any{"client_id": "pre", "client_secret": "shh", "token_endpoint_auth_method": "client_secret_basic",
		"redirect_uris": []any{"http://127.0.0.1:33333/callback"}})
	if err != nil {
		t.Fatal(err)
	}
	p.clientInfo = info
	exchange, _, err := p.exchangeTokenAuthorizationCode(context.Background(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if exchange.Header.Get("User-Agent") != "UA/1" || !strings.HasPrefix(exchange.Header.Get("Authorization"), "Basic ") {
		t.Fatalf("headers %v", exchange.Header)
	}
}

func TestOAuthUserAgentDeviceFlowTokenPollCarriesAUserAgentOnTheWire(t *testing.T) {
	freshManager(t)
	var mu sync.Mutex
	var paths, agents []string
	tokenPolls := 0
	var base string
	fs := newOAuthFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		mu.Lock()
		paths, agents = append(paths, r.URL.Path), append(agents, r.Header.Get("User-Agent"))
		if r.URL.Path == "/token" {
			tokenPolls++
		}
		polls := tokenPolls
		mu.Unlock()
		switch r.URL.Path {
		case "/device":
			writeJSON(w, 200, map[string]any{"device_code": "fixture-device-code", "user_code": "TEST-CODE",
				"verification_uri": base + "/verify", "interval": 0.01, "expires_in": 30})
		case "/token":
			if polls == 1 {
				writeJSON(w, 400, map[string]any{"error": "authorization_pending"})
				return
			}
			writeJSON(w, 200, map[string]any{"access_token": "at", "refresh_token": "rt", "token_type": "Bearer", "expires_in": 3600})
		default:
			writeJSON(w, 404, map[string]any{})
		}
	})
	base = fs.URL
	ctx := context.Background()
	cfg, storage := prepareOAuthConfig(ctx, "srv", base+"/mcp", map[string]any{})
	port, ok := cfg["redirect_port"]
	if !ok {
		port = 8420
	}
	cfg["_resolved_port"] = port
	md, err := buildClientMetadata(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newOAuthProvider(oauthProviderOptions{ServerURL: base + "/mcp", ClientMetadata: md, Storage: storage, TokenUserAgent: tokenRequestUserAgent(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	p.oauthMetadata = &OAuthMetadata{Issuer: base, TokenEndpoint: base + "/token", DeviceAuthorizationEndpoint: strPtr(base + "/device")}
	if p.clientInfo, err = validateClientInfo(map[string]any{"client_id": "fixture-client", "token_endpoint_auth_method": "none",
		"redirect_uris": []any{"http://127.0.0.1:33333/callback"}}); err != nil {
		t.Fatal(err)
	}
	tok, err := deviceAuthorize(ctx, &http.Client{Timeout: 5 * time.Second}, p, map[string]any{"timeout": 5})
	if err != nil || tok == nil || tok.AccessToken != "at" {
		t.Fatalf("token %+v %v", tok, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(paths, ",") != "/device,/token,/token" {
		t.Fatalf("paths %v", paths)
	}
	for i, a := range agents {
		if a == "" || (paths[i] == "/token" && a != defaultAuthRequestUserAgent()) {
			t.Fatalf("agents %v", agents)
		}
	}
}
