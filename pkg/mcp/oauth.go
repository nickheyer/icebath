package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Configured oauth.user_agent for token endpoint requests, empty when unset or blank
func tokenRequestUserAgent(cfg map[string]any) string {
	ua, ok := cfg["user_agent"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(ua)
}

// Seconds an interactive login probe may take, the connect timeout or the callback window plus 15 seconds
func LoginConnectTimeout(serverConfig map[string]any) float64 {
	seconds := func(value any, def float64) float64 {
		f, ok := pyFloat(value)
		if !ok {
			return def
		}
		if math.IsNaN(f) {
			return 0
		}
		return math.Max(0, f)
	}
	oauthCfg, _ := serverConfig["oauth"].(map[string]any)
	return math.Max(seconds(serverConfig["connect_timeout"], 0), seconds(oauthCfg["timeout"], 300)+15)
}

// Python int() of a config value with its error text where int raises
func pyIntStrict(v any) (int, error) {
	n, ok := pyInt(v)
	if ok {
		return n, nil
	}
	if s, isStr := v.(string); isStr {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyReprStr(s))
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))
}

// Python float() of a config value with its error text where float raises
func pyFloatStrict(v any) (float64, error) {
	f, ok := pyFloat(v)
	if ok {
		return f, nil
	}
	if s, isStr := v.(string); isStr {
		return 0, fmt.Errorf("could not convert string to float: %s", pyReprStr(s))
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%s'", pyTypeName(v))
}

// Resolves the callback port into cfg _resolved_port, preferring dashboard or cached proxy URIs, then CIMD, config, cache, ephemeral
func configureCallbackPort(ctx context.Context, cfg map[string]any, storage *TokenStorage) (int, error) {
	flow := dashboardOAuthFlowFrom(ctx)
	pinnedLoopback := pyTruthy(cfg["client_id"]) && pyTruthy(cfg["redirect_port"])
	if flow != nil && !pinnedLoopback {
		cfg["_resolved_port"] = 0
		if !pyTruthy(cfg["redirect_uri"]) {
			cfg["redirect_uri"] = flow.RedirectURI
		}
		return 0, nil
	}
	cachedURI, cachedPort := cachedRedirect(storage)
	if cachedURI != "" && !pyTruthy(cfg["redirect_uri"]) && !pinnedLoopback {
		cfg["redirect_uri"] = cachedURI
		cfg["_resolved_port"] = 0
		return 0, nil
	}
	var port int
	if url, cimdPort, ok := maybeUseCIMD(ctx, cfg, storage); ok {
		cfg["_cimd_url"], port = url, cimdPort
	} else {
		raw, present := cfg["redirect_port"]
		if !present {
			raw = 0
		}
		configured, err := pyIntStrict(raw)
		if err != nil {
			return 0, err
		}
		port = configured
		if port == 0 {
			port = cachedPort
		}
		if port == 0 {
			if port, err = reserveCallbackPort(); err != nil {
				return 0, err
			}
		}
		if isCIMDPort(port) {
			claimCIMDPort(port)
		}
	}
	cfg["_resolved_port"] = port
	return port, nil
}

// Configured proxy redirect_uri or the loopback callback URL on the redirect host
func resolveRedirectURI(cfg map[string]any, port int) string {
	if v := cfg["redirect_uri"]; pyTruthy(v) {
		return pyStr(v)
	}
	host := cfg["redirect_host"]
	if !pyTruthy(host) {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d/callback", pyStr(host), port)
}

// Figma's remote MCP registers only allowlisted client names, the default scope it grants
const (
	figmaDCRClientName = "Claude Code"
	figmaDefaultScope  = "mcp:connect"
)

// Lowercased hostname of a base URL that may lack a scheme
func oauthBaseURLHostname(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := pyURLParse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimRight(netlocHostname(u.Netloc), ".")
}

// Reports whether a base URL's host is the domain or one of its subdomains
func oauthBaseURLHostMatches(raw, domain string) bool {
	host := oauthBaseURLHostname(raw)
	domain = strings.TrimRight(strings.ToLower(strings.TrimSpace(domain)), ".")
	return host != "" && domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

// Reports whether the server is Figma's hosted remote MCP endpoint
func isFigmaRemoteMCP(serverName, serverURL string) bool {
	u := strings.ToLower(serverURL)
	if oauthBaseURLHostMatches(u, "mcp.figma.com") || (oauthBaseURLHostMatches(u, "figma.com") && strings.Contains(u, "/mcp")) {
		return true
	}
	return strings.Contains(strings.ToLower(serverName), "figma") && (u == "" || strings.Contains(oauthBaseURLHostname(u), "figma"))
}

// Fills provider specific OAuth defaults the user left unset, returning cfg
func ApplyOAuthProviderDefaults(cfg map[string]any, serverName, serverURL string) map[string]any {
	if isFigmaRemoteMCP(serverName, serverURL) {
		if !pyTruthy(cfg["client_name"]) {
			cfg["client_name"] = figmaDCRClientName
			label := serverName
			if label == "" {
				label = serverURL
			}
			mcpLogger().Info(fmt.Sprintf("MCP OAuth '%s': Figma DCR allowlist — registering as client_name=%s (override via oauth.client_name)",
				label, pyReprStr(figmaDCRClientName)))
		}
		if !pyTruthy(cfg["scope"]) {
			cfg["scope"] = figmaDefaultScope
		}
		if !pyTruthy(cfg["token_endpoint_auth_method"]) {
			cfg["token_endpoint_auth_method"] = "client_secret_post"
		}
	}
	return cfg
}

// Client registration metadata for the resolved callback, dropping an unaccepted application_type
func buildClientMetadata(cfg map[string]any) (*OAuthClientMetadata, error) {
	portValue, ok := cfg["_resolved_port"]
	if !ok || portValue == nil {
		return nil, errors.New("_configure_callback_port() must be called before _build_client_metadata()")
	}
	port, err := pyIntStrict(portValue)
	if err != nil {
		return nil, err
	}
	authMethod := cfg["token_endpoint_auth_method"]
	if !pyTruthy(authMethod) {
		authMethod = "none"
		if pyTruthy(cfg["client_secret"]) {
			authMethod = "client_secret_post"
		}
	}
	redirect, err := normalizeURL(resolveRedirectURI(cfg, port), false)
	if err != nil {
		return nil, &ValidationError{Model: "url", Errors: []FieldError{{"", err.Error(), err.(*urlError).typ}}}
	}
	clientName, hasName := cfg["client_name"]
	if !hasName {
		clientName = "Icebath Agent"
	}
	appType, hasApp := cfg["application_type"]
	if !hasApp {
		appType = "native"
	}
	kwargs := map[string]any{
		"client_name":                clientName,
		"redirect_uris":              []any{redirect},
		"grant_types":                []any{"authorization_code", "refresh_token"},
		"response_types":             []any{"code"},
		"token_endpoint_auth_method": authMethod,
		"application_type":           appType,
	}
	if pyTruthy(cfg["scope"]) {
		kwargs["scope"] = cfg["scope"]
	}
	md, err := validateClientMetadata(kwargs)
	if err == nil {
		return md, nil
	}
	delete(kwargs, "application_type")
	return validateClientMetadata(kwargs)
}

// Python equality of two decoded config or JSON scalars
func pyScalarEqual(a, b any) bool {
	num := func(v any) (float64, bool) {
		switch x := v.(type) {
		case json.Number:
			f, err := strconv.ParseFloat(string(x), 64)
			return f, err == nil
		case bool:
			if x {
				return 1, true
			}
			return 0, true
		case int:
			return float64(x), true
		case int64:
			return float64(x), true
		case uint64:
			return float64(x), true
		case float64:
			return x, true
		}
		return 0, false
	}
	if fa, ok := num(a); ok {
		fb, okb := num(b)
		return okb && fa == fb
	}
	if sa, ok := a.(string); ok {
		sb, okb := b.(string)
		return okb && sa == sb
	}
	return a == nil && b == nil
}

// Python or of two values
func pyOr(a, b any) any {
	if jsonTruthy(a) {
		return a
	}
	return b
}

// Drops tokens and metadata minted under a previous client identity before the new one is written
func invalidateTokensOnClientChange(storage *TokenStorage, newClientID, newClientSecret any) {
	existing, _ := readOAuthJSON(storage.ClientInfoPath()).(*jsonObject)
	if existing == nil {
		return
	}
	oldClientID := existing.get("client_id")
	if !jsonTruthy(oldClientID) {
		return
	}
	if pyScalarEqual(oldClientID, newClientID) && pyScalarEqual(pyOr(existing.get("client_secret"), nil), pyOr(newClientSecret, nil)) {
		return
	}
	removed := false
	for _, path := range []string{storage.TokensPath(), storage.MetaPath()} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			mcpLogger().Warn(fmt.Sprintf("MCP OAuth '%s': could not remove stale %s after client change: %v", storage.serverName, filepath.Base(path), err))
			continue
		}
		removed = true
	}
	if removed {
		mcpLogger().Warn(fmt.Sprintf("MCP OAuth '%s': configured OAuth client changed (client_id %s -> %s); discarded tokens minted under "+
			"the previous client. Re-authorize with: icebath mcp login %s", storage.serverName, pyRepr(jsonToPlain(oldClientID)), pyRepr(newClientID), storage.serverName))
	}
}

// Decoded JSON value with exact numbers turned into the plain types pyRepr renders
func jsonToPlain(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return int(i)
		}
		f, _ := strconv.ParseFloat(string(n), 64)
		return f
	}
	return v
}

// Persists a configured pre-registered client so no dynamic registration runs
func maybePreregisterClient(storage *TokenStorage, cfg map[string]any, md *OAuthClientMetadata) error {
	clientID := cfg["client_id"]
	if !pyTruthy(clientID) {
		return nil
	}
	invalidateTokensOnClientChange(storage, clientID, cfg["client_secret"])
	portValue, ok := cfg["_resolved_port"]
	if !ok {
		return errors.New("KeyError: '_resolved_port'")
	}
	port, err := pyIntStrict(portValue)
	if err != nil {
		return err
	}
	info := map[string]any{
		"client_id":      clientID,
		"redirect_uris":  []any{resolveRedirectURI(cfg, port)},
		"grant_types":    stringsToAny(md.GrantTypes),
		"response_types": stringsToAny(md.ResponseTypes),
	}
	if md.TokenEndpointAuthMethod != nil {
		info["token_endpoint_auth_method"] = *md.TokenEndpointAuthMethod
	}
	for _, key := range []string{"client_secret", "client_name", "scope"} {
		if pyTruthy(cfg[key]) {
			info[key] = cfg[key]
		}
	}
	validated, err := validateClientInfo(info)
	if err != nil {
		return err
	}
	if err := writeOAuthJSON(storage.ClientInfoPath(), validated.dump()); err != nil {
		return err
	}
	mcpLogger().Debug(fmt.Sprintf("Pre-registered client_id=%s for '%s'", pyStr(clientID), storage.serverName))
	return nil
}

// Strings as a generic list
func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// Actionable next step for a registration refusal or missing registration endpoint, false to keep the original text
func HumanizeOAuthRegistrationError(serverName, msg, serverURL string) (string, bool) {
	lowered := strings.ToLower(msg)
	if strings.HasPrefix(msg, discoveryContextLead) {
		return "", false
	}
	mentionsRegistration := strings.Contains(lowered, "regist") || strings.Contains(lowered, "dcr") || strings.Contains(lowered, "dynamic client")
	if strings.Contains(msg, "404") && (strings.Contains(lowered, "not found") || strings.Contains(lowered, "/register")) && mentionsRegistration {
		return fmt.Sprintf("'%s' does not support automatic client registration — the registration request returned "+
			"404 (typical for Google's hosted Gmail/Drive MCP servers and other providers without RFC 7591 dynamic "+
			"client registration). Create an OAuth client for this provider and add it under config.yaml as "+
			"`oauth: {client_id: ..., client_secret: ...}`, then re-run:\n  icebath mcp login %s", serverName, serverName), true
	}
	trimmed := strings.TrimSpace(lowered)
	looksLikeRegistration := (strings.Contains(msg, "403") || strings.Contains(lowered, "forbidden")) &&
		(mentionsRegistration || trimmed == "forbidden" || trimmed == "403 forbidden" || trimmed == "http 403: forbidden" ||
			(strings.Contains(msg, "403") && strings.Contains(lowered, "forbidden")))
	if !looksLikeRegistration {
		return "", false
	}
	if isFigmaRemoteMCP(serverName, serverURL) {
		return fmt.Sprintf("'%s' is Figma's remote MCP — DCR is allowlisted by exact client_name "+
			"(\"%s\" and \"Codex\" work; most other names 403). Icebath defaults to "+
			"client_name: %s automatically. If you set oauth.client_name yourself, "+
			"change it to one of those, or clear it and re-run:\n  icebath mcp login %s", serverName, figmaDCRClientName, pyReprStr(figmaDCRClientName), serverName), true
	}
	return fmt.Sprintf("'%s' only allows pre-approved OAuth clients — it rejected client registration (403), so no "+
		"browser flow can start. Options: set oauth.client_name to a name the provider allowlists, add a "+
		"pre-registered client (oauth: {client_id: ..., client_secret: ...}), or use the provider's stdio / "+
		"API-key / local server instead.", serverName), true
}

// Copies the oauth block, applies provider defaults and opens the server's token storage
func prepareOAuthConfig(ctx context.Context, serverName, serverURL string, oauthConfig map[string]any) (map[string]any, *TokenStorage) {
	cfg := make(map[string]any, len(oauthConfig))
	for k, v := range oauthConfig {
		cfg[k] = v
	}
	ApplyOAuthProviderDefaults(cfg, serverName, serverURL)
	return cfg, NewTokenStorage(ctx, serverName, "")
}

// oauthProviderOptions are the shared provider constructor arguments resolved from an oauth block
type oauthProviderOptions struct {
	ServerURL         string
	ClientMetadata    *OAuthClientMetadata
	Storage           *TokenStorage
	RedirectHandler   func(ctx context.Context, authorizationURL string) error
	CallbackHandler   func(ctx context.Context) (*AuthorizationCodeResult, error)
	TokenUserAgent    string
	OAuthFlow         string
	ClientMetadataURL string
	HTTP              HTTPOptions
}

// Resolves the callback port, metadata and pre-registration into provider options, in that order
func buildProviderOptions(ctx context.Context, cfg map[string]any, storage *TokenStorage, sshProxyHint bool) (*oauthProviderOptions, error) {
	port, err := configureCallbackPort(ctx, cfg, storage)
	if err != nil {
		return nil, err
	}
	md, err := buildClientMetadata(cfg)
	if err != nil {
		return nil, err
	}
	if err := maybePreregisterClient(storage, cfg, md); err != nil {
		return nil, err
	}
	redirectURI := ""
	if sshProxyHint && pyTruthy(cfg["redirect_uri"]) {
		redirectURI = pyStr(cfg["redirect_uri"])
	}
	redirectHost := ""
	if pyTruthy(cfg["redirect_host"]) {
		redirectHost = pyStr(cfg["redirect_host"])
	}
	timeoutValue, present := cfg["timeout"]
	if !present {
		timeoutValue = 300
	}
	timeout, err := pyFloatStrict(timeoutValue)
	if err != nil {
		return nil, err
	}
	cimdURL, _ := cfg["_cimd_url"].(string)
	flowName := "browser"
	if v, ok := cfg["flow"]; ok {
		flowName = pyStr(v)
	}
	return &oauthProviderOptions{
		ClientMetadata:    md,
		Storage:           storage,
		RedirectHandler:   makeRedirectHandler(port, redirectURI, redirectHost),
		CallbackHandler:   makeCallbackWaiter(port, cimdURL, time.Duration(timeout*float64(time.Second))),
		TokenUserAgent:    tokenRequestUserAgent(cfg),
		OAuthFlow:         flowName,
		ClientMetadataURL: cimdURL,
	}, nil
}

// OAuth authenticator for a remote MCP server whose requests share the transport's TLS and proxy settings
func BuildOAuthAuth(ctx context.Context, serverName, serverURL string, oauthConfig map[string]any, transport HTTPOptions) (*OAuthProvider, error) {
	cfg, storage := prepareOAuthConfig(ctx, serverName, serverURL, oauthConfig)
	if !isInteractive(ctx) && !storage.HasCachedTokens() {
		return nil, &OAuthNonInteractiveError{fmt.Sprintf("MCP OAuth for '%s': non-interactive environment and no cached tokens found. The OAuth flow "+
			"requires browser authorization. Run `icebath mcp login %s` interactively first to complete "+
			"initial authorization, then cached tokens will be reused.", serverName, serverName)}
	}
	opts, err := buildProviderOptions(ctx, cfg, storage, true)
	if err != nil {
		return nil, err
	}
	opts.ServerURL = serverURL
	opts.HTTP = transport
	return newOAuthProvider(*opts)
}
