package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// ---- user configured provider definitions (hermes_cli/providers.py) ----

// ProviderDef is a resolved provider merged from models.dev, the overlays, plugin profiles and user config
type ProviderDef struct {
	ID           string
	Name         string
	Transport    string
	KeyEnv       []string
	BaseURL      string
	BaseURLEnv   string
	IsAggregator bool
	AuthType     string
	Doc          string
	Source       string
}

// Shared user-config definition for providers and custom_providers entries
func userPdef(pid, name, baseURL, keyEnv, transport string) *ProviderDef {
	if transport == "" {
		transport = "openai_chat"
	}
	p := &ProviderDef{ID: pid, Name: name, Transport: transport, BaseURL: baseURL, AuthType: "api_key", Source: "user-config"}
	if keyEnv != "" {
		p.KeyEnv = []string{keyEnv}
	}
	return p
}

// Provider definition from a providers block that carries an endpoint, nil for built in tuning blocks
func resolveUserProvider(name string, userConfig map[string]any) *ProviderDef {
	if len(userConfig) == 0 {
		return nil
	}
	entry, ok := userConfig[name].(map[string]any)
	if !ok {
		return nil
	}
	baseURL := firstTruthyString(entry["api"], entry["url"], entry["base_url"])
	if baseURL == "" {
		return nil
	}
	display := cfgString(entry["name"])
	if display == "" {
		display = name
	}
	keyEnv := firstTruthyString(entry["key_env"], entry["api_key_env"])
	transport := cfgString(entry["transport"])
	if transport == "" {
		transport = "openai_chat"
	}
	return userPdef(name, display, baseURL, keyEnv, transport)
}

// First value that renders as a non empty string
func firstTruthyString(values ...any) string {
	for _, v := range values {
		if s := cfgString(v); s != "" {
			return s
		}
	}
	return ""
}

// CustomProviderSlug is the stable custom identity, keyed providers entries by their config key
func CustomProviderSlug(displayName, providerKey string) string {
	identity := strings.TrimSpace(providerKey)
	if identity == "" {
		identity = strings.TrimSpace(displayName)
	}
	normalized := strings.ReplaceAll(strings.ToLower(identity), " ", "-")
	if strings.HasPrefix(normalized, "custom:") {
		return normalized
	}
	return "custom:" + normalized
}

// CustomProviderAliases returns every current and legacy identity accepted for one endpoint
func CustomProviderAliases(displayName, providerKey string) map[string]bool {
	aliases := map[string]bool{}
	for _, value := range []string{displayName, providerKey} {
		raw := strings.ToLower(strings.TrimSpace(value))
		if raw == "" {
			continue
		}
		normalized := strings.ReplaceAll(raw, " ", "-")
		aliases[raw] = true
		aliases[normalized] = true
		aliases[CustomProviderSlug(normalized, "")] = true
		if strings.HasPrefix(normalized, "custom:") {
			if suffix := normalized[len("custom:"):]; suffix != "" {
				aliases[suffix] = true
				aliases["custom:"+normalized] = true
			}
		}
	}
	return aliases
}

// Provider definition from the legacy custom_providers list, bare custom healing onto the first entry
func resolveCustomProvider(name string, customProviders []any) *ProviderDef {
	requested := strings.ToLower(strings.TrimSpace(name))
	if requested == "" || len(customProviders) == 0 {
		return nil
	}
	var firstValid *ProviderDef
	for _, raw := range customProviders {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		displayName := strings.TrimSpace(cfgString(entry["name"]))
		apiURL := strings.TrimSpace(firstTruthyString(entry["base_url"], entry["url"], entry["api"]))
		if displayName == "" || apiURL == "" {
			continue
		}
		providerKey := strings.TrimSpace(cfgString(entry["provider_key"]))
		pdef := userPdef(CustomProviderSlug(displayName, providerKey), displayName, apiURL, strings.TrimSpace(cfgString(entry["key_env"])), "")
		if firstValid == nil {
			firstValid = pdef
		}
		if CustomProviderAliases(displayName, providerKey)[requested] {
			return pdef
		}
	}
	if requested == "custom" && firstValid != nil {
		return firstValid
	}
	return nil
}

// Provider definition a name selects from the legacy custom_providers list
func ResolveCustomProvider(name string, customProviders []any) *ProviderDef {
	return resolveCustomProvider(name, customProviders)
}

// ---- credential ownership of custom entries (agent/custom_entry_pool.py) ----

var declaredCredentialKeys = []string{"api_key", "key_env", "api_key_env", "key_cmd"}

// EntryDeclaresCredential reports whether a custom entry names its own credential
func EntryDeclaresCredential(entry map[string]any) bool {
	if entry == nil {
		return false
	}
	if v, ok := entry["declares_credential"]; ok {
		return truthy(v)
	}
	for _, k := range declaredCredentialKeys {
		if strings.TrimSpace(cfgString(entry[k])) != "" {
			return true
		}
	}
	return false
}

// Custom provider name normalized for use as a pool key suffix
func normalizeCustomPoolName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "-")
}

func normPoolURL(u any) string { return strings.TrimRight(strings.TrimSpace(cfgString(u)), "/") }

type namedEntry struct {
	name  string
	entry map[string]any
}

// Normalized name and entry pairs from the merged custom provider config view
func iterCustomProviders(cfg map[string]any) []namedEntry {
	var out []namedEntry
	for _, entry := range CompatibleCustomProviders(cfg) {
		if name, ok := entry["name"].(string); ok {
			out = append(out, namedEntry{normalizeCustomPoolName(name), entry})
		}
	}
	return out
}

func customEntryNameAliases(normName string, entry map[string]any) map[string]bool {
	aliases := map[string]bool{normName: true}
	if pk := normalizeCustomPoolName(cfgString(entry["provider_key"])); pk != "" {
		aliases[pk] = true
	}
	return aliases
}

func requestedCustomNameAliases(providerName string) map[string]bool {
	normalized := normalizeCustomPoolName(providerName)
	aliases := map[string]bool{}
	if normalized == "" {
		return aliases
	}
	aliases[normalized] = true
	if strings.HasPrefix(normalized, "custom:") {
		if suffix := normalizeCustomPoolName(normalized[len("custom:"):]); suffix != "" {
			aliases[suffix] = true
		}
	}
	return aliases
}

// Durable providers key slug first, then the legacy custom prefixed name
func poolKeysForCustomEntry(normName string, entry map[string]any) []string {
	var keys []string
	providerKey := normalizeCustomPoolName(cfgString(entry["provider_key"]))
	legacy := ""
	if normName != "" {
		legacy = "custom:" + normName
	}
	for _, key := range []string{providerKey, legacy} {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized == "" {
			continue
		}
		dup := false
		for _, k := range keys {
			if k == normalized {
				dup = true
			}
		}
		if !dup {
			keys = append(keys, normalized)
		}
	}
	return keys
}

// Named entry the provider name resolves to in the custom provider config view
func namedCustomEntry(providerName string, cfg map[string]any) *namedEntry {
	aliases := requestedCustomNameAliases(providerName)
	if len(aliases) == 0 {
		return nil
	}
	for _, ne := range iterCustomProviders(cfg) {
		for alias := range customEntryNameAliases(ne.name, ne.entry) {
			if aliases[alias] {
				found := ne
				return &found
			}
		}
	}
	return nil
}

// Configured custom entry that owns a base URL, by name first and then by URL
func customEntryForEndpoint(baseURL, providerName string, cfg map[string]any) *namedEntry {
	if baseURL == "" {
		return nil
	}
	u := normPoolURL(baseURL)
	if providerName != "" {
		if named := namedCustomEntry(providerName, cfg); named != nil {
			entryURL := normPoolURL(named.entry["base_url"])
			if entryURL != "" && entryURL != u {
				return nil
			}
			return named
		}
	}
	for _, ne := range iterCustomProviders(cfg) {
		if u != "" && normPoolURL(ne.entry["base_url"]) == u {
			found := ne
			return &found
		}
	}
	return nil
}

// CustomEntryForEndpoint is the normalized name and entry of the custom provider owning a base url
func CustomEntryForEndpoint(baseURL, providerName string, cfg map[string]any) (string, map[string]any, bool) {
	if m := customEntryForEndpoint(baseURL, providerName, cfg); m != nil {
		return m.name, m.entry, true
	}
	return "", nil, false
}

// Pool keys to try for a custom endpoint, durable slug then legacy custom name
func customProviderPoolKeyCandidates(baseURL, providerName string, cfg map[string]any) []string {
	match := customEntryForEndpoint(baseURL, providerName, cfg)
	if match == nil {
		return nil
	}
	return poolKeysForCustomEntry(match.name, match.entry)
}

// ---- key_cmd token minting (agent/command_token_source.py) ----

const (
	tokenRefreshLeeway  = 60 * time.Second
	mintTimeout         = 15 * time.Second
	noTTLRefreshSeconds = 900 * time.Second
	// Display label source for a key minted by a command
	CredentialSourceKeyCmd = "key-cmd"
)

// CommandTokenError is a key_cmd that failed to produce a usable token
type CommandTokenError struct{ Message string }

func (e *CommandTokenError) Error() string { return e.Message }

// CommandTokenSource mints a bearer token by running a command and caches it until shortly before expiry
type CommandTokenSource struct {
	Command   string
	Label     string
	home      string
	fileEnv   func() map[string]string
	mu        sync.Mutex
	token     string
	expiresAt time.Time
	now       func() time.Time
	run       func(ctx context.Context, command string, env []string) ([]byte, int, error)
}

// Runs the command and returns the token plus its ttl, zero when the helper advertised none
func (s *CommandTokenSource) mint(ctx context.Context) (string, time.Duration, error) {
	runCtx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	stdout, code, err := s.run(runCtx, s.Command, s.childEnv())
	if runCtx.Err() == context.DeadlineExceeded {
		return "", 0, &CommandTokenError{fmt.Sprintf("key_cmd for provider %q timed out after %ds", s.Label, int(mintTimeout.Seconds()))}
	}
	if err != nil && code == 0 {
		return "", 0, &CommandTokenError{fmt.Sprintf("key_cmd for provider %q could not be executed: %v", s.Label, err)}
	}
	if code != 0 {
		return "", 0, &CommandTokenError{fmt.Sprintf("key_cmd for provider %q exited %d. Run that provider's key_cmd manually to see why "+
			"(e.g. `databricks auth login` if its OAuth session expired).", s.Label, code)}
	}
	out := string(stdout)
	if strings.TrimSpace(out) == "" {
		return "", 0, &CommandTokenError{fmt.Sprintf("key_cmd for provider %q produced no output", s.Label)}
	}
	if strings.HasPrefix(strings.TrimLeft(out, " \t\r\n"), "{") {
		var payload map[string]any
		if json.Unmarshal([]byte(out), &payload) == nil && payload != nil {
			token := strings.TrimSpace(cfgString(payload["access_token"]))
			if token == "" {
				return "", 0, &CommandTokenError{fmt.Sprintf("key_cmd for provider %q returned JSON without an 'access_token' field", s.Label)}
			}
			if ttl, ok := floatValue(payload["expires_in"]); ok && ttl > 0 {
				return token, time.Duration(ttl * float64(time.Second)), nil
			}
			for _, field := range []string{"expiry", "expiresOn"} {
				if deadline, ok := ParseISOTimestamp(payload[field]); ok {
					if remaining := time.Until(time.Unix(0, int64(deadline*float64(time.Second)))); remaining > 0 {
						return token, remaining, nil
					}
				}
			}
			return token, 0, nil
		}
	}
	token := strings.TrimSpace(out)
	if strings.Contains(token, "\n") {
		return "", 0, &CommandTokenError{fmt.Sprintf("key_cmd for provider %q printed multiple lines; it must "+
			"print only the token (or JSON with an 'access_token' field)", s.Label)}
	}
	return token, 0, nil
}

// Child environment of the served profile, its .env overlaid on the process environment
func (s *CommandTokenSource) childEnv() []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.Index(kv, "="); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	if s.fileEnv != nil {
		for k, v := range s.fileEnv() {
			env[k] = v
		}
	}
	if s.home != "" {
		env[home.EnvHome] = s.home
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Runs a shell command and returns its stdout and exit code
func runShellCommand(ctx context.Context, command string, env []string) ([]byte, int, error) {
	cmd, err := pyShellCommand(ctx, command)
	if err != nil {
		return nil, 0, err
	}
	cmd.Env = env
	cmd.Stdin = nil
	stdout, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return stdout, exitErr.ExitCode(), err
		}
		return stdout, 0, err
	}
	return stdout, 0, nil
}

// CacheIdentity is the stable catalog identity so token rotation never mints on cache reads
func (s *CommandTokenSource) CacheIdentity() string { return "cmd:" + s.Command }

// Token returns the cached bearer or mints a fresh one when the cache is stale
func (s *CommandTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.expiresAt) {
		return s.token, nil
	}
	token, ttl, err := s.mint(ctx)
	if err != nil {
		return "", err
	}
	s.token = token
	if ttl > 0 {
		lease := ttl - tokenRefreshLeeway
		if lease < 5*time.Second {
			lease = 5 * time.Second
		}
		s.expiresAt = s.now().Add(lease)
	} else {
		s.expiresAt = s.now().Add(noTTLRefreshSeconds)
	}
	return token, nil
}

// BuildCommandTokenSource returns a per request token source for a key_cmd or nil when unset
func BuildCommandTokenSource(keyCmd, providerLabel, h string, fileEnv func() map[string]string) *CommandTokenSource {
	command := strings.TrimSpace(keyCmd)
	if command == "" {
		return nil
	}
	if providerLabel == "" {
		providerLabel = "custom"
	}
	return &CommandTokenSource{Command: command, Label: providerLabel, home: h, fileEnv: fileEnv, now: time.Now, run: runShellCommand}
}

// ---- a declared key the profile scope does not have (runtime_provider_missing_key.py) ----

// Transports that never send the resolved key
var keylessAPIModes = map[APIMode]bool{modeCodexAppServer: true, modeBedrockConverse: true}

func declaredKeyEnv(entry map[string]any) string {
	if entry == nil {
		return ""
	}
	return strings.TrimSpace(firstTruthyString(entry["key_env"], entry["api_key_env"]))
}

// Label and key_env of the config block that declared a key variable for this custom endpoint
func (r *resolver) declaredCustomKey(requestedProvider string, rt *Runtime, modelCfg map[string]any) (string, string) {
	baseURL := rt.BaseURL
	name := ""
	if requestedProvider != "" && requestedProvider != "auto" && requestedProvider != "custom" {
		name = requestedProvider
	}
	var match *namedEntry
	if name != "" && strings.HasPrefix(rt.Source, "custom_provider:") {
		match = namedCustomEntry(name, r.cfg)
	} else {
		match = customEntryForEndpoint(baseURL, name, r.cfg)
	}
	if match != nil {
		if keyEnv := declaredKeyEnv(match.entry); keyEnv != "" {
			label := cfgString(match.entry["name"])
			if label == "" {
				label = match.name
			}
			return fmt.Sprintf("custom provider '%s'", label), keyEnv
		}
	}
	cfgURL := strings.TrimRight(strings.TrimSpace(cfgString(modelCfg["base_url"])), "/")
	if cfgURL != "" && cfgURL == strings.TrimRight(strings.TrimSpace(baseURL), "/") {
		if keyEnv := declaredKeyEnv(modelCfg); keyEnv != "" {
			return "the custom endpoint in config.yaml (model.key_env)", keyEnv
		}
	}
	return "", ""
}

// Reports whether the resolver serves a profile other than the process's own
func (r *resolver) servesRoutedProfile() bool {
	return home.Key(r.home) != home.Key(home.Process())
}

// Sentence naming the profile whose .env must hold the variable and the command that writes it there
func (r *resolver) whereToSetKey(envVar string) string {
	profile := home.ProfileNameFor(r.home)
	if profile == "" {
		profile = "default"
	}
	selector := ""
	if profile != "default" {
		selector = "-p " + profile + " "
	}
	envPath := home.Display(r.home) + "/.env"
	hint := fmt.Sprintf("Add it to profile '%s': run `icebath %sconfig set %s <your key>` (stored in %s).", profile, selector, envVar, envPath)
	if r.servesRoutedProfile() && os.Getenv(envVar) != "" {
		hint = fmt.Sprintf("%s is set in this server's environment, but profile '%s' reads keys only from its own .env. %s", envVar, profile, hint)
	}
	return hint
}

// Where to set hint for a registry provider's missing key while a named profile is active, else empty
func (r *resolver) namedProfileKeyHint(envVars []string) string {
	var names []string
	for _, v := range envVars {
		if strings.TrimSpace(v) != "" {
			names = append(names, v)
		}
	}
	if len(names) == 0 {
		return ""
	}
	if profile := home.ProfileNameFor(r.home); profile == "" || profile == "default" {
		return ""
	}
	return " " + r.whereToSetKey(names[0])
}

// Refuses a synthesized placeholder key when a routed profile's declared key variable resolves to nothing
func (r *resolver) raiseIfDeclaredKeyMissing(requestedProvider string, rt *Runtime, modelCfg map[string]any, explicitAPIKey string) error {
	if rt.Provider != "custom" || rt.APIKey != NoKeyPlaceholder {
		return nil
	}
	if strings.TrimSpace(explicitAPIKey) != "" || keylessAPIModes[rt.APIMode] {
		return nil
	}
	if !r.servesRoutedProfile() {
		return nil
	}
	label, keyEnv := r.declaredCustomKey(strings.ToLower(strings.TrimSpace(requestedProvider)), rt, modelCfg)
	if keyEnv == "" || strings.TrimSpace(r.secret(keyEnv)) != "" {
		return nil
	}
	provider := requestedProvider
	if provider == "" {
		provider = "custom"
	}
	return newAuthErr(provider, fmt.Sprintf("No API key for %s: it reads %s, which is not set for this profile, so the "+
		"request was not sent. %s", label, keyEnv, r.whereToSetKey(keyEnv)), "missing_api_key", false)
}

// ---- named custom providers (runtime_provider_custom.py) ----

func normalizeCustomProviderName(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), " ", "-")
}

func normalizeBaseURLForMatch(value any) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(cfgString(value)), "/"))
}

// Exported base URL match form, trimmed, lowercased and without trailing slashes
func NormalizeBaseURLForMatch(value any) string { return normalizeBaseURLForMatch(value) }

// Endpoint compared for identity, scheme and host fold case while the path does not
func routeKey(value any) string { return NormalizeRouteBaseURL(strings.TrimSpace(cfgString(value))) }

// RouteKey is the endpoint compared for identity with scheme and host folded
func RouteKey(value any) string { return routeKey(value) }

func clean(value any) string { return strings.TrimSpace(cfgString(value)) }

// Credential named by key_env or api_key_env on a config block, warning when it resolves to nothing
func (r *resolver) keyEnvSecret(entry map[string]any, label string) string {
	keyEnv := clean(firstTruthyString(entry["key_env"], entry["api_key_env"]))
	if keyEnv == "" {
		return ""
	}
	value := strings.TrimSpace(r.secret(keyEnv))
	if value == "" {
		slog.Default().Warn(fmt.Sprintf("%s: key_env %s is set but the variable is empty/unset — unless another credential "+
			"is configured for it, the request carries the placeholder no-key-required and the endpoint will reject it", label, keyEnv))
	}
	return value
}

// model.key_env for a bare custom runtime only when the base URL is the configured model.base_url
func (r *resolver) modelCfgKeyEnvFor(modelCfg map[string]any, baseURL string) string {
	cfgBaseURL := strings.TrimRight(clean(modelCfg["base_url"]), "/")
	if cfgBaseURL == "" || cfgBaseURL != strings.TrimRight(clean(baseURL), "/") {
		return ""
	}
	return r.keyEnvSecret(modelCfg, "model")
}

func entryURL(entry map[string]any) string {
	return firstTruthyString(entry["api"], entry["url"], entry["base_url"])
}

// Exported entry URL read, api before url before base_url
func EntryURL(entry map[string]any) string { return entryURL(entry) }

// Copies explicit boolean per model capabilities onto the runtime
func liftModelCapabilities(entry map[string]any, model string, result map[string]any) {
	caps := filterCapabilities(entry["capabilities"])
	if models, ok := entry["models"].(map[string]any); ok && model != "" {
		if mc, ok := models[model].(map[string]any); ok {
			for k, v := range filterCapabilities(mc) {
				caps[k] = v
			}
		}
	}
	if len(caps) > 0 {
		result["capabilities"] = caps
	}
}

// Copies a validated extra_headers mapping onto the runtime, values may carry credentials so never log
func liftExtraHeaders(entry map[string]any, result map[string]any) {
	if headers := NormalizeExtraHeaders(entry["extra_headers"]); len(headers) > 0 {
		result["extra_headers"] = headers
	}
}

// Copies the optional fields shared by providers and legacy custom_providers entries
func liftCommonCustomFields(entry map[string]any, result map[string]any, providerKey, keyEnv string, apiMode APIMode) {
	if keyEnv != "" {
		result["key_env"] = keyEnv
	}
	if providerKey != "" {
		result["provider_key"] = providerKey
	}
	if eb, ok := entry["extra_body"].(map[string]any); ok {
		result["extra_body"] = copyAnyMap(eb)
	}
	liftExtraHeaders(entry, result)
	if apiMode != "" {
		result["api_mode"] = string(apiMode)
	}
	liftModelCapabilities(entry, "", result)
}

// Raw names map to custom providers only when they are not canonical built ins
func (r *resolver) shadowedByBuiltin(requestedNorm string) bool {
	if requestedNorm == "custom" || strings.HasPrefix(requestedNorm, "custom:") {
		return false
	}
	canonical, err := r.resolveProvider(requestedNorm, "", "", false)
	if err != nil {
		return false
	}
	return strings.ToLower(strings.TrimSpace(canonical)) == requestedNorm
}

// Scans the keyed providers mapping for the requested name and returns the resolved entry and config key
func (r *resolver) matchNewStyleProvider(requestedNorm string, providers map[string]any, resolveCredentials bool) (map[string]any, string, bool) {
	for _, epName := range sortedKeys(providers) {
		entry, ok := providers[epName].(map[string]any)
		if !ok || !IsProviderEnabled(entry) {
			continue
		}
		name := cfgString(entry["name"])
		if name == "" {
			name = epName
		}
		if !CustomProviderAliases(name, epName)[requestedNorm] {
			continue
		}
		baseURL := entryURL(entry)
		if baseURL == "" {
			continue
		}
		keyEnv := clean(firstTruthyString(entry["key_env"], entry["api_key_env"]))
		apiKey := ""
		if keyEnv != "" && resolveCredentials {
			apiKey = strings.TrimSpace(r.secret(keyEnv))
		}
		if apiKey == "" {
			apiKey = clean(entry["api_key"])
		}
		result := map[string]any{"name": name, "base_url": strings.TrimSpace(baseURL), "api_key": apiKey, "model": cfgString(entry["default_model"])}
		if keyCmd := clean(entry["key_cmd"]); keyCmd != "" {
			result["key_cmd"] = keyCmd
		}
		liftCommonCustomFields(entry, result, clean(epName), keyEnv, r.parseAPIMode(firstTruthyString(entry["api_mode"], entry["transport"])))
		return result, epName, true
	}
	return nil, "", false
}

// Scans the legacy custom_providers list for the requested name and returns the resolved entry and raw entry
func (r *resolver) matchLegacyCustomProvider(requestedNorm string, customProviders []map[string]any) (map[string]any, map[string]any, bool) {
	for _, entry := range customProviders {
		name, nameOK := entry["name"].(string)
		baseURL, urlOK := entry["base_url"].(string)
		if !nameOK || !urlOK {
			continue
		}
		providerKey := clean(entry["provider_key"])
		if !CustomProviderAliases(name, providerKey)[requestedNorm] {
			continue
		}
		result := map[string]any{"name": strings.TrimSpace(name), "base_url": strings.TrimSpace(baseURL), "api_key": clean(entry["api_key"])}
		if modelName := clean(entry["model"]); modelName != "" {
			result["model"] = modelName
		}
		liftCommonCustomFields(entry, result, providerKey, clean(entry["key_env"]), r.parseAPIMode(entry["api_mode"]))
		return result, entry, true
	}
	return nil, nil, false
}

// ConfigPath names where one custom entry lives in config
type ConfigPath struct {
	Section string
	Key     string
	Index   int
}

// Resolved entry and config path for the first usable match, providers first then the legacy list
func (r *resolver) selectNamedCustomProvider(requestedProvider string, cfg map[string]any, resolveCredentials bool) (map[string]any, *ConfigPath) {
	requestedNorm := normalizeCustomProviderName(requestedProvider)
	if requestedNorm == "" || requestedNorm == "auto" || r.shadowedByBuiltin(requestedNorm) {
		return nil, nil
	}
	if cfg == nil {
		cfg = r.cfg
	}
	if providers, ok := cfg["providers"].(map[string]any); ok {
		if found, key, ok := r.matchNewStyleProvider(requestedNorm, providers, resolveCredentials); ok {
			return found, &ConfigPath{Section: "providers", Key: key}
		}
	}
	if _, isDict := cfg["custom_providers"].(map[string]any); isDict {
		slog.Default().Warn("custom_providers in config.yaml is a dict, not a list. Each entry must be prefixed with '-' in YAML. Run 'icebath doctor' for details.")
		return nil, nil
	}
	customProviders := CompatibleCustomProviders(cfg)
	if len(customProviders) == 0 {
		return nil, nil
	}
	found, raw, ok := r.matchLegacyCustomProvider(requestedNorm, customProviders)
	if !ok {
		return nil, nil
	}
	return found, compatibleEntryPath(cfg, raw)
}

// Where a compatible view entry lives in config, the first raw entry normalizing to it
func compatibleEntryPath(cfg map[string]any, entry map[string]any) *ConfigPath {
	if key := cfgString(entry["provider_key"]); key != "" {
		if providers, ok := cfg["providers"].(map[string]any); ok {
			for _, k := range sortedKeys(providers) {
				if strings.TrimSpace(k) == key {
					return &ConfigPath{Section: "providers", Key: k}
				}
			}
			return nil
		}
	}
	if legacy, ok := asList(cfg["custom_providers"]); ok {
		for i, raw := range legacy {
			if sameAnyMap(NormalizeCustomProviderEntry(raw, ""), entry) {
				return &ConfigPath{Section: "custom_providers", Index: i}
			}
		}
	}
	return nil
}

// Deep equality of two JSON shaped maps
func sameAnyMap(a, b map[string]any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ab) == string(bb)
}

// Resolved named endpoint entry or nil, reading credentials unless told not to
func (r *resolver) getNamedCustomProvider(requestedProvider string, cfg map[string]any, resolveCredentials bool) map[string]any {
	found, _ := r.selectNamedCustomProvider(requestedProvider, cfg, resolveCredentials)
	return found
}

// GetNamedCustomProvider is the named endpoint entry with credentials resolved, nil when none matches
func GetNamedCustomProvider(ctx context.Context, name string) map[string]any {
	return modelsResolver(ctx).getNamedCustomProvider(name, nil, true)
}

// Named endpoint entry a provider name selects in cfg without reading credentials, nil when none matches
func NamedCustomProviderEntry(ctx context.Context, name string, cfg map[string]any) map[string]any {
	return newResolver(ctx, config.Tree(cfg), config.NewSecrets(home.Dir(ctx)), home.Dir(ctx), homeCredentialSource(home.Dir(ctx))).getNamedCustomProvider(name, cfg, false)
}

// Resolved named endpoint entry with credentials, nil when no entry matches
func NamedCustomProvider(ctx context.Context, h, name string) (map[string]any, error) {
	r, cfg, err := strictHomeResolver(ctx, h)
	if err != nil {
		return nil, err
	}
	return r.getNamedCustomProvider(name, cfg, true), nil
}

// NamedCustomProviderConfigPath is the config path of the one entry the runtime routes a provider through
func (r *resolver) NamedCustomProviderConfigPath(requestedProvider string, cfg map[string]any) *ConfigPath {
	_, path := r.selectNamedCustomProvider(requestedProvider, cfg, false)
	return path
}

// Config path of the entry the active home runtime routes a provider through, reading no credential
func NamedCustomProviderPath(ctx context.Context, name string, cfg map[string]any) *ConfigPath {
	return newResolver(ctx, config.Tree(cfg), config.NewSecrets(home.Dir(ctx)), home.Dir(ctx), homeCredentialSource(home.Dir(ctx))).NamedCustomProviderConfigPath(name, cfg)
}

// CustomProviderConfigPathForURL is the config path of the entry a bare custom route at a URL takes its key from
func CustomProviderConfigPathForURL(baseURL any, cfg map[string]any) *ConfigPath {
	u := strings.TrimRight(strings.TrimSpace(cfgString(baseURL)), "/")
	if u == "" {
		return nil
	}
	if _, isDict := cfg["custom_providers"].(map[string]any); isDict {
		return nil
	}
	for _, entry := range CompatibleCustomProviders(cfg) {
		if strings.TrimRight(strings.TrimSpace(cfgString(entry["base_url"])), "/") == u {
			return compatibleEntryPath(cfg, entry)
		}
	}
	return nil
}

// HasNamedCustomProvider reports whether config defines an entry matching the request
func (r *resolver) HasNamedCustomProvider(requestedProvider string) bool {
	return r.getNamedCustomProvider(requestedProvider, nil, true) != nil
}

// CodexModelProviderID is the codex model_providers key for a configured named custom provider or empty
func (r *resolver) CodexModelProviderID(requestedProvider string) string {
	norm := normalizeCustomProviderName(requestedProvider)
	if norm == "" || norm == "custom" {
		return ""
	}
	entry := r.getNamedCustomProvider(requestedProvider, nil, true)
	if entry == nil {
		return ""
	}
	slug := CustomProviderSlug(cfgString(entry["name"]), cfgString(entry["provider_key"]))
	if i := strings.Index(slug, ":"); i >= 0 {
		return slug[i+1:]
	}
	return ""
}

// First enabled entry matching the predicate as its canonical custom slug, preferring a held one
func findCustomIdentity(matches func(map[string]any) bool, cfg map[string]any, prefer string) string {
	if cfg == nil {
		return ""
	}
	first := ""
	owned := map[string]bool{}
	if providers, ok := cfg["providers"].(map[string]any); ok {
		for _, epName := range sortedKeys(providers) {
			entry, ok := providers[epName].(map[string]any)
			if !ok || !IsProviderEnabled(entry) {
				continue
			}
			slug := CustomProviderSlug(epName, epName)
			if entryURL(entry) != "" {
				owned[slug] = true
			}
			if matches(entry) {
				if prefer == "" || slug == prefer {
					return slug
				}
				if first == "" {
					first = slug
				}
			}
		}
	}
	for _, entry := range CompatibleCustomProviders(cfg) {
		name, ok := entry["name"].(string)
		if !ok || strings.TrimSpace(name) == "" || !matches(entry) {
			continue
		}
		slug := CustomProviderSlug(name, cfgString(entry["provider_key"]))
		if owned[slug] && !truthy(entry["provider_key"]) {
			continue
		}
		if prefer == "" || slug == prefer {
			return slug
		}
		if first == "" {
			first = slug
		}
	}
	return first
}

// FindCustomProviderIdentity maps an endpoint URL back to its canonical custom menu key
func FindCustomProviderIdentity(baseURL string, cfg map[string]any) string {
	target := routeKey(baseURL)
	if target == "" {
		return ""
	}
	return findCustomIdentity(func(entry map[string]any) bool { return routeKey(entryURL(entry)) == target }, cfg, "")
}

func modelIDMatches(value any, target string) bool {
	s, ok := value.(string)
	return ok && strings.ToLower(strings.TrimSpace(s)) == target
}

// FindCustomProviderIdentityByModel maps a model id back to the custom entry that serves it
func FindCustomProviderIdentityByModel(model string, cfg map[string]any) string {
	target := strings.ToLower(strings.TrimSpace(model))
	if target == "" {
		return ""
	}
	return findCustomIdentity(func(entry map[string]any) bool { return entryServesModel(entry, target) }, cfg, "")
}

func entryServesModel(entry map[string]any, target string) bool {
	for _, key := range []string{"model", "default_model"} {
		if modelIDMatches(entry[key], target) {
			return true
		}
	}
	switch models := entry["models"].(type) {
	case map[string]any:
		for mid := range models {
			if strings.ToLower(strings.TrimSpace(mid)) == target {
				return true
			}
		}
	case []any:
		for _, item := range models {
			if m, ok := item.(map[string]any); ok {
				if modelIDMatches(firstTruthyString(m["id"], m["name"]), target) {
					return true
				}
				continue
			}
			if modelIDMatches(item, target) {
				return true
			}
		}
	}
	return false
}

// RoutedCustomIdentity names the configured entry a bare custom pick on a URL or model resolves to
func RoutedCustomIdentity(baseURL, model string, cfg map[string]any, held string) string {
	u := routeKey(baseURL)
	target := strings.ToLower(strings.TrimSpace(model))
	onURL := func(entry map[string]any) bool { return u == "" || routeKey(entryURL(entry)) == u }
	if target != "" {
		if id := findCustomIdentity(func(e map[string]any) bool { return onURL(e) && entryServesModel(e, target) }, cfg, held); id != "" {
			return id
		}
	}
	if u != "" {
		return findCustomIdentity(onURL, cfg, "")
	}
	return ""
}

// Menu identity that owns a base URL, a configured entry else the managed local server
func (r *resolver) endpointIdentity(baseURL string) string {
	if identity := FindCustomProviderIdentity(baseURL, r.cfg); identity != "" {
		return identity
	}
	if endpointURL, _, ok := managedLocalStateEndpoint(r.home); ok && routeKey(baseURL) == routeKey(endpointURL) {
		return "llamacpp"
	}
	return ""
}

// CanonicalCustomIdentity recovers the durable menu identity for a bare custom provider
func (r *resolver) CanonicalCustomIdentity(baseURL, configProvider, model, requested string) string {
	identity := "custom"
	if baseURL != "" {
		identity = r.SessionProviderIdentity("custom", requested, baseURL, "")
	}
	if identity != "custom" {
		return identity
	}
	if model != "" {
		if id := FindCustomProviderIdentityByModel(model, r.cfg); id != "" {
			return id
		}
	}
	candidate := strings.TrimSpace(configProvider)
	if candidate == "" {
		candidate = strings.TrimSpace(cfgString(r.modelConfig()["provider"]))
	}
	if candidate == "" {
		candidate = strings.TrimSpace(os.Getenv("ICEBATH_INFERENCE_PROVIDER"))
	}
	candidateNorm := normalizeCustomProviderName(candidate)
	if candidateNorm == "" || candidateNorm == "custom" || candidateNorm == "auto" || candidateNorm == "openrouter" {
		return ""
	}
	entry := r.getNamedCustomProvider(candidate, nil, true)
	if entry == nil {
		return ""
	}
	if identity := FindCustomProviderIdentity(cfgString(entry["base_url"]), r.cfg); identity != "" {
		return identity
	}
	return CustomProviderSlug(candidateNorm, "")
}

// SessionProviderIdentity is the requested entry while it still serves the URL, else the endpoint owner, else the adapter
func (r *resolver) SessionProviderIdentity(provider, requestedProvider, baseURL, model string) string {
	adapter := clean(provider)
	if strings.ToLower(adapter) != "custom" {
		return adapter
	}
	requested := clean(requestedProvider)
	u := routeKey(baseURL)
	lowered := strings.ToLower(requested)
	if u != "" && lowered != "" && lowered != "custom" && lowered != "auto" {
		if entry := r.getNamedCustomProvider(requested, r.cfg, false); entry != nil && routeKey(entryURL(entry)) == u {
			return requested
		}
	}
	if u != "" {
		if id := r.endpointIdentity(clean(baseURL)); id != "" {
			return id
		}
		return adapter
	}
	if id := r.CanonicalCustomIdentity("", "", clean(model), ""); id != "" {
		return id
	}
	return adapter
}

// IsRoutableProvider reports whether a provider name currently resolves to a routable route
func (r *resolver) IsRoutableProvider(provider string) bool {
	name := strings.TrimSpace(provider)
	if name == "" || strings.ToLower(name) == "auto" {
		return true
	}
	if strings.ToLower(name) == "custom" {
		return false
	}
	userProviders, _ := r.cfg["providers"].(map[string]any)
	if resolveUserProvider(strings.ToLower(name), userProviders) != nil {
		return true
	}
	if _, ok := lookupProviderDef(NormalizeProvider(name)); ok {
		return true
	}
	var customList []any
	for _, e := range CompatibleCustomProviders(r.cfg) {
		customList = append(customList, e)
	}
	if resolveCustomProvider(name, customList) != nil {
		return true
	}
	return IsLlamaCppAlias(name)
}

// ---- runtime builders ----

// The providers entry that owns a custom endpoint
func (r *resolver) owningEntry(baseURL, providerName string) map[string]any {
	match := customEntryForEndpoint(baseURL, providerName, r.cfg)
	if match == nil {
		return nil
	}
	return match.entry
}

// Declared flag and the entry's own credential for a bare custom route whose URL a providers entry owns
func (r *resolver) owningEntryCredential(baseURL string) (bool, string, *CommandTokenSource) {
	entry := r.owningEntry(baseURL, "")
	if entry == nil || !EntryDeclaresCredential(entry) {
		return false, "", nil
	}
	name := cfgString(entry["name"])
	if name == "" {
		name = "custom"
	}
	key, src := r.entryCredential(entry, name)
	return true, key, src
}

// An entry's own credential, key_cmd first, else the first usable of api_key and key_env
func (r *resolver) entryCredential(entry map[string]any, name string) (string, *CommandTokenSource) {
	if keyCmd := clean(entry["key_cmd"]); keyCmd != "" {
		if src := BuildCommandTokenSource(keyCmd, name, r.home, r.fileEnv); src != nil {
			return "", src
		}
	}
	for _, c := range []string{clean(entry["api_key"]), r.keyEnvSecret(entry, fmt.Sprintf("custom provider '%s'", name))} {
		if hasUsableSecret(c) {
			return c, nil
		}
	}
	return "", nil
}

// Runtime from the first credential pool that owns this custom endpoint, nil when none does
func (r *resolver) tryResolveFromCustomPool(baseURL, providerLabel string, apiModeOverride APIMode, providerName string) *Runtime {
	rawKeys := customProviderPoolKeyCandidates(baseURL, providerName, r.cfg)
	owner := r.owningEntry(baseURL, providerName)
	var candidates []string
	seen := map[string]bool{}
	for _, key := range rawKeys {
		k := strings.ToLower(strings.TrimSpace(key))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		candidates = append(candidates, k)
	}
	for _, poolKey := range candidates {
		cred, ok := r.scopedCredential(poolKey, owner)
		if !ok {
			continue
		}
		poolAPIKey := cred.APIKey
		if poolAPIKey == "" && cred.TokenSource == nil {
			continue
		}
		if poolAPIKey != "" && !hasUsableSecret(poolAPIKey) && isLoopbackHostname(Hostname(baseURL)) {
			poolAPIKey = NoKeyPlaceholder
		}
		mode := apiModeOverride
		if mode == "" {
			mode = detectAPIModeForURL(baseURL)
		}
		if mode == "" {
			mode = ModeChatCompletions
		}
		rt := r.runtime(providerLabel, mode, baseURL, poolAPIKey, "pool:"+poolKey)
		rt.TokenSource = cred.TokenSource
		rt.CredentialPool = cred.Pool
		return rt
	}
	return nil
}

// Credential for a pool key, scoped to the owning custom entry when the source supports it
func (r *resolver) scopedCredential(poolKey string, owner map[string]any) (Credential, bool) {
	if r.creds == nil {
		return Credential{}, false
	}
	if scoped, ok := r.creds.(ScopedCredentialSource); ok {
		return scoped.CredentialForEntry(poolKey, owner)
	}
	return r.creds.Credential(poolKey)
}

func customProviderRequestOverrides(customProvider map[string]any) map[string]any {
	eb, ok := customProvider["extra_body"].(map[string]any)
	if !ok || len(eb) == 0 {
		return nil
	}
	return map[string]any{"extra_body": copyAnyMap(eb)}
}

// Copies model, capabilities, extra headers, and request overrides onto a resolved custom runtime
func applyCustomProviderExtras(customProvider map[string]any, targetModel string, rt *Runtime) {
	modelName := targetModel
	if modelName == "" {
		modelName = cfgString(customProvider["model"])
	}
	if modelName != "" {
		rt.Model = modelName
	}
	result := map[string]any{}
	liftModelCapabilities(customProvider, modelName, result)
	if caps, ok := result["capabilities"].(map[string]bool); ok {
		rt.Capabilities = caps
	}
	if headers, ok := customProvider["extra_headers"].(map[string]string); ok && len(headers) > 0 {
		rt.ExtraHeaders = copyStringMap(headers)
	} else if raw, ok := customProvider["extra_headers"].(map[string]any); ok && len(raw) > 0 {
		rt.ExtraHeaders = NormalizeExtraHeaders(raw)
	}
	if overrides := customProviderRequestOverrides(customProvider); len(overrides) > 0 {
		merged := copyAnyMap(rt.RequestOverrides)
		if merged == nil {
			merged = map[string]any{}
		}
		for k, v := range overrides {
			merged[k] = v
		}
		rt.RequestOverrides = merged
	}
}

func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// LocalRuntimeUnavailableError means the selected model is the managed local runtime and its server is not serving
type LocalRuntimeUnavailableError struct {
	Message   string
	Retryable bool
}

func (e *LocalRuntimeUnavailableError) Error() string { return e.Message }

// ErrorCode names the readiness probe code for the managed server being down
func (e *LocalRuntimeUnavailableError) ErrorCode() string { return "local_runtime_unavailable" }

// Managed llama.cpp runtime, the supervised or detected server, or a typed error
func (r *resolver) resolveLlamacppRuntime(requestedProvider, explicitAPIKey string) (*Runtime, error) {
	if baseURL, apiKey, ok := resolveLlamacppEndpoint(r.ctx, r.cfg, r.home); ok {
		key := strings.TrimSpace(explicitAPIKey)
		if key == "" {
			key = apiKey
		}
		if key == "" {
			key = NoKeyPlaceholder
		}
		rt := r.runtime("custom", ModeChatCompletions, baseURL, key, "local-runtime")
		rt.RequestedProvider = requestedProvider
		return rt, nil
	}
	enabled := false
	if lr, ok := r.cfg["local_runtime"].(map[string]any); ok {
		enabled = truthy(lr["enabled"])
	}
	if enabled {
		return nil, &LocalRuntimeUnavailableError{Message: "The local model server isn't running. It may still be " +
			"starting — try again in a moment, or switch to another model with /model.", Retryable: true}
	}
	return nil, &LocalRuntimeUnavailableError{Message: "The local model server is turned off. Start it from the Icebath " +
		"desktop app or `icebath model` → Local models, or switch to another model with /model."}
}

// Custom runtime with URL detected api mode fallback and the no auth placeholder
func (r *resolver) customRuntime(baseURL, apiKey string, apiMode APIMode, source string) *Runtime {
	if apiMode == "" {
		apiMode = detectAPIModeForURL(baseURL)
	}
	if apiMode == "" {
		apiMode = ModeChatCompletions
	}
	if apiKey == "" {
		apiKey = NoKeyPlaceholder
	}
	return r.runtime("custom", apiMode, baseURL, apiKey, source)
}

// Aliases for direct REST APIs not modeled in the registry
var directAPIBaseURLs = map[string]string{"openai": "https://api.openai.com/v1"}

// Expands provider openai onto custom plus the user's OpenAI endpoint, api.openai.com only as the last resort
func (r *resolver) expandDirectAPIAlias(provider, existingBase string) (string, string) {
	if provider == "" {
		return provider, existingBase
	}
	targetBase, ok := directAPIBaseURLs[strings.ToLower(strings.TrimSpace(provider))]
	if !ok || r.getNamedCustomProvider(provider, nil, true) != nil {
		return provider, existingBase
	}
	base := strings.TrimSpace(existingBase)
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(r.secret("OPENAI_BASE_URL")), "/")
	}
	if base == "" {
		base = targetBase
	}
	return "custom", base
}

// Provider openai expanded onto custom plus the configured OpenAI endpoint, other providers unchanged
func ExpandDirectAPIAlias(ctx context.Context, provider, existingBase string) (string, string) {
	return modelsResolver(ctx).expandDirectAPIAlias(provider, existingBase)
}

// Bare custom plus an explicit base URL, such as a model_aliases direct alias
func (r *resolver) resolveDirectAliasRuntime(requestedProvider, explicitAPIKey, explicitBaseURL string) *Runtime {
	baseURL := strings.TrimRight(strings.TrimSpace(explicitBaseURL), "/")
	explicitKey := strings.TrimSpace(explicitAPIKey)
	if !hasUsableSecret(explicitKey) {
		if pooled := r.tryResolveFromCustomPool(baseURL, "custom", "", ""); pooled != nil {
			pooled.Source = "direct-alias"
			pooled.RequestedProvider = requestedProvider
			return pooled
		}
	}
	declared, entryKey, entrySrc := r.owningEntryCredential(baseURL)
	candidates := []string{explicitKey, r.modelCfgKeyEnvFor(r.modelConfig(), baseURL)}
	if !declared {
		candidates = append(candidates, r.hostGatedEnvKeyCandidates(baseURL, true)...)
	}
	apiKey := ""
	for _, c := range candidates {
		if hasUsableSecret(c) {
			apiKey = c
			break
		}
	}
	if apiKey == "" {
		apiKey = entryKey
	}
	rt := r.customRuntime(baseURL, apiKey, "", "direct-alias")
	rt.RequestedProvider = requestedProvider
	if apiKey == "" && entrySrc != nil {
		rt.TokenSource = TagCredentialSource(entrySrc.Token, CredentialSourceKeyCmd).Func()
		rt.KeyCmd = entrySrc.Command
	}
	return rt
}

// OpenCode family by provider name, else by opencode.ai host
func opencodeFamilyForCustom(requestedProvider, baseURL string) string {
	if family := OpenCodeFamily(requestedProvider); family != "" {
		return family
	}
	if strings.ToLower(Hostname(baseURL)) == "opencode.ai" {
		if strings.Contains(strings.ToLower(baseURL), "/zen/go") {
			return "opencode-go"
		}
		return "opencode-zen"
	}
	return ""
}

// OpencodeFamilyForCustom is the OpenCode family by provider name or opencode.ai host, empty for none
func OpencodeFamilyForCustom(requestedProvider, baseURL string) string {
	return opencodeFamilyForCustom(requestedProvider, baseURL)
}

// Runtime for a llamacpp alias, a bare custom direct alias, or a configured custom entry
func (r *resolver) resolveNamedCustomRuntime(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel string, cfg map[string]any) (*Runtime, error) {
	requestedNorm := strings.ToLower(strings.TrimSpace(requestedProvider))
	var customProvider map[string]any
	if IsLlamaCppAlias(requestedNorm) && explicitBaseURL == "" {
		customProvider = r.getNamedCustomProvider(requestedProvider, nil, true)
		if customProvider == nil {
			return r.resolveLlamacppRuntime(requestedProvider, explicitAPIKey)
		}
	}
	if requestedNorm != "" && requestedNorm != "custom" && r.resolvesToCustom(requestedNorm) {
		requestedNorm = "custom"
	}
	if requestedNorm == "custom" && explicitBaseURL != "" {
		return r.resolveDirectAliasRuntime(requestedProvider, explicitAPIKey, explicitBaseURL), nil
	}
	if customProvider == nil {
		customProvider = r.getNamedCustomProvider(requestedProvider, cfg, true)
	}
	if customProvider == nil {
		return nil, nil
	}
	baseURL := strings.TrimSpace(explicitBaseURL)
	if baseURL == "" {
		baseURL = cfgString(customProvider["base_url"])
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, nil
	}
	explicitKey := strings.TrimSpace(explicitAPIKey)
	entryMode := APIMode(cfgString(customProvider["api_mode"]))
	if !hasUsableSecret(explicitKey) {
		providerName := cfgString(customProvider["provider_key"])
		if providerName == "" {
			providerName = cfgString(customProvider["name"])
		}
		if pooled := r.tryResolveFromCustomPool(baseURL, "custom", entryMode, providerName); pooled != nil {
			applyCustomProviderExtras(customProvider, targetModel, pooled)
			pooled.RequestedProvider = requestedProvider
			return pooled, nil
		}
	}
	name := cfgString(customProvider["name"])
	if name == "" {
		name = requestedProvider
	}
	if name == "" {
		name = "custom"
	}
	apiKey := ""
	var tokenSrc *CommandTokenSource
	if hasUsableSecret(explicitKey) {
		apiKey = explicitKey
	} else {
		apiKey, tokenSrc = r.entryCredential(customProvider, name)
		if apiKey == "" && tokenSrc == nil && !EntryDeclaresCredential(customProvider) {
			for _, c := range r.hostGatedEnvKeyCandidates(baseURL, false) {
				if hasUsableSecret(c) {
					apiKey = c
					break
				}
			}
		}
	}
	sourceName := cfgString(customProvider["name"])
	if sourceName == "" {
		sourceName = requestedProvider
	}
	rt := r.customRuntime(baseURL, apiKey, entryMode, "custom_provider:"+sourceName)
	rt.RequestedProvider = requestedProvider
	if apiKey == "" && tokenSrc != nil {
		rt.TokenSource = TagCredentialSource(tokenSrc.Token, CredentialSourceKeyCmd).Func()
		rt.KeyCmd = tokenSrc.Command
	}
	rt.KeyEnv = cfgString(customProvider["key_env"])
	rt.ProviderKey = cfgString(customProvider["provider_key"])
	applyCustomProviderExtras(customProvider, targetModel, rt)
	if family := opencodeFamilyForCustom(requestedProvider, baseURL); family != "" && entryMode == "" {
		effectiveModel := strings.TrimSpace(targetModel)
		if effectiveModel == "" {
			effectiveModel = strings.TrimSpace(cfgString(customProvider["model"]))
		}
		if effectiveModel == "" {
			effectiveModel = strings.TrimSpace(cfgString(r.modelConfig()["default"]))
		}
		if effectiveModel != "" {
			rt.APIMode = OpenCodeModelMode(family, effectiveModel)
		}
		rt.BaseURL = NormalizeOpenCodeBaseURL(family, rt.APIMode, rt.BaseURL)
	}
	return rt, nil
}
