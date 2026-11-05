package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	pluginOAuthSection        = "plugin_oauth"
	pluginOAuthExpiryMarginMS = 120_000
	pluginOAuthTokenTimeout   = 10 * time.Second
)

var (
	pluginOAuthClientErrorCodes           = map[string]bool{"invalid_client": true, "unauthorized_client": true, "invalid_scope": true}
	pluginOAuthIDRe                       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\n?$`)
	pluginOAuthEnvRe                      = regexp.MustCompile(`^[A-Z][A-Z0-9_]*\n?$`)
	pluginOAuthHostRe                     = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z](?:[a-z0-9-]*[a-z0-9])?\n?$`)
	pluginOAuthOut              io.Writer = os.Stdout
	pluginOAuthNowMS                      = func() int64 { return time.Now().UnixMilli() }
)

// OAuthClient is one oauth entry of a plugin manifest holding variable names and never their values
type OAuthClient struct {
	ID              string
	AuthorizeURL    string
	TokenURL        string
	Scopes          []string
	ClientIDEnv     string
	Hosts           []string
	ClientSecretEnv string
	AuthorizeParams [][2]string
}

// Fingerprint hashes every field so a grant is used only with the entry it was made for
func (c OAuthClient) Fingerprint() string {
	list := func(items []string) string {
		quoted := make([]string, len(items))
		for i, s := range items {
			quoted[i] = pyJSONQuote(s)
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	}
	params := make([]string, len(c.AuthorizeParams))
	for i, kv := range c.AuthorizeParams {
		params[i] = list([]string{kv[0], kv[1]})
	}
	text := "{" + strings.Join([]string{
		`"authorize_params": [` + strings.Join(params, ", ") + "]",
		`"authorize_url": ` + pyJSONQuote(c.AuthorizeURL),
		`"client_id_env": ` + pyJSONQuote(c.ClientIDEnv),
		`"client_secret_env": ` + pyJSONQuote(c.ClientSecretEnv),
		`"hosts": ` + list(c.Hosts),
		`"id": ` + pyJSONQuote(c.ID),
		`"scopes": ` + list(c.Scopes),
		`"token_url": ` + pyJSONQuote(c.TokenURL),
	}, ", ") + "}"
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Parsed URL of a string, nil for a non string or a URL with a bad IPv6 literal or port
func pluginOAuthParseURL(raw any) *url.URL {
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	parsed, err := url.Parse(s)
	if err != nil {
		return nil
	}
	if port := parsed.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			return nil
		}
	}
	return parsed
}

// Problems of an authorize or token URL, absolute https without user info or fragment
func pluginOAuthURLProblems(where string, raw any) []string {
	parsed := pluginOAuthParseURL(raw)
	if parsed == nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return []string{where + ": must be an absolute URL without user info or fragment"}
	}
	if parsed.Scheme != "https" {
		return []string{where + ": must use https"}
	}
	return nil
}

// A variable icebath reads itself, a provider key or platform secret, never a plugin client variable
func pluginOAuthOwnedEnv(name string) bool {
	if IsRegisteredEnvName(name) {
		return true
	}
	for _, p := range Profiles() {
		for _, v := range p.KeyEnv {
			if v == name {
				return true
			}
		}
	}
	return false
}

// Defects of one oauth entry, one string per problem
func pluginOAuthEntryProblems(entry map[string]any, where string) []string {
	var problems []string
	if id, ok := entry["id"].(string); !ok || !pluginOAuthIDRe.MatchString(id) {
		problems = append(problems, fmt.Sprintf("%s.id: %s must be lowercase letters, digits and dashes", where, pyRepr(entry["id"])))
	}
	for _, field := range []string{"authorize_url", "token_url"} {
		problems = append(problems, pluginOAuthURLProblems(where+"."+field, entry[field])...)
	}
	scopesOK := false
	if scopes, ok := entry["scopes"].([]any); ok && len(scopes) > 0 {
		scopesOK = true
		for _, s := range scopes {
			if str, ok := s.(string); !ok || pyStripSpace(str) == "" {
				scopesOK = false
			}
		}
	}
	if !scopesOK {
		problems = append(problems, where+".scopes: must be a non-empty list of scope strings")
	}
	for _, spec := range []struct {
		field    string
		required bool
	}{{"client_id_env", true}, {"client_secret_env", false}} {
		name := entry[spec.field]
		str, isStr := name.(string)
		if (spec.required || truthy(name)) && !(isStr && pluginOAuthEnvRe.MatchString(str)) {
			problems = append(problems, fmt.Sprintf("%s.%s: %s must be an UPPER_SNAKE environment variable name", where, spec.field, pyRepr(name)))
		} else if truthy(name) && pluginOAuthOwnedEnv(str) {
			problems = append(problems, fmt.Sprintf("%s.%s: %s is a variable Icebath uses itself; declare the plugin's own", where, spec.field, str))
		}
	}
	hostsOK := false
	if hosts, ok := entry["hosts"].([]any); ok && len(hosts) > 0 {
		hostsOK = true
		for _, h := range hosts {
			if str, ok := h.(string); !ok || !pluginOAuthHostRe.MatchString(str) {
				hostsOK = false
			}
		}
	}
	if !hostsOK {
		problems = append(problems, where+".hosts: must list lowercase DNS host names (no scheme, port, path or IP address)")
	}
	params := entry["authorize_params"]
	if truthy(params) {
		m, ok := params.(map[string]any)
		valid := ok
		for _, v := range m {
			if _, isStr := v.(string); !isStr {
				valid = false
			}
		}
		if !valid {
			problems = append(problems, where+".authorize_params: must map strings to strings")
		}
	}
	return problems
}

// Client built from a validated entry, authorize params sorted by key
func pluginOAuthClientFrom(entry map[string]any) OAuthClient {
	c := OAuthClient{ID: entry["id"].(string), AuthorizeURL: entry["authorize_url"].(string), TokenURL: entry["token_url"].(string),
		ClientIDEnv: entry["client_id_env"].(string)}
	for _, s := range entry["scopes"].([]any) {
		c.Scopes = append(c.Scopes, s.(string))
	}
	for _, h := range entry["hosts"].([]any) {
		c.Hosts = append(c.Hosts, h.(string))
	}
	if s, ok := entry["client_secret_env"].(string); ok {
		c.ClientSecretEnv = s
	}
	if params, ok := entry["authorize_params"].(map[string]any); ok {
		for k, v := range params {
			c.AuthorizeParams = append(c.AuthorizeParams, [2]string{k, v.(string)})
		}
		sort.Slice(c.AuthorizeParams, func(i, j int) bool { return c.AuthorizeParams[i][0] < c.AuthorizeParams[j][0] })
	}
	return c
}

// ParseOAuthClients reads the manifest oauth block into well formed clients and one problem per defect
func ParseOAuthClients(manifest map[string]any) ([]OAuthClient, []string) {
	raw, present := manifest["oauth"]
	if !present || raw == nil {
		return nil, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, []string{"oauth: must be a list of client entries"}
	}
	var clients []OAuthClient
	var problems []string
	for index, item := range entries {
		where := fmt.Sprintf("oauth[%d]", index)
		entry, isMap := item.(map[string]any)
		var entryProblems []string
		if !isMap {
			entryProblems = []string{where + ": must be a mapping"}
		} else {
			entryProblems = pluginOAuthEntryProblems(entry, where)
		}
		if len(entryProblems) == 0 {
			for _, c := range clients {
				if c.ID == entry["id"] {
					entryProblems = []string{fmt.Sprintf("%s.id: %s is declared twice", where, pyRepr(entry["id"]))}
					break
				}
			}
		}
		problems = append(problems, entryProblems...)
		if len(entryProblems) == 0 {
			clients = append(clients, pluginOAuthClientFrom(entry))
		}
	}
	return clients, problems
}

// The plugin's own oauth clients, a malformed block failing loud instead of reading as undeclared
func pluginOAuthDeclaredClients(pluginDir string) ([]OAuthClient, error) {
	manifest := map[string]any{}
	file, err := pm.NativeManifestFile(pluginDir)
	if err != nil {
		return nil, err
	}
	if file != "" {
		if manifest, err = pm.ReadNativeManifest(file); err != nil {
			return nil, err
		}
	}
	clients, problems := ParseOAuthClients(manifest)
	if len(problems) > 0 {
		return nil, &PluginOAuthManifestError{Message: fmt.Sprintf("Plugin '%s' has an invalid oauth: block in plugin.yaml: %s", filepath.Base(pluginDir), strings.Join(problems, "; "))}
	}
	return clients, nil
}

// PluginOAuthManifestError is the ValueError of a malformed oauth block or an ambiguous plugin name
type PluginOAuthManifestError struct{ Message string }

func (e *PluginOAuthManifestError) Error() string { return e.Message }

// PluginKey is the plugin's path under the plugins root it lives in, the store key of its grants
func PluginKey(pluginDir string) string {
	abs, err := filepath.Abs(pluginDir)
	if err != nil {
		abs = filepath.Clean(pluginDir)
	}
	candidates := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		candidates = append(candidates, resolved)
	} else {
		candidates = append(candidates, abs)
	}
	for _, path := range candidates {
		for _, root := range pluginProviderRequestRoots() {
			if pathUnder(path, root) {
				rel, _ := filepath.Rel(root, path)
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.Base(pluginDir)
}

// Authorize and token hosts in order without repeats
func pluginOAuthIdPHosts(c OAuthClient) string {
	var hosts []string
	seen := map[string]bool{}
	for _, raw := range []string{c.AuthorizeURL, c.TokenURL} {
		host := ""
		if u, err := url.Parse(raw); err == nil {
			host = strings.ToLower(u.Hostname())
		}
		if !seen[host] {
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	return strings.Join(hosts, ", ")
}

// OAuthNotice is the install and show text for the declared clients, empty when none are declared
func OAuthNotice(manifest map[string]any) string {
	name := "<plugin>"
	if truthy(manifest["name"]) {
		name = pyStr(manifest["name"])
	}
	clients, _ := ParseOAuthClients(manifest)
	parts := make([]string, len(clients))
	for i, c := range clients {
		parts[i] = fmt.Sprintf("Signs in to %s at %s for %s with `icebath auth add %s:%s` (Icebath stores and refreshes the token; the plugin never receives it)",
			c.ID, pluginOAuthIdPHosts(c), strings.Join(c.Hosts, ", "), name, c.ID)
	}
	return strings.Join(parts, "; ")
}

// OAuthSurface is one line per client for the re-pin consent delta
func OAuthSurface(manifest map[string]any) map[string]bool {
	lines := map[string]bool{}
	clients, _ := ParseOAuthClients(manifest)
	for _, c := range clients {
		lines[fmt.Sprintf("%s (%s -> %s; %s)", c.ID, pluginOAuthIdPHosts(c), strings.Join(c.Hosts, ", "), strings.Join(c.Scopes, " "))] = true
	}
	return lines
}

// Names under requires_env and optional_env
func pluginOAuthDeclaredEnvNames(manifest map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, field := range []string{"requires_env", "optional_env"} {
		items, _ := manifest[field].([]any)
		for _, entry := range items {
			if m, ok := entry.(map[string]any); ok {
				names[pyStr(m["name"])] = true
			} else {
				names[pyStr(entry)] = true
			}
		}
	}
	return names
}

// OAuthAdmissionProblems adds catalog admission checks to the shape, declared variables and no core id clash
func OAuthAdmissionProblems(manifest map[string]any, pluginDir string) []string {
	clients, problems := ParseOAuthClients(manifest)
	declared := pluginOAuthDeclaredEnvNames(manifest)
	core := pluginProviderRequestProviderIDs()
	for _, c := range clients {
		if core[c.ID] {
			problems = append(problems, fmt.Sprintf("oauth id %s is a Icebath sign-in provider; pick another id", pyRepr(c.ID)))
		}
		for _, name := range []string{c.ClientIDEnv, c.ClientSecretEnv} {
			if name != "" && !declared[name] {
				problems = append(problems, fmt.Sprintf("oauth %s: %s must be declared under requires_env or optional_env", pyRepr(c.ID), name))
			}
		}
	}
	if len(clients) > 0 && filepath.Base(pluginDir) == "custom" {
		problems = append(problems, "a plugin named 'custom' cannot declare oauth: `icebath auth add custom:<name>` is a custom provider")
	}
	return problems
}

// Registered domain of a host, three labels kept for a two letter country suffix
func pluginOAuthParent(host string) string {
	labels := strings.Split(host, ".")
	keep := 2
	if len(labels) > 2 && len(labels[len(labels)-1]) == 2 && len(labels[len(labels)-2]) <= 3 {
		keep = 3
	}
	if keep > len(labels) {
		keep = len(labels)
	}
	return strings.Join(labels[len(labels)-keep:], ".")
}

// OAuthAdmissionWarnings flags hosts outside the IdP domains and plugin code naming a token URL
func OAuthAdmissionWarnings(manifest map[string]any, pluginDir string) []string {
	clients, _ := ParseOAuthClients(manifest)
	var warnings []string
	for _, c := range clients {
		parents := map[string]bool{}
		for _, raw := range []string{c.AuthorizeURL, c.TokenURL} {
			host := ""
			if u, err := url.Parse(raw); err == nil {
				host = strings.ToLower(u.Hostname())
			}
			parents[pluginOAuthParent(host)] = true
		}
		for _, host := range c.Hosts {
			if !parents[pluginOAuthParent(host)] {
				warnings = append(warnings, fmt.Sprintf("oauth %s: host %s shares no domain with %s; check the provider owns it", pyRepr(c.ID), host, pluginOAuthIdPHosts(c)))
			}
		}
	}
	urlSet := map[string]bool{}
	for _, c := range clients {
		urlSet[c.TokenURL] = true
	}
	urls := sortedBoolKeys(urlSet)
	var files []string
	_ = filepath.WalkDir(pluginDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".py") {
			files = append(files, path)
		}
		return nil
	})
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.ToValidUTF8(strings.TrimPrefix(string(raw), "\uFEFF"), "\uFFFD")
		rel, _ := filepath.Rel(pluginDir, path)
		for _, u := range urls {
			if strings.Contains(text, u) {
				warnings = append(warnings, fmt.Sprintf("oauth: %s names the token URL %s; Icebath refreshes the grant, so the plugin should not call it", filepath.ToSlash(rel), u))
			}
		}
	}
	return warnings
}

// Sorted keys of a set
func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Stored grant row for a key, nil when absent
func pluginOAuthReadGrant(env PoolEnv, key string) (map[string]any, error) {
	data, err := env.Store.Load()
	if err != nil {
		return nil, err
	}
	sectionMap, _ := data[pluginOAuthSection].(map[string]any)
	row, ok := sectionMap[key].(map[string]any)
	if !ok {
		return nil, nil
	}
	return cloneMap(row), nil
}

// ForgetPluginGrants drops every grant of a plugin key from the active profile
func ForgetPluginGrants(plugin string) error {
	env := pkceEnvFn()
	unlock, err := env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := env.Store.Load()
	if err != nil {
		return err
	}
	sectionMap, ok := data[pluginOAuthSection].(map[string]any)
	if !ok {
		return nil
	}
	var stale []string
	for k := range sectionMap {
		if i := strings.LastIndex(k, ":"); (i >= 0 && k[:i] == plugin) || (i < 0 && k == plugin) {
			stale = append(stale, k)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	for _, k := range stale {
		delete(sectionMap, k)
	}
	return env.Store.Save(data)
}

// Replaces one grant, nil removing it, under the auth store lock
func pluginOAuthWriteGrant(env PoolEnv, key string, row map[string]any) error {
	unlock, err := env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := env.Store.Load()
	if err != nil {
		return err
	}
	sectionMap := section(data, pluginOAuthSection)
	if row == nil {
		delete(sectionMap, key)
	} else {
		sectionMap[key] = row
	}
	return env.Store.Save(data)
}

func pluginOAuthAuthError(key, message, code string) *AuthError {
	return newAuthErr(key, key+": "+message, code, false)
}

// Declared client variable from the secret scope, an auth error naming it when unset
func pluginOAuthEnv(env PoolEnv, name, code, key string) (string, error) {
	value := strings.TrimSpace(env.scope().GetSecretStr(name, ""))
	if value == "" {
		return "", pluginOAuthAuthError(key, name+" is not set. Add it to this profile's .env (the plugin declares it).", code)
	}
	return value, nil
}

// Token endpoint failure keeping an actionable OAuth error code and never echoing the body
func pluginOAuthTokenError(key string, status int, payload map[string]any) *AuthError {
	code := strings.TrimSpace(pyStrOrBlank(payload["error"]))
	if oauthGrantDeadCodes[code] || pluginOAuthClientErrorCodes[code] {
		return pluginOAuthAuthError(key, fmt.Sprintf("the token endpoint refused the request (HTTP %d, %s).", status, code), code)
	}
	return pluginOAuthAuthError(key, fmt.Sprintf("the token endpoint failed with HTTP %d.", status), "oauth_refresh_failed")
}

// Python str of a value, empty for None and other falsy values
func pyStrOrBlank(v any) string {
	if !truthy(v) {
		return ""
	}
	return pyStr(v)
}

// Refuses a code exchange whose granted scope string omits a declared scope
func pluginOAuthRequireGrantedScopes(c OAuthClient, payload map[string]any, key string) error {
	granted, ok := payload["scope"].(string)
	if !ok || pyStripSpace(granted) == "" {
		return nil
	}
	have := map[string]bool{}
	for _, s := range strings.Fields(granted) {
		have[s] = true
	}
	var missing []string
	for _, s := range c.Scopes {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return pluginOAuthAuthError(key, "not every permission was granted, so nothing was saved. Missing: "+strings.Join(missing, ", "), "oauth_scope_not_granted")
	}
	return nil
}

// Token request reading the client credentials right before each POST, redirects never followed
func pluginOAuthTokenRequest(env PoolEnv, c OAuthClient, key string) func(map[string]string) (map[string]any, error) {
	return func(fields map[string]string) (map[string]any, error) {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		clientID, err := pluginOAuthEnv(env, c.ClientIDEnv, "oauth_client_id_missing", key)
		if err != nil {
			return nil, err
		}
		form.Set("client_id", clientID)
		if c.ClientSecretEnv != "" {
			secret, err := pluginOAuthEnv(env, c.ClientSecretEnv, "oauth_client_secret_missing", key)
			if err != nil {
				return nil, err
			}
			form.Set("client_secret", secret)
		}
		var client http.Client
		if env.HTTP != nil {
			client = *env.HTTP
		} else {
			client = *oauthHTTPClient(env, pluginOAuthTokenTimeout, defaultOAuthVerify())
		}
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := oauthPostForm(env.ctx(), &client, c.TokenURL, form, map[string]string{"Accept": "application/json"})
		if err != nil {
			return nil, pluginOAuthAuthError(key, fmt.Sprintf("the token request failed (%s).", pkceTransportErrorName(err)), "oauth_refresh_failed")
		}
		payload := map[string]any{}
		if decoded, err := resp.JSON(); err == nil {
			if m, ok := decoded.(map[string]any); ok {
				payload = m
			}
		}
		if resp.Status < 200 || resp.Status >= 300 {
			return nil, pluginOAuthTokenError(key, resp.Status, payload)
		}
		if fields["grant_type"] == "authorization_code" {
			if err := pluginOAuthRequireGrantedScopes(c, payload, key); err != nil {
				return nil, err
			}
		}
		return payload, nil
	}
}

// PKCE config of a client whose token request posts the declared client credentials
func pluginOAuthPKCEConfig(env PoolEnv, c OAuthClient, key string, timeout float64) (OAuthPKCEConfig, error) {
	clientID, err := pluginOAuthEnv(env, c.ClientIDEnv, "oauth_client_id_missing", key)
	if err != nil {
		return OAuthPKCEConfig{}, err
	}
	cfg := NewOAuthPKCEConfig(clientID, c.AuthorizeURL, c.TokenURL)
	cfg.Scopes = append([]string(nil), c.Scopes...)
	cfg.ExtraAuthorizeParams = map[string]string{}
	for _, kv := range c.AuthorizeParams {
		cfg.ExtraAuthorizeParams[kv[0]] = kv[1]
	}
	cfg.Label = key
	cfg.TokenRequest = pluginOAuthTokenRequest(env, c, key)
	cfg.TimeoutSeconds = 180
	if timeout != 0 {
		cfg.TimeoutSeconds = timeout
	}
	return cfg, nil
}

// Plugin name of a grant key, the part before the last colon
func pluginOAuthPluginOf(key string) string {
	if i := strings.LastIndex(key, ":"); i >= 0 {
		return key[:i]
	}
	return key
}

func pluginOAuthRevoked(c OAuthClient, key, code string) error {
	return newProviderNotSignedIn(fmt.Sprintf("The %s sign-in for plugin '%s' was revoked (%s). Run `icebath auth add %s` to sign in again.",
		c.ID, pluginOAuthPluginOf(key), code, key))
}

// The grant row when it is signed in, not revoked and made for this exact client entry
func pluginOAuthUsableGrant(row map[string]any, c OAuthClient, key string) (map[string]any, error) {
	plugin := pluginOAuthPluginOf(key)
	if row == nil {
		return nil, newProviderNotSignedIn(fmt.Sprintf("Not signed in to %s for plugin '%s' in this Icebath profile. Run `icebath auth add %s`.", c.ID, plugin, key))
	}
	if truthy(row["dead"]) {
		return nil, pluginOAuthRevoked(c, key, pyStr(row["dead"]))
	}
	if fp, _ := row["fingerprint"].(string); fp != c.Fingerprint() {
		return nil, newProviderNotSignedIn(fmt.Sprintf("Plugin '%s' changed its oauth entry '%s' since you signed in. Run `icebath auth add %s` to sign in again.", plugin, c.ID, key))
	}
	return row, nil
}

// Python int of a stored number or numeric string
func pluginOAuthInt(v any) (int64, error) {
	switch t := v.(type) {
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case float64:
		return int64(t), nil
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, nil
		}
		f, err := t.Float64()
		return int64(f), err
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(strings.ReplaceAll(t, "_", "")), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyRepr(t))
		}
		return n, nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))
}

// Access token while it outlives the expiry margin, empty otherwise
func pluginOAuthLiveToken(row map[string]any) (string, error) {
	token := pyStrOrBlank(row["access_token"])
	expires, present := row["expires_at_ms"]
	live := !present || expires == nil
	if !live {
		n, err := pluginOAuthInt(expires)
		if err != nil {
			return "", err
		}
		live = n > pluginOAuthNowMS()+pluginOAuthExpiryMarginMS
	}
	if token != "" && live {
		return token, nil
	}
	return "", nil
}

// Rotates the grant under the caller's auth lock, a dead grant code recorded without its tokens
func pluginOAuthRefresh(env PoolEnv, c OAuthClient, key string, row map[string]any) (string, error) {
	refreshToken := pyStrOrBlank(row["refresh_token"])
	if refreshToken == "" {
		return "", pluginOAuthRevoked(c, key, "no_refresh_token")
	}
	cfg, err := pluginOAuthPKCEConfig(env, c, key, 0)
	if err != nil {
		return "", err
	}
	fields, err := pkcePostToken(env, key, cfg, map[string]string{"grant_type": "refresh_token", "refresh_token": refreshToken}, "oauth_refresh_failed")
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) && oauthGrantDeadCodes[authErr.Code] {
			if werr := pluginOAuthWriteGrant(env, key, map[string]any{"dead": authErr.Code, "fingerprint": row["fingerprint"]}); werr != nil {
				return "", werr
			}
			return "", pluginOAuthRevoked(c, key, authErr.Code)
		}
		return "", err
	}
	merged := cloneMap(row)
	for k, v := range fields {
		merged[k] = v
	}
	if err := pluginOAuthWriteGrant(env, key, merged); err != nil {
		return "", err
	}
	return pyStr(fields["access_token"]), nil
}

// Live access token for the plugin's grant, refreshed under the lock after re-reading a peer's rotation
func pluginOAuthBearer(env PoolEnv, pluginDir string, c OAuthClient) (string, error) {
	key := PluginKey(pluginDir) + ":" + c.ID
	row, err := pluginOAuthReadGrant(env, key)
	if err != nil {
		return "", err
	}
	if row, err = pluginOAuthUsableGrant(row, c, key); err != nil {
		return "", err
	}
	if token, err := pluginOAuthLiveToken(row); err != nil || token != "" {
		return token, err
	}
	unlock, err := env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return "", err
	}
	defer unlock()
	if row, err = pluginOAuthReadGrant(env, key); err != nil {
		return "", err
	}
	if row, err = pluginOAuthUsableGrant(row, c, key); err != nil {
		return "", err
	}
	if token, err := pluginOAuthLiveToken(row); err != nil || token != "" {
		return token, err
	}
	return pluginOAuthRefresh(env, c, key, row)
}

// PluginOAuthPermissionError is the PermissionError of a bearer request to a URL carrying user info
type PluginOAuthPermissionError struct{ Message string }

func (e *PluginOAuthPermissionError) Error() string { return e.Message }

func (e *PluginOAuthPermissionError) Unwrap() error { return fs.ErrPermission }

// PluginOrigin is one scheme, host and port a plugin bearer may travel to
type PluginOrigin struct {
	Scheme string
	Host   string
	Port   int
}

// PluginOAuthResolver returns the auth headers and request URL for one origin checked request
type PluginOAuthResolver func(rawURL string) (map[string]string, string, error)

// Origins and bearer resolver of a provider the plugin declares under oauth, ok false when it declares none
func pluginOAuthSpec(pluginDir, provider string) ([]PluginOrigin, PluginOAuthResolver, bool, error) {
	clients, err := pluginOAuthDeclaredClients(pluginDir)
	if err != nil {
		return nil, nil, false, err
	}
	for _, c := range clients {
		if c.ID != provider {
			continue
		}
		client := c
		resolve := func(rawURL string) (map[string]string, string, error) {
			if parsed, err := url.Parse(rawURL); err == nil && parsed.User != nil {
				return nil, "", &PluginOAuthPermissionError{Message: fmt.Sprintf("Refusing to send the %s sign-in to a URL with user info", client.ID)}
			}
			token, err := pluginOAuthBearer(pkceEnvFn(), pluginDir, client)
			if err != nil {
				return nil, "", err
			}
			return map[string]string{"Authorization": "Bearer " + token}, rawURL, nil
		}
		return pluginOAuthOriginsFn(client), resolve, true, nil
	}
	return nil, nil, false, nil
}

// Origin set a plugin bearer may travel to, replaceable the way hermes tests patch _origins
var pluginOAuthOriginsFn = pluginOAuthOrigins

// HTTPS default port origin of each declared host
func pluginOAuthOrigins(c OAuthClient) []PluginOrigin {
	seen := map[string]bool{}
	var out []PluginOrigin
	for _, h := range c.Hosts {
		if !seen[h] {
			seen[h] = true
			out = append(out, PluginOrigin{Scheme: "https", Host: h, Port: 443})
		}
	}
	return out
}

// Installed plugin directory and its oauth client, found false when either is missing
func pluginOAuthInstalledClient(plugin, oauthID string) (string, OAuthClient, bool, error) {
	dir, found, err := findPluginEntryDir(plugin)
	if err != nil {
		return "", OAuthClient{}, false, &PluginOAuthManifestError{Message: err.Error()}
	}
	if !found || dir == "" {
		return "", OAuthClient{}, false, nil
	}
	clients, err := pluginOAuthDeclaredClients(dir)
	if err != nil {
		return "", OAuthClient{}, false, err
	}
	for _, c := range clients {
		if c.ID == oauthID {
			return dir, c, true, nil
		}
	}
	return "", OAuthClient{}, false, nil
}

// Exit error carrying a SystemExit message with status one
func pluginOAuthExit(message string) error {
	return &OAuthLoginExitError{Code: 1, Err: errors.New(message)}
}

// Timeout and no browser flags of an auth add invocation
func pluginOAuthAddFlags(args []string) (float64, bool, error) {
	timeout, noBrowser := 0.0, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-browser":
			noBrowser = true
		case a == "--timeout" && i+1 < len(args):
			i++
			a = "--timeout=" + args[i]
			fallthrough
		case strings.HasPrefix(a, "--timeout="):
			v, err := strconv.ParseFloat(strings.TrimPrefix(a, "--timeout="), 64)
			if err != nil {
				return 0, false, &OAuthLoginExitError{Code: 2, Err: fmt.Errorf("argument --timeout: invalid float value: %s", pyRepr(strings.TrimPrefix(a, "--timeout=")))}
			}
			timeout = v
		}
	}
	return timeout, noBrowser, nil
}

// Browser sign in storing the grant with the client fingerprint
func pluginOAuthCLIAdd(env PoolEnv, args []string, c OAuthClient, key string) (bool, error) {
	if pluginOAuthUnattended() {
		return false, pluginOAuthExit(fmt.Sprintf("`icebath auth add %s` signs you in through a browser; run it in your own terminal, not from a gateway, cron job or agent session.", key))
	}
	timeout, noBrowser, err := pluginOAuthAddFlags(args)
	if err != nil {
		return false, err
	}
	secret := ""
	if c.ClientSecretEnv != "" {
		secret = " and the client secret"
	}
	authorizeHost, tokenHost := "", ""
	if u, err := url.Parse(c.AuthorizeURL); err == nil {
		authorizeHost = strings.ToLower(u.Hostname())
	}
	if u, err := url.Parse(c.TokenURL); err == nil {
		tokenHost = strings.ToLower(u.Hostname())
	}
	fmt.Fprintf(pluginOAuthOut, "Plugin '%s' signs in to %s at %s. The sign-in code%s go to %s; the token is sent only to %s.\n",
		pluginOAuthPluginOf(key), c.ID, authorizeHost, secret, tokenHost, strings.Join(c.Hosts, ", "))
	cfg, err := pluginOAuthPKCEConfig(env, c, key, timeout)
	if err != nil {
		return false, pluginOAuthExit("Login failed: " + err.Error())
	}
	fields, err := pkceLogin(env, pluginOAuthOut, key, cfg, !noBrowser)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return false, pluginOAuthExit("Login failed: " + authErr.Error())
		}
		return false, err
	}
	row := cloneMap(fields)
	row["fingerprint"] = c.Fingerprint()
	if err := pluginOAuthWriteGrant(env, key, row); err != nil {
		return false, err
	}
	fmt.Fprintf(pluginOAuthOut, "Signed in to %s for plugin '%s'; the grant is stored in this Icebath profile.\n", c.ID, pluginOAuthPluginOf(key))
	return true, nil
}

// Prints the grant state and the access token expiry when known
func pluginOAuthCLIStatus(env PoolEnv, c OAuthClient, key string) (bool, error) {
	row, err := pluginOAuthReadGrant(env, key)
	if err != nil {
		return false, err
	}
	if _, err := pluginOAuthUsableGrant(row, c, key); err != nil {
		if !isProviderNotSignedIn(err) {
			return false, err
		}
		state := "sign-in needed"
		if row == nil {
			state = "logged out"
		} else if truthy(row["dead"]) {
			state = "revoked"
		}
		fmt.Fprintf(pluginOAuthOut, "%s: %s\n  %s\n", key, state, err.Error())
		return true, nil
	}
	text := fmt.Sprintf("%s: logged in\n  hosts: %s", key, strings.Join(c.Hosts, ", "))
	if expires := row["expires_at_ms"]; truthy(expires) {
		n, err := pluginOAuthInt(expires)
		if err != nil {
			return false, err
		}
		text += "\n  access token valid until " + time.UnixMilli(n).UTC().Format("2006-01-02T15:04:05") + "+00:00"
	}
	fmt.Fprintln(pluginOAuthOut, text)
	return true, nil
}

// Forces a grant rotation under the auth store lock
func pluginOAuthCLIRefresh(env PoolEnv, c OAuthClient, key string) (bool, error) {
	err := func() error {
		unlock, err := env.Store.Lock(AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		row, err := pluginOAuthReadGrant(env, key)
		if err != nil {
			return err
		}
		if row, err = pluginOAuthUsableGrant(row, c, key); err != nil {
			return err
		}
		_, err = pluginOAuthRefresh(env, c, key, row)
		return err
	}()
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) || isProviderNotSignedIn(err) {
			return false, pluginOAuthExit("Refresh failed: " + err.Error())
		}
		return false, err
	}
	fmt.Fprintf(pluginOAuthOut, "%s: refreshed\n", key)
	return true, nil
}

// Removes the grant of a plugin client
func pluginOAuthCLILogout(env PoolEnv, c OAuthClient, key string) (bool, error) {
	if err := pluginOAuthWriteGrant(env, key, nil); err != nil {
		return false, err
	}
	fmt.Fprintf(pluginOAuthOut, "Signed out of %s for plugin '%s'; the grant was removed from this profile.\n", c.ID, pluginOAuthPluginOf(key))
	return true, nil
}

// Stored grant key a logout means, the installed plugin key first then the typed name
func pluginOAuthLogoutKey(env PoolEnv, plugin, oauthID string) (string, bool, error) {
	var names []string
	dir, found, err := findPluginEntryDir(plugin)
	if err != nil {
		return "", false, err
	}
	if found && dir != "" {
		names = append(names, PluginKey(dir)+":"+oauthID)
	}
	names = append(names, plugin+":"+oauthID)
	for _, key := range names {
		row, err := pluginOAuthReadGrant(env, key)
		if err != nil {
			return "", false, err
		}
		if row != nil {
			return key, true, nil
		}
	}
	return "", false, nil
}

// Auth add, status, refresh or logout of a plugin oauth client, handled false when the provider names none
func pluginOAuthHandleAuthAction(action string, args []string, provider string) (owned, handled bool, err error) {
	plugin, oauthID, sep := strings.Cut(provider, ":")
	switch action {
	case "add", "status", "refresh", "logout":
	default:
		return false, false, nil
	}
	if !sep || plugin == "" || oauthID == "" || plugin == "custom" {
		return false, false, nil
	}
	env := pkceEnvFn()
	if action == "logout" {
		key, found, err := pluginOAuthLogoutKey(env, plugin, oauthID)
		if err != nil {
			return false, true, err
		}
		if found {
			if err := pluginOAuthWriteGrant(env, key, nil); err != nil {
				return false, true, err
			}
			fmt.Fprintf(pluginOAuthOut, "Signed out of %s; the grant was removed from this profile.\n", key)
			return true, true, nil
		}
	}
	dir, client, found, err := pluginOAuthInstalledClient(plugin, oauthID)
	if err != nil {
		var manifestErr *PluginOAuthManifestError
		if errors.As(err, &manifestErr) {
			return false, true, pluginOAuthExit(err.Error())
		}
		return false, true, err
	}
	if !found {
		return false, false, nil
	}
	key := PluginKey(dir) + ":" + client.ID
	switch action {
	case "add":
		owned, err = pluginOAuthCLIAdd(env, args, client, key)
	case "status":
		owned, err = pluginOAuthCLIStatus(env, client, key)
	case "refresh":
		owned, err = pluginOAuthCLIRefresh(env, client, key)
	default:
		owned, err = pluginOAuthCLILogout(env, client, key)
	}
	return owned, true, err
}
