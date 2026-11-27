package mcp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

// OAuth failure of the flow itself: state, issuer, a missing code
type OAuthFlowError struct{ Msg string }

func (e *OAuthFlowError) Error() string { return e.Msg }

// Token endpoint refusal or an unusable token answer
type OAuthTokenError struct{ Msg string }

func (e *OAuthTokenError) Error() string { return e.Msg }

// Client registration refusal or an unusable registration
type OAuthRegistrationError struct{ Msg string }

func (e *OAuthRegistrationError) Error() string { return e.Msg }

// Sign in needed where no one can sign in, such as a connection made in the background
type NonInteractiveError struct{ Msg string }

func (e *NonInteractiveError) Error() string { return e.Msg }

// Whether the failure is one of authentication: an OAuth failure, or an HTTP 401
func AuthFailure(err error) bool {
	if err == nil {
		return false
	}
	if status, ok := errors.AsType[*HTTPStatusError](err); ok {
		return status.Status == http.StatusUnauthorized
	}
	_, flow := errors.AsType[*OAuthFlowError](err)
	_, token := errors.AsType[*OAuthTokenError](err)
	_, registration := errors.AsType[*OAuthRegistrationError](err)
	_, interactive := errors.AsType[*NonInteractiveError](err)
	return flow || token || registration || interactive
}

// OAuth settings of a server, its oauth block
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	// Name the client registers under, which a person reads where the server asks them to allow it: icebath when empty
	ClientName string
	Scope      string
	// Where the browser comes back: a whole URI, else the loopback host and port
	RedirectURI  string
	RedirectHost string
	RedirectPort int
	// Client authentication at the token endpoint, none for a public client
	AuthMethod string
	// native when empty
	ApplicationType string
	// User-Agent of token requests, the default one when empty
	UserAgent string
	// Client ID metadata document presented in place of registering, the published one when empty; off when CIMD is false
	MetadataURL string
	CIMD        bool
	// browser or device
	Flow string
	// Longest a person gets to sign in
	Timeout time.Duration
}

// Settings of an oauth block, the defaults for what it leaves out
func ParseOAuthConfig(block map[string]any) (OAuthConfig, error) {
	text := func(key string) string {
		if v, ok := block[key]; ok && v != nil {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	c := OAuthConfig{ClientID: text("client_id"), ClientSecret: text("client_secret"), ClientName: text("client_name"), Scope: text("scope"),
		RedirectURI: text("redirect_uri"), RedirectHost: cmp.Or(text("redirect_host"), "127.0.0.1"), AuthMethod: text("token_endpoint_auth_method"),
		ApplicationType: cmp.Or(text("application_type"), "native"), UserAgent: text("user_agent"), MetadataURL: cmp.Or(text("client_metadata_url"), cimdURL),
		CIMD: block["cimd"] != false, Flow: cmp.Or(strings.ToLower(text("flow")), "browser"), Timeout: 300 * time.Second}
	if port := text("redirect_port"); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return c, fmt.Errorf("The OAuth setting redirect_port must be a port number from 0 to 65,535, not “%s”.", port)
		}
		c.RedirectPort = n
	}
	if seconds := text("timeout"); seconds != "" {
		f, err := strconv.ParseFloat(seconds, 64)
		if err != nil || f <= 0 {
			return c, fmt.Errorf("The OAuth setting timeout must be a number of seconds above 0, not “%s”.", seconds)
		}
		c.Timeout = time.Duration(f * float64(time.Second))
	}
	if c.Flow != "browser" && c.Flow != "device" {
		return c, errors.New("The OAuth setting flow must be browser or device.")
	}
	if c.AuthMethod == "" {
		c.AuthMethod = "none"
		if c.ClientSecret != "" {
			c.AuthMethod = "client_secret_post"
		}
	}
	return c, nil
}

// Tokens of a server
type Token struct {
	AccessToken  string
	TokenType    string
	RefreshToken string
	Scope        string
	// Seconds the access token lived for when issued, zero when it did not say
	ExpiresIn int64
	// When the access token stops working, zero when it does not say
	Expiry time.Time
	// Members this client does not read, kept as the server sent them
	extra map[string]any
}

func parseToken(m map[string]any) (*Token, error) {
	t := &Token{extra: map[string]any{}}
	for k, v := range m {
		switch k {
		case "access_token":
			t.AccessToken, _ = v.(string)
		case "token_type":
			t.TokenType, _ = v.(string)
		case "refresh_token":
			t.RefreshToken, _ = v.(string)
		case "scope":
			t.Scope, _ = v.(string)
		case "expires_in":
			if n, ok := number(v); ok {
				t.ExpiresIn = int64(n)
			}
		case "expires_at", issuerKey:
		default:
			t.extra[k] = v
		}
	}
	if t.AccessToken == "" {
		return nil, errors.New("access_token is missing")
	}
	if t.TokenType == "" {
		t.TokenType = "Bearer"
	}
	if !strings.EqualFold(t.TokenType, "bearer") {
		return nil, fmt.Errorf("token_type %q is not Bearer", t.TokenType)
	}
	if t.ExpiresIn > 0 {
		t.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	return t, nil
}

func (t *Token) object() map[string]any {
	m := copyObject(t.extra)
	if m == nil {
		m = map[string]any{}
	}
	m["access_token"], m["token_type"] = t.AccessToken, "Bearer"
	if t.RefreshToken != "" {
		m["refresh_token"] = t.RefreshToken
	}
	if t.Scope != "" {
		m["scope"] = t.Scope
	}
	if t.ExpiresIn > 0 {
		m["expires_in"] = t.ExpiresIn
	}
	return m
}

// Whether the access token is held and not past its expiry
func (t *Token) Valid() bool {
	return t != nil && t.AccessToken != "" && (t.Expiry.IsZero() || time.Now().Before(t.Expiry))
}

// Number of a decoded JSON value
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func texts(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, item := range list {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// Registration of this client at an authorization server
type ClientInfo struct {
	ClientID     string
	ClientSecret string
	AuthMethod   string
	RedirectURIs []string
	GrantTypes   []string
	// Authorization server it was registered with, empty for one bound to none
	Issuer string
	raw    map[string]any
}

func parseClientInfo(m map[string]any) (*ClientInfo, error) {
	info := &ClientInfo{raw: m}
	info.ClientID, _ = m["client_id"].(string)
	info.ClientSecret, _ = m["client_secret"].(string)
	info.AuthMethod, _ = m["token_endpoint_auth_method"].(string)
	info.Issuer, _ = m["issuer"].(string)
	info.RedirectURIs, info.GrantTypes = texts(m["redirect_uris"]), texts(m["grant_types"])
	if info.ClientID == "" {
		return nil, errors.New("client_id is missing")
	}
	return info, nil
}

func (c *ClientInfo) object() map[string]any {
	m := copyObject(c.raw)
	if m == nil {
		m = map[string]any{}
	}
	m["client_id"] = c.ClientID
	set := func(key, value string) {
		if value != "" {
			m[key] = value
		} else {
			delete(m, key)
		}
	}
	set("client_secret", c.ClientSecret)
	set("token_endpoint_auth_method", c.AuthMethod)
	set("issuer", c.Issuer)
	if c.RedirectURIs != nil {
		m["redirect_uris"] = c.RedirectURIs
	}
	if c.GrantTypes != nil {
		m["grant_types"] = c.GrantTypes
	}
	return m
}

// Authorization server metadata, RFC 8414 or OpenID
type ServerMetadata struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	DeviceEndpoint        string
	Scopes                []string
	HasScopes             bool
	GrantTypes            []string
	HasGrantTypes         bool
	IssParameter          bool
	CIMDSupported         bool
	raw                   map[string]any
}

func parseServerMetadata(m map[string]any, allowDevice bool) (*ServerMetadata, error) {
	md := &ServerMetadata{raw: m}
	md.Issuer, _ = m["issuer"].(string)
	md.AuthorizationEndpoint, _ = m["authorization_endpoint"].(string)
	md.TokenEndpoint, _ = m["token_endpoint"].(string)
	md.RegistrationEndpoint, _ = m["registration_endpoint"].(string)
	md.DeviceEndpoint, _ = m["device_authorization_endpoint"].(string)
	_, md.HasScopes = m["scopes_supported"].([]any)
	md.Scopes = texts(m["scopes_supported"])
	_, md.HasGrantTypes = m["grant_types_supported"].([]any)
	md.GrantTypes = texts(m["grant_types_supported"])
	md.IssParameter = m["authorization_response_iss_parameter_supported"] == true
	md.CIMDSupported = m["client_id_metadata_document_supported"] == true
	switch {
	case md.Issuer == "":
		return nil, errors.New("issuer is missing")
	case md.TokenEndpoint == "":
		return nil, errors.New("token_endpoint is missing")
	case md.AuthorizationEndpoint == "" && !(allowDevice && md.DeviceEndpoint != ""):
		return nil, errors.New("authorization_endpoint is missing")
	}
	return md, nil
}

// Protected resource metadata, RFC 9728
type ResourceMetadata struct {
	Resource             string
	AuthorizationServers []string
	Scopes               []string
	HasScopes            bool
}

func parseResourceMetadata(data []byte) (*ResourceMetadata, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	prm := &ResourceMetadata{AuthorizationServers: texts(m["authorization_servers"]), Scopes: texts(m["scopes_supported"])}
	prm.Resource, _ = m["resource"].(string)
	_, prm.HasScopes = m["scopes_supported"].([]any)
	if len(prm.AuthorizationServers) == 0 {
		return nil, errors.New("authorization_servers is missing")
	}
	return prm, nil
}

// Published client ID metadata document, on an origin that never redirects as CIMD fetchers require
const cimdURL = "https://icebath.app/oauth/client-metadata.json"

// Loopback callback ports and hosts the published document declares
var (
	cimdPorts = []int{27890, 27891, 27892, 27893, 27894}
	cimdHosts = map[string]bool{"127.0.0.1": true, "localhost": true}
)

// Figma's remote MCP registers only allowlisted client names, the default scope it grants
const (
	figmaClientName = "Claude Code"
	figmaScope      = "mcp:connect"
)

// Whether the server is Figma's hosted remote MCP endpoint
func figma(server, serverURL string) bool {
	host := ""
	if u, err := url.Parse(serverURL); err == nil {
		host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	}
	if host == "mcp.figma.com" || strings.HasSuffix(host, ".mcp.figma.com") ||
		(host == "figma.com" || strings.HasSuffix(host, ".figma.com")) && strings.Contains(strings.ToLower(serverURL), "/mcp") {
		return true
	}
	return strings.Contains(strings.ToLower(server), "figma") && (serverURL == "" || strings.Contains(host, "figma"))
}

// Provider defaults the settings leave unset: Figma's allowlisted client name, scope and auth method
func (c *OAuthConfig) defaults(server, serverURL string) {
	if !figma(server, serverURL) {
		return
	}
	if c.ClientName == "" {
		c.ClientName = figmaClientName
		slog.Info("MCP OAuth: Figma registers allowlisted client names only; registering as Claude Code (override with oauth.client_name)", "server", server)
	}
	c.Scope = cmp.Or(c.Scope, figmaScope)
	if c.AuthMethod == "none" && c.ClientSecret == "" {
		c.AuthMethod = "client_secret_post"
	}
}

// Next step for a registration refusal the user can act on, false to keep the error as it is
func HumanizeRegistration(server, msg, serverURL string) (string, bool) {
	lowered := strings.ToLower(msg)
	registration := strings.Contains(lowered, "regist") || strings.Contains(lowered, "dcr") || strings.Contains(lowered, "dynamic client")
	if strings.Contains(msg, "404") && (strings.Contains(lowered, "not found") || strings.Contains(lowered, "/register")) && registration {
		return fmt.Sprintf("%s does not let icebath register itself, as its registration address answered 404, as Google's Gmail and Drive servers do. "+
			"Create an OAuth client at the provider, put its client_id and client_secret in the oauth part of the server's settings, then run icebath "+
			"mcp login %s.", server, server), true
	}
	if !(strings.Contains(msg, "403") || strings.Contains(lowered, "forbidden")) || !(registration || strings.Contains(msg, "403") && strings.Contains(lowered, "forbidden") || strings.TrimSpace(lowered) == "forbidden") {
		return "", false
	}
	if figma(server, serverURL) {
		return fmt.Sprintf("%s is the Figma server, which lets only some client names register, such as “%s” and “Codex”, and refuses the rest. "+
			"icebath uses “%s” unless you set client_name in the oauth part of the server's settings. Change your client_name to one of those, or "+
			"clear it, then run icebath mcp login %s.", server, figmaClientName, figmaClientName, server), true
	}
	return fmt.Sprintf("%s accepts only OAuth clients it approved before and refused to register icebath with 403, so no sign-in can start. "+
		"Set client_name in the oauth part of the server's settings to a name the provider accepts, add a client_id and client_secret you created at "+
		"the provider, or use its local server or an API key instead.", server), true
}

// Issuers that advertise the iss parameter of RFC 9207 but leave it out of their redirects
var issOmitting = map[string]bool{"https://accounts.google.com": true}

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// Signs requests to a server with OAuth 2.1: discovers its authorization server, registers or presents this client, has a person sign in
// and refreshes the tokens
type Provider struct {
	server string
	url    string
	config OAuthConfig
	store  *Store
	client *http.Client
	// Redirect URI this provider registered and authorizes with
	redirect string
	// Client ID metadata document presented, empty when this client registers
	cimd string

	sem       chan struct{}
	prm       *ResourceMetadata
	asm       *ServerMetadata
	authURL   string
	protocol  string
	info      *ClientInfo
	tokens    *Token
	loaded    bool
	stamp     Stamp
	scope     string
	preloaded bool
}

// Provider of a server, its requests sent with the server's TLS and proxy settings and never following redirects
func NewProvider(server, serverURL string, config OAuthConfig, store *Store, opts HTTPOptions) (*Provider, error) {
	config.defaults(server, serverURL)
	opts.URL, opts.Auth = cmp.Or(opts.URL, serverURL), nil
	client, err := NewClient(opts)
	if err != nil {
		return nil, err
	}
	p := &Provider{server: server, url: serverURL, config: config, store: store, sem: make(chan struct{}, 1), scope: config.Scope,
		client: &http.Client{Transport: client.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := p.choose(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Provider) Store() *Store { return p.store }

func (p *Provider) Config() OAuthConfig { return p.config }

// Chooses the redirect URI: the configured one, else the loopback port configured, registered before, of the client ID metadata document,
// or free now; and with a configured client, keeps its registration
func (p *Provider) choose() error {
	c := p.config
	cached := p.store.Client()
	switch {
	case c.RedirectURI != "":
		p.redirect = c.RedirectURI
	case c.RedirectPort != 0:
		p.redirect = loopback(c.RedirectHost, c.RedirectPort)
	case cached != nil && len(cached.RedirectURIs) > 0 && cached.ClientID != c.MetadataURL:
		p.redirect = cached.RedirectURIs[0]
	default:
		if p.cimdEligible(cached) {
			if port, ok := cimdPort(); ok {
				p.redirect, p.cimd = loopback(c.RedirectHost, port), c.MetadataURL
				break
			}
		}
		port, err := Reserve(0)
		if err != nil {
			return err
		}
		p.redirect = loopback(c.RedirectHost, port)
	}
	if c.ClientID == "" {
		return nil
	}
	if cached != nil && (cached.ClientID != c.ClientID || cached.ClientSecret != c.ClientSecret) {
		os.Remove(p.store.TokensPath())
		os.Remove(p.store.metaPath())
		slog.Warn("MCP OAuth client changed; discarded tokens minted under the previous client. Sign in again with icebath mcp login", "server", p.server)
	}
	p.preloaded = true
	return p.store.SetClient(&ClientInfo{ClientID: c.ClientID, ClientSecret: c.ClientSecret, AuthMethod: c.AuthMethod, RedirectURIs: []string{p.redirect},
		GrantTypes: []string{"authorization_code", "refresh_token"}})
}

func loopback(host string, port int) string {
	return fmt.Sprintf("http://%s:%d/callback", host, port)
}

// Whether this client may present the published metadata document rather than register
func (p *Provider) cimdEligible(cached *ClientInfo) bool {
	c := p.config
	if md := p.store.Metadata(); md != nil && !md.CIMDSupported {
		return false
	}
	return c.CIMD && validCIMD(c.MetadataURL) && c.ClientID == "" && c.ClientSecret == "" && c.ClientName == "" && c.AuthMethod == "none" &&
		c.RedirectURI == "" && c.RedirectPort == 0 && cimdHosts[c.RedirectHost] && cached == nil && !p.store.CIMDRejected()
}

// Whether a URL may be a client ID: https, a path other than the root, no user, fragment or dot segments
func validCIMD(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Path == "" || u.Path == "/" || u.User != nil || u.Fragment != "" {
		return false
	}
	return !slices.Contains(strings.Split(u.Path, "/"), ".") && !slices.Contains(strings.Split(u.Path, "/"), "..")
}

func (p *Provider) lock(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Provider) unlock() { <-p.sem }

// Reads the tokens and registration again when another process changed them since they were read
func (p *Provider) load() {
	stamp := p.store.Stamp()
	if p.loaded && stamp == p.stamp {
		return
	}
	p.tokens, p.info, p.loaded, p.stamp = p.store.Tokens(), p.store.Client(), true, stamp
	if p.asm == nil {
		p.asm = p.store.Metadata()
	}
	p.bindIssuer()
}

// Issuer of the discovered metadata without its trailing slash, empty before discovery
func (p *Provider) issuer() string {
	if p.asm == nil {
		return ""
	}
	return strings.TrimRight(p.asm.Issuer, "/")
}

// Drops a refresh token minted by another issuer than the one in use, stamping files written before issuers were recorded
func (p *Provider) bindIssuer() {
	current := p.issuer()
	if current == "" || p.tokens == nil || p.tokens.RefreshToken == "" {
		return
	}
	stored := strings.TrimRight(p.store.LoadedIssuer(), "/")
	if stored == "" {
		p.store.StampIssuer(current)
		return
	}
	if stored != current {
		slog.Warn("MCP OAuth authorization server changed; dropping the stored refresh token rather than sending it to another issuer", "server", p.server,
			"from", stored, "to", current)
		p.store.StripRefresh()
		p.tokens.RefreshToken = ""
	}
}

// Whether the tokens kept on disk are there
func (p *Provider) HasTokens() bool { return p.store.HasTokens() }

func (p *Provider) Prepare(ctx context.Context, req *http.Request) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	p.load()
	p.protocol = req.Header.Get("MCP-Protocol-Version")
	if !p.tokens.Valid() && p.tokens != nil && p.tokens.RefreshToken != "" && p.info != nil {
		if err := p.refresh(ctx); err != nil {
			return err
		}
	}
	if p.tokens.Valid() {
		req.Header.Set("Authorization", "Bearer "+p.tokens.AccessToken)
	}
	return nil
}

// Refreshes the tokens, keeping the refresh token and scope the answer leaves out; a refusal drops them unless another process already
// rotated them
func (p *Provider) refresh(ctx context.Context) error {
	if p.info.AuthMethod == "" || p.info.AuthMethod == "none" {
		if p.info.ClientSecret != "" {
			p.info.AuthMethod = "client_secret_post"
		}
	}
	data := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {p.tokens.RefreshToken}, "client_id": {p.info.ClientID}}
	if p.resourceParam() {
		data.Set("resource", p.resource())
	}
	if p.google() {
		data.Set("access_type", "offline")
	}
	resp, err := p.post(ctx, p.tokenEndpoint(), data)
	if err != nil {
		return err
	}
	if resp.status < 200 || resp.status >= 300 {
		slog.Warn("MCP OAuth token refresh refused", "server", p.server, "status", resp.status)
		p.inspect(resp)
		if disk := p.store.Tokens(); disk != nil && disk.RefreshToken != p.tokens.RefreshToken && disk.Valid() {
			p.tokens, p.stamp = disk, p.store.Stamp()
			return nil
		}
		p.tokens = nil
		return nil
	}
	fresh, err := tokenOf(resp.body)
	if err != nil {
		slog.Warn("MCP OAuth refresh answer unusable", "server", p.server, "error", err)
		p.tokens = nil
		return nil
	}
	fresh.RefreshToken = cmp.Or(fresh.RefreshToken, p.tokens.RefreshToken)
	fresh.Scope = cmp.Or(fresh.Scope, p.tokens.Scope)
	return p.keep(fresh)
}

func tokenOf(body []byte) (*Token, error) {
	value, err := DecodeExact(body)
	m, ok := value.(map[string]any)
	if err != nil || !ok {
		return nil, errors.New("not a JSON object")
	}
	return parseToken(m)
}

// Holds and keeps the tokens, stamped with the issuer that minted them
func (p *Provider) keep(t *Token) error {
	p.tokens = t
	if issuer := p.issuer(); issuer != "" {
		p.store.Bind(issuer)
	}
	if err := p.store.SetTokens(t); err != nil {
		return err
	}
	p.stamp = p.store.Stamp()
	return nil
}

// Answer read whole, never followed through a redirect
type flowResponse struct {
	status int
	header http.Header
	body   []byte
	url    *url.URL
}

func (p *Provider) do(req *http.Request) (*flowResponse, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", provider.UserAgent("Icebath-Agent"))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readCapped(resp.Body, MaxBodyBytes, req.URL.String())
	if err != nil {
		return nil, err
	}
	return &flowResponse{status: resp.StatusCode, header: resp.Header, body: body, url: req.URL}, nil
}

func (p *Provider) get(ctx context.Context, target string) (*flowResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("MCP-Protocol-Version", cmp.Or(p.protocol, HandshakeVersion))
	return p.do(req)
}

// Form posted to a token endpoint with this client's authentication
func (p *Provider) post(ctx context.Context, target string, data url.Values) (*flowResponse, error) {
	header := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	if p.info != nil && p.info.ClientSecret != "" {
		switch p.info.AuthMethod {
		case "client_secret_basic":
			header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(p.info.ClientID)+":"+url.QueryEscape(p.info.ClientSecret))))
			data.Del("client_secret")
		case "client_secret_post":
			data.Set("client_id", p.info.ClientID)
			data.Set("client_secret", p.info.ClientSecret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header = header
	if p.config.UserAgent != "" {
		req.Header.Set("User-Agent", p.config.UserAgent)
	}
	return p.do(req)
}

var invalidClient = regexp.MustCompile(`\binvalid_client\b`)

// Drops a registration the token endpoint refused as invalid_client, retiring a refused metadata document too, so the next sign in
// registers again
func (p *Provider) inspect(resp *flowResponse) {
	if p.preloaded || resp.status != http.StatusBadRequest && resp.status != http.StatusUnauthorized || !invalidClient.Match(bytes.ToLower(resp.body)) {
		return
	}
	if p.cimd != "" && p.info != nil && p.info.ClientID == p.cimd {
		slog.Warn("MCP OAuth authorization server refused the client ID metadata document; falling back to dynamic client registration", "server", p.server)
		p.cimd = ""
		p.store.MarkCIMDRejected()
	}
	p.store.DropClient()
	p.info = nil
}

func (p *Provider) tokenEndpoint() string {
	if p.asm != nil {
		return p.asm.TokenEndpoint
	}
	return root(p.url) + "/token"
}

// Scheme and authority of a URL
func root(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Scheme + "://" + u.Host
}

// RFC 8707 resource of the server: its URL with scheme and host lowered and no fragment, or the protected resource's when that is a parent
func (p *Provider) resource() string {
	resource := p.url
	if u, err := url.Parse(p.url); err == nil {
		u.Scheme, u.Host, u.Fragment = strings.ToLower(u.Scheme), strings.ToLower(u.Host), ""
		resource = u.String()
	}
	if p.prm != nil && p.prm.Resource != "" && under(resource, p.prm.Resource) {
		return p.prm.Resource
	}
	return resource
}

// Whether a requested resource sits under a configured one on the same origin
func under(requested, configured string) bool {
	r, err1 := url.Parse(requested)
	c, err2 := url.Parse(configured)
	if err1 != nil || err2 != nil || !strings.EqualFold(r.Scheme, c.Scheme) || !strings.EqualFold(r.Host, c.Host) {
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

// Whether OAuth requests carry the resource parameter: with protected resource metadata, or for a server on 2025-06-18 or later
func (p *Provider) resourceParam() bool {
	return p.prm != nil || p.protocol != "" && VersionAtLeast(p.protocol, "2025-06-18")
}

func (p *Provider) google() bool {
	u, err := url.Parse(p.issuer())
	return err == nil && u.Host == "accounts.google.com"
}

// Value of a WWW-Authenticate field, quoted or bare
func challengeField(h http.Header, field string) (string, bool) {
	header := strings.Join(h.Values("WWW-Authenticate"), ", ")
	m := regexp.MustCompile(regexp.QuoteMeta(field) + `=(?:"([^"]+)"|([^\s,]+))`).FindStringSubmatch(header)
	if m == nil {
		return "", false
	}
	return cmp.Or(m[1], m[2]), true
}

// Discovers, registers and has a person sign in on a 401 or a 403 asking for more scope; true when the request should be sent again
func (p *Provider) HandleUnauthorized(ctx context.Context, req *http.Request, resp *http.Response) (bool, error) {
	if err := p.lock(ctx); err != nil {
		return false, err
	}
	defer p.unlock()
	p.load()
	if p.tokens.Valid() && "Bearer "+p.tokens.AccessToken != req.Header.Get("Authorization") {
		req.Header.Set("Authorization", "Bearer "+p.tokens.AccessToken)
		return true, nil
	}
	reason, _ := challengeField(resp.Header, "error")
	stepUp := resp.StatusCode == http.StatusForbidden && reason == "insufficient_scope"
	if resp.StatusCode != http.StatusUnauthorized && !stepUp {
		return false, nil
	}
	if err := p.authorize(ctx, resp.StatusCode, resp.Header, stepUp); err != nil {
		slog.Error("MCP OAuth flow failed", "server", p.server, "error", err)
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+p.tokens.AccessToken)
	return true, nil
}

// Signs in again from scratch, whatever is held, through the person of the context
func (p *Provider) Login(ctx context.Context) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	p.load()
	p.tokens = nil
	return p.authorize(ctx, http.StatusUnauthorized, p.challenge(ctx), false)
}

// Challenge the server answers an unauthenticated request with, for its resource metadata link and scope
func (p *Provider) challenge(ctx context.Context) http.Header {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, strings.NewReader(`{"jsonrpc":"2.0","id":"_probe","method":"initialize","params":{"protocolVersion":"`+
		HandshakeVersion+`","capabilities":{},"clientInfo":{"name":"`+ClientName+`","version":"`+provider.Version()+`"}}}`))
	if err != nil {
		return http.Header{}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := p.do(req)
	if err != nil || resp.status != http.StatusUnauthorized {
		return http.Header{}
	}
	return resp.header
}

func (p *Provider) authorize(ctx context.Context, status int, header http.Header, stepUp bool) error {
	granted := ""
	if p.tokens != nil {
		granted = p.tokens.Scope
	}
	var failures []string
	if status == http.StatusUnauthorized || p.asm == nil {
		if err := p.discover(ctx, header, &failures); err != nil {
			return err
		}
	}
	challenged, _ := challengeField(header, "scope")
	scope := p.scopes(challenged)
	if stepUp {
		p.scope = union(union(p.scope, granted), scope)
	} else {
		p.scope = cmp.Or(scope, p.config.Scope)
	}
	if p.info == nil {
		if err := p.identify(ctx); err != nil {
			if registration, ok := errors.AsType[*OAuthRegistrationError](err); ok && len(failures) > 0 && p.asm == nil {
				return &OAuthRegistrationError{"MCP OAuth discovery found no authorization server metadata (" + strings.Join(failures, "; ") + "): " + registration.Msg}
			}
			return err
		}
	}
	if p.config.Flow == "device" || slices.Contains(p.info.GrantTypes, deviceGrant) && !slices.Contains(p.info.GrantTypes, "authorization_code") {
		return &NonInteractiveError{"Signing in with a code on another device needs icebath mcp login <server> --flow device, as a reconnect in the background cannot start it."}
	}
	code, verifier, err := p.grant(ctx)
	if err != nil {
		return err
	}
	data := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {p.redirect}, "client_id": {p.info.ClientID},
		"code_verifier": {verifier}}
	if p.resourceParam() {
		data.Set("resource", p.resource())
	}
	if p.info.ClientSecret != "" && (p.info.AuthMethod == "" || p.info.AuthMethod == "none") {
		p.info.AuthMethod = "client_secret_post"
	}
	resp, err := p.post(ctx, p.tokenEndpoint(), data)
	if err != nil {
		return err
	}
	if resp.status < 200 || resp.status >= 300 {
		p.inspect(resp)
		excerpt := strings.Join(strings.Fields(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(string(resp.body), " ")), " ")
		return &OAuthTokenError{strings.TrimRight(fmt.Sprintf("The server did not finish the sign-in, with HTTP status %d: %s", resp.status, Redact(excerpt[:min(len(excerpt), 200)])), ": ")}
	}
	t, err := tokenOf(resp.body)
	if err != nil {
		return &OAuthTokenError{"Invalid token response"}
	}
	if err := p.keep(t); err != nil {
		return err
	}
	if p.asm != nil {
		if kept := p.store.Metadata(); kept == nil || kept.TokenEndpoint != p.asm.TokenEndpoint {
			p.store.SetMetadata(p.asm)
		}
	}
	return nil
}

// Scope to ask for: the challenge's, else the resource's, else the authorization server's, with offline_access added when refresh is
// possible
func (p *Provider) scopes(challenged string) string {
	selected := challenged
	switch {
	case selected != "":
	case p.prm != nil && p.prm.HasScopes:
		selected = strings.Join(p.prm.Scopes, " ")
	case p.asm != nil && p.asm.HasScopes:
		selected = strings.Join(p.asm.Scopes, " ")
	}
	if selected != "" && p.asm != nil && slices.Contains(p.asm.Scopes, "offline_access") && !slices.Contains(strings.Fields(selected), "offline_access") {
		selected += " offline_access"
	}
	return selected
}

// Scopes of both, in order without repeats
func union(previous, next string) string {
	merged := strings.Fields(previous)
	for _, s := range strings.Fields(next) {
		if !slices.Contains(merged, s) {
			merged = append(merged, s)
		}
	}
	return strings.Join(merged, " ")
}

// Protected resource metadata, then the authorization server's, dropping credentials bound to another server
func (p *Provider) discover(ctx context.Context, header http.Header, failures *[]string) error {
	link, _ := challengeField(header, "resource_metadata")
	failed := 0
	p.prm, p.authURL = nil, ""
	for _, candidate := range resourceMetadataURLs(link, p.url) {
		resp, err := p.get(ctx, candidate)
		if err != nil {
			return err
		}
		if resp.status >= 500 || resp.status == http.StatusTooManyRequests {
			failed = resp.status
		}
		if resp.status != http.StatusOK {
			*failures = append(*failures, fmt.Sprintf("%s: HTTP %d", candidate, resp.status))
			continue
		}
		prm, err := parseResourceMetadata(resp.body)
		if err != nil {
			*failures = append(*failures, candidate+": "+err.Error())
			continue
		}
		if prm.Resource != "" && !under(p.resource(), prm.Resource) {
			return &OAuthFlowError{fmt.Sprintf("The server says it is %s, but icebath expected %s, so the sign-in stopped.", prm.Resource, p.resource())}
		}
		p.prm, p.authURL = prm, prm.AuthorizationServers[0]
		break
	}
	if p.prm == nil && failed != 0 {
		return &OAuthFlowError{fmt.Sprintf("The server did not say where to sign in, with HTTP status %d.", failed)}
	}
	expected := cmp.Or(p.authURL, root(p.url))
	if p.info != nil && p.info.ClientID != p.cimd && p.info.Issuer != "" && !sameIssuer(p.info.Issuer, expected) {
		slog.Info("MCP OAuth authorization server changed; discarding the bound registration and tokens", "server", p.server)
		p.info, p.tokens, p.asm = nil, nil, nil
	}
	for _, candidate := range serverMetadataURLs(p.authURL, p.url) {
		resp, err := p.get(ctx, candidate)
		if err != nil {
			return err
		}
		if resp.status >= 500 {
			break
		}
		if resp.status != http.StatusOK {
			*failures = append(*failures, fmt.Sprintf("%s: HTTP %d", candidate, resp.status))
			continue
		}
		value, err := DecodeExact(resp.body)
		m, ok := value.(map[string]any)
		if err != nil || !ok {
			continue
		}
		asm, err := parseServerMetadata(m, false)
		if err != nil {
			*failures = append(*failures, candidate+": "+err.Error())
			continue
		}
		if p.authURL == "" && sameIssuer(asm.Issuer, expected) {
			expected = asm.Issuer
		}
		if !sameIssuer(asm.Issuer, expected) {
			return &OAuthFlowError{fmt.Sprintf("The sign-in server says it is %s, but icebath expected %s, so the sign-in stopped.", asm.Issuer, expected)}
		}
		p.asm = asm
		break
	}
	return nil
}

// Whether two issuers are one, a root issuer matching itself with a trailing slash
func sameIssuer(a, b string) bool {
	return a == b || strings.TrimRight(a, "/") == strings.TrimRight(b, "/") && (root(a) == strings.TrimRight(a, "/") || root(b) == strings.TrimRight(b, "/"))
}

// Protected resource metadata URLs: the challenge's, then the path and root well-known documents
func resourceMetadataURLs(link, serverURL string) []string {
	var urls []string
	if link != "" {
		urls = append(urls, link)
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return urls
	}
	base := u.Scheme + "://" + u.Host
	if u.Path != "" && u.Path != "/" {
		urls = append(urls, base+"/.well-known/oauth-protected-resource"+u.Path)
	}
	return append(urls, base+"/.well-known/oauth-protected-resource")
}

// Authorization server metadata URLs: RFC 8414 path aware then OpenID, the server's own origin when no authorization server is named
func serverMetadataURLs(authServer, serverURL string) []string {
	if authServer == "" {
		return []string{root(serverURL) + "/.well-known/oauth-authorization-server"}
	}
	u, err := url.Parse(authServer)
	if err != nil {
		return nil
	}
	base := u.Scheme + "://" + u.Host
	if path := strings.TrimRight(u.Path, "/"); path != "" {
		return []string{base + "/.well-known/oauth-authorization-server" + path, base + "/.well-known/openid-configuration" + path,
			base + path + "/.well-known/openid-configuration"}
	}
	return []string{base + "/.well-known/oauth-authorization-server", base + "/.well-known/openid-configuration"}
}

// Presents the client ID metadata document when the server takes one, else registers this client dynamically
func (p *Provider) identify(ctx context.Context) error {
	issuer := ""
	if p.asm != nil {
		issuer = cmp.Or(p.authURL, root(p.url))
	}
	if p.cimd != "" && p.asm != nil && p.asm.CIMDSupported {
		info := &ClientInfo{ClientID: p.cimd, AuthMethod: "none", RedirectURIs: []string{p.redirect}, GrantTypes: []string{"authorization_code", "refresh_token"}, Issuer: issuer}
		p.info = info
		return p.store.SetClient(info)
	}
	endpoint := root(p.url) + "/register"
	if p.asm != nil {
		if p.asm.RegistrationEndpoint == "" {
			return &OAuthRegistrationError{fmt.Sprintf("%s does not let icebath register itself, as it names no registration address, and a guess at %s "+
				"would fail as it does for Google's Gmail and Drive servers. Create an OAuth client at the provider, put its client_id and client_secret in "+
				"the oauth part of the server's settings, then run icebath mcp login %s.", p.server, endpoint, p.server)}
		}
		endpoint = p.asm.RegistrationEndpoint
	}
	method := p.config.AuthMethod
	metadata := map[string]any{"client_name": cmp.Or(p.config.ClientName, "icebath"), "redirect_uris": []string{p.redirect},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "token_endpoint_auth_method": method,
		"application_type": p.config.ApplicationType}
	if p.scope != "" {
		metadata["scope"] = p.scope
	}
	body, _ := json.Marshal(metadata)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.do(req)
	if err != nil {
		return err
	}
	if resp.status == http.StatusBadRequest && bytes.Contains(resp.body, []byte("application_type")) {
		delete(metadata, "application_type")
		body, _ = json.Marshal(metadata)
		req, _ = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if resp, err = p.do(req); err != nil {
			return err
		}
	}
	if resp.status != http.StatusOK && resp.status != http.StatusCreated {
		note := ""
		if location := resp.header.Get("Location"); resp.status >= 300 && resp.status < 400 && location != "" {
			note = ", sending it on to " + location
		}
		return &OAuthRegistrationError{strings.TrimSpace(fmt.Sprintf("The server refused to register icebath, with HTTP status %d%s. %s", resp.status, note,
			strings.TrimSpace(string(resp.body))))}
	}
	value, err := DecodeExact(resp.body)
	m, ok := value.(map[string]any)
	if err != nil || !ok {
		return &OAuthRegistrationError{"Invalid registration response: not a JSON object"}
	}
	delete(m, "issuer")
	info, err := parseClientInfo(m)
	if err != nil {
		return &OAuthRegistrationError{"Invalid registration response: " + err.Error()}
	}
	switch info.AuthMethod {
	case "", "none", "client_secret_post", "client_secret_basic":
	default:
		return &OAuthRegistrationError{fmt.Sprintf("The server registered icebath with the sign-in method “%s”, which icebath does not support.", info.AuthMethod)}
	}
	if (info.AuthMethod == "client_secret_post" || info.AuthMethod == "client_secret_basic") && info.ClientSecret == "" {
		return &OAuthRegistrationError{fmt.Sprintf("The server registered icebath for the sign-in method “%s” but gave no client_secret.", info.AuthMethod)}
	}
	if issuer != "" && (p.asm.RegistrationEndpoint != "" || root(issuer) == root(p.url)) {
		info.Issuer = issuer
	}
	if len(info.RedirectURIs) == 0 {
		info.RedirectURIs = []string{p.redirect}
	}
	p.info = info
	return p.store.SetClient(info)
}

// PKCE verifier of 128 unreserved characters and its S256 challenge
func pkce() (string, string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	verifier := make([]byte, 128)
	for i := range verifier {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		verifier[i] = alphabet[n.Int64()]
	}
	sum := sha256.Sum256(verifier)
	return string(verifier), base64.RawURLEncoding.EncodeToString(sum[:])
}

// Has the person of the context sign in at the authorization endpoint, the code and the PKCE verifier it was asked with
func (p *Provider) grant(ctx context.Context) (string, string, error) {
	signer := SignerOf(ctx)
	if signer == nil {
		return "", "", &NonInteractiveError{fmt.Sprintf("The MCP server %s needs you to sign in, and no one can sign in here. Run icebath mcp login %s, or "+
			"sign in from /mcp, then connect the servers again with /reload-mcp.", p.server, p.server)}
	}
	endpoint := root(p.url) + "/authorize"
	if p.asm != nil && p.asm.AuthorizationEndpoint != "" {
		endpoint = p.asm.AuthorizationEndpoint
	}
	verifier, challenge := pkce()
	state := strs.Token(32)
	params := url.Values{"response_type": {"code"}, "client_id": {p.info.ClientID}, "redirect_uri": {p.redirect}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	if p.resourceParam() {
		params.Set("resource", p.resource())
	}
	if p.scope != "" {
		params.Set("scope", p.scope)
		if slices.Contains(strings.Fields(p.scope), "offline_access") {
			params.Set("prompt", "consent")
		}
	}
	if p.google() {
		params.Set("access_type", "offline")
		if params.Get("prompt") == "" {
			params.Set("prompt", "consent")
		}
	}
	link := endpoint
	if strings.Contains(link, "?") {
		link += "&" + params.Encode()
	} else {
		link += "?" + params.Encode()
	}
	signCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	back, err := signer.Authorize(signCtx, link, p.redirect)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return "", "", &OAuthFlowError{fmt.Sprintf("The sign-in to %s stopped after waiting %s for the browser.", p.server, strs.Span(p.config.Timeout))}
		}
		return "", "", err
	}
	if failure := back.Get("error"); failure != "" {
		return "", "", &OAuthFlowError{"Authorization failed: " + cmp.Or(back.Get("error_description"), failure)}
	}
	returned := back.Get("state")
	if returned == "" || subtle.ConstantTimeCompare([]byte(returned), []byte(state)) != 1 {
		return "", "", &OAuthFlowError{fmt.Sprintf("The browser came back from a different sign-in, with state %s instead of %s, so this one stopped.", cmp.Or(returned, "None"), state)}
	}
	iss := back.Get("iss")
	if iss == "" && issOmitting[p.issuer()] && back.Get("code") != "" {
		iss = p.asm.Issuer
	}
	if err := p.checkIss(iss); err != nil {
		return "", "", err
	}
	code := back.Get("code")
	if code == "" {
		return "", "", &OAuthFlowError{"No authorization code received"}
	}
	return code, verifier, nil
}

// Checks the RFC 9207 iss of the redirect against the issuer, required when the server advertises it
func (p *Provider) checkIss(iss string) error {
	if iss != "" {
		expected := "None"
		if p.asm != nil {
			expected = p.asm.Issuer
		}
		if iss != expected {
			return &OAuthFlowError{fmt.Sprintf("The browser came back from the sign-in server %s instead of %s, so the sign-in stopped.", iss, expected)}
		}
		return nil
	}
	if p.asm != nil && p.asm.IssParameter {
		return &OAuthFlowError{"Authorization response missing iss parameter advertised by the authorization server"}
	}
	return nil
}

type signerKey struct{}

// Person who signs in: shown the link, they bring back the query the browser came back to the redirect URI with
type Signer interface {
	Authorize(ctx context.Context, link, redirect string) (url.Values, error)
}

// Context whose sign ins go to the signer
func WithSigner(ctx context.Context, signer Signer) context.Context {
	return context.WithValue(ctx, signerKey{}, signer)
}

// Signer of the context, nil where no one can sign in
func SignerOf(ctx context.Context) Signer {
	signer, _ := ctx.Value(signerKey{}).(Signer)
	return signer
}

// Sign in on another device: the person opens the verification link anywhere and enters the code shown, while this client asks the token
// endpoint until they approved
func (p *Provider) LoginDevice(ctx context.Context, show func(link, code string)) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	p.load()
	var failures []string
	if err := p.discover(ctx, p.challenge(ctx), &failures); err != nil {
		return err
	}
	if p.asm == nil {
		return errors.New("The server does not say where to sign in.")
	}
	if p.asm.DeviceEndpoint == "" {
		return errors.New("The server does not offer sign-in with a code on another device. Sign in with --flow browser instead.")
	}
	if p.asm.HasGrantTypes && !slices.Contains(p.asm.GrantTypes, deviceGrant) {
		return errors.New("The server does not offer sign-in with a code on another device.")
	}
	p.scope = cmp.Or(p.scopes(""), p.config.Scope)
	if err := p.registerDevice(ctx); err != nil {
		return err
	}
	resource := p.resource()
	data := url.Values{"client_id": {p.info.ClientID}, "resource": {resource}}
	if p.scope != "" {
		data.Set("scope", p.scope)
	}
	if p.google() {
		data.Set("access_type", "offline")
	}
	resp, err := p.post(ctx, p.asm.DeviceEndpoint, data)
	if err != nil {
		return err
	}
	var started struct {
		DeviceCode      string      `json:"device_code"`
		UserCode        string      `json:"user_code"`
		VerificationURI string      `json:"verification_uri"`
		Complete        string      `json:"verification_uri_complete"`
		Interval        json.Number `json:"interval"`
		ExpiresIn       json.Number `json:"expires_in"`
	}
	if resp.status < 200 || resp.status >= 300 {
		return fmt.Errorf("The server refused to start a sign-in with a code, with HTTP status %d.", resp.status)
	}
	if err := json.Unmarshal(resp.body, &started); err != nil {
		return fmt.Errorf("The server answered the sign-in with a code in a way icebath cannot read: %w", err)
	}
	for field, value := range map[string]string{"device_code": started.DeviceCode, "user_code": started.UserCode, "verification_uri": started.VerificationURI} {
		if value == "" {
			return errors.New("The server's answer to the sign-in with a code lacks " + field + ".")
		}
	}
	interval, err := started.Interval.Float64()
	if started.Interval == "" {
		interval, err = 5, nil
	}
	if err != nil || interval <= 0 {
		return errors.New("The server's answer to the sign-in with a code has an unusable interval.")
	}
	expires, err := started.ExpiresIn.Float64()
	if err != nil || expires <= 0 {
		return errors.New("The server's answer to the sign-in with a code has an unusable expires_in.")
	}
	show(cmp.Or(started.Complete, started.VerificationURI), started.UserCode)
	deadline := time.Now().Add(min(time.Duration(expires*float64(time.Second)), p.config.Timeout))
	wait := time.Duration(interval * float64(time.Second))
	poll := url.Values{"client_id": {p.info.ClientID}, "device_code": {started.DeviceCode}, "grant_type": {deviceGrant}, "resource": {resource}}
	for {
		if time.Until(deadline) <= wait {
			return errors.New("The code expired before you approved it. Run icebath mcp login again.")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		answer, err := p.post(ctx, p.asm.TokenEndpoint, cloneValues(poll))
		if err != nil {
			if Timeout(err) && ctx.Err() == nil {
				wait *= 2
				continue
			}
			return err
		}
		if answer.status >= 200 && answer.status < 300 {
			t, err := tokenOf(answer.body)
			if err != nil {
				return &OAuthTokenError{"Invalid token response"}
			}
			return p.keep(t)
		}
		var refusal struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		json.Unmarshal(answer.body, &refusal)
		switch refusal.Error {
		case "authorization_pending":
		case "slow_down":
			wait += 5 * time.Second
		case "access_denied":
			return &OAuthFlowError{"Device authorization was denied"}
		case "expired_token":
			return errors.New("The code expired before you approved it. Run icebath mcp login again.")
		default:
			return &OAuthTokenError{fmt.Sprintf("The server did not finish the sign-in with a code, with HTTP status %d: %s", answer.status, cmp.Or(refusal.Description,
				refusal.Error, Redact(string(answer.body))))}
		}
	}
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, values := range v {
		out[k] = slices.Clone(values)
	}
	return out
}

// Registers a device client, or adopts the configured one, bound to the authorization server
func (p *Provider) registerDevice(ctx context.Context) error {
	issuer := cmp.Or(p.authURL, p.asm.Issuer)
	if p.config.ClientID != "" {
		p.info = &ClientInfo{ClientID: p.config.ClientID, ClientSecret: p.config.ClientSecret, AuthMethod: p.config.AuthMethod,
			GrantTypes: []string{deviceGrant, "refresh_token"}, Issuer: issuer}
		return p.store.SetClient(p.info)
	}
	if p.asm.RegistrationEndpoint == "" {
		return errors.New("The server does not let icebath register itself. Create an OAuth client at the server, then put its client_id, and client_secret if it has one, in the oauth part of the server's settings.")
	}
	metadata := map[string]any{"client_name": cmp.Or(p.config.ClientName, "icebath"), "grant_types": []string{deviceGrant, "refresh_token"},
		"response_types": []string{}, "token_endpoint_auth_method": p.config.AuthMethod, "redirect_uris": []string{p.redirect}}
	if p.scope != "" {
		metadata["scope"] = p.scope
	}
	body, _ := json.Marshal(metadata)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.asm.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.do(req)
	if err != nil {
		return err
	}
	if resp.status < 200 || resp.status >= 300 {
		return fmt.Errorf("The server refused to register icebath, with HTTP status %d.", resp.status)
	}
	value, err := DecodeExact(resp.body)
	m, ok := value.(map[string]any)
	if err != nil || !ok {
		return errors.New("The server answered the registration of icebath in a way icebath cannot read.")
	}
	m["issuer"] = issuer
	info, err := parseClientInfo(m)
	if err != nil {
		return err
	}
	if info.ClientSecret != "" && (info.AuthMethod == "" || info.AuthMethod == "none") {
		info.AuthMethod = "client_secret_post"
	}
	switch {
	case info.AuthMethod != "" && info.AuthMethod != "none" && info.AuthMethod != "client_secret_post" && info.AuthMethod != "client_secret_basic",
		info.AuthMethod != "" && info.AuthMethod != "none" && info.ClientSecret == "":
		return errors.New("The OAuth client for sign-in with a code signs in to the server in a way icebath does not support, or lacks part of it.")
	}
	p.info = info
	return p.store.SetClient(info)
}

// Credential shapes kept out of text a model or person reads
var credentials = regexp.MustCompile(`(?i)(?:ghp_[A-Za-z0-9_]{1,255}|sk-[A-Za-z0-9_-](?:\.?[A-Za-z0-9_-]){0,254}|Bearer\s+\S+` +
	`|(?:token|key|API_KEY|password|secret)=[^\s&,;"']{1,255})`)

// Text with credential shaped parts replaced by [REDACTED]
func Redact(text string) string {
	return credentials.ReplaceAllString(text, "[REDACTED]")
}
