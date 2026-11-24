package messagetools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	DefaultGraphScope        = "https://graph.microsoft.com/.default"
	DefaultGraphAuthorityURL = "https://login.microsoftonline.com"
	DefaultTokenSkewSeconds  = 120
)

var graphRequiredEnv = []string{"MSGRAPH_TENANT_ID", "MSGRAPH_CLIENT_ID", "MSGRAPH_CLIENT_SECRET"}

// Wall clock seconds since the epoch
var timeNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Microsoft Graph auth failure, the base class of the config and token errors
type MicrosoftGraphAuthError struct {
	Msg string
	Err error
}

func (e *MicrosoftGraphAuthError) Error() string { return e.Msg }
func (e *MicrosoftGraphAuthError) Unwrap() error { return e.Err }

// Graph credentials missing or invalid
type MicrosoftGraphConfigError struct{ MicrosoftGraphAuthError }

// Matches a MicrosoftGraphAuthError target the way except catches a subclass
func (e *MicrosoftGraphConfigError) As(target any) bool {
	return asAuthError(&e.MicrosoftGraphAuthError, target)
}

// Graph token acquisition failure
type MicrosoftGraphTokenError struct{ MicrosoftGraphAuthError }

// Matches a MicrosoftGraphAuthError target the way except catches a subclass
func (e *MicrosoftGraphTokenError) As(target any) bool {
	return asAuthError(&e.MicrosoftGraphAuthError, target)
}

func asAuthError(base *MicrosoftGraphAuthError, target any) bool {
	if t, ok := target.(**MicrosoftGraphAuthError); ok {
		*t = base
		return true
	}
	return false
}

// Key lookup on a decoded JSON object or a plain map, false for any other value
func jsonDict(v any) (func(string) any, bool) {
	switch d := v.(type) {
	case *jsonutil.Object:
		if d == nil {
			return nil, false
		}
		return func(k string) any { x, _ := d.Get(k); return x }, true
	case map[string]any:
		return func(k string) any { return d[k] }, true
	}
	return nil, false
}

// Graph error object or bare string rendered as code colon message, false when unusable
func FormatGraphError(errValue any) (string, bool) {
	if s, ok := errValue.(string); ok {
		return s, true
	}
	get, ok := jsonDict(errValue)
	if !ok {
		return "", false
	}
	code, message := get("code"), get("message")
	if anyutil.IsTrue(code) && anyutil.IsTrue(message) {
		return anyutil.String(code) + ": " + anyutil.String(message), true
	}
	if anyutil.IsTrue(message) {
		return anyutil.String(message), true
	}
	return "", false
}

// Normalized Microsoft Graph app only credentials
type GraphCredentials struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	Scope        string
	AuthorityURL string
}

// OAuth v2 token endpoint of the tenant under the authority
func (c GraphCredentials) TokenURL() string {
	return strings.TrimRight(c.AuthorityURL, "/") + "/" + strings.Trim(strings.TrimSpace(c.TenantID), "/") + "/oauth2/v2.0/token"
}

// Credentials read from the MSGRAPH variables of environ or the process environment, nil when absent and optional
func GraphCredentialsFromEnv(environ map[string]string, required bool) (*GraphCredentials, error) {
	get := func(name string) string {
		if environ != nil {
			return environ[name]
		}
		return os.Getenv(name)
	}
	values := make([]string, len(graphRequiredEnv))
	var missing []string
	for i, name := range graphRequiredEnv {
		values[i] = strings.TrimSpace(get(name))
		if values[i] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 && !required {
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, &MicrosoftGraphConfigError{MicrosoftGraphAuthError{Msg: "Missing Microsoft Graph configuration: " + strings.Join(missing, ", ")}}
	}
	orDefault := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	return &GraphCredentials{TenantID: values[0], ClientID: values[1], ClientSecret: values[2],
		Scope:        strings.TrimSpace(orDefault(get("MSGRAPH_SCOPE"), DefaultGraphScope)),
		AuthorityURL: strings.TrimSpace(orDefault(get("MSGRAPH_AUTHORITY_URL"), DefaultGraphAuthorityURL))}, nil
}

// Cached app only Graph access token
type CachedAccessToken struct {
	AccessToken string
	ExpiresAt   float64
	TokenType   string
}

// Whether the token expires within skewSeconds from now, negative skew counted as zero
func (t *CachedAccessToken) IsExpired(skewSeconds int) bool {
	return t.ExpiresAt <= timeNow()+float64(max(0, skewSeconds))
}

// Whole seconds left before expiry, zero once expired
func (t *CachedAccessToken) ExpiresInSeconds() int {
	return max(0, int(math.Trunc(t.ExpiresAt-timeNow())))
}

// Token provider settings, nil fields take the twenty second timeout, the default skew and the default transport
type MicrosoftGraphTokenProviderOptions struct {
	Timeout     *time.Duration
	SkewSeconds *int
	Transport   http.RoundTripper
}

// Acquires and caches Microsoft Graph app only access tokens
type MicrosoftGraphTokenProvider struct {
	Credentials GraphCredentials
	Timeout     time.Duration
	SkewSeconds int
	transport   http.RoundTripper
	cached      atomic.Pointer[CachedAccessToken]
	lock        chan struct{}
	fetch       func(ctx context.Context) (*CachedAccessToken, error)
}

// Provider for the credentials with the option overrides applied
func NewMicrosoftGraphTokenProvider(creds GraphCredentials, opts ...MicrosoftGraphTokenProviderOptions) *MicrosoftGraphTokenProvider {
	p := &MicrosoftGraphTokenProvider{Credentials: creds, Timeout: 20 * time.Second, SkewSeconds: DefaultTokenSkewSeconds, lock: make(chan struct{}, 1)}
	for _, o := range opts {
		if o.Timeout != nil {
			p.Timeout = *o.Timeout
		}
		if o.SkewSeconds != nil {
			p.SkewSeconds = max(0, *o.SkewSeconds)
		}
		if o.Transport != nil {
			p.transport = o.Transport
		}
	}
	p.fetch = p.fetchAccessToken
	return p
}

// Provider over the required MSGRAPH credentials of environ, nil environ reading the process environment
func MicrosoftGraphTokenProviderFromEnv(environ map[string]string, opts ...MicrosoftGraphTokenProviderOptions) (*MicrosoftGraphTokenProvider, error) {
	creds, err := GraphCredentialsFromEnv(environ, true)
	if err != nil {
		return nil, err
	}
	return NewMicrosoftGraphTokenProvider(*creds, opts...), nil
}

// Drops the cached token so the next call fetches a new one
func (p *MicrosoftGraphTokenProvider) ClearCache() { p.cached.Store(nil) }

// Configuration and cache state report with expiry figures null when nothing is cached
func (p *MicrosoftGraphTokenProvider) InspectTokenHealth() *jsonutil.Object {
	cached, creds := p.cached.Load(), p.Credentials
	out := jsonutil.NewObject()
	out.Set("configured", true)
	out.Set("tenant_id", creds.TenantID)
	out.Set("client_id", creds.ClientID)
	out.Set("scope", creds.Scope)
	out.Set("authority_url", creds.AuthorityURL)
	out.Set("token_url", creds.TokenURL())
	out.Set("cached", cached != nil)
	if cached != nil {
		out.Set("expires_in_seconds", int64(cached.ExpiresInSeconds()))
		out.Set("is_expired", cached.IsExpired(0))
	} else {
		out.Set("expires_in_seconds", nil)
		out.Set("is_expired", nil)
	}
	out.Set("refresh_skew_seconds", int64(p.SkewSeconds))
	return out
}

// Cached token unless it expires within the refresh skew
func (p *MicrosoftGraphTokenProvider) freshCached() *CachedAccessToken {
	if c := p.cached.Load(); c != nil && !c.IsExpired(p.SkewSeconds) {
		return c
	}
	return nil
}

// Access token from the cache or one fetch shared by concurrent callers, forceRefresh always fetching
func (p *MicrosoftGraphTokenProvider) GetAccessToken(ctx context.Context, forceRefresh bool) (string, error) {
	if c := p.freshCached(); !forceRefresh && c != nil {
		return c.AccessToken, nil
	}
	select {
	case p.lock <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-p.lock }()
	if c := p.freshCached(); !forceRefresh && c != nil {
		return c.AccessToken, nil
	}
	tok, err := p.fetch(ctx)
	if err != nil {
		return "", err
	}
	p.cached.Store(tok)
	return tok.AccessToken, nil
}

func tokenError(msg string, cause error) error {
	return &MicrosoftGraphTokenError{MicrosoftGraphAuthError{Msg: msg, Err: cause}}
}

// One fresh httpx style client per call, closed when the call ends
func freshHTTPClient(transport http.RoundTripper) (*http.Client, func()) {
	if transport != nil {
		return &http.Client{Transport: transport, CheckRedirect: httputil.RedirectPolicy(false)}, func() {}
	}
	t := netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())
	return &http.Client{Transport: t, CheckRedirect: httputil.RedirectPolicy(false)}, t.CloseIdleConnections
}

// Client credentials grant against the token endpoint parsed into a cached token
func (p *MicrosoftGraphTokenProvider) fetchAccessToken(ctx context.Context) (*CachedAccessToken, error) {
	form := [][2]string{{"grant_type", "client_credentials"}, {"client_id", p.Credentials.ClientID},
		{"client_secret", p.Credentials.ClientSecret}, {"scope", p.Credentials.Scope}}
	parts := make([]string, len(form))
	for i, kv := range form {
		parts[i] = url.QueryEscape(kv[0]) + "=" + url.QueryEscape(kv[1])
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Credentials.TokenURL(), strings.NewReader(strings.Join(parts, "&")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client, closeClient := freshHTTPClient(p.transport)
	resp, err := httputil.Do(client, req, httputil.EachPhase(p.Timeout))
	closeClient()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, tokenError(fmt.Sprintf("Microsoft Graph token request failed with HTTP %d: %s", resp.StatusCode, extractErrorDetail(resp)), nil)
	}
	payload, err := resp.JSON()
	if err != nil {
		return nil, tokenError("Microsoft Graph token response was not valid JSON.", err)
	}
	get, ok := jsonDict(payload)
	if !ok {
		return nil, fmt.Errorf("the Microsoft Graph token response must be a JSON object, got %s", errchain.Kind(payload))
	}
	accessToken := ""
	if v := get("access_token"); anyutil.IsTrue(v) {
		accessToken = strings.TrimSpace(anyutil.String(v))
	}
	if accessToken == "" {
		return nil, tokenError("Microsoft Graph token response did not include access_token.", nil)
	}
	n, err := anyutil.ToInt(get("expires_in"))
	if err != nil {
		if errors.Is(err, anyutil.ErrRange) {
			return nil, err
		}
		return nil, tokenError("Microsoft Graph token response did not include a valid expires_in.", err)
	}
	lifetime := float64(max(n, 0))
	tokenType := "Bearer"
	if v := get("token_type"); anyutil.IsTrue(v) {
		tokenType = strings.TrimSpace(anyutil.String(v))
	}
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return &CachedAccessToken{AccessToken: accessToken, ExpiresAt: timeNow() + lifetime, TokenType: tokenType}, nil
}

// Readable detail of a token endpoint error body, error_description first then the Graph error, code or raw text
func extractErrorDetail(resp *httputil.Response) string {
	payload, err := resp.JSON()
	if err != nil {
		if text := strings.TrimSpace(resp.Text()); text != "" {
			return text
		}
		return "unknown error"
	}
	get, ok := jsonDict(payload)
	if !ok {
		return anyutil.String(payload)
	}
	if s, ok := get("error_description").(string); ok {
		return s
	}
	errValue := get("error")
	if detail, ok := FormatGraphError(errValue); ok {
		return detail
	}
	if eget, ok := jsonDict(errValue); ok && anyutil.IsTrue(eget("code")) {
		return anyutil.String(eget("code"))
	}
	return anyutil.String(payload)
}
