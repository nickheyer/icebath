package dashboardauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyjwt"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	jwksCacheSeconds          = 300
	tokenEndpointTimeout      = 10 * time.Second
	DefaultTokenLeewaySeconds = 60.0
)

var jsonHeaders = [][2]string{{"Accept", "application/json"}}

// HTTP client of the token endpoint posts, redirects never followed
var tokenHTTP = pyhttpx.PlainClient(false)

// Config block at path as a map, empty when the config cannot load or the value is not a mapping
func loadConfigSection(ctx context.Context, logger *slog.Logger, tag string, path ...string) map[string]any {
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), config.EnvRefLookupContext(ctx))
	if err != nil {
		logger.Debug(fmt.Sprintf("%s: load_config() raised %s; falling back to env-only configuration", tag, err))
		return map[string]any{}
	}
	section, _ := config.CfgGet(map[string]any(cfg), path...)
	if m, ok := section.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Env value when set and not blank, the stripped config value otherwise
func resolveEnvOrCfg(envName string, cfgValue any) string {
	if v := pystr.Strip(os.Getenv(envName)); v != "" {
		return v
	}
	if !pyrepr.Truthy(cfgValue) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(cfgValue))
}

// Clock skew tolerance in seconds, the default when unset and zero for anything not a finite non negative number
func parseLeeway(raw any) float64 {
	if raw == nil {
		return DefaultTokenLeewaySeconds
	}
	text := pystr.Strip(pyrepr.Str(raw))
	if text == "" {
		return DefaultTokenLeewaySeconds
	}
	value, err := pyint.ParseFloat(text)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
		return 0
	}
	return value
}

// Declined registration with the operator facing reason and the log level it is reported at
type skipRegistration struct {
	reason string
	level  string
}

func (s *skipRegistration) Error() string { return s.reason }

func skipDebug(reason string) error { return &skipRegistration{reason: reason, level: "debug"} }

func skipAt(level, reason string) error { return &skipRegistration{reason: reason, level: level} }

func logAt(logger *slog.Logger, level, msg string) {
	switch level {
	case "info":
		logger.Info(msg)
	case "warning":
		logger.Warn(msg)
	case "error":
		logger.Error(msg)
	default:
		logger.Debug(msg)
	}
}

// Builds the provider from its settings and registers it, the skip reason returned when settings or construction declined
func registerProvider[S any](pctx *plugins.Context, logger *slog.Logger, tag, className string, settings func() (S, error), construct func(S) (gateway.DashboardAuthProvider, error)) (*S, string, error) {
	kwargs, err := settings()
	var provider gateway.DashboardAuthProvider
	if err == nil {
		provider, err = construct(kwargs)
	}
	var skip *skipRegistration
	var valueErr *gateway.DashboardAuthValueError
	var providerErr *gateway.ProviderError
	switch {
	case errors.As(err, &skip):
		logAt(logger, skip.level, fmt.Sprintf("%s: %s", tag, skip.reason))
		return nil, skip.reason, nil
	case errors.As(err, &valueErr), errors.As(err, &providerErr):
		reason := fmt.Sprintf("%s construction failed: %s", className, err.Error())
		logger.Warn(fmt.Sprintf("%s: %s", tag, reason))
		return nil, reason, nil
	case err != nil:
		return nil, "", err
	}
	if _, err := pctx.RegisterDashboardAuthProvider(provider); err != nil {
		return nil, "", err
	}
	return &kwargs, "", nil
}

// Base64url without padding
func b64urlNoPad(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func tokenBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// Refuses a redirect uri that is not http or https or whose path does not end in the auth callback
func validateRedirectURI(redirectURI string) error {
	parsed, err := pyurl.Parse(redirectURI)
	if err != nil {
		return &gateway.DashboardAuthValueError{Msg: err.Error()}
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return &gateway.ProviderError{Msg: "redirect_uri must be http(s), got " + pyrepr.Repr(redirectURI)}
	}
	if parsed.Path == "" || !strings.HasSuffix(parsed.Path, "/auth/callback") {
		return &gateway.ProviderError{Msg: "redirect_uri path must end with '/auth/callback', got " + pyrepr.Repr(redirectURI)}
	}
	return nil
}

// Authorization code plus PKCE S256 redirect and the flat state and verifier cookie payload
func pkceLoginStart(authorizeURL, clientID, scope, redirectURI string) gateway.LoginStart {
	codeVerifier := b64urlNoPad(tokenBytes(64))
	state := b64urlNoPad(tokenBytes(32))
	challenge := sha256.Sum256([]byte(codeVerifier))
	params := [][2]string{
		{"response_type", "code"}, {"client_id", clientID}, {"redirect_uri", redirectURI}, {"scope", scope}, {"state", state},
		{"code_challenge", b64urlNoPad(challenge[:])}, {"code_challenge_method", "S256"},
	}
	return gateway.LoginStart{
		RedirectURL:   authorizeURL + "?" + pyurl.URLEncode(params),
		CookiePayload: map[string]string{"icebath_session_pkce": "state=" + state + ";verifier=" + codeVerifier},
	}
}

// JSON object body, empty for another content type, a parse error or a non object
func parseJSONBody(response *pyhttpx.Response) *pyjson.Object {
	if !strings.HasPrefix(pyhttpx.HeaderGet(response.Header, "content-type", ""), "application/json") {
		return pyjson.NewObject()
	}
	body, err := response.JSON()
	if obj, ok := body.(*pyjson.Object); err == nil && ok {
		return obj
	}
	return pyjson.NewObject()
}

// Token grant request of exchangeToken
type tokenGrant struct {
	url        string
	data       [][2]string
	headers    [][2]string
	badRequest func(msg string) error
	idp        string
	endpoint   string
	tokenKey   string
	missingMsg string
}

// Posts a token grant and returns the token and payload, a 400 raising the grant's own refusal
func exchangeToken(ctx context.Context, g tokenGrant) (string, *pyjson.Object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url, strings.NewReader(pyurl.URLEncode(g.data)))
	if err != nil {
		return "", nil, &gateway.ProviderError{Msg: fmt.Sprintf("%s unreachable: %s", g.endpoint, pyhttpx.ErrorText(err))}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, h := range append(append([][2]string{}, jsonHeaders...), g.headers...) {
		req.Header.Set(h[0], h[1])
	}
	response, err := pyhttpx.Do(tokenHTTP, req, tokenEndpointTimeout)
	if err != nil {
		return "", nil, &gateway.ProviderError{Msg: fmt.Sprintf("%s unreachable: %s", g.endpoint, pyhttpx.ErrorText(err))}
	}
	if response.StatusCode == 400 {
		code := parseJSONBody(response).GetOr("error", "invalid_request")
		return "", nil, g.badRequest(fmt.Sprintf("%s rejected token request: %s", g.idp, pyrepr.Str(code)))
	}
	if response.StatusCode != 200 {
		text := []rune(response.Text())
		return "", nil, &gateway.ProviderError{Msg: fmt.Sprintf("%s returned %d: %s", g.endpoint, response.StatusCode, pyrepr.Repr(string(text[:min(len(text), 200)])))}
	}
	payload := parseJSONBody(response)
	token, _ := payload.GetOr(g.tokenKey, nil).(string)
	if token == "" {
		return "", nil, &gateway.ProviderError{Msg: g.missingMsg}
	}
	tokenType := strings.ToLower(pyrepr.Str(payload.GetOr("token_type", "")))
	if tokenType != "" && tokenType != "bearer" {
		return "", nil, &gateway.ProviderError{Msg: "unexpected token_type=" + pyrepr.Repr(tokenType)}
	}
	return token, payload, nil
}

// Refresh token of a token response, the fallback when absent or not a string
func refreshTokenFrom(payload *pyjson.Object, fallback string) string {
	if rt, ok := payload.GetOr("refresh_token", nil).(string); ok && rt != "" {
		return rt
	}
	return fallback
}

// Session fields that come from the provider rather than the claims
type sessionExtras struct {
	label       string
	email       string
	displayName string
	orgID       string
}

// Session of verified claims, an error when the sub claim is missing
func sessionFromClaims(provider string, claims *pyjson.Object, accessToken, refreshToken string, x sessionExtras) (*gateway.DashboardSession, error) {
	if x.label == "" {
		x.label = "token"
	}
	userID := pyrepr.Str(claims.GetOr("sub", ""))
	if userID == "" {
		return nil, &gateway.ProviderError{Msg: x.label + " missing 'sub' (user_id) claim"}
	}
	exp, ok := pyrepr.Int(claims.GetOr("exp", nil))
	if !ok {
		f, isFloat := claims.GetOr("exp", nil).(float64)
		if !isFloat {
			return nil, &gateway.DashboardAuthTypeError{Msg: "int() argument must be a string, a bytes-like object or a real number"}
		}
		exp, _ = pyrepr.Int(int64(f))
	}
	issuer := ""
	if iss := claims.GetOr("iss", nil); pyrepr.Truthy(iss) {
		issuer = pyrepr.Str(iss)
	}
	return &gateway.DashboardSession{
		UserID: userID, Email: x.email, DisplayName: x.displayName, OrgID: x.orgID, Provider: provider,
		ExpiresAt: exp.Int64(), AccessToken: accessToken, RefreshToken: refreshToken, Issuer: issuer,
	}, nil
}

// JWKS client with explicit Accept and User-Agent headers and the five minute key cache
func makeJWKSClient(jwksURL string) *pyjwt.JWKClient {
	client := pyjwt.NewJWKClient(jwksURL, map[string]string{"Accept": "application/json", "User-Agent": "IcebathAgent/1.0"})
	client.Lifespan = jwksCacheSeconds * time.Second
	return client
}

// JWT expectations of verifyJWT
type jwtCheck struct {
	algorithms []string
	audience   string
	issuer     string
	label      string
	leeway     float64
}

// Claims of a token verified against the JWKS with pinned audience and issuer, token failures as InvalidCodeError
func verifyJWT(token string, client *pyjwt.JWKClient, c jwtCheck) (*pyjson.Object, error) {
	key, err := client.SigningKeyFromJWT(token)
	if err != nil {
		return nil, gateway.ClassifyJWKSLookupError(err)
	}
	claims, err := pyjwt.Decode(token, key.Key, pyjwt.DecodeOptions{
		Algorithms: c.algorithms, Audience: c.audience, Issuer: c.issuer,
		Leeway: time.Duration(c.leeway * float64(time.Second)), Require: []string{"exp", "iat", "aud", "iss", "sub"},
	})
	switch {
	case err == nil:
		return claims, nil
	case pyjwt.IsInstance(err, "ExpiredSignatureError"):
		return nil, &gateway.InvalidCodeError{Msg: fmt.Sprintf("%s expired: %s", c.label, err)}
	case pyjwt.IsInstance(err, "InvalidSignatureError"):
		return nil, &gateway.InvalidCodeError{Msg: fmt.Sprintf("%s signature invalid: %s", c.label, err)}
	case pyjwt.IsInstance(err, "ImmatureSignatureError"):
		return nil, &gateway.InvalidCodeError{Msg: fmt.Sprintf("%s not yet valid: %s", c.label, err)}
	case pyjwt.IsInstance(err, "InvalidTokenError"):
		details := ""
		if unverified, uerr := pyjwt.UnverifiedClaims(token); uerr == nil {
			details = fmt.Sprintf(" [token iss=%s aud=%s; expected iss=%s aud=%s]", pyrepr.Repr(unverified.GetOr("iss", nil)),
				pyrepr.Repr(unverified.GetOr("aud", nil)), pyrepr.Repr(c.issuer), pyrepr.Repr(c.audience))
		}
		return nil, &gateway.ProviderError{Msg: fmt.Sprintf("%s verification failed: %s%s", c.label, err, details)}
	}
	return nil, err
}

// OAuth redirect refusals of a provider without a browser login flow
type nonInteractive struct {
	notInteractive string
	noStartLogin   string
}

func (n nonInteractive) StartLogin(context.Context, string) (gateway.LoginStart, error) {
	msg := n.noStartLogin
	if msg == "" {
		msg = n.notInteractive
	}
	return gateway.LoginStart{}, &gateway.DashboardAuthNotImplementedError{Msg: msg}
}

func (n nonInteractive) CompleteLogin(context.Context, string, string, string, string) (*gateway.DashboardSession, error) {
	return nil, &gateway.DashboardAuthNotImplementedError{Msg: n.notInteractive}
}

// Provider specific steps of an authorization code plus PKCE provider whose session token is a JWT
type jwtOAuthSteps interface {
	jwksURI() (string, error)
	claimsFor(ctx context.Context, token string) (*pyjson.Object, error)
	grant(ctx context.Context, data [][2]string, badRequest func(string) error, headers [][2]string, previousRefreshToken string) (*gateway.DashboardSession, error)
	refreshRequest(refreshToken string) ([][2]string, [][2]string)
	session(token, refreshToken string, claims *pyjson.Object) (*gateway.DashboardSession, error)
}

// Shared login completion, refresh and verification of the JWT OAuth providers
type jwtOAuthProvider struct {
	steps      jwtOAuthSteps
	clientID   string
	jwksMu     sync.Mutex
	jwksClient *pyjwt.JWKClient
}

// JWKS client bound to the provider's jwks uri, built on first use and after a rebind
func (p *jwtOAuthProvider) getJWKSClient() (*pyjwt.JWKClient, error) {
	p.jwksMu.Lock()
	client := p.jwksClient
	p.jwksMu.Unlock()
	if client != nil {
		return client, nil
	}
	uri, err := p.steps.jwksURI()
	if err != nil {
		return nil, err
	}
	p.jwksMu.Lock()
	defer p.jwksMu.Unlock()
	if p.jwksClient == nil {
		p.jwksClient = makeJWKSClient(uri)
	}
	return p.jwksClient, nil
}

// Replaces the JWKS client, nil making the next verification bind to the freshly discovered jwks uri
func (p *jwtOAuthProvider) setJWKSClient(client *pyjwt.JWKClient) {
	p.jwksMu.Lock()
	p.jwksClient = client
	p.jwksMu.Unlock()
}

func invalidCode(msg string) error { return &gateway.InvalidCodeError{Msg: msg} }

func refreshExpired(msg string) error { return &gateway.RefreshExpiredError{Msg: msg} }

func (p *jwtOAuthProvider) CompleteLogin(ctx context.Context, code, state, codeVerifier, redirectURI string) (*gateway.DashboardSession, error) {
	return p.steps.grant(ctx, [][2]string{{"grant_type", "authorization_code"}, {"code", code}, {"redirect_uri", redirectURI},
		{"client_id", p.clientID}, {"code_verifier", codeVerifier}}, invalidCode, nil, "")
}

func (p *jwtOAuthProvider) RefreshSession(ctx context.Context, refreshToken string) (*gateway.DashboardSession, error) {
	if refreshToken == "" {
		return nil, refreshExpired("no refresh token present in session")
	}
	data, headers := p.steps.refreshRequest(refreshToken)
	return p.steps.grant(ctx, data, refreshExpired, headers, refreshToken)
}

func (p *jwtOAuthProvider) VerifySession(ctx context.Context, accessToken string) (*gateway.DashboardSession, error) {
	claims, err := p.steps.claimsFor(ctx, accessToken)
	var invalid *gateway.InvalidCodeError
	if errors.As(err, &invalid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p.steps.session(accessToken, "", claims)
}
