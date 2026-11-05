package provider

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Mapping keys older releases wrote inside a provider's models mapping
var declaredSentinelKeys = map[string]bool{"__explicit_model_allowlist__": true, "__discovered_model_catalog__": true}

// Configured model ids from a mapping, a list of ids or id and name entries, or one string, deduped case insensitively
func declaredModelIDs(value any) []string {
	var candidates []any
	switch v := value.(type) {
	case string:
		candidates = []any{v}
	case map[string]any:
		for _, k := range sortedKeys(v) {
			if !declaredSentinelKeys[k] {
				candidates = append(candidates, k)
			}
		}
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				candidates = append(candidates, declaredItemID(m))
			} else {
				candidates = append(candidates, item)
			}
		}
	case []string:
		for _, item := range v {
			candidates = append(candidates, item)
		}
	default:
		return []string{}
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		s, ok := candidate.(string)
		if !ok {
			continue
		}
		modelID := pyStripSpace(s)
		if modelID != "" && !seen[strings.ToLower(modelID)] {
			seen[strings.ToLower(modelID)] = true
			ids = append(ids, modelID)
		}
	}
	return ids
}

// Entry id, falling back to its name when the id is blank or missing
func declaredItemID(item map[string]any) any {
	if s, ok := item["id"].(string); ok && pyStripSpace(s) != "" {
		return s
	}
	return item["name"]
}

// Reports whether the entry's models mapping was discovered and persisted by icebath rather than pinned by the user
func entryModelsDiscovered(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	if b, ok := m["models_discovered"].(bool); ok && b {
		return true
	}
	models, ok := m["models"].(map[string]any)
	if !ok {
		return false
	}
	b, ok := models["__discovered_model_catalog__"].(bool)
	return ok && b
}

// Reports whether a models value is an intentional id allowlist, never for metadata mappings or discovered catalogs
func modelsConfigIsAllowlist(value any, discovered bool) bool {
	if discovered {
		return false
	}
	switch v := value.(type) {
	case string:
		return pyStripSpace(v) != ""
	case []any, []string:
		return len(declaredModelIDs(v)) > 0
	}
	return false
}

// Provider definition for a direct model.provider custom endpoint, nil without a base URL
func bareCustomProviderDef(currentBaseURL string) *ProviderDef {
	baseURL := msClean(currentBaseURL)
	if baseURL == "" {
		return nil
	}
	return &ProviderDef{ID: "custom", Name: "Custom endpoint", Transport: "openai_chat", BaseURL: baseURL, AuthType: "api_key", Source: "model-config"}
}

// String form of a config value with falsy values empty and surrounding whitespace removed
func msClean(value any) string {
	if !pyTruthy(value) {
		return ""
	}
	return pyStripSpace(pyStr(value))
}

const hermesModelWarning = "Nous Research Hermes 3 & 4 models are NOT agentic and are not designed " +
	"for use with Hermes Agent. They lack the tool-calling capabilities " +
	"required for agent workflows. Consider using an agentic model instead " +
	"(Claude, GPT, Gemini, DeepSeek, etc.)."

var nousHermesNonAgenticRE = regexp.MustCompile(`(?i)(?:^|[/:])hermes[-_ ]?[34](?:[-_.:]|\n?\z)`)

var opaqueModelPrefixes = []string{"ri.language-model-service..language-model."}

// FormatModelForDisplay strips opaque proxy prefixes from a model id for status output only
func FormatModelForDisplay(modelName string) string {
	for _, prefix := range opaqueModelPrefixes {
		if modelName != "" && strings.HasPrefix(modelName, prefix) {
			if rest := modelName[len(prefix):]; rest != "" {
				return rest
			}
			return modelName
		}
	}
	return modelName
}

// IsNousHermesNonAgentic reports whether a model id is a real Nous Hermes 3 or 4 chat model
func IsNousHermesNonAgentic(modelName string) bool {
	return modelName != "" && nousHermesNonAgenticRE.MatchString(modelName)
}

// Non agentic warning text for a Nous Hermes 3 or 4 chat model, empty for every other model
func checkHermesModelWarning(modelName string) string {
	if IsNousHermesNonAgentic(modelName) {
		return hermesModelWarning
	}
	return ""
}

// ModelIdentity is the vendor slug and family prefix a short alias resolves against
type ModelIdentity struct {
	Vendor string
	Family string
}

// ModelAliases maps short names to a vendor and family resolved against the live catalog
var ModelAliases = map[string]ModelIdentity{
	"sonnet":   {"anthropic", "claude-sonnet"},
	"opus":     {"anthropic", "claude-opus"},
	"haiku":    {"anthropic", "claude-haiku"},
	"claude":   {"anthropic", "claude"},
	"gpt5":     {"openai", "gpt-5"},
	"gpt":      {"openai", "gpt"},
	"codex":    {"openai", "codex"},
	"o3":       {"openai", "o3"},
	"o4":       {"openai", "o4"},
	"gemini":   {"google", "gemini"},
	"deepseek": {"deepseek", "deepseek-chat"},
	"grok":     {"x-ai", "grok"},
	"llama":    {"meta-llama", "llama"},
	"qwen":     {"qwen", "qwen"},
	"minimax":  {"minimax", "minimax"},
	"nemotron": {"nvidia", "nemotron"},
	"kimi":     {"moonshotai", "kimi"},
	"glm":      {"z-ai", "glm"},
	"step":     {"stepfun", "step"},
	"mimo":     {"xiaomi", "mimo"},
	"trinity":  {"arcee-ai", "trinity"},
}

// Vendor and family of a lowercased short alias
func modelAliasIdentity(nameLower string) (string, string, bool) {
	identity, ok := ModelAliases[nameLower]
	return identity.Vendor, identity.Family, ok
}

// DirectAlias is an exact model, provider and endpoint with the endpoint's own credential
type DirectAlias struct {
	Model    string
	Provider string
	BaseURL  string
	APIKey   string
	KeyEnv   string
}

// NamedDirectAlias is one alias entry in table order
type NamedDirectAlias struct {
	Name  string
	Alias DirectAlias
}

// DirectAliasTable is an insertion ordered alias map safe for concurrent use
type DirectAliasTable struct {
	mu     sync.Mutex
	names  []string
	byName map[string]DirectAlias
}

// NewDirectAliasTable builds a table holding the entries in order, a later duplicate replacing the earlier value
func NewDirectAliasTable(entries ...NamedDirectAlias) *DirectAliasTable {
	t := &DirectAliasTable{}
	t.replace(entries)
	return t
}

// Get returns the alias stored under a name
func (t *DirectAliasTable) Get(name string) (DirectAlias, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.byName[name]
	return a, ok
}

// Set stores an alias, keeping the original position of an existing name
func (t *DirectAliasTable) Set(name string, alias DirectAlias) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setLocked(name, alias)
}

func (t *DirectAliasTable) setLocked(name string, alias DirectAlias) {
	if t.byName == nil {
		t.byName = map[string]DirectAlias{}
	}
	if _, ok := t.byName[name]; !ok {
		t.names = append(t.names, name)
	}
	t.byName[name] = alias
}

// Clear removes every entry in place
func (t *DirectAliasTable) Clear() { t.replace(nil) }

// Len counts the entries
func (t *DirectAliasTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.names)
}

// Entries returns a snapshot of the entries in insertion order
func (t *DirectAliasTable) Entries() []NamedDirectAlias {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]NamedDirectAlias, 0, len(t.names))
	for _, name := range t.names {
		out = append(out, NamedDirectAlias{Name: name, Alias: t.byName[name]})
	}
	return out
}

// Swaps the contents in place so holders of the table see the new entries
func (t *DirectAliasTable) replace(entries []NamedDirectAlias) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.names, t.byName = nil, map[string]DirectAlias{}
	for _, e := range entries {
		t.setLocked(e.Name, e.Alias)
	}
}

// Order insensitive equality with a loaded snapshot
func (t *DirectAliasTable) equals(entries []NamedDirectAlias) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	want := map[string]DirectAlias{}
	for _, e := range entries {
		want[e.Name] = e.Alias
	}
	if len(want) != len(t.byName) {
		return false
	}
	for name, alias := range t.byName {
		if other, ok := want[name]; !ok || other != alias {
			return false
		}
	}
	return true
}

// Built in direct aliases, extended by the model_aliases config section
var builtinDirectAliases []NamedDirectAlias

// DirectAliases holds the builtin plus active profile aliases, reloaded in place when the profile config changes
var DirectAliases = NewDirectAliasTable()

var (
	directAliasMu         sync.Mutex
	directAliasIdentity   string
	directAliasIdentityOK bool
	directAliasLoaded     []NamedDirectAlias
)

// Direct aliases from model_aliases and model.aliases of the given config, or of the active profile config when nil
func loadDirectAliases(ctx context.Context, cfg map[string]any) *DirectAliasTable {
	merged := NewDirectAliasTable(builtinDirectAliases...)
	if cfg == nil {
		cfg = LoadConfigSafe(home.Dir(ctx))
	}
	if userAliases, ok := cfg["model_aliases"].(map[string]any); ok {
		for _, name := range sortedKeys(userAliases) {
			entry, ok := userAliases[name].(map[string]any)
			if !ok || !pyTruthy(entry["model"]) {
				continue
			}
			provider := "custom"
			if v, present := entry["provider"]; present {
				provider = pyStrOrEmpty(v)
			}
			merged.Set(strings.ToLower(pyStripSpace(name)), DirectAlias{
				Model: pyStrOrEmpty(entry["model"]), Provider: provider, BaseURL: pyStrOrEmpty(entry["base_url"]),
				APIKey: msClean(entry["api_key"]), KeyEnv: msClean(entry["key_env"])})
		}
	}
	modelSection, _ := cfg["model"].(map[string]any)
	simpleAliases, ok := modelSection["aliases"].(map[string]any)
	if !ok {
		return merged
	}
	currentProvider := pyStrOrEmpty(modelSection["provider"])
	for _, name := range sortedKeys(simpleAliases) {
		key := strings.ToLower(pyStripSpace(name))
		if _, taken := merged.Get(key); key == "" || taken {
			continue
		}
		switch value := simpleAliases[name].(type) {
		case map[string]any:
			model := msClean(value["model"])
			if model == "" {
				continue
			}
			provider := msClean(value["provider"])
			if provider == "" {
				provider = currentProvider
			}
			if provider == "" {
				provider = "custom"
			}
			merged.Set(key, DirectAlias{Model: model, Provider: provider, BaseURL: msClean(value["base_url"]),
				APIKey: msClean(value["api_key"]), KeyEnv: msClean(value["key_env"])})
		case string:
			val := pyStripSpace(value)
			if val == "" {
				continue
			}
			provider, model := currentProvider, val
			if before, after, found := strings.Cut(val, "/"); found {
				provider, model = before, after
			}
			provider = pyStripSpace(provider)
			if provider == "" {
				provider = currentProvider
			}
			merged.Set(key, DirectAlias{Model: pyStripSpace(model), Provider: provider})
		}
	}
	return merged
}

// Python str of a config value with None as empty
func pyStrOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return pyStr(v)
}

// Path and parse signature of the active profile config, false when the signature cannot be read
func directAliasSourceIdentity(ctx context.Context) (string, bool) {
	path := home.ConfigPath(home.Dir(ctx))
	version, err := config.ConfigCacheVersion(ctx, path)
	if err != nil {
		if config.IsOSError(err) {
			return path + "\x00missing", true
		}
		return "", false
	}
	return path + "\x00" + fmt.Sprintf("%#v", version), true
}

// Loads the active profile aliases into the shared table unless it already holds them or a caller seeded it
func ensureDirectAliases(ctx context.Context) {
	directAliasMu.Lock()
	defer directAliasMu.Unlock()
	identity, ok := directAliasSourceIdentity(ctx)
	if DirectAliases.Len() > 0 && (!DirectAliases.equals(directAliasLoaded) ||
		(ok && directAliasIdentityOK && identity == directAliasIdentity)) {
		return
	}
	loaded := loadDirectAliases(ctx, nil).Entries()
	DirectAliases.replace(loaded)
	directAliasIdentity, directAliasIdentityOK = identity, ok
	directAliasLoaded = loaded
}

// Scoped secret read failing closed under multiplexing, otherwise the profile dotenv before the process env
func scopedKeyEnv(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if scope := SecretScopeFrom(ctx); scope.Mapping != nil || IsMultiplexActive(ctx) {
		value, _, err := scope.Secret(name)
		if err != nil {
			return "", nil
		}
		return pyStripSpace(value), nil
	}
	return pyStripSpace(GetEnvPreferDotenv(modelsPoolEnv(ctx), name)), nil
}

// DirectAliasAPIKey resolves the alias credential from an env template, a literal key, then key_env
func DirectAliasAPIKey(ctx context.Context, alias DirectAlias) (string, error) {
	raw := pyStripSpace(alias.APIKey)
	if strings.HasPrefix(raw, "${") && strings.HasSuffix(raw, "}") && len(raw) >= 3 {
		return scopedKeyEnv(ctx, pyStripSpace(raw[2:len(raw)-1]))
	}
	if raw != "" {
		return raw, nil
	}
	return scopedKeyEnv(ctx, pyStripSpace(alias.KeyEnv))
}

// DirectAliasRuntimeRequest returns the provider to resolve and the explicit key, custom whenever the alias names a URL
func DirectAliasRuntimeRequest(ctx context.Context, alias DirectAlias) (string, string, error) {
	key, err := DirectAliasAPIKey(ctx, alias)
	if err != nil {
		return "", "", err
	}
	if alias.BaseURL != "" {
		return "custom", key, nil
	}
	if alias.Provider != "" {
		return alias.Provider, key, nil
	}
	return "custom", key, nil
}

var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "0.0.0.0": true}

// Scheme, lowercased host and effective port of a base URL that may lack a scheme, zero values on no host or bad port
func baseURLOriginPort(raw string) (string, string, int) {
	parsed := parseBaseURL(raw)
	if parsed == nil {
		return "", "", 0
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "" {
		return "", "", 0
	}
	scheme := strings.ToLower(parsed.Scheme)
	if p := parsed.Port(); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil || port < 0 || port > 65535 {
			return "", "", 0
		}
		return scheme, hostname, port
	}
	switch scheme {
	case "https":
		return scheme, hostname, 443
	case "http":
		return scheme, hostname, 80
	}
	return scheme, hostname, 0
}

// Reports whether the session key may follow a switch to the alias URL, same origin and https or loopback only
func mayReuseSessionCredential(sessionBaseURL, aliasBaseURL string) bool {
	sScheme, sHost, sPort := baseURLOriginPort(sessionBaseURL)
	aScheme, aHost, aPort := baseURLOriginPort(aliasBaseURL)
	if sHost == "" || sScheme != aScheme || sHost != aHost || sPort != aPort {
		return false
	}
	return aScheme == "https" || loopbackHosts[aHost]
}

// StartupModelRoute is the model and provider pair resolved before an agent is built
type StartupModelRoute struct {
	Model    string
	Provider string
	BaseURL  string
	APIKey   string
}

// StartupRouteOptions carries the caller's routing context and an optional alias config snapshot
type StartupRouteOptions struct {
	ExplicitProvider string
	CurrentProvider  string
	UserProviders    map[string]any
	CustomProviders  []any
	AliasConfig      map[string]any
}

// ResolveStartupModelRoute resolves aliases, provider qualified input and configured provider slash model at startup
func ResolveStartupModelRoute(ctx context.Context, rawModel string, opts StartupRouteOptions) (*StartupModelRoute, error) {
	raw := msClean(rawModel)
	if raw == "" {
		return nil, nil
	}
	aliases := DirectAliases
	if opts.AliasConfig == nil {
		ensureDirectAliases(ctx)
	} else {
		aliases = loadDirectAliases(ctx, opts.AliasConfig)
	}
	if direct, ok := aliases.Get(strings.ToLower(raw)); ok {
		if opts.ExplicitProvider != "" {
			return &StartupModelRoute{Model: direct.Model, Provider: opts.ExplicitProvider, BaseURL: direct.BaseURL}, nil
		}
		aliasProvider, aliasKey, err := DirectAliasRuntimeRequest(ctx, direct)
		if err != nil {
			return nil, err
		}
		return &StartupModelRoute{Model: direct.Model, Provider: aliasProvider, BaseURL: direct.BaseURL, APIKey: aliasKey}, nil
	}
	if opts.ExplicitProvider != "" {
		return nil, nil
	}
	customIDs := map[string]bool{}
	for _, key := range sortedKeys(opts.UserProviders) {
		entry, ok := opts.UserProviders[key].(map[string]any)
		if !ok {
			continue
		}
		name := key
		if pyTruthy(entry["name"]) {
			name = pyStr(entry["name"])
		}
		customIDs[CustomProviderSlug(name, key)] = true
	}
	for _, entry := range customEntries(opts.CustomProviders) {
		if msClean(entry["name"]) != "" {
			customIDs[CustomProviderSlug(pyStr(entry["name"]), "")] = true
		}
	}
	if qualifiedProvider, qualifiedModel := ParseModelInput(ctx, raw, "", customIDs); qualifiedProvider != "" {
		return &StartupModelRoute{Model: qualifiedModel, Provider: qualifiedProvider}, nil
	}
	before, after, found := strings.Cut(raw, "/")
	if !found {
		return nil, nil
	}
	prefix, model := pyStripSpace(before), pyStripSpace(after)
	if prefix == "" || model == "" {
		return nil, nil
	}
	if opts.CurrentProvider != "" && isRoutingAggregatorCtx(ctx, NormalizeProviderDef(ctx, opts.CurrentProvider)) &&
		findOpenRouterSlugFn(ctx, raw) != "" {
		return nil, nil
	}
	configured := map[string]bool{}
	for name := range opts.UserProviders {
		if n := strings.ToLower(pyStripSpace(name)); n != "" {
			configured[n] = true
		}
	}
	for _, entry := range customEntries(opts.CustomProviders) {
		if msClean(entry["name"]) != "" {
			configured["custom:"+strings.ToLower(pyStripSpace(pyStr(entry["name"])))] = true
		}
	}
	canonical := catalogNormalizeProvider(prefix)
	var provider string
	switch {
	case configured[strings.ToLower(prefix)]:
		provider = prefix
	case configured[strings.ToLower(canonical)]:
		provider = canonical
	default:
		return nil, nil
	}
	if isAggregatorCtx(ctx, canonical) {
		return nil, nil
	}
	return &StartupModelRoute{Model: model, Provider: provider}, nil
}
