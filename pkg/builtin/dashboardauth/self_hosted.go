package dashboardauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	selfHostedTag              = "dashboard-auth-self-hosted"
	selfHostedDefaultScopes    = "openid profile email"
	selfHostedDiscoveryTimeout = 10 * time.Second
	selfHostedDiscoveryTTLSec  = 3600
)

var (
	selfHostedAllowedIDTokenAlgs = []string{"RS256", "ES256", "RS384", "RS512", "ES384", "ES512"}
	selfHostedDiscoveryHTTP      = pyhttpx.PlainClient(true)
	selfHostedSkipMu             sync.Mutex
	selfHostedLastSkip           string
	selfHostedClock              = time.Now
)

func selfHostedLog() *slog.Logger {
	return slog.Default().With("logger", "plugins.dashboard_auth.self_hosted")
}

// Reason the self hosted provider last declined to register, empty after a registration
func SelfHostedLastSkipReason() string {
	selfHostedSkipMu.Lock()
	defer selfHostedSkipMu.Unlock()
	return selfHostedLastSkip
}

func setSelfHostedLastSkip(reason string) {
	selfHostedSkipMu.Lock()
	selfHostedLastSkip = reason
	selfHostedSkipMu.Unlock()
}

// Accepts https endpoint urls and http only on loopback hosts, a ProviderError otherwise
func requireHTTPSOrLoopback(rawURL, field string) error {
	parsed, err := pyurl.Parse(rawURL)
	if err != nil {
		return err
	}
	if parsed.Scheme == "https" || (parsed.Scheme == "http" && slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, parsed.Hostname())) {
		return nil
	}
	return &gateway.ProviderError{Msg: fmt.Sprintf("OIDC %s must be https:// (or http on localhost), got %s", field, pyrepr.Repr(rawURL))}
}

// Scheme, host and port of an origin compare with the scheme default port filled in
type oidcOrigin struct {
	scheme, host string
	port         int
}

func originOf(rawURL string) (oidcOrigin, error) {
	parts, err := pyurl.Parse(rawURL)
	if err != nil {
		return oidcOrigin{}, err
	}
	scheme := strings.ToLower(parts.Scheme)
	port, _, err := parts.Port()
	if err != nil {
		return oidcOrigin{}, err
	}
	if port == 0 {
		port = map[string]int{"https": 443, "http": 80}[scheme]
	}
	return oidcOrigin{scheme: scheme, host: strings.ToLower(parts.Hostname()), port: port}, nil
}

// Endpoints and client auth methods of a validated OIDC discovery document
type oidcDiscovery struct {
	issuer                string
	authorizationEndpoint string
	tokenEndpoint         string
	jwksURI               string
	revocationEndpoint    string
	authMethods           []string
}

// Generic self hosted OpenID Connect provider with authorization code plus PKCE and ID token sessions
type SelfHostedOIDCProvider struct {
	jwtOAuthProvider
	issuer             string
	scopes             string
	idTokenLeeway      float64
	clientSecret       string
	discoveryMu        sync.Mutex
	discovery          *oidcDiscovery
	discoveryFetchedAt float64
}

// Provider for an https or loopback issuer and a client id, discovery and keys resolved on first use
func NewSelfHostedOIDCProvider(issuer, clientID, scopes, clientSecret string, idTokenLeeway any) (*SelfHostedOIDCProvider, error) {
	if issuer == "" {
		return nil, &gateway.DashboardAuthValueError{Msg: "issuer is required"}
	}
	if clientID == "" {
		return nil, &gateway.DashboardAuthValueError{Msg: "client_id is required"}
	}
	p := &SelfHostedOIDCProvider{issuer: strings.TrimRight(issuer, "/")}
	if err := requireHTTPSOrLoopback(p.issuer, "issuer"); err != nil {
		var providerErr *gateway.ProviderError
		if errors.As(err, &providerErr) {
			return nil, err
		}
		return nil, &gateway.DashboardAuthValueError{Msg: err.Error()}
	}
	p.jwtOAuthProvider = jwtOAuthProvider{steps: p, clientID: clientID}
	p.scopes = pystr.Strip(scopes)
	if p.scopes == "" {
		p.scopes = selfHostedDefaultScopes
	}
	p.idTokenLeeway = parseLeeway(idTokenLeeway)
	p.clientSecret = pystr.Strip(clientSecret)
	return p, nil
}

// Default object repr of the provider class, never its fields
func (p *SelfHostedOIDCProvider) PyRepr() string {
	return fmt.Sprintf("<plugins.dashboard_auth.self_hosted.SelfHostedOIDCProvider object at %p>", p)
}

// Same default object form for fmt verbs so the client secret never reaches a formatted log line
func (p *SelfHostedOIDCProvider) String() string { return p.PyRepr() }

func (p *SelfHostedOIDCProvider) Name() string { return "self-hosted" }

func (p *SelfHostedOIDCProvider) DisplayName() string { return "Self-Hosted OIDC" }

// Authorize redirect of the discovered endpoint, the redirect uri checked before discovery
func (p *SelfHostedOIDCProvider) StartLogin(_ context.Context, redirectURI string) (gateway.LoginStart, error) {
	if err := validateRedirectURI(redirectURI); err != nil {
		return gateway.LoginStart{}, err
	}
	disco, err := p.getDiscovery()
	if err != nil {
		return gateway.LoginStart{}, err
	}
	return pkceLoginStart(disco.authorizationEndpoint, p.clientID, p.scopes, redirectURI), nil
}

// RFC 7009 revocation at an advertised endpoint, every failure logged and ignored
func (p *SelfHostedOIDCProvider) RevokeSession(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	disco, err := p.getDiscovery()
	var providerErr *gateway.ProviderError
	if errors.As(err, &providerErr) {
		return nil
	}
	if err != nil {
		return err
	}
	endpoint := pystr.Strip(disco.revocationEndpoint)
	if endpoint == "" {
		return nil
	}
	extraData, extraHeaders := p.tokenEndpointAuth(disco)
	data := append([][2]string{{"token", refreshToken}, {"token_type_hint", "refresh_token"}, {"client_id", p.clientID}}, extraData...)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(pyurl.URLEncode(data)))
	if err == nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for _, h := range append(append([][2]string{}, jsonHeaders...), extraHeaders...) {
			req.Header.Set(h[0], h[1])
		}
		_, err = pyhttpx.Do(tokenHTTP, req, tokenEndpointTimeout)
	}
	if err != nil {
		selfHostedLog().Debug("self-hosted OIDC: revoke failed (ignored): " + pyhttpx.ErrorText(err))
	}
	return nil
}

// Client auth of the token endpoint, nothing for a public client, a body secret or Basic header otherwise
func (p *SelfHostedOIDCProvider) tokenEndpointAuth(disco *oidcDiscovery) ([][2]string, [][2]string) {
	if p.clientSecret == "" {
		return nil, nil
	}
	if slices.Contains(disco.authMethods, "client_secret_post") && !slices.Contains(disco.authMethods, "client_secret_basic") {
		return [][2]string{{"client_secret", p.clientSecret}}, nil
	}
	userpass := pyurl.Quote(p.clientID, "") + ":" + pyurl.Quote(p.clientSecret, "")
	return nil, [][2]string{{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(userpass))}}
}

// Refresh grant re-requesting the configured scopes so the rotated ID token keeps its identity claims
func (p *SelfHostedOIDCProvider) refreshRequest(refreshToken string) ([][2]string, [][2]string) {
	return [][2]string{{"grant_type", "refresh_token"}, {"client_id", p.clientID}, {"refresh_token", refreshToken},
		{"scope", p.scopes}}, nil
}

// Exchanges a grant at the discovered token endpoint with client auth and verifies the returned ID token
func (p *SelfHostedOIDCProvider) grant(ctx context.Context, data [][2]string, badRequest func(string) error, _ [][2]string, previousRefreshToken string) (*gateway.DashboardSession, error) {
	disco, err := p.getDiscovery()
	if err != nil {
		return nil, err
	}
	extraData, extraHeaders := p.tokenEndpointAuth(disco)
	idToken, payload, err := exchangeToken(ctx, tokenGrant{url: disco.tokenEndpoint, data: append(append([][2]string{}, data...), extraData...),
		headers: extraHeaders, badRequest: badRequest, idp: "IDP", endpoint: "OIDC token endpoint", tokenKey: "id_token",
		missingMsg: "OIDC token response missing id_token — ensure the 'openid' " +
			"scope is configured and the client is allowed to receive an " +
			"ID token."})
	if err != nil {
		return nil, err
	}
	claims, err := p.verifyIDToken(idToken)
	if err != nil {
		return nil, err
	}
	return p.session(idToken, refreshTokenFrom(payload, previousRefreshToken), claims)
}

// Cached discovery document while younger than the cache ttl
func (p *SelfHostedOIDCProvider) freshDiscovery() *oidcDiscovery {
	if p.discovery != nil && float64(selfHostedClock().UnixNano())/1e9-p.discoveryFetchedAt < selfHostedDiscoveryTTLSec {
		return p.discovery
	}
	return nil
}

// Cached discovery document, fetched under the lock when stale with the JWKS client rebound
func (p *SelfHostedOIDCProvider) getDiscovery() (*oidcDiscovery, error) {
	p.discoveryMu.Lock()
	defer p.discoveryMu.Unlock()
	if disco := p.freshDiscovery(); disco != nil {
		return disco, nil
	}
	disco, err := p.fetchDiscovery()
	if err != nil {
		return nil, err
	}
	p.discovery = disco
	p.discoveryFetchedAt = float64(selfHostedClock().UnixNano()) / 1e9
	p.setJWKSClient(nil)
	return disco, nil
}

// Discovery document of the issuer served from the issuer's own origin, its endpoints and issuer pinned
func (p *SelfHostedOIDCProvider) fetchDiscovery() (*oidcDiscovery, error) {
	url := p.issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	var response *pyhttpx.Response
	if err == nil {
		for _, h := range jsonHeaders {
			req.Header.Set(h[0], h[1])
		}
		response, err = pyhttpx.Do(selfHostedDiscoveryHTTP, req, selfHostedDiscoveryTimeout)
	}
	if err != nil {
		return nil, &gateway.ProviderError{Msg: "OIDC discovery unreachable: " + pyhttpx.ErrorText(err)}
	}
	if response.StatusCode != 200 {
		return nil, &gateway.ProviderError{Msg: fmt.Sprintf("OIDC discovery returned %d for %s", response.StatusCode, pyrepr.Repr(url))}
	}
	served, err := originOf(response.URL)
	if err != nil {
		return nil, err
	}
	configured, err := originOf(p.issuer)
	if err != nil {
		return nil, err
	}
	if served != configured {
		return nil, &gateway.ProviderError{Msg: fmt.Sprintf("OIDC discovery resolved to %s, outside the configured "+
			"issuer's origin (%s)", response.URL, pyrepr.Repr(p.issuer))}
	}
	payload := parseJSONBody(response)
	if payload.Len() == 0 {
		return nil, &gateway.ProviderError{Msg: "OIDC discovery returned a non-JSON body"}
	}
	field := func(key string) string {
		v := payload.GetOr(key, "")
		if !pyrepr.Truthy(v) {
			return ""
		}
		return pystr.Strip(pyrepr.Str(v))
	}
	disco := &oidcDiscovery{authorizationEndpoint: field("authorization_endpoint"), tokenEndpoint: field("token_endpoint"),
		jwksURI: field("jwks_uri")}
	endpoints := [][2]string{{"authorization_endpoint", disco.authorizationEndpoint}, {"token_endpoint", disco.tokenEndpoint},
		{"jwks_uri", disco.jwksURI}}
	for _, e := range endpoints {
		if e[1] == "" {
			return nil, &gateway.ProviderError{Msg: "OIDC discovery missing one of authorization_endpoint / token_endpoint / jwks_uri"}
		}
	}
	advertised := field("issuer")
	if advertised != "" && strings.TrimRight(advertised, "/") != p.issuer {
		return nil, &gateway.ProviderError{Msg: fmt.Sprintf("OIDC discovery issuer mismatch: document advertises %s "+
			"but configured issuer is %s", pyrepr.Repr(advertised), pyrepr.Repr(p.issuer))}
	}
	for _, e := range endpoints {
		if err := requireHTTPSOrLoopback(e[1], e[0]); err != nil {
			return nil, err
		}
	}
	disco.issuer = advertised
	if disco.issuer == "" {
		disco.issuer = p.issuer
	}
	disco.revocationEndpoint = field("revocation_endpoint")
	disco.authMethods = []string{}
	if methods, isList := payload.GetOr("token_endpoint_auth_methods_supported", nil).([]any); isList {
		for _, m := range methods {
			disco.authMethods = append(disco.authMethods, pyrepr.Str(m))
		}
	}
	return disco, nil
}

func (p *SelfHostedOIDCProvider) jwksURI() (string, error) {
	disco, err := p.getDiscovery()
	if err != nil {
		return "", err
	}
	return disco.jwksURI, nil
}

// Claims of an ID token verified against the discovered issuer and keys with the configured clock skew leeway
func (p *SelfHostedOIDCProvider) verifyIDToken(idToken string) (*pyjson.Object, error) {
	disco, err := p.getDiscovery()
	if err != nil {
		return nil, err
	}
	client, err := p.getJWKSClient()
	if err != nil {
		return nil, err
	}
	return verifyJWT(idToken, client, jwtCheck{algorithms: selfHostedAllowedIDTokenAlgs, audience: p.clientID,
		issuer: disco.issuer, label: "ID token", leeway: p.idTokenLeeway})
}

func (p *SelfHostedOIDCProvider) claimsFor(_ context.Context, token string) (*pyjson.Object, error) {
	return p.verifyIDToken(token)
}

// Session of verified OIDC claims storing the ID token as the access token, org from common spellings or groups
func (p *SelfHostedOIDCProvider) session(idToken, refreshToken string, claims *pyjson.Object) (*gateway.DashboardSession, error) {
	truthyStr := func(keys ...string) string {
		for _, k := range keys {
			if v := claims.GetOr(k, nil); pyrepr.Truthy(v) {
				return pyrepr.Str(v)
			}
		}
		return ""
	}
	email := truthyStr("email")
	orgID := truthyStr("org_id", "organization")
	if groups, isList := claims.GetOr("groups", nil).([]any); orgID == "" && isList && len(groups) > 0 {
		names := make([]string, len(groups))
		for i, g := range groups {
			names[i] = pyrepr.Str(g)
		}
		orgID = strings.Join(names, ",")
	}
	displayName := truthyStr("name", "preferred_username", "nickname")
	if displayName == "" {
		displayName = email
	}
	return sessionFromClaims(p.Name(), claims, idToken, refreshToken, sessionExtras{label: "ID token", email: email,
		displayName: displayName, orgID: orgID})
}

// Mapping under dashboard.oauth.self_hosted of the active config, empty when absent or unloadable
func loadConfigOAuthSection(ctx context.Context) map[string]any {
	return loadConfigSection(ctx, selfHostedLog(), selfHostedTag, "dashboard", "oauth", "self_hosted")
}

// Constructor arguments of the self hosted provider resolved from env and config
type selfHostedSettings struct {
	issuer        string
	clientID      string
	scopes        string
	clientSecret  string
	idTokenLeeway float64
}

// Provider settings from env over config, a skip naming both surfaces when issuer or client id is missing
func selfHostedResolveSettings(ctx context.Context) (selfHostedSettings, error) {
	cfg := loadConfigOAuthSection(ctx)
	setting := func(envName, cfgKey string) string { return resolveEnvOrCfg(envName, sectionGet(cfg, cfgKey, nil)) }
	issuer := setting("ICEBATH_DASHBOARD_OIDC_ISSUER", "issuer")
	clientID := setting("ICEBATH_DASHBOARD_OIDC_CLIENT_ID", "client_id")
	if issuer == "" || clientID == "" {
		return selfHostedSettings{}, skipDebug(fmt.Sprintf("Self-hosted OIDC dashboard auth is not configured. Set both an issuer and "+
			"a client_id — either as env vars (ICEBATH_DASHBOARD_OIDC_ISSUER + "+
			"ICEBATH_DASHBOARD_OIDC_CLIENT_ID) or under "+
			"dashboard.oauth.self_hosted.{issuer,client_id} in config.yaml — or pass "+
			"--insecure to skip the OAuth gate entirely. (issuer set: %s; client_id set: %s)",
			pyrepr.Str(issuer != ""), pyrepr.Str(clientID != "")))
	}
	scopes := setting("ICEBATH_DASHBOARD_OIDC_SCOPES", "scopes")
	if scopes == "" {
		scopes = selfHostedDefaultScopes
	}
	return selfHostedSettings{issuer: issuer, clientID: clientID, scopes: scopes,
		clientSecret:  setting("ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET", "client_secret"),
		idTokenLeeway: parseLeeway(sectionGet(cfg, "id_token_leeway", nil))}, nil
}

// Bundled self hosted OpenID Connect dashboard auth plugin
type SelfHostedPlugin struct{}

func (SelfHostedPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name: "self-hosted", Key: "dashboard_auth/self_hosted", Version: "1.0.0", Author: "NousResearch", Kind: "backend",
		Description: "Dashboard auth provider — generic self-hosted OpenID Connect (authorization-code + PKCE, public client). Works against any conformant OIDC identity provider (Authentik, Keycloak, Zitadel, Authelia, Auth0, Okta, Google, …) via OIDC discovery. Auto-activates when an issuer + client_id are configured, either under dashboard.oauth.self_hosted.{issuer,client_id} in config.yaml (canonical surface) or via the ICEBATH_DASHBOARD_OIDC_ISSUER + ICEBATH_DASHBOARD_OIDC_CLIENT_ID env vars (operator override / secret injection). Scopes default to 'openid profile email'. Verifies the OIDC ID token (RS256/ES256) against the discovered jwks_uri.",
		RequiresEnv: []plugins.EnvSpec{{Name: "ICEBATH_DASHBOARD_OIDC_ISSUER"}, {Name: "ICEBATH_DASHBOARD_OIDC_CLIENT_ID"}},
	}
}

// Registers the self hosted provider when an issuer and client id are configured
func (SelfHostedPlugin) Register(pctx *plugins.Context) error {
	ctx := pctx.HomeContext()
	setSelfHostedLastSkip("")
	kw, reason, err := registerProvider(pctx, selfHostedLog(), selfHostedTag, "SelfHostedOIDCProvider",
		func() (selfHostedSettings, error) { return selfHostedResolveSettings(ctx) },
		func(s selfHostedSettings) (gateway.DashboardAuthProvider, error) {
			return NewSelfHostedOIDCProvider(s.issuer, s.clientID, s.scopes, s.clientSecret, s.idTokenLeeway)
		})
	setSelfHostedLastSkip(reason)
	if err != nil {
		return err
	}
	if kw != nil {
		selfHostedLog().Info(fmt.Sprintf("dashboard-auth-self-hosted: registered provider "+
			"(issuer=%s, client_id=%s, scopes=%s, confidential=%s, id_token_leeway=%ss)",
			kw.issuer, kw.clientID, pyrepr.Repr(kw.scopes), pyrepr.Str(kw.clientSecret != ""), pyrepr.Str(kw.idTokenLeeway)))
	}
	return nil
}

func init() { plugins.RegisterBundled(SelfHostedPlugin{}) }
