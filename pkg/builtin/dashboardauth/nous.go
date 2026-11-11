package dashboardauth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	nousTag                       = "dashboard-auth-nous"
	nousDefaultPortalURL          = "https://portal.nousresearch.com"
	nousScope                     = "agent_dashboard:access"
	nousExpectedContractVersion   = 1
	nousProviderName              = "nous"
	nousProviderDisplayName       = "Nous Research"
	nousProviderClassName         = "NousDashboardAuthProvider"
	nousOAuthClientIDEnv          = "ICEBATH_DASHBOARD_OAUTH_CLIENT_ID"
	nousPortalURLEnv              = "ICEBATH_DASHBOARD_PORTAL_URL"
	nousMissingClientIDSkipReason = nousOAuthClientIDEnv + " is not set (and dashboard.oauth.client_id " +
		"in config.yaml is empty). The Nous Portal provisions this env var (shape " +
		"'agent:{instance_id}') when it deploys a Hermes Agent instance — set it to " +
		"your provisioned client id (either as an env var or under " +
		"dashboard.oauth.client_id in config.yaml), or pass --insecure to skip the " +
		"OAuth gate entirely."
)

var (
	nousSkipMu   sync.Mutex
	nousLastSkip string
)

// Reason the nous provider last declined to register, empty after a registration
func NousLastSkipReason() string {
	nousSkipMu.Lock()
	defer nousSkipMu.Unlock()
	return nousLastSkip
}

func setNousLastSkip(reason string) {
	nousSkipMu.Lock()
	nousLastSkip = reason
	nousSkipMu.Unlock()
}

// Constructor arguments of the nous provider
type nousSettings struct {
	clientID    string
	portalURL   string
	tokenLeeway float64
}

// Nous Portal OAuth through authorization code plus PKCE with RS256 access tokens
type NousDashboardAuthProvider struct {
	jwtOAuthProvider
	agentInstanceID string
	portalURL       string
	tokenLeeway     float64
	jwksURL         string
	authorizeURL    string
	tokenURL        string
}

// Provider for an agent client id, a ValueError for a client id outside the contract shape
func NewNousDashboardAuthProvider(clientID, portalURL string, tokenLeeway any) (*NousDashboardAuthProvider, error) {
	if !strings.HasPrefix(clientID, "agent:") {
		return nil, &gateway.DashboardAuthValueError{Msg: "client_id must match contract shape 'agent:{instance_id}', got " + pyrepr.Repr(clientID)}
	}
	p := &NousDashboardAuthProvider{agentInstanceID: clientID[len("agent:"):], portalURL: strings.TrimRight(portalURL, "/"),
		tokenLeeway: parseLeeway(tokenLeeway)}
	p.jwtOAuthProvider = jwtOAuthProvider{steps: p, clientID: clientID}
	p.jwksURL = p.portalURL + "/.well-known/jwks.json"
	p.authorizeURL = p.portalURL + "/oauth/authorize"
	p.tokenURL = p.portalURL + "/api/oauth/token"
	return p, nil
}

func (p *NousDashboardAuthProvider) Name() string { return nousProviderName }

func (p *NousDashboardAuthProvider) DisplayName() string { return nousProviderDisplayName }

func (p *NousDashboardAuthProvider) StartLogin(_ context.Context, redirectURI string) (gateway.LoginStart, error) {
	if err := validateRedirectURI(redirectURI); err != nil {
		return gateway.LoginStart{}, err
	}
	return pkceLoginStart(p.authorizeURL, p.clientID, nousScope, redirectURI), nil
}

// Portal has no revocation grant, logout clears cookies and the refresh token lapses within its day
func (p *NousDashboardAuthProvider) RevokeSession(context.Context, string) error { return nil }

func (p *NousDashboardAuthProvider) jwksURI() (string, error) { return p.jwksURL, nil }

// Refresh grant carrying the refresh token in the body and the x-nous-refresh-token header
func (p *NousDashboardAuthProvider) refreshRequest(refreshToken string) ([][2]string, [][2]string) {
	return [][2]string{{"grant_type", "refresh_token"}, {"client_id", p.clientID}, {"refresh_token", refreshToken}},
		[][2]string{{"x-nous-refresh-token", refreshToken}}
}

// Exchanges a grant at the Portal token endpoint for a session with the rotated refresh token
func (p *NousDashboardAuthProvider) grant(ctx context.Context, data [][2]string, badRequest func(string) error, headers [][2]string, _ string) (*gateway.DashboardSession, error) {
	accessToken, payload, err := exchangeToken(ctx, tokenGrant{url: p.tokenURL, data: data, headers: headers, badRequest: badRequest,
		idp: "Portal", endpoint: "Portal token endpoint", tokenKey: "access_token", missingMsg: "Portal token response missing access_token"})
	if err != nil {
		return nil, err
	}
	claims, err := p.claimsFor(ctx, accessToken)
	if err != nil {
		return nil, err
	}
	return p.session(accessToken, refreshTokenFrom(payload, ""), claims)
}

// Verified access token claims bound to this agent instance and contract version
func (p *NousDashboardAuthProvider) claimsFor(_ context.Context, accessToken string) (*pyjson.Object, error) {
	client, err := p.getJWKSClient()
	if err != nil {
		return nil, err
	}
	claims, err := verifyJWT(accessToken, client, jwtCheck{algorithms: []string{"RS256"}, audience: p.clientID,
		issuer: p.portalURL, label: "access token", leeway: p.tokenLeeway})
	if err != nil {
		return nil, err
	}
	if instance := claims.GetOr("agent_instance_id", nil); instance != nil && !pyEqual(instance, p.agentInstanceID) {
		return nil, &gateway.ProviderError{Msg: fmt.Sprintf("agent_instance_id mismatch: token=%s vs configured=%s",
			pyrepr.Repr(instance), pyrepr.Repr(p.agentInstanceID))}
	}
	version := claims.GetOr("oauth_contract_version", nil)
	if version == nil {
		nousLog().Warn(fmt.Sprintf("Nous Portal token missing oauth_contract_version claim (contract says it should be %d); proceeding anyway.",
			nousExpectedContractVersion))
	} else if !pyEqual(version, nousExpectedContractVersion) {
		return nil, &gateway.ProviderError{Msg: fmt.Sprintf("unsupported oauth_contract_version=%s, expected %d",
			pyrepr.Repr(version), nousExpectedContractVersion)}
	}
	return claims, nil
}

// Session of the claims with the org id, the tokens carrying no email or display name
func (p *NousDashboardAuthProvider) session(accessToken, refreshToken string, claims *pyjson.Object) (*gateway.DashboardSession, error) {
	orgID := ""
	if org := claims.GetOr("org_id", nil); pyrepr.Truthy(org) {
		orgID = pyrepr.Str(org)
	}
	return sessionFromClaims(nousProviderName, claims, accessToken, refreshToken, sessionExtras{orgID: orgID})
}

// Python equality of a decoded claim with a Go string or int
func pyEqual(claim any, want any) bool {
	switch w := want.(type) {
	case string:
		s, ok := claim.(string)
		return ok && s == w
	case int:
		if n, ok := pyrepr.Int(claim); ok {
			return n.IsInt64() && n.Int64() == int64(w)
		}
		if f, ok := claim.(float64); ok {
			return f == float64(w)
		}
		if b, ok := claim.(bool); ok {
			return (b && w == 1) || (!b && w == 0)
		}
	}
	return false
}

// Nous provider arguments from the listener's own client or the configured client id
func nousSettingsFor(ctx context.Context) (nousSettings, error) {
	section := loadConfigSection(ctx, nousLog(), nousTag, "dashboard", "oauth")
	if own := gateway.ListenerClientCurrent(); own != nil {
		return nousSettings{clientID: own.ClientID, portalURL: own.PortalURL, tokenLeeway: parseLeeway(section["token_leeway"])}, nil
	}
	clientID := resolveEnvOrCfg(nousOAuthClientIDEnv, sectionGet(section, "client_id", ""))
	portalURL := resolveEnvOrCfg(nousPortalURLEnv, sectionGet(section, "portal_url", ""))
	if portalURL == "" {
		portalURL = nousDefaultPortalURL
	}
	if clientID == "" {
		return nousSettings{}, skipDebug(nousMissingClientIDSkipReason)
	}
	if !strings.HasPrefix(clientID, "agent:") {
		return nousSettings{}, skipAt("warning", fmt.Sprintf("%s=%s doesn't match the contract "+
			"shape 'agent:{instance_id}'. The Nous Portal provisions this value at deploy "+
			"time; check your Fly app's secrets or override with the value from the Portal admin UI.",
			nousOAuthClientIDEnv, pyrepr.Repr(clientID)))
	}
	return nousSettings{clientID: clientID, portalURL: portalURL, tokenLeeway: parseLeeway(section["token_leeway"])}, nil
}

// Value of a config section key, the default when absent
func sectionGet(section map[string]any, key string, def any) any {
	if v, ok := section[key]; ok {
		return v
	}
	return def
}

func nousLog() *slog.Logger { return slog.Default().With("logger", "plugins.dashboard_auth.nous") }

func newNousFromSettings(s nousSettings) (gateway.DashboardAuthProvider, error) {
	return NewNousDashboardAuthProvider(s.clientID, s.portalURL, s.tokenLeeway)
}

// Puts a provider on the listener's own client in place of the registered one, the undo restoring that one
func nousSwapProvider(ctx context.Context) func() {
	previous := gateway.SnapshotDashboardAuthRegistration(nousProviderName, "")
	settings, err := nousSettingsFor(ctx)
	if err != nil {
		panic(err)
	}
	provider, err := newNousFromSettings(settings)
	if err != nil {
		panic(err)
	}
	if err := gateway.RegisterGlobalDashboardAuthProvider(provider); err != nil {
		panic(err)
	}
	return func() { gateway.RestoreDashboardAuthRegistration(nousProviderName, provider, previous, "") }
}

// Bundled nous dashboard auth plugin
type NousPlugin struct{}

func (NousPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name: "nous", Key: "dashboard_auth/nous", Version: "1.0.0", Author: "NousResearch", Kind: "backend",
		Description: "Dashboard auth provider — OAuth 2.0 (authorization-code + PKCE) against Nous Portal. Auto-activates when a client_id is configured via either dashboard.oauth.client_id in config.yaml (canonical surface) or ICEBATH_DASHBOARD_OAUTH_CLIENT_ID env var (operator override; Portal injects this at Fly.io provisioning). dashboard.oauth.portal_url / ICEBATH_DASHBOARD_PORTAL_URL are optional and default to https://portal.nousresearch.com.",
		RequiresEnv: []plugins.EnvSpec{{Name: nousOAuthClientIDEnv}},
	}
}

// Registers the nous provider when a client id is configured, recording why it declined otherwise
func (NousPlugin) Register(pctx *plugins.Context) error {
	ctx := pctx.HomeContext()
	gateway.ListenerClientOffer(func() func() { return nousSwapProvider(ctx) })
	setNousLastSkip("")
	kwargs, reason, err := registerProvider(pctx, nousLog(), nousTag, nousProviderClassName,
		func() (nousSettings, error) { return nousSettingsFor(ctx) }, newNousFromSettings)
	setNousLastSkip(reason)
	if err != nil {
		return err
	}
	if kwargs != nil {
		nousLog().Info(fmt.Sprintf("dashboard-auth-nous: registered provider (client_id=%s, portal=%s, token_leeway=%ss)",
			kwargs.clientID, kwargs.portalURL, pyrepr.Float(kwargs.tokenLeeway)))
	}
	return nil
}

func init() { plugins.RegisterBundled(NousPlugin{}) }
