package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// OAuthFlowError is a failure of the OAuth client flow
type OAuthFlowError struct{ Msg string }

func (e *OAuthFlowError) Error() string { return e.Msg }

// OAuthTokenError is a failed token exchange or refresh, a kind of flow error
type OAuthTokenError struct{ Msg string }

func (e *OAuthTokenError) Error() string { return e.Msg }

// Lets errors.As match a token failure as a flow error
func (e *OAuthTokenError) As(target any) bool {
	if t, ok := target.(**OAuthFlowError); ok {
		*t = &OAuthFlowError{e.Msg}
		return true
	}
	return false
}

// OAuthRegistrationError is a failed client registration, a kind of flow error
type OAuthRegistrationError struct{ Msg string }

func (e *OAuthRegistrationError) Error() string { return e.Msg }

// Lets errors.As match a registration failure as a flow error
func (e *OAuthRegistrationError) As(target any) bool {
	if t, ok := target.(**OAuthFlowError); ok {
		*t = &OAuthFlowError{e.Msg}
		return true
	}
	return false
}

// Restarts the flow because a peer rotated the grant this process was about to present
var errRefreshCompletedByPeer = errors.New("refresh completed by peer")

// Issuers that advertise the RFC 9207 iss parameter and then omit it from the redirect
var issOmittingIssuers = map[string]bool{"https://api.figma.com": true}

// Authorization server metadata documents tried during discovery
var asmDiscoveryPaths = []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"}

const (
	discoveryContextLead = "Could not read authorization-server metadata"
	authRedirectLimit    = 5
	deviceCodeGrant      = "urn:ietf:params:oauth:grant-type:device_code"
)

// Versioned User-Agent for OAuth requests that would otherwise carry none
func defaultAuthRequestUserAgent() string { return "Icebath-Agent/" + ClientVersion }

// Gives an OAuth request the default User-Agent when it has none
func stampDefaultUserAgent(req *http.Request) {
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header.Set("User-Agent", defaultAuthRequestUserAgent())
	}
}

// flowResponse is one fully read response to a request the OAuth flow made
type flowResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	Request    *http.Request
	ReadErr    error
}

// Body decoded as text with invalid bytes replaced
func (r *flowResponse) text() string { return decodeReplace(r.Body) }

// Where a redirect response points, without userinfo, query or fragment, nil when it is not a followable redirect
func (r *flowResponse) redirectLocation() *url.URL {
	switch r.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return nil
	}
	loc := r.Header.Get("Location")
	if loc == "" || r.Request == nil {
		return nil
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return nil
	}
	return r.Request.URL.ResolveReference(ref)
}

// Suffix naming a redirect that was not followed, empty otherwise
func redirectNote(r *flowResponse) string {
	loc := r.redirectLocation()
	if loc == nil {
		return ""
	}
	clean := *loc
	clean.User, clean.RawQuery, clean.ForceQuery, clean.Fragment, clean.RawFragment = nil, "", false, "", ""
	return fmt.Sprintf(" (redirected to %s; not followed)", clean.String())
}

// Status and URL of a failed authorization server metadata fetch, empty for anything else
func asmDiscoveryFailure(r *flowResponse) string {
	if r == nil || r.Request == nil || (r.StatusCode >= 200 && r.StatusCode < 300) {
		return ""
	}
	u := r.Request.URL.String()
	for _, p := range asmDiscoveryPaths {
		if strings.Contains(u, p) {
			return fmt.Sprintf("%d from %s", r.StatusCode, u)
		}
	}
	return ""
}

// Registration error reshaped to lead with the metadata discovery failures that caused it
func withDiscoveryContext(err *OAuthRegistrationError, failures []string) *OAuthRegistrationError {
	return &OAuthRegistrationError{fmt.Sprintf("%s (%s); dynamic client registration then fell back to a guessed endpoint on the MCP host and failed: %s",
		discoveryContextLead, strings.Join(failures, "; "), err.Msg)}
}

var wwwAuthFieldPatterns sync.Map

// Value of a WWW-Authenticate field, quoted or bare, found false when absent
func extractFieldFromWWWAuth(h http.Header, field string) (string, bool) {
	header := strings.Join(h.Values("WWW-Authenticate"), ", ")
	if header == "" {
		return "", false
	}
	re, ok := wwwAuthFieldPatterns.Load(field)
	if !ok {
		re, _ = wwwAuthFieldPatterns.LoadOrStore(field, regexp.MustCompile(regexp.QuoteMeta(field)+`=(?:"([^"]+)"|([^\s,]+))`))
	}
	m := re.(*regexp.Regexp).FindStringSubmatch(header)
	if m == nil {
		return "", false
	}
	if m[1] != "" {
		return m[1], true
	}
	return m[2], true
}

// RFC 9728 resource_metadata URL of a 401 or 403 challenge
func extractResourceMetadataFromWWWAuth(status int, h http.Header) string {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return ""
	}
	v, _ := extractFieldFromWWWAuth(h, "resource_metadata")
	return v
}

// Protected resource metadata URLs to try, the challenge URL then the path and root well-known documents
func buildProtectedResourceMetadataDiscoveryURLs(wwwAuthURL, serverURL string) []string {
	var urls []string
	if wwwAuthURL != "" {
		urls = append(urls, wwwAuthURL)
	}
	parsed, _ := pyURLParse(serverURL)
	base := parsed.Scheme + "://" + parsed.Netloc
	if parsed.Path != "" && parsed.Path != "/" {
		urls = append(urls, pyURLJoinAbsPath(base, "/.well-known/oauth-protected-resource"+parsed.Path))
	}
	return append(urls, pyURLJoinAbsPath(base, "/.well-known/oauth-protected-resource"))
}

// Authorization server metadata URLs to try, RFC 8414 path aware then OIDC, the legacy origin document without a server
func buildOAuthAuthorizationServerMetadataDiscoveryURLs(authServerURL, serverURL string) []string {
	if authServerURL == "" {
		parsed, _ := pyURLParse(serverURL)
		return []string{parsed.Scheme + "://" + parsed.Netloc + "/.well-known/oauth-authorization-server"}
	}
	parsed, _ := pyURLParse(authServerURL)
	base := parsed.Scheme + "://" + parsed.Netloc
	if parsed.Path != "" && parsed.Path != "/" {
		trimmed := strings.TrimRight(parsed.Path, "/")
		return []string{
			pyURLJoinAbsPath(base, "/.well-known/oauth-authorization-server"+trimmed),
			pyURLJoinAbsPath(base, "/.well-known/openid-configuration"+trimmed),
			pyURLJoinAbsPath(base, trimmed+"/.well-known/openid-configuration"),
		}
	}
	return []string{pyURLJoinAbsPath(base, "/.well-known/oauth-authorization-server"), pyURLJoinAbsPath(base, "/.well-known/openid-configuration")}
}

// Scope to request, challenge first then resource then server scopes, with offline_access when refresh is possible
func getClientMetadataScopes(wwwScope *string, prm *ProtectedResourceMetadata, asm *OAuthMetadata, grantTypes []string) *string {
	var selected *string
	switch {
	case wwwScope != nil:
		selected = wwwScope
	case prm != nil && prm.HasScopesSupported:
		selected = strPtr(strings.Join(prm.ScopesSupported, " "))
	case asm != nil && asm.HasScopesSupported:
		selected = strPtr(strings.Join(asm.ScopesSupported, " "))
	}
	if selected != nil && asm != nil && asm.HasScopesSupported && containsString(asm.ScopesSupported, "offline_access") &&
		grantTypes != nil && containsString(grantTypes, "refresh_token") && !containsString(strings.Fields(*selected), "offline_access") {
		selected = strPtr(*selected + " offline_access")
	}
	return selected
}

// Union of two scope strings keeping order and dropping duplicates
func unionScopes(previous, next *string) *string {
	if previous == nil || *previous == "" {
		return next
	}
	if next == nil || *next == "" {
		return previous
	}
	merged := strings.Fields(*previous)
	seen := map[string]bool{}
	for _, s := range merged {
		seen[s] = true
	}
	for _, s := range strings.Fields(*next) {
		if !seen[s] {
			merged = append(merged, s)
			seen[s] = true
		}
	}
	return strPtr(strings.Join(merged, " "))
}

// Protected resource metadata of a 200 answer, nil to try the next URL
func handleProtectedResourceResponse(r *flowResponse) *ProtectedResourceMetadata {
	if r.StatusCode != http.StatusOK {
		return nil
	}
	prm, err := parseProtectedResourceMetadata(r.Body)
	if err != nil {
		return nil
	}
	return prm
}

// Metadata of a discovery answer, ok false when discovery must stop at a server error
func handleAuthMetadataResponse(r *flowResponse) (bool, *OAuthMetadata) {
	if r.StatusCode == http.StatusOK {
		md, err := parseOAuthMetadata(r.Body)
		if err != nil {
			return true, nil
		}
		return true, md
	}
	if r.StatusCode >= 300 && r.StatusCode < 500 {
		return true, nil
	}
	return false, nil
}

// Checks the RFC 9207 iss redirect parameter against the issuer, requiring it when advertised
func validateAuthorizationResponseIss(iss string, md *OAuthMetadata) error {
	expected := "None"
	if md != nil {
		expected = md.Issuer
	}
	if iss != "" {
		if iss != expected {
			return &OAuthFlowError{fmt.Sprintf("Authorization response iss mismatch: %s != %s", iss, expected)}
		}
		return nil
	}
	if md != nil && md.AuthorizationResponseIssParameterSupported != nil && *md.AuthorizationResponseIssParameterSupported {
		return &OAuthFlowError{"Authorization response missing iss parameter advertised by the authorization server"}
	}
	return nil
}

// Checks that metadata names the issuer its well-known URL was built from
func validateMetadataIssuer(md *OAuthMetadata, expected string) error {
	if md.Issuer != expected {
		return &OAuthFlowError{fmt.Sprintf("Authorization server metadata issuer mismatch: %s != %s", md.Issuer, expected)}
	}
	return nil
}

// Reports whether two issuers are the same string, a root issuer matching itself with a trailing slash
func issuersMatch(a, b string) bool {
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len(b) < len(a) {
		shorter, longer = b, a
	}
	parsed, _ := pyURLParse(shorter)
	return longer == shorter+"/" && shorter == parsed.Scheme+"://"+parsed.Netloc
}

// Reports whether stored credentials may be presented to an issuer, CIMD and unbound records always matching
func credentialsMatchIssuer(info *OAuthClientInfo, issuer, clientMetadataURL string) bool {
	if clientMetadataURL != "" && info.ClientID == clientMetadataURL {
		return true
	}
	if info.Issuer == nil {
		return true
	}
	return issuersMatch(*info.Issuer, issuer)
}

// Reports whether the server advertises CIMD and a document URL is configured
func shouldUseClientMetadataURL(md *OAuthMetadata, clientMetadataURL string) bool {
	return clientMetadataURL != "" && md != nil && md.ClientIDMetadataDocumentSupported != nil && *md.ClientIDMetadataDocumentSupported
}

// Client record whose client_id is the CIMD document URL
func createClientInfoFromMetadataURL(clientMetadataURL string, redirectURIs []string) *OAuthClientInfo {
	return &OAuthClientInfo{
		clientMetadataBase:      clientMetadataBase{ResponseTypes: []string{"code"}},
		ClientID:                clientMetadataURL,
		TokenEndpointAuthMethod: strPtr("none"),
		RedirectURIs:            redirectURIs,
		HasRedirectURIs:         redirectURIs != nil,
		GrantTypes:              append([]string(nil), defaultGrantTypes...),
	}
}

// Client record of a registration answer, the wire issuer never trusted
func handleRegistrationResponse(r *flowResponse) (*OAuthClientInfo, error) {
	if r.StatusCode != http.StatusOK && r.StatusCode != http.StatusCreated {
		return nil, &OAuthRegistrationError{fmt.Sprintf("Registration failed: %d%s %s", r.StatusCode, redirectNote(r), r.text())}
	}
	doc, err := decodeJSONDocument(r.Body)
	if err != nil {
		return nil, &OAuthRegistrationError{"Invalid registration response: Invalid JSON: " + err.Error()}
	}
	m, ok := documentMap(doc)
	if !ok {
		return nil, &OAuthRegistrationError{"Invalid registration response: 1 validation error for OAuthClientInformationFull\n" +
			"  Input should be a valid dictionary or instance of OAuthClientInformationFull [type=model_type]"}
	}
	delete(m, "issuer")
	info, err := validateClientInfo(m)
	if err != nil {
		return nil, &OAuthRegistrationError{"Invalid registration response: " + err.Error()}
	}
	return info, nil
}

// Rejects a registration whose auth method this flow cannot apply or whose secret method came without a secret
func checkRegistrationUsable(info *OAuthClientInfo) error {
	method := info.TokenEndpointAuthMethod
	switch {
	case method == nil, *method == "none", *method == "client_secret_post", *method == "client_secret_basic":
	default:
		return &OAuthRegistrationError{fmt.Sprintf("Authorization server registered the client with unsupported token_endpoint_auth_method %s", pyReprStr(*method))}
	}
	if method != nil && (*method == "client_secret_post" || *method == "client_secret_basic") && info.ClientSecret == nil {
		return &OAuthRegistrationError{fmt.Sprintf("Authorization server registered the client for %s but issued no client_secret", pyReprStr(*method))}
	}
	return nil
}

// Canonical RFC 8707 resource URL of a server URL, scheme and host lowercased and fragment dropped
func resourceURLFromServerURL(serverURL string) string {
	u, _ := pyURLSplit(serverURL)
	u.Scheme, u.Netloc, u.Fragment = strings.ToLower(u.Scheme), strings.ToLower(u.Netloc), ""
	return pyURLUnparse(u)
}

// Reports whether a requested resource sits under a configured one on the same origin
func checkResourceAllowed(requested, configured string) bool {
	r, _ := pyURLParse(requested)
	c, _ := pyURLParse(configured)
	if !strings.EqualFold(r.Scheme, c.Scheme) || strings.ToLower(r.Netloc) != strings.ToLower(c.Netloc) {
		return false
	}
	rp, cp := r.Path, c.Path
	if !strings.HasSuffix(rp, "/") {
		rp += "/"
	}
	if !strings.HasSuffix(cp, "/") {
		cp += "/"
	}
	return strings.HasPrefix(rp, cp)
}

// Reports whether a protocol version is a known revision at least as new as the minimum
func protocolVersionAtLeast(version, minimum string) bool {
	known := append(append([]string(nil), SupportedProtocolVersions...), LatestProtocolVersion)
	vi, mi := -1, -1
	for i, k := range known {
		if k == version {
			vi = i
		}
		if k == minimum {
			mi = i
		}
	}
	return vi >= 0 && mi >= 0 && vi >= mi
}

// Origin of a server URL rendered the way metadata issuers render, the legacy expected issuer
func originIssuer(serverURL string) string {
	parsed, _ := pyURLParse(serverURL)
	n, err := normalizeURL(parsed.Scheme+"://"+parsed.Netloc, true)
	if err != nil {
		return parsed.Scheme + "://" + parsed.Netloc
	}
	return n
}

// PKCE verifier of 128 unreserved characters and its S256 challenge
func generatePKCE() (verifier, challenge string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var b strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < 128; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	verifier = b.String()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// URL safe random state of 32 bytes
func tokenURLSafe(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// form is an ordered urlencoded body
type form [][2]string

// Sets a field, replacing it in place when present
func (f *form) set(k, v string) {
	for i := range *f {
		if (*f)[i][0] == k {
			(*f)[i][1] = v
			return
		}
	}
	*f = append(*f, [2]string{k, v})
}

// Removes a field
func (f *form) del(k string) {
	out := (*f)[:0]
	for _, kv := range *f {
		if kv[0] != k {
			out = append(out, kv)
		}
	}
	*f = out
}

// requestClock is a request timeout budget spent only while nobody is waiting on a person
type requestClock struct {
	mu       sync.Mutex
	budget   time.Duration
	deadline time.Time
	pauses   int
	timer    *time.Timer
	cancel   context.CancelCauseFunc
}

type requestClockKey struct{}

// Context whose cancellation fires once the clock has run for timeout, the clock itself not yet started
func withRequestClock(ctx context.Context, timeout time.Duration) (context.Context, *requestClock, context.CancelFunc) {
	child, cancel := context.WithCancelCause(ctx)
	clock := &requestClock{budget: timeout, pauses: 1, cancel: cancel}
	child = context.WithValue(child, requestClockKey{}, clock)
	return child, clock, func() {
		clock.mu.Lock()
		if clock.timer != nil {
			clock.timer.Stop()
		}
		clock.mu.Unlock()
		cancel(context.Canceled)
	}
}

// Starts the clock running
func (c *requestClock) start() { c.resume() }

// Stops the clock, keeping the remaining budget
func (c *requestClock) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pauses == 0 {
		c.budget = time.Until(c.deadline)
		if c.timer != nil {
			c.timer.Stop()
		}
	}
	c.pauses++
}

// Restarts the clock with the remaining budget once every pause has ended
func (c *requestClock) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pauses--
	if c.pauses == 0 {
		c.deadline = time.Now().Add(c.budget)
		c.timer = time.AfterFunc(c.budget, func() { c.cancel(context.DeadlineExceeded) })
	}
}

// Pauses the clock of the request ctx serves until the returned func runs, a no op without one
func waitingOnAPerson(ctx context.Context) func() {
	clock, _ := ctx.Value(requestClockKey{}).(*requestClock)
	if clock == nil {
		return func() {}
	}
	clock.pause()
	return clock.resume
}

// oauthFlowHooks lets a wrapping provider observe and veto the flow's traffic and lifecycle
type oauthFlowHooks interface {
	beforeFlow(ctx context.Context, p *OAuthProvider)
	afterInitialize(ctx context.Context, p *OAuthProvider)
	checkOutgoing(ctx context.Context, p *OAuthProvider, req *http.Request) error
	inspectResponse(ctx context.Context, p *OAuthProvider, resp *flowResponse)
	flowFinished(ctx context.Context, p *OAuthProvider)
}

// OAuthProvider authenticates MCP requests with OAuth 2.1, discovering, registering, authorizing and refreshing itself
type OAuthProvider struct {
	serverURL       string
	clientMetadata  *OAuthClientMetadata
	storage         *TokenStorage
	redirectHandler func(ctx context.Context, authorizationURL string) error
	callbackHandler func(ctx context.Context) (*AuthorizationCodeResult, error)
	tokenUserAgent  string
	oauthFlow       string
	client          *http.Client
	hooks           oauthFlowHooks

	sem chan struct{}

	clientMetadataURL         string
	protectedResourceMetadata *ProtectedResourceMetadata
	oauthMetadata             *OAuthMetadata
	authServerURL             string
	protocolVersion           string
	clientInfo                *OAuthClientInfo
	currentTokens             *OAuthToken
	tokenExpiry               *float64
	initialized               bool
	offlineAccessWrapped      bool
	fence                     *os.File
}

// Provider from resolved options with an auth client sharing the transport's TLS and proxy settings
func newOAuthProvider(opts oauthProviderOptions) (*OAuthProvider, error) {
	if opts.ClientMetadataURL != "" && !isValidClientMetadataURL(opts.ClientMetadataURL) {
		return nil, fmt.Errorf("client_metadata_url must be a valid HTTPS URL with a non-root pathname, got: %s", opts.ClientMetadataURL)
	}
	httpOpts := opts.HTTP
	if httpOpts.URL == "" {
		httpOpts.URL = opts.ServerURL
	}
	httpOpts.Auth = nil
	oc, err := newOwnedClient(httpOpts)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: oc.client.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &OAuthProvider{
		serverURL:         opts.ServerURL,
		clientMetadata:    opts.ClientMetadata,
		storage:           opts.Storage,
		redirectHandler:   opts.RedirectHandler,
		callbackHandler:   opts.CallbackHandler,
		tokenUserAgent:    opts.TokenUserAgent,
		oauthFlow:         opts.OAuthFlow,
		clientMetadataURL: opts.ClientMetadataURL,
		client:            client,
		sem:               make(chan struct{}, 1),
	}, nil
}

// Takes the flow lock, giving up when ctx ends
func (p *OAuthProvider) lock(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Releases the flow lock
func (p *OAuthProvider) unlock() { <-p.sem }

// Storage the provider persists into
func (p *OAuthProvider) Storage() *TokenStorage { return p.storage }

// Registration metadata the provider sends, its scope updated by discovery
func (p *OAuthProvider) ClientMetadata() *OAuthClientMetadata { return p.clientMetadata }

// Tokens currently held in memory, nil when none
func (p *OAuthProvider) CurrentTokens() *OAuthToken {
	if p.currentTokens == nil {
		return nil
	}
	return p.currentTokens.clone()
}

// Scheme and authority of a server URL, where guessed endpoints hang
func authorizationBaseURL(serverURL string) string { return pyURLOrigin(serverURL) }

// Absolute expiry of a token from now, nil when the token carries none
func (p *OAuthProvider) updateTokenExpiry(t *OAuthToken) {
	if t == nil || t.ExpiresIn == nil {
		p.tokenExpiry = nil
		return
	}
	at := oauthNow() + float64(*t.ExpiresIn)
	p.tokenExpiry = &at
}

// Reports whether the held access token is present and not past its expiry
func (p *OAuthProvider) isTokenValid() bool {
	if p.currentTokens == nil || p.currentTokens.AccessToken == "" {
		return false
	}
	return p.tokenExpiry == nil || *p.tokenExpiry == 0 || oauthNow() <= *p.tokenExpiry
}

// Reports whether a refresh token and a client record are held
func (p *OAuthProvider) canRefreshToken() bool {
	return p.currentTokens != nil && derefStr(p.currentTokens.RefreshToken) != "" && p.clientInfo != nil
}

// Forgets the held tokens
func (p *OAuthProvider) clearTokens() {
	p.currentTokens = nil
	p.tokenExpiry = nil
}

// RFC 8707 resource, the protected resource's own value when it is a parent of the server URL
func (p *OAuthProvider) getResourceURL() string {
	resource := resourceURLFromServerURL(p.serverURL)
	if p.protectedResourceMetadata != nil && p.protectedResourceMetadata.Resource != "" {
		if checkResourceAllowed(resource, p.protectedResourceMetadata.Resource) {
			resource = p.protectedResourceMetadata.Resource
		}
	}
	return resource
}

// Reports whether OAuth requests carry the resource parameter
func (p *OAuthProvider) shouldIncludeResourceParam() bool {
	if p.protectedResourceMetadata != nil {
		return true
	}
	if p.protocolVersion == "" {
		return false
	}
	return protocolVersionAtLeast(p.protocolVersion, "2025-06-18")
}

// Adds client authentication to a token request by the registered auth method
func (p *OAuthProvider) prepareTokenAuth(data *form, headers http.Header) error {
	if p.clientInfo == nil {
		return nil
	}
	method := p.clientInfo.TokenEndpointAuthMethod
	secret := derefStr(p.clientInfo.ClientSecret)
	switch {
	case method != nil && *method == "client_secret_basic" && secret != "":
		creds := pyQuote(p.clientInfo.ClientID, "") + ":" + pyQuote(secret, "")
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
		data.del("client_secret")
	case method != nil && *method == "client_secret_post" && secret != "":
		data.set("client_id", p.clientInfo.ClientID)
		data.set("client_secret", secret)
	case method == nil || tokenEndpointAuthMethods[*method]:
	default:
		return &OAuthTokenError{fmt.Sprintf("Registered client uses unsupported token_endpoint_auth_method %s", pyReprStr(*method))}
	}
	return nil
}

// Discovered issuer without its trailing slash, empty before discovery
func (p *OAuthProvider) metadataIssuer() string {
	if p.oauthMetadata == nil {
		return ""
	}
	return strings.TrimRight(p.oauthMetadata.Issuer, "/")
}

// The issuer metadata and credentials must belong to, the advertised server or the resource origin
func (p *OAuthProvider) expectedIssuer() string {
	if p.authServerURL != "" {
		return p.authServerURL
	}
	return originIssuer(p.serverURL)
}

// Records the discovered issuer for the next token write
func (p *OAuthProvider) bindIssuerFromContext() {
	if issuer := p.metadataIssuer(); issuer != "" {
		p.storage.BindIssuer(issuer)
	}
}

// Strips a refresh token minted by a different issuer, adopting the current issuer for unstamped files
func (p *OAuthProvider) enforceRefreshTokenIssuer(ctx context.Context) {
	tokens := p.currentTokens
	if tokens == nil || derefStr(tokens.RefreshToken) == "" {
		return
	}
	current := p.metadataIssuer()
	if current == "" {
		return
	}
	stored := strings.TrimRight(p.storage.LoadedIssuer(), "/")
	if stored == "" {
		p.storage.StampIssuer(ctx, current)
		return
	}
	if stored != current {
		mcpLogger().Warn(fmt.Sprintf("MCP OAuth: authorization server issuer changed (%s -> %s); dropping the stored "+
			"refresh token rather than sending it to a different issuer", stored, current))
		p.storage.StripRefreshToken(ctx)
		tokens.RefreshToken = nil
	}
}

// Google's refresh token request parameter when the discovered issuer is Google's
func (p *OAuthProvider) googleOfflineAccessParams() [][2]string {
	issuer := p.metadataIssuer()
	if issuer == "" {
		return nil
	}
	u, err := pyURLSplit(issuer)
	if err != nil || u.Netloc != "accounts.google.com" {
		return nil
	}
	return [][2]string{{"access_type", "offline"}}
}

// Reports whether metadata at the derived path scoped well-known URL names exactly the advertised server's origin
func metadataIssuedByOrigin(md *OAuthMetadata, authServerURL string, r *flowResponse) bool {
	if authServerURL == "" {
		return false
	}
	parts, err := pyURLSplit(authServerURL)
	if err != nil {
		return false
	}
	path := strings.TrimRight(parts.Path, "/")
	hasDotDot := false
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			hasDotDot = true
		}
	}
	if path == "" || hasDotDot || strings.Contains(parts.Netloc, "@") || parts.Query != "" || parts.Fragment != "" || r.StatusCode != http.StatusOK {
		return false
	}
	origin := parts.Scheme + "://" + parts.Netloc
	derived := origin + "/.well-known/oauth-authorization-server" + path
	return r.Request != nil && r.Request.URL.String() == derived && strings.TrimRight(md.Issuer, "/") == origin
}

// Installs origin issued metadata of a path scoped server and answers 204 so discovery stops on it
func (p *OAuthProvider) acceptOriginIssuedMetadata(r *flowResponse) *flowResponse {
	if r.Request == nil {
		return r
	}
	path := r.Request.URL.EscapedPath()
	matched := false
	for _, base := range asmDiscoveryPaths {
		if path == base || strings.HasPrefix(path, base+"/") {
			matched = true
		}
	}
	if !matched {
		return r
	}
	md, err := parseOAuthMetadata(r.Body)
	if err != nil || !metadataIssuedByOrigin(md, p.authServerURL, r) {
		return r
	}
	mcpLogger().Info(fmt.Sprintf("MCP OAuth: accepting authorization-server metadata from %s whose issuer %s is the origin of the "+
		"advertised server %s", r.Request.URL.String(), md.Issuer, p.authServerURL))
	p.oauthMetadata = md
	return &flowResponse{StatusCode: http.StatusNoContent, Header: http.Header{}, Request: r.Request}
}

// Request a redirect leads to when it keeps its method, adds no userinfo and stays on the origin
func nextRequestWithinOrigin(r *flowResponse, sent *http.Request, body []byte) *http.Request {
	loc := r.redirectLocation()
	if loc == nil {
		return nil
	}
	method := sent.Method
	switch {
	case r.StatusCode == http.StatusSeeOther && method != http.MethodHead:
		method = http.MethodGet
	case (r.StatusCode == http.StatusFound || r.StatusCode == http.StatusMovedPermanently) && method == http.MethodPost:
		method = http.MethodGet
	}
	if method != sent.Method {
		return nil
	}
	if loc.User != nil && (sent.URL.User == nil || loc.User.String() != sent.URL.User.String()) {
		return nil
	}
	from, to := originOf(sent.URL), originOf(loc)
	upgrade := from.host == to.host && from.scheme == "http" && from.port == "80" && to.scheme == "https" && to.port == "443"
	if from != to && !upgrade {
		return nil
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	next, err := http.NewRequestWithContext(sent.Context(), method, loc.String(), reader)
	if err != nil {
		return nil
	}
	next.Header = sent.Header.Clone()
	next.Header.Del("Cookie")
	return next
}

// Sends one flow request, stamping it, following same origin redirects and passing each hop through the hooks
func (p *OAuthProvider) send(ctx context.Context, req *http.Request, body []byte, failures *[]string) (*flowResponse, error) {
	for hops := 0; ; hops++ {
		stampDefaultUserAgent(req)
		if p.hooks != nil {
			if err := p.hooks.checkOutgoing(ctx, p, req); err != nil {
				return nil, err
			}
		}
		resp, err := p.client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		data, rerr := readCapped(resp.Body, MaxHTTPBodyBytes, req.URL.String())
		resp.Body.Close()
		if rerr != nil {
			return &flowResponse{StatusCode: resp.StatusCode, Header: resp.Header, Request: req, ReadErr: rerr}, rerr
		}
		fr := &flowResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: data, Request: req}
		if p.hooks != nil {
			p.hooks.inspectResponse(ctx, p, fr)
		}
		if failure := asmDiscoveryFailure(fr); failure != "" {
			*failures = append(*failures, failure)
		} else if fr.StatusCode == http.StatusOK {
			fr = p.acceptOriginIssuedMetadata(fr)
		}
		if hops >= authRedirectLimit {
			return fr, nil
		}
		next := nextRequestWithinOrigin(fr, req, body)
		if next == nil {
			return fr, nil
		}
		req = next
	}
}

// Builds a form POST to a token style endpoint
func newFormRequest(ctx context.Context, endpoint string, data form, headers http.Header) (*http.Request, []byte, error) {
	body := []byte(pyURLEncode(data))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header[k] = v
	}
	return req, body, nil
}

// Builds a discovery GET carrying the newest protocol version
func newMetadataRequest(ctx context.Context, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("MCP-Protocol-Version", LatestProtocolVersion)
	return req, nil
}

// Token endpoint from metadata or guessed on the server origin
func (p *OAuthProvider) tokenEndpoint() string {
	if p.oauthMetadata != nil && p.oauthMetadata.TokenEndpoint != "" {
		return p.oauthMetadata.TokenEndpoint
	}
	return pyURLJoinAbsPath(authorizationBaseURL(p.serverURL), "/token")
}

// Sets client_secret_post on the held client record when a secret came without a usable method
func (p *OAuthProvider) coerceClientSecretPost() {
	if p.clientInfo == nil {
		return
	}
	method := p.clientInfo.TokenEndpointAuthMethod
	if derefStr(p.clientInfo.ClientSecret) != "" && (method == nil || *method == "none" || *method == "") {
		c := *p.clientInfo
		c.TokenEndpointAuthMethod = strPtr("client_secret_post")
		p.clientInfo = &c
	}
}

// Stamps a token request with the configured User-Agent, else the default one
func (p *OAuthProvider) prepareTokenRequest(req *http.Request) {
	if p.tokenUserAgent != "" {
		req.Header.Set("User-Agent", p.tokenUserAgent)
	}
	stampDefaultUserAgent(req)
}

// Loads stored state, restores persisted metadata and enforces refresh token issuer binding
func (p *OAuthProvider) initialize(ctx context.Context) error {
	tokens, err := p.storage.GetTokens()
	if err != nil {
		return err
	}
	info, err := p.storage.GetClientInfo(ctx)
	if err != nil {
		return err
	}
	p.currentTokens, p.clientInfo, p.initialized = tokens, info, true
	if p.oauthMetadata == nil {
		if md := p.storage.LoadOAuthMetadata(); md != nil {
			p.oauthMetadata = md
		}
	}
	p.enforceRefreshTokenIssuer(ctx)
	if p.hooks != nil {
		p.hooks.afterInitialize(ctx, p)
	}
	return nil
}

// Holds the tokens, binds the issuer and persists them
func (p *OAuthProvider) storeTokens(ctx context.Context, t *OAuthToken) error {
	p.currentTokens = t
	p.updateTokenExpiry(t)
	p.bindIssuerFromContext()
	return p.storage.SetTokens(ctx, t)
}

// Takes the refresh fence on the token file, replacing any fence this provider still holds
func (p *OAuthProvider) acquireRefreshFence(ctx context.Context) error {
	p.releaseRefreshFence()
	f, err := acquireRefreshFence(ctx, p.storage.TokensPath(), refreshFenceTimeout)
	if err != nil {
		return err
	}
	p.fence = f
	return nil
}

// Releases the refresh fence when held
func (p *OAuthProvider) releaseRefreshFence() {
	f := p.fence
	p.fence = nil
	releaseRefreshFence(f)
}

// Disk token pair a peer rotated past the held one, nil when disk holds nothing newer
func (p *OAuthProvider) rotatedCandidate() (*OAuthToken, error) {
	stored, err := p.storage.GetTokens()
	if err != nil || stored == nil {
		return nil, err
	}
	if derefStr(stored.RefreshToken) == "" || stored.AccessToken == "" {
		return nil, nil
	}
	if p.currentTokens != nil && derefStr(stored.RefreshToken) == derefStr(p.currentTokens.RefreshToken) {
		return nil, nil
	}
	return stored, nil
}

// Publishes a disk pair and reruns issuer binding, false when binding stripped its refresh token
func (p *OAuthProvider) installDiskPair(ctx context.Context, t *OAuthToken) bool {
	p.currentTokens = t
	p.updateTokenExpiry(t)
	p.enforceRefreshTokenIssuer(ctx)
	return derefStr(p.currentTokens.RefreshToken) != ""
}

// Reports whether the held token is not known to be past due
func (p *OAuthProvider) liveTTL() bool {
	if p.currentTokens == nil {
		return false
	}
	return p.currentTokens.ExpiresIn == nil || *p.currentTokens.ExpiresIn > 0
}

// Refresh request built under the fence from the token this process owns, or the peer completion signal
func (p *OAuthProvider) refreshTokenRequest(ctx context.Context) (*http.Request, []byte, error) {
	p.coerceClientSecretPost()
	if err := p.acquireRefreshFence(ctx); err != nil {
		return nil, nil, err
	}
	keep := false
	defer func() {
		if !keep {
			p.releaseRefreshFence()
		}
	}()
	candidate, err := p.rotatedCandidate()
	if err != nil {
		return nil, nil, err
	}
	if candidate != nil {
		if !p.installDiskPair(ctx, candidate) {
			return nil, nil, errRefreshCompletedByPeer
		}
		if p.liveTTL() && p.isTokenValid() {
			return nil, nil, errRefreshCompletedByPeer
		}
	}
	if p.currentTokens == nil || derefStr(p.currentTokens.RefreshToken) == "" {
		return nil, nil, &OAuthTokenError{"No refresh token available"}
	}
	if p.clientInfo == nil || p.clientInfo.ClientID == "" {
		return nil, nil, &OAuthTokenError{"No client info available"}
	}
	data := form{{"grant_type", "refresh_token"}, {"refresh_token", *p.currentTokens.RefreshToken}, {"client_id", p.clientInfo.ClientID}}
	if p.shouldIncludeResourceParam() {
		data.set("resource", p.getResourceURL())
	}
	headers := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	if err := p.prepareTokenAuth(&data, headers); err != nil {
		return nil, nil, err
	}
	req, body, err := newFormRequest(ctx, p.tokenEndpoint(), data, headers)
	if err != nil {
		return nil, nil, err
	}
	p.prepareTokenRequest(req)
	keep = true
	return req, body, nil
}

// Rereads disk after a rejected refresh, true only when it holds a different still live pair
func (p *OAuthProvider) reloadTokensAfterRefreshFailure(ctx context.Context) (bool, error) {
	candidate, err := p.rotatedCandidate()
	if err != nil || candidate == nil {
		return false, err
	}
	previous := p.currentTokens
	if p.installDiskPair(ctx, candidate) && p.liveTTL() && p.isTokenValid() {
		return true, nil
	}
	p.currentTokens = previous
	p.updateTokenExpiry(previous)
	return false, nil
}

// Accepts any 2xx refresh answer carrying forward an omitted refresh token and scope, always releasing the fence
func (p *OAuthProvider) handleRefreshResponse(ctx context.Context, r *flowResponse) (bool, error) {
	defer p.releaseRefreshFence()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		mcpLogger().Warn(fmt.Sprintf("Token refresh failed: %d", r.StatusCode))
		recovered, err := p.reloadTokensAfterRefreshFailure(ctx)
		if err != nil {
			return false, err
		}
		if recovered {
			mcpLogger().Info("Recovered a peer-rotated refresh token instead of clearing the session")
			return true, nil
		}
		p.clearTokens()
		return false, nil
	}
	t, err := parseOAuthToken(r.Body)
	if r.ReadErr != nil || err != nil {
		mcpLogger().Warn(fmt.Sprintf("Invalid refresh response: %d", r.StatusCode))
		p.clearTokens()
		return false, nil
	}
	if prior := p.currentTokens; prior != nil {
		if t.RefreshToken == nil {
			t.RefreshToken = prior.RefreshToken
		}
		if t.Scope == nil {
			t.Scope = prior.Scope
		}
	}
	if err := p.storeTokens(ctx, t); err != nil {
		return false, err
	}
	return true, nil
}

var htmlTagPattern = regexp.MustCompile(`<[^>]+>`)

// Accepts any 2xx token answer, a failure carrying a short tag stripped redacted excerpt and never a token body
func (p *OAuthProvider) handleTokenResponse(ctx context.Context, r *flowResponse) error {
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		if r.ReadErr != nil {
			return r.ReadErr
		}
		excerpt := pyHead(strings.Join(pyWhitespaceFields(htmlTagPattern.ReplaceAllString(r.text(), " ")), " "), 200)
		msg := strings.TrimRight(fmt.Sprintf("Token exchange failed (%d): %s", r.StatusCode, sanitizeError(excerpt)), ": ")
		return &OAuthTokenError{msg}
	}
	t, err := parseOAuthToken(r.Body)
	if r.ReadErr != nil || err != nil {
		return &OAuthTokenError{"Invalid token response"}
	}
	return p.storeTokens(ctx, t)
}

// Wraps the callback so a known iss omitting issuer gets the discovered issuer filled in
func (p *OAuthProvider) tolerateMissingIssForKnownServer() {
	issuer := p.metadataIssuer()
	if !issOmittingIssuers[issuer] {
		return
	}
	inner := p.callbackHandler
	p.callbackHandler = func(ctx context.Context) (*AuthorizationCodeResult, error) {
		result, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		if result.Iss == "" && result.Code != "" {
			mcpLogger().Warn(fmt.Sprintf("MCP OAuth: %s omitted the iss parameter it advertises; accepting the redirect for that issuer only", issuer))
			filled := *result
			filled.Iss = p.oauthMetadata.Issuer
			result = &filled
		}
		return result, nil
	}
}

// Wraps the redirect once so Google's authorization URL asks for a refresh token
func (p *OAuthProvider) requestGoogleOfflineAccess() {
	inner := p.redirectHandler
	if inner == nil || p.offlineAccessWrapped {
		return
	}
	p.offlineAccessWrapped = true
	p.redirectHandler = func(ctx context.Context, authorizationURL string) error {
		params := p.googleOfflineAccessParams()
		if len(params) == 0 {
			return inner(ctx, authorizationURL)
		}
		parts, err := pyURLSplit(authorizationURL)
		if err != nil {
			return err
		}
		var query form
		for _, kv := range pyParseQSL(parts.Query, true) {
			query.set(kv[0], kv[1])
		}
		for _, kv := range params {
			query.set(kv[0], kv[1])
		}
		hasPrompt := false
		for _, kv := range query {
			if kv[0] == "prompt" {
				hasPrompt = true
			}
		}
		if !hasPrompt {
			query.set("prompt", "consent")
		}
		parts.Query = pyURLEncode(query)
		return inner(ctx, pyURLUnparse(parts))
	}
}

// Runs the browser redirect and callback with PKCE and state, returning the code and verifier
func (p *OAuthProvider) performAuthorizationCodeGrant(ctx context.Context) (string, string, error) {
	if p.clientMetadata.RedirectURIs == nil {
		return "", "", &OAuthFlowError{"No redirect URIs provided for authorization code grant"}
	}
	if p.redirectHandler == nil {
		return "", "", &OAuthFlowError{"No redirect handler provided for authorization code grant"}
	}
	if p.callbackHandler == nil {
		return "", "", &OAuthFlowError{"No callback handler provided for authorization code grant"}
	}
	authEndpoint := pyURLJoinAbsPath(authorizationBaseURL(p.serverURL), "/authorize")
	if p.oauthMetadata != nil && p.oauthMetadata.AuthorizationEndpoint != nil {
		authEndpoint = *p.oauthMetadata.AuthorizationEndpoint
	}
	if p.clientInfo == nil {
		return "", "", &OAuthFlowError{"No client info available for authorization"}
	}
	verifier, challenge := generatePKCE()
	state := tokenURLSafe(32)
	params := form{{"response_type", "code"}, {"client_id", p.clientInfo.ClientID}, {"redirect_uri", p.clientMetadata.RedirectURIs[0]},
		{"state", state}, {"code_challenge", challenge}, {"code_challenge_method", "S256"}}
	if p.shouldIncludeResourceParam() {
		params.set("resource", p.getResourceURL())
	}
	if scope := derefStr(p.clientMetadata.Scope); scope != "" {
		params.set("scope", scope)
		if containsString(strings.Fields(scope), "offline_access") {
			params.set("prompt", "consent")
		}
	}
	authorizationURL := authEndpoint + "?" + pyURLEncode(params)
	resume := waitingOnAPerson(ctx)
	err := p.redirectHandler(ctx, authorizationURL)
	var result *AuthorizationCodeResult
	if err == nil {
		result, err = p.callbackHandler(ctx)
	}
	resume()
	if err != nil {
		return "", "", err
	}
	if result.State == "" || subtle.ConstantTimeCompare([]byte(result.State), []byte(state)) != 1 {
		returned := result.State
		if returned == "" {
			returned = "None"
		}
		return "", "", &OAuthFlowError{fmt.Sprintf("State parameter mismatch: %s != %s", returned, state)}
	}
	if err := validateAuthorizationResponseIss(result.Iss, p.oauthMetadata); err != nil {
		return "", "", err
	}
	if result.Code == "" {
		return "", "", &OAuthFlowError{"No authorization code received"}
	}
	return result.Code, verifier, nil
}

// Authorization code exchange request with the coerced auth method and token User-Agent
func (p *OAuthProvider) exchangeTokenAuthorizationCode(ctx context.Context, code, verifier string) (*http.Request, []byte, error) {
	p.coerceClientSecretPost()
	if p.clientMetadata.RedirectURIs == nil {
		return nil, nil, &OAuthFlowError{"No redirect URIs provided for authorization code grant"}
	}
	if p.clientInfo == nil {
		return nil, nil, &OAuthFlowError{"Missing client info"}
	}
	data := form{{"grant_type", "authorization_code"}, {"code", code}, {"redirect_uri", p.clientMetadata.RedirectURIs[0]},
		{"client_id", p.clientInfo.ClientID}, {"code_verifier", verifier}}
	if p.shouldIncludeResourceParam() {
		data.set("resource", p.getResourceURL())
	}
	headers := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	if err := p.prepareTokenAuth(&data, headers); err != nil {
		return nil, nil, err
	}
	req, body, err := newFormRequest(ctx, p.tokenEndpoint(), data, headers)
	if err != nil {
		return nil, nil, err
	}
	p.prepareTokenRequest(req)
	return req, body, nil
}

// Refuses device registrations, applies the issuer quirks and builds the exchange after the browser grant
func (p *OAuthProvider) performAuthorization(ctx context.Context) (*http.Request, []byte, error) {
	var grants []string
	if p.clientInfo != nil {
		grants = p.clientInfo.GrantTypes
	}
	if p.oauthFlow == "device" || (containsString(grants, deviceCodeGrant) && !containsString(grants, "authorization_code")) {
		return nil, nil, &OAuthNonInteractiveError{"MCP device authorization requires `icebath mcp login <server> --flow device`; " +
			"background reconnects cannot start a device login"}
	}
	p.tolerateMissingIssForKnownServer()
	p.requestGoogleOfflineAccess()
	code, verifier, err := p.performAuthorizationCodeGrant(ctx)
	if err != nil {
		return nil, nil, err
	}
	return p.exchangeTokenAuthorizationCode(ctx, code, verifier)
}

// Adds the bearer header when an access token is held
func (p *OAuthProvider) addAuthHeader(req *http.Request) {
	if p.currentTokens != nil && p.currentTokens.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.currentTokens.AccessToken)
	}
}

// Prepare loads state, refreshes an expired token under the fence and adds the bearer header
func (p *OAuthProvider) Prepare(ctx context.Context, req *http.Request) error {
	if p.hooks != nil {
		p.hooks.beforeFlow(ctx, p)
	}
	for {
		err := p.prepareOnce(ctx, req)
		if errors.Is(err, errRefreshCompletedByPeer) {
			continue
		}
		return err
	}
}

// One pass of request preparation under the flow lock
func (p *OAuthProvider) prepareOnce(ctx context.Context, req *http.Request) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	defer p.releaseRefreshFence()
	if !p.initialized {
		if err := p.initialize(ctx); err != nil {
			return err
		}
	}
	p.protocolVersion = req.Header.Get("MCP-Protocol-Version")
	if !p.isTokenValid() && p.canRefreshToken() {
		refreshReq, body, err := p.refreshTokenRequest(ctx)
		if err != nil {
			return err
		}
		var failures []string
		resp, err := p.send(ctx, refreshReq, body, &failures)
		if err != nil && (resp == nil || resp.ReadErr == nil) {
			return err
		}
		ok, err := p.handleRefreshResponse(ctx, resp)
		if err != nil {
			return err
		}
		if !ok {
			p.initialized = false
		}
	}
	if p.isTokenValid() {
		p.addAuthHeader(req)
	}
	if p.hooks != nil {
		p.hooks.flowFinished(ctx, p)
	}
	return nil
}

// Bearer token a request carried, empty when it carried none
func sentAccessToken(req *http.Request) string {
	if after, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok {
		return after
	}
	return ""
}

// HandleUnauthorized runs discovery, registration and authorization on a 401 or scope step up, true to retry
func (p *OAuthProvider) HandleUnauthorized(ctx context.Context, req *http.Request, resp *http.Response) (bool, error) {
	if err := p.lock(ctx); err != nil {
		return false, err
	}
	defer p.unlock()
	if p.hooks != nil {
		p.hooks.inspectResponse(ctx, p, &flowResponse{StatusCode: resp.StatusCode, Header: resp.Header, Request: req})
	}
	if p.isTokenValid() && p.currentTokens.AccessToken != sentAccessToken(req) {
		p.addAuthHeader(req)
		return true, nil
	}
	errorField, _ := extractFieldFromWWWAuth(resp.Header, "error")
	stepUp := resp.StatusCode == http.StatusForbidden && errorField == "insufficient_scope"
	if resp.StatusCode != http.StatusUnauthorized && !stepUp {
		return false, nil
	}
	var failures []string
	err := p.authorizeAfterChallenge(ctx, req, resp, stepUp, &failures)
	if err != nil {
		mcpLogger().Error(fmt.Sprintf("OAuth flow error: %v", err))
		var regErr *OAuthRegistrationError
		if errors.As(err, &regErr) && len(failures) > 0 && p.oauthMetadata == nil {
			return false, withDiscoveryContext(regErr, failures)
		}
		return false, err
	}
	p.addAuthHeader(req)
	if p.hooks != nil {
		p.hooks.flowFinished(ctx, p)
	}
	return true, nil
}

// Discovery, scope selection, client identification and the authorization code exchange of one challenge
func (p *OAuthProvider) authorizeAfterChallenge(ctx context.Context, req *http.Request, resp *http.Response, stepUp bool, failures *[]string) error {
	var grantedScope *string
	if p.currentTokens != nil {
		grantedScope = p.currentTokens.Scope
	}
	if resp.StatusCode == http.StatusUnauthorized || p.oauthMetadata == nil {
		if err := p.discover(ctx, resp, failures); err != nil {
			return err
		}
	}
	var wwwScope *string
	if s, ok := extractFieldFromWWWAuth(resp.Header, "scope"); ok {
		wwwScope = &s
	}
	challenged := getClientMetadataScopes(wwwScope, p.protectedResourceMetadata, p.oauthMetadata, p.clientMetadata.GrantTypes)
	if stepUp {
		p.clientMetadata.Scope = unionScopes(unionScopes(p.clientMetadata.Scope, grantedScope), challenged)
	} else {
		p.clientMetadata.Scope = challenged
	}
	if p.clientInfo == nil {
		if err := p.identifyClient(ctx, failures); err != nil {
			return err
		}
	}
	tokenReq, body, err := p.performAuthorization(ctx)
	if err != nil {
		return err
	}
	tokenResp, err := p.send(ctx, tokenReq, body, failures)
	if err != nil && (tokenResp == nil || tokenResp.ReadErr == nil) {
		return err
	}
	return p.handleTokenResponse(ctx, tokenResp)
}

// Walks the protected resource then authorization server metadata ladders, dropping credentials bound elsewhere
func (p *OAuthProvider) discover(ctx context.Context, resp *http.Response, failures *[]string) error {
	wwwAuthURL := extractResourceMetadataFromWWWAuth(resp.StatusCode, resp.Header)
	prmFailed := 0
	found := false
	for _, u := range buildProtectedResourceMetadataDiscoveryURLs(wwwAuthURL, p.serverURL) {
		dreq, err := newMetadataRequest(ctx, u)
		if err != nil {
			return err
		}
		dresp, err := p.send(ctx, dreq, nil, failures)
		if err != nil {
			return err
		}
		if dresp.StatusCode >= 500 || dresp.StatusCode == http.StatusTooManyRequests {
			prmFailed = dresp.StatusCode
		}
		prm := handleProtectedResourceResponse(dresp)
		if prm == nil {
			mcpLogger().Debug(fmt.Sprintf("Protected resource metadata discovery failed: %s", u))
			continue
		}
		if err := p.validateResourceMatch(prm); err != nil {
			return err
		}
		p.protectedResourceMetadata = prm
		p.authServerURL = prm.AuthorizationServers[0]
		found = true
		break
	}
	if !found && prmFailed != 0 {
		return &OAuthFlowError{fmt.Sprintf("Protected resource metadata request failed: HTTP %d", prmFailed)}
	}
	expected := p.expectedIssuer()
	if p.clientInfo != nil && !credentialsMatchIssuer(p.clientInfo, expected, p.clientMetadataURL) {
		mcpLogger().Debug("Authorization server changed; discarding bound credentials and re-registering")
		p.clientInfo = nil
		p.clearTokens()
		p.oauthMetadata = nil
	}
	for _, u := range buildOAuthAuthorizationServerMetadataDiscoveryURLs(p.authServerURL, p.serverURL) {
		dreq, err := newMetadataRequest(ctx, u)
		if err != nil {
			return err
		}
		dresp, err := p.send(ctx, dreq, nil, failures)
		if err != nil {
			return err
		}
		ok, asm := handleAuthMetadataResponse(dresp)
		if !ok {
			break
		}
		if asm == nil {
			mcpLogger().Debug(fmt.Sprintf("OAuth metadata discovery failed: %s", u))
			continue
		}
		if p.authServerURL == "" && issuersMatch(asm.Issuer, expected) {
			expected = asm.Issuer
		}
		if err := validateMetadataIssuer(asm, expected); err != nil {
			return err
		}
		p.oauthMetadata = asm
		break
	}
	return nil
}

// Rejects protected resource metadata whose resource is not a parent of the server URL
func (p *OAuthProvider) validateResourceMatch(prm *ProtectedResourceMetadata) error {
	if prm.Resource == "" {
		return nil
	}
	def := resourceURLFromServerURL(p.serverURL)
	if !checkResourceAllowed(def, prm.Resource) {
		return &OAuthFlowError{fmt.Sprintf("Protected resource %s does not match expected %s", prm.Resource, def)}
	}
	return nil
}

// Presents the CIMD document or registers dynamically, binding the record to the issuer it was made with
func (p *OAuthProvider) identifyClient(ctx context.Context, failures *[]string) error {
	discovered := ""
	if p.oauthMetadata != nil {
		discovered = p.expectedIssuer()
	}
	if shouldUseClientMetadataURL(p.oauthMetadata, p.clientMetadataURL) {
		mcpLogger().Debug(fmt.Sprintf("Using URL-based client ID (CIMD): %s", p.clientMetadataURL))
		info := createClientInfoFromMetadataURL(p.clientMetadataURL, p.clientMetadata.RedirectURIs)
		if discovered != "" {
			info.Issuer = strPtr(discovered)
		}
		p.clientInfo = info
		return p.storage.SetClientInfo(ctx, info)
	}
	fallbackBase := authorizationBaseURL(p.serverURL)
	registrationURL := pyURLJoinAbsPath(fallbackBase, "/register")
	if p.oauthMetadata != nil && p.oauthMetadata.RegistrationEndpoint != nil {
		registrationURL = *p.oauthMetadata.RegistrationEndpoint
	}
	body, err := p.clientMetadata.dump().MarshalJSON()
	if err != nil {
		return err
	}
	rreq, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	rreq.Header.Set("Content-Type", "application/json")
	rresp, err := p.send(ctx, rreq, body, failures)
	if err != nil {
		return err
	}
	info, err := handleRegistrationResponse(rresp)
	if err != nil {
		return err
	}
	if err := checkRegistrationUsable(info); err != nil {
		return err
	}
	if p.oauthMetadata != nil && discovered != "" &&
		(p.oauthMetadata.RegistrationEndpoint != nil || authorizationBaseURL(discovered) == fallbackBase) {
		info.Issuer = strPtr(discovered)
	}
	p.clientInfo = info
	return p.storage.SetClientInfo(ctx, info)
}
