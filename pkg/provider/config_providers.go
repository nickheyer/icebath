package provider

import (
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Alias spellings of api_mode accepted from older configs mapped onto the canonical transport names
var apiModeAliases = map[string]string{
	"openai": "chat_completions", "openai_chat": "chat_completions", "openai-chat": "chat_completions",
	"chat-completions": "chat_completions", "chatcompletions": "chat_completions",
	"responses": "codex_responses", "openai_responses": "codex_responses", "openai-responses": "codex_responses",
	"anthropic": "anthropic_messages", "anthropic-messages": "anthropic_messages", "messages": "anthropic_messages",
	"bedrock": "bedrock_converse", "bedrock-converse": "bedrock_converse",
}

var falseWords = map[string]bool{"false": true, "0": true, "no": true, "off": true}
var trueWords = map[string]bool{"true": true, "1": true, "yes": true, "on": true}

// CanonicalAPIMode maps alias spellings onto the canonical transport name and passes unknown values through
func CanonicalAPIMode(apiMode string) string {
	cleaned := pystr.Strip(apiMode)
	if canon, ok := apiModeAliases[pystr.Lower(cleaned)]; ok {
		return canon
	}
	return cleaned
}

// CoerceProviderID renders a provider identity field as a trimmed string
func CoerceProviderID(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(cfgString(value))
}

// StringifyProviderMap copies a providers mapping so every key is a string
func StringifyProviderMap(providers any) map[string]any {
	m, ok := providers.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for stored, value := range m {
		if key := CoerceProviderID(stored); key != "" {
			out[key] = value
		}
	}
	return out
}

// FindProviderEntry returns the stored key and block matching a provider key by string identity
func FindProviderEntry(providers any, key any) (string, map[string]any) {
	m, ok := providers.(map[string]any)
	if !ok {
		return "", nil
	}
	want := CoerceProviderID(key)
	if want == "" {
		return "", nil
	}
	if exact, ok := m[want].(map[string]any); ok {
		return want, exact
	}
	for stored, entry := range m {
		if e, ok := entry.(map[string]any); ok && CoerceProviderID(stored) == want {
			return stored, e
		}
	}
	return "", nil
}

var camelAliases = []struct{ camel, snake string }{
	{"apiKey", "api_key"}, {"baseUrl", "base_url"}, {"apiMode", "api_mode"}, {"keyEnv", "key_env"},
	{"apiKeyEnv", "key_env"}, {"defaultModel", "default_model"}, {"contextLength", "context_length"},
	{"rateLimitDelay", "rate_limit_delay"}, {"sessionAffinityHeader", "session_affinity_header"},
}

var knownProviderKeys = map[string]bool{
	"provider": true, "name": true, "api": true, "url": true, "base_url": true, "api_key": true, "key_env": true,
	"api_key_env": true, "key_cmd": true, "api_mode": true, "transport": true, "model": true, "default_model": true,
	"models": true, "models_discovered": true, "context_length": true, "rate_limit_delay": true,
	"request_timeout_seconds": true, "stale_timeout_seconds": true, "discover_models": true, "extra_body": true,
	"extra_headers": true, "capabilities": true, "ssl_ca_cert": true, "ssl_verify": true, "catalog_provider": true,
	"session_affinity_header": true,
}

var (
	providerNormalizeWarnedMu sync.Mutex
	providerNormalizeWarned   = map[string]bool{}
)

// Logs a provider normalization warning once per provider and signature for the process lifetime
func warnOncePerProvider(providerKey, signature, msg string, args ...any) {
	key := providerKey
	if key == "" {
		key = "?"
	}
	providerNormalizeWarnedMu.Lock()
	defer providerNormalizeWarnedMu.Unlock()
	dedup := key + "\x00" + signature
	if providerNormalizeWarned[dedup] {
		return
	}
	providerNormalizeWarned[dedup] = true
	slog.Default().Warn(fmt.Sprintf(msg, args...))
}

// Clears the once per provider warning memory for tests
func resetProviderNormalizeWarnings() {
	providerNormalizeWarnedMu.Lock()
	defer providerNormalizeWarnedMu.Unlock()
	providerNormalizeWarned = map[string]bool{}
}

var placeholderTokenRe = regexp.MustCompile(`\{[^}]+\}`)

// First usable URL among base_url, url, and api, with unresolved placeholders accepted unvalidated
func pickProviderBaseURL(entry map[string]any, providerKey string) string {
	label := providerKey
	if label == "" {
		label = "?"
	}
	for _, urlKey := range []string{"base_url", "url", "api"} {
		raw, ok := entry[urlKey].(string)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		candidate := strings.TrimSpace(raw)
		if placeholderTokenRe.MatchString(candidate) {
			return candidate
		}
		if parsed, err := url.Parse(candidate); err == nil && parsed.Scheme != "" && parsed.Host != "" {
			return candidate
		}
		slog.Default().Warn(fmt.Sprintf("providers.%s: '%s' value '%s' is not a valid URL (no scheme or host) — skipped", label, urlKey, candidate))
	}
	return ""
}

// Normalizes an entry's models to a mapping plus the discovered flag, accepting lists of ids or rows
func normalizeProviderModels(models any) (map[string]any, bool) {
	discovered := false
	if m, ok := models.(map[string]any); ok && len(m) > 0 {
		copyM := make(map[string]any, len(m))
		for k, v := range m {
			copyM[k] = v
		}
		if flag, ok := copyM["__discovered_model_catalog__"].(bool); ok && flag {
			discovered = true
		}
		delete(copyM, "__discovered_model_catalog__")
		delete(copyM, "__explicit_model_allowlist__")
		return copyM, discovered
	}
	if list, ok := asList(models); ok && len(list) > 0 {
		out := map[string]any{}
		for _, item := range list {
			if s, ok := item.(string); ok {
				if strings.TrimSpace(s) != "" {
					out[strings.TrimSpace(s)] = map[string]any{}
				}
				continue
			}
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			modelID, _ := row["id"].(string)
			if strings.TrimSpace(modelID) == "" {
				modelID, _ = row["name"].(string)
			}
			if strings.TrimSpace(modelID) == "" {
				continue
			}
			rest := map[string]any{}
			for k, v := range row {
				if k != "id" && k != "name" {
					rest[k] = v
				}
			}
			out[strings.TrimSpace(modelID)] = rest
		}
		return out, discovered
	}
	return map[string]any{}, discovered
}

// NormalizeCustomProviderEntry returns the runtime shape of one providers or custom_providers block or nil
func NormalizeCustomProviderEntry(raw any, providerKey string) map[string]any {
	src, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	entry := make(map[string]any, len(src))
	for k, v := range src {
		entry[k] = v
	}
	providerKey = CoerceProviderID(providerKey)
	label := providerKey
	if label == "" {
		label = "?"
	}
	if v, ok := entry["api_key_env"]; ok {
		if _, has := entry["key_env"]; !has {
			entry["key_env"] = v
		}
	}
	for _, ca := range camelAliases {
		if v, ok := entry[ca.camel]; ok {
			if _, has := entry[ca.snake]; !has {
				warnOncePerProvider(providerKey, "camel:"+ca.camel,
					"providers.%s: camelCase key '%s' auto-mapped to '%s' (use snake_case to avoid this warning)", label, ca.camel, ca.snake)
				entry[ca.snake] = v
			}
		}
	}
	var unknown []string
	for k := range entry {
		if knownProviderKeys[k] {
			continue
		}
		isCamel := false
		for _, ca := range camelAliases {
			if ca.camel == k {
				isCamel = true
				break
			}
		}
		if !isCamel {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		warnOncePerProvider(providerKey, "unknown:"+strings.Join(unknown, ","),
			"providers.%s: unknown config keys ignored: %s", label, strings.Join(unknown, ", "))
	}
	baseURL := pickProviderBaseURL(entry, providerKey)
	name := CoerceProviderID(entry["name"])
	if name == "" {
		name = providerKey
	}
	if baseURL == "" || name == "" {
		return nil
	}
	normalized := map[string]any{"name": name, "base_url": baseURL}
	if providerKey != "" {
		normalized["provider_key"] = providerKey
	}
	stripped := func(keys ...string) string {
		var val any
		for _, k := range keys {
			val = entry[k]
			if truthy(val) {
				break
			}
		}
		if s, ok := val.(string); ok {
			return strings.TrimSpace(s)
		}
		return ""
	}
	put := func(field string, value any) {
		if truthy(value) {
			normalized[field] = value
		}
	}
	put("api_key", stripped("api_key"))
	put("key_cmd", stripped("key_cmd"))
	keyEnv := stripped("key_env", "api_key_env")
	put("key_env", keyEnv)
	if keyEnv != "" && truthy(entry["api_key_env"]) && !truthy(entry["key_env"]) {
		normalized["api_key_env"] = keyEnv
	}
	if apiMode := stripped("api_mode", "transport"); apiMode != "" {
		put("api_mode", CanonicalAPIMode(apiMode))
	}
	put("model", stripped("model", "default_model"))
	put("catalog_provider", stripped("catalog_provider"))
	modelsDict, discovered := normalizeProviderModels(entry["models"])
	put("models", modelsDict)
	if flag, ok := entry["models_discovered"].(bool); (ok && flag) || discovered {
		normalized["models_discovered"] = true
	}
	if caps, ok := entry["capabilities"].(map[string]any); ok {
		put("capabilities", filterCapabilities(caps))
	}
	if v, ok := entry["context_length"]; ok {
		if n, ok := intValue(v); ok && n > 0 {
			normalized["context_length"] = v
		}
	}
	if v, ok := entry["rate_limit_delay"]; ok {
		if f, ok := floatValue(v); ok && f >= 0 {
			normalized["rate_limit_delay"] = v
		}
	}
	if v, ok := entry["discover_models"]; ok {
		switch v.(type) {
		case bool, string:
			normalized["discover_models"] = v
		}
	}
	if eb, ok := entry["extra_body"].(map[string]any); ok {
		normalized["extra_body"] = copyAnyMap(eb)
	}
	put("extra_headers", NormalizeExtraHeaders(entry["extra_headers"]))
	put("session_affinity_header", stripped("session_affinity_header"))
	put("ssl_ca_cert", stripped("ssl_ca_cert"))
	switch sv := entry["ssl_verify"].(type) {
	case bool:
		normalized["ssl_verify"] = sv
	case string:
		if strings.TrimSpace(sv) != "" {
			normalized["ssl_verify"] = strings.TrimSpace(sv)
		}
	}
	return normalized
}

// Python truthiness for config scalars and containers
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case map[string]any:
		return len(x) > 0
	case map[string]bool:
		return len(x) > 0
	case map[string]string:
		return len(x) > 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	}
	return true
}

// Integer value of a config scalar that is an int and not a bool
func intValue(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		if x == float64(int64(x)) {
			return int(x), true
		}
	}
	return 0, false
}

// Float value of a numeric config scalar that is not a bool
func floatValue(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func copyAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// String keyed boolean capabilities accepted at runtime
func filterCapabilities(value any) map[string]bool {
	out := map[string]bool{}
	switch m := value.(type) {
	case map[string]bool:
		for k, v := range m {
			out[k] = v
		}
	case map[string]any:
		for k, v := range m {
			if b, ok := v.(bool); ok {
				out[k] = b
			}
		}
	}
	return out
}

// Translates a legacy custom provider entry into the v12 providers shape
func customProviderEntryToProviderConfig(entry any, providerKey string) map[string]any {
	normalized := NormalizeCustomProviderEntry(entry, providerKey)
	if normalized == nil {
		return nil
	}
	out := map[string]any{"api": normalized["base_url"]}
	for _, field := range []string{"name", "api_key", "key_env", "key_cmd", "models", "models_discovered", "context_length",
		"rate_limit_delay", "discover_models", "extra_body", "extra_headers", "session_affinity_header", "ssl_ca_cert",
		"ssl_verify", "catalog_provider"} {
		if v, ok := normalized[field]; ok {
			out[field] = v
		}
	}
	if v, ok := normalized["model"]; ok {
		out["default_model"] = v
	}
	if v, ok := normalized["api_mode"]; ok {
		out["transport"] = v
	}
	return out
}

// Sorted keys so map iteration order is stable across calls
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ProvidersDictToCustomProviders normalizes enabled providers blocks into the legacy list shape
func ProvidersDictToCustomProviders(providersDict any) []map[string]any {
	m, ok := providersDict.(map[string]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, key := range sortedKeys(m) {
		entry := m[key]
		if block, ok := entry.(map[string]any); ok && !IsProviderEnabled(block) {
			continue
		}
		if normalized := NormalizeCustomProviderEntry(entry, CoerceProviderID(key)); normalized != nil {
			out = append(out, normalized)
		}
	}
	return out
}

// CompatibleCustomProviders is the deduplicated list view over custom_providers and providers blocks
func CompatibleCustomProviders(cfg map[string]any) []map[string]any {
	var legacy []any
	if raw, ok := cfg["custom_providers"]; ok && raw != nil {
		list, isList := asList(raw)
		if !isList {
			slog.Default().Warn(fmt.Sprintf("custom_providers is a %s, expected a list — skipping legacy entries; "+
				"'providers:' entries are still used. Move provider configs to the 'providers:' section.", config.PyTypeName(raw)))
		}
		legacy = list
	}
	var candidates []map[string]any
	for _, e := range legacy {
		candidates = append(candidates, NormalizeCustomProviderEntry(e, ""))
	}
	candidates = append(candidates, ProvidersDictToCustomProviders(cfg["providers"])...)
	norm := func(entry map[string]any, field string) string {
		return strings.ToLower(strings.TrimSpace(cfgString(entry[field])))
	}
	var compatible []map[string]any
	seenKeys := map[string]bool{}
	seenPairs := map[[3]string]bool{}
	for _, entry := range candidates {
		if entry == nil {
			continue
		}
		providerKey := norm(entry, "provider_key")
		name := norm(entry, "name")
		baseURL := strings.ToLower(strings.TrimRight(strings.TrimSpace(cfgString(entry["base_url"])), "/"))
		pair := [3]string{name, baseURL, norm(entry, "model")}
		if providerKey != "" && seenKeys[providerKey] {
			continue
		}
		if name != "" && baseURL != "" && seenPairs[pair] {
			continue
		}
		compatible = append(compatible, entry)
		if providerKey != "" {
			seenKeys[providerKey] = true
		}
		if name != "" && baseURL != "" {
			seenPairs[pair] = true
		}
	}
	return compatible
}

// Entries whose normalized route identity equals the base URL, from the given list or the config view
func entriesForRoute(baseURL string, customProviders []map[string]any, cfg map[string]any) []map[string]any {
	if customProviders == nil {
		customProviders = CompatibleCustomProviders(cfg)
	}
	if baseURL == "" {
		return nil
	}
	target := NormalizeRouteBaseURL(baseURL)
	if target == "" {
		return nil
	}
	var out []map[string]any
	for _, entry := range customProviders {
		if entry == nil {
			continue
		}
		entryURL := NormalizeRouteBaseURL(cfgString(entry["base_url"]))
		if entryURL != "" && entryURL == target {
			out = append(out, entry)
		}
	}
	return out
}

// EntriesForRoute exposes the route entry match to the agent capability checks
func EntriesForRoute(baseURL string, customProviders []map[string]any, cfg map[string]any) []map[string]any {
	return entriesForRoute(baseURL, customProviders, cfg)
}

// CustomProviderAPIMode is the canonical api_mode of the first custom entry serving the base URL or empty
func CustomProviderAPIMode(baseURL string, customProviders []map[string]any, cfg map[string]any) string {
	for _, entry := range entriesForRoute(baseURL, customProviders, cfg) {
		for _, field := range []string{"api_mode", "transport"} {
			if value, ok := entry[field].(string); ok && strings.TrimSpace(value) != "" {
				return CanonicalAPIMode(value)
			}
		}
	}
	return ""
}

// The models.<model> mapping of an entry when both are mappings
func routeModelCfg(entry map[string]any, model string) map[string]any {
	models, ok := entry["models"].(map[string]any)
	if !ok {
		return nil
	}
	modelCfg, ok := models[model].(map[string]any)
	if !ok {
		return nil
	}
	return modelCfg
}

// The models.<model> mapping of every entry matching the base URL
func routeModelCfgs(model, baseURL string, customProviders []map[string]any, cfg map[string]any) []map[string]any {
	var out []map[string]any
	for _, entry := range entriesForRoute(baseURL, customProviders, cfg) {
		if mc := routeModelCfg(entry, model); mc != nil {
			out = append(out, mc)
		}
	}
	return out
}

// Boolean reading of an ssl_verify value, with ok false when it is absent or unrecognised
func coerceSSLVerify(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		lowered := strings.ToLower(strings.TrimSpace(v))
		if falseWords[lowered] {
			return false, true
		}
		if trueWords[lowered] {
			return true, true
		}
	}
	return false, false
}

// TLSSettings are the per route certificate knobs a custom entry declares
type TLSSettings struct {
	CACert    string
	Verify    bool
	HasVerify bool
}

// CustomProviderTLSSettings returns the TLS settings of the first entry serving the base URL
func CustomProviderTLSSettings(baseURL string, customProviders []map[string]any, cfg map[string]any) TLSSettings {
	for _, entry := range entriesForRoute(baseURL, customProviders, cfg) {
		var out TLSSettings
		if ca, ok := entry["ssl_ca_cert"].(string); ok && strings.TrimSpace(ca) != "" {
			out.CACert = strings.TrimSpace(ca)
		}
		if verify, ok := coerceSSLVerify(entry["ssl_verify"]); ok {
			out.Verify, out.HasVerify = verify, true
		}
		return out
	}
	return TLSSettings{}
}

// Attaches the TLS knobs of the entry serving the base URL to OpenAI client kwargs
func ApplyCustomProviderTLSToClientKwargs(clientKwargs map[string]any, baseURL string, customProviders []map[string]any, cfg map[string]any) {
	tls := CustomProviderTLSSettings(baseURL, customProviders, cfg)
	if tls.CACert != "" {
		clientKwargs["ssl_ca_cert"] = tls.CACert
	}
	if tls.HasVerify {
		clientKwargs["ssl_verify"] = tls.Verify
	}
}

// NormalizeExtraHeaders renders a raw extra_headers mapping as string pairs, dropping nil values
func NormalizeExtraHeaders(extraHeaders any) map[string]string {
	if typed, ok := extraHeaders.(map[string]string); ok {
		return copyStringMap(typed)
	}
	m, ok := extraHeaders.(map[string]any)
	if !ok || len(m) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		out[k] = cfgString(v)
	}
	return out
}

// CustomProviderExtraHeaders returns the headers of the first route matching entry declaring any
func CustomProviderExtraHeaders(baseURL string, customProviders []map[string]any, cfg map[string]any) map[string]string {
	for _, entry := range entriesForRoute(baseURL, customProviders, cfg) {
		if headers := NormalizeExtraHeaders(entry["extra_headers"]); len(headers) > 0 {
			return headers
		}
	}
	return map[string]string{}
}

// MergeExtraHeaders layers route headers over default headers with the route winning
func MergeExtraHeaders(defaults map[string]string, baseURL string, customProviders []map[string]any, cfg map[string]any) map[string]string {
	extra := CustomProviderExtraHeaders(baseURL, customProviders, cfg)
	if len(extra) == 0 {
		return defaults
	}
	merged := make(map[string]string, len(defaults)+len(extra))
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return merged
}

// CustomProviderSessionAffinityHeader is the header name a route matching entry opted into or empty
func CustomProviderSessionAffinityHeader(baseURL string, customProviders []map[string]any, cfg map[string]any) string {
	for _, entry := range entriesForRoute(baseURL, customProviders, cfg) {
		if header, ok := entry["session_affinity_header"].(string); ok && strings.TrimSpace(header) != "" {
			return strings.TrimSpace(header)
		}
	}
	return ""
}

// Positive integer reading of a context length value
func positiveInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case int, int64, float64:
		n, ok := intValue(v)
		if ok && n > 0 {
			return n, true
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// CustomProviderContextLength is the per model or entry level context_length override for a route or 0
func CustomProviderContextLength(model, baseURL string, customProviders []map[string]any, cfg map[string]any) int {
	if model == "" || baseURL == "" {
		return 0
	}
	if customProviders == nil {
		customProviders = CompatibleCustomProviders(cfg)
	}
	for _, mc := range routeModelCfgs(model, baseURL, customProviders, cfg) {
		if ctx, ok := positiveInt(mc["context_length"]); ok {
			return ctx
		}
	}
	for _, entry := range entriesForRoute(baseURL, customProviders, cfg) {
		if ctx, ok := positiveInt(entry["context_length"]); ok {
			return ctx
		}
	}
	return 0
}

// CustomProviderModelCapability is the explicit boolean capability for one custom model, known false when absent
func CustomProviderModelCapability(model, baseURL, capability string, customProviders []map[string]any, cfg map[string]any) (value bool, known bool) {
	if model == "" || baseURL == "" || capability == "" {
		return false, false
	}
	if customProviders == nil {
		customProviders = CompatibleCustomProviders(cfg)
	}
	for _, mc := range routeModelCfgs(model, baseURL, customProviders, cfg) {
		if b, ok := mc[capability].(bool); ok {
			return b, true
		}
	}
	return false, false
}

// IsProviderEnabled reports whether a providers block is enabled, with only an explicit false hiding it
func IsProviderEnabled(providerCfg any) bool {
	block, isMap := providerCfg.(map[string]any)
	if !isMap {
		return true
	}
	flag, ok := block["enabled"]
	if !ok {
		return true
	}
	switch f := flag.(type) {
	case bool:
		return f
	case string:
		return !falseWords[pystr.Lower(pystr.Strip(f))]
	}
	return truthy(flag)
}

// ---- provider profile env var entries (config_provider_env) ----

var (
	providerEnvMu         sync.Mutex
	providerEnvCatalog    []config.NamedEnvVar
	providerEnvCatalogSet bool
	providerEnvNames      = map[string]bool{}
	providerEnvStatic     = map[string]bool{}
	providerEnvTable      = config.OptionalEnvVars
	providerEnvBundled    = BundledProfiles
)

// Records the names core declares and arms the table so its first read adds the bundled provider entries
func armProviderEnv(table *config.OptionalEnvTable, static map[string]bool) {
	providerEnvMu.Lock()
	providerEnvStatic = maps.Clone(static)
	providerEnvMu.Unlock()
	table.Arm(ProviderEnvEntries, publishProviderEnvNames)
}

// Adds the names of entries a fill or a sync put in the table to the provider declared names
func publishProviderEnvNames(entries []config.NamedEnvVar) {
	providerEnvMu.Lock()
	defer providerEnvMu.Unlock()
	for _, e := range entries {
		providerEnvNames[e.Name] = true
	}
}

// Names config declared before any platform manifest or provider profile entry was added
func staticDeclaredEnvNames() map[string]bool {
	providerEnvMu.Lock()
	defer providerEnvMu.Unlock()
	out := make(map[string]bool, len(providerEnvStatic))
	for name := range providerEnvStatic {
		out[name] = true
	}
	return out
}

// Key env names then the base URL env name, the order the env_vars tuple of the profile lists them
func profileEnvVars(p Profile) []string {
	out := append([]string(nil), p.KeyEnv...)
	if p.BaseURLEnv != "" && !slices.Contains(out, p.BaseURLEnv) {
		out = append(out, p.BaseURLEnv)
	}
	return out
}

// Entries for the api_key profiles' env vars minus skipped names in profile order, first profile to declare a name wins
func providerEnvEntriesFor(profiles []Profile, skip map[string]bool) []config.NamedEnvVar {
	var entries []config.NamedEnvVar
	seen := map[string]bool{}
	for _, p := range profiles {
		if p.AuthType != AuthTypeAPIKey {
			continue
		}
		for _, v := range profileEnvVars(p) {
			if skip[v] || seen[v] {
				continue
			}
			seen[v] = true
			isKey := !(strings.HasSuffix(v, "_BASE_URL") || strings.HasSuffix(v, "_URL"))
			label := p.Label()
			entry := config.EnvVar{Category: "provider", Advanced: true, Password: isKey, URL: p.SignupURL}
			if isKey {
				entry.Description = label + " API key"
				entry.Prompt = label + " API key"
			} else {
				entry.Description = label + " base URL override"
				entry.Prompt = label + " base URL (leave empty for default)"
			}
			entries = append(entries, config.NamedEnvVar{Name: v, Entry: entry})
		}
	}
	return entries
}

// ProviderEnvEntries returns the bundled provider profiles' env entries in profile order, computed once and config free
func ProviderEnvEntries() []config.NamedEnvVar {
	providerEnvMu.Lock()
	if providerEnvCatalogSet {
		defer providerEnvMu.Unlock()
		return slices.Clone(providerEnvCatalog)
	}
	bundled, static := providerEnvBundled, providerEnvStatic
	providerEnvMu.Unlock()
	entries := providerEnvEntriesFor(bundled(), static)
	providerEnvMu.Lock()
	defer providerEnvMu.Unlock()
	providerEnvCatalog, providerEnvCatalogSet = entries, true
	return slices.Clone(providerEnvCatalog)
}

// FillProviderEnvVars reads the table once so its armed provider fill has run
func FillProviderEnvVars() {
	providerEnvMu.Lock()
	table := providerEnvTable
	providerEnvMu.Unlock()
	table.Has("")
}

// SyncRegisteredProviders adds the env vars of the process wide plugin provider profiles discovery registered
func SyncRegisteredProviders(profiles []Profile) {
	providerEnvMu.Lock()
	skip := map[string]bool{}
	for name := range providerEnvStatic {
		skip[name] = true
	}
	for name := range providerEnvNames {
		skip[name] = true
	}
	for _, e := range providerEnvCatalog {
		skip[e.Name] = true
	}
	table := providerEnvTable
	providerEnvMu.Unlock()
	entries := providerEnvEntriesFor(profiles, skip)
	for _, e := range entries {
		table.Set(e.Name, e.Entry)
	}
	publishProviderEnvNames(entries)
}

// CoreDeclaredEnvNames lists every name core declares, static entries plus provider profile entries
func CoreDeclaredEnvNames() map[string]bool {
	FillProviderEnvVars()
	providerEnvMu.Lock()
	defer providerEnvMu.Unlock()
	out := map[string]bool{}
	for name := range providerEnvStatic {
		out[name] = true
	}
	for name := range providerEnvNames {
		out[name] = true
	}
	return out
}

// ---- which config keys live in .env (config_env_routing) ----

// Names read straight from the environment that no profile entry enumerates
var extraEnvKeys = map[string]bool{
	"OPENAI_API_KEY": true, "OPENAI_BASE_URL": true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_TOKEN": true,
	"DISCORD_HOME_CHANNEL": true, "DISCORD_HOME_CHANNEL_NAME": true,
	"TELEGRAM_HOME_CHANNEL": true, "TELEGRAM_HOME_CHANNEL_NAME": true,
	"SLACK_HOME_CHANNEL": true, "SLACK_HOME_CHANNEL_NAME": true,
	"SIGNAL_ACCOUNT": true, "SIGNAL_HTTP_URL": true, "SIGNAL_ALLOWED_USERS": true, "SIGNAL_GROUP_ALLOWED_USERS": true,
	"SIGNAL_HOME_CHANNEL": true, "SIGNAL_HOME_CHANNEL_NAME": true, "SMS_HOME_CHANNEL": true, "SMS_HOME_CHANNEL_NAME": true,
	"DINGTALK_CLIENT_ID": true, "DINGTALK_CLIENT_SECRET": true, "DINGTALK_HOME_CHANNEL": true, "DINGTALK_HOME_CHANNEL_NAME": true,
	"FEISHU_APP_ID": true, "FEISHU_APP_SECRET": true, "FEISHU_ENCRYPT_KEY": true, "FEISHU_VERIFICATION_TOKEN": true,
	"FEISHU_HOME_CHANNEL": true, "FEISHU_HOME_CHANNEL_NAME": true, "YUANBAO_HOME_CHANNEL": true, "YUANBAO_HOME_CHANNEL_NAME": true,
	"WECOM_BOT_ID": true, "WECOM_SECRET": true, "WECOM_CALLBACK_CORP_ID": true, "WECOM_CALLBACK_CORP_SECRET": true,
	"WECOM_CALLBACK_AGENT_ID": true, "WECOM_CALLBACK_TOKEN": true, "WECOM_CALLBACK_ENCODING_AES_KEY": true,
	"WECOM_CALLBACK_HOST": true, "WECOM_CALLBACK_PORT": true, "WECOM_HOME_CHANNEL": true, "WECOM_HOME_CHANNEL_NAME": true,
	"WEIXIN_ACCOUNT_ID": true, "WEIXIN_TOKEN": true, "WEIXIN_BASE_URL": true, "WEIXIN_CDN_BASE_URL": true,
	"WEIXIN_HOME_CHANNEL": true, "WEIXIN_HOME_CHANNEL_NAME": true, "WEIXIN_DM_POLICY": true, "WEIXIN_GROUP_POLICY": true,
	"WEIXIN_ALLOWED_USERS": true, "WEIXIN_GROUP_ALLOWED_USERS": true, "WEIXIN_ALLOW_ALL_USERS": true,
	"BLUEBUBBLES_SERVER_URL": true, "BLUEBUBBLES_PASSWORD": true, "BLUEBUBBLES_HOME_CHANNEL": true, "BLUEBUBBLES_HOME_CHANNEL_NAME": true,
	"QQ_APP_ID": true, "QQ_CLIENT_SECRET": true, "QQBOT_HOME_CHANNEL": true, "QQBOT_HOME_CHANNEL_NAME": true,
	"QQ_HOME_CHANNEL": true, "QQ_HOME_CHANNEL_NAME": true,
	"QQ_ALLOWED_USERS": true, "QQ_GROUP_ALLOWED_USERS": true, "QQ_ALLOW_ALL_USERS": true, "QQ_MARKDOWN_SUPPORT": true,
	"QQ_STT_API_KEY": true, "QQ_STT_BASE_URL": true, "QQ_STT_MODEL": true,
	"IRC_SERVER": true, "IRC_PORT": true, "IRC_NICKNAME": true, "IRC_CHANNEL": true, "IRC_USE_TLS": true, "IRC_SERVER_PASSWORD": true,
	"IRC_NICKSERV_PASSWORD": true, "TERMINAL_ENV": true, "TERMINAL_SSH_KEY": true, "TERMINAL_SSH_PORT": true,
	"ICEBATH_TOOL_PROGRESS_MODE": true,
	"WHATSAPP_MODE":              true, "WHATSAPP_ENABLED": true,
	"MATTERMOST_HOME_CHANNEL": true, "MATTERMOST_HOME_CHANNEL_NAME": true, "MATTERMOST_REPLY_MODE": true,
	"MATRIX_PASSWORD": true, "MATRIX_ENCRYPTION": true, "MATRIX_DEVICE_ID": true, "MATRIX_HOME_ROOM": true,
	"MATRIX_REQUIRE_MENTION": true, "MATRIX_FREE_RESPONSE_ROOMS": true, "MATRIX_AUTO_THREAD": true, "MATRIX_DM_AUTO_THREAD": true,
	"MATRIX_RECOVERY_KEY":     true,
	"ICEBATH_ACP_AUTH_METHOD": true, "ICEBATH_ACP_AUTO_APPROVE": true,
}

// Suffixes of self configuring platform variables hidden from setup forms
var setupHiddenEnvSuffixes = []string{
	"_HOME_CHANNEL", "_HOME_CHANNEL_NAME", "_HOME_CHANNEL_THREAD_ID", "_HOME_ADDRESS", "_ALLOW_ALL_USERS",
	"_REPLY_TO_MODE", "_REPLY_MODE", "_REQUIRE_MENTION", "_AUTO_THREAD", "_FREE_RESPONSE_CHANNELS",
	"_FREE_RESPONSE_ROOMS", "_ALLOWED_CHANNELS", "_PROXY",
}

// IsSetupHiddenEnv reports whether a variable is self configuring and stays out of setup forms
func IsSetupHiddenEnv(name string) bool {
	for _, suffix := range setupHiddenEnvSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// IsRegisteredEnvName reports whether icebath itself enumerates the variable name
func IsRegisteredEnvName(name string) bool {
	FillProviderEnvVars()
	if config.OptionalEnvVars.Has(name) {
		return true
	}
	if extraEnvKeys[name] {
		return true
	}
	for _, moved := range leftCoreProfileEnv() {
		if moved == name {
			return true
		}
	}
	return IsSetupHiddenEnv(name)
}

var envShapeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// IsEnvSettingKey reports whether a bare config key is stored in .env rather than config.yaml
func IsEnvSettingKey(key string) bool {
	if strings.Contains(key, ".") {
		return false
	}
	return envShapeRe.MatchString(key) || IsRegisteredEnvName(strings.ToUpper(key))
}

// Removes same named top level config.yaml copies so the .env value is the only one
func dropConfigYAMLCopies(ctx context.Context, h, key string) (bool, error) {
	doc, err := config.ReadUser(h)
	if err != nil {
		return false, err
	}
	var stale []string
	for _, name := range []string{key, strings.ToUpper(key)} {
		if _, ok := doc[name]; ok {
			stale = append(stale, name)
		}
	}
	for _, name := range stale {
		delete(doc, name)
	}
	if len(stale) == 0 {
		return false, nil
	}
	return true, config.SaveUser(ctx, h, doc)
}

// SaveEnvSetting writes an environment setting to .env and drops stale config.yaml copies
func SaveEnvSetting(ctx context.Context, h, key, value string) error {
	upper := strings.ToUpper(key)
	if refusal := envWriteRefusal(ctx, h, upper, "set"); refusal != "" {
		return fmt.Errorf("%s", refusal)
	}
	if err := config.SaveEnvValue(h, upper, value); err != nil {
		return err
	}
	_, err := dropConfigYAMLCopies(ctx, h, key)
	return err
}

// RemoveEnvSetting removes the .env entry and any stale config.yaml copy and reports whether either existed
func RemoveEnvSetting(ctx context.Context, h, key string) (bool, error) {
	upper := strings.ToUpper(key)
	if refusal := envWriteRefusal(ctx, h, upper, "remove"); refusal != "" {
		return false, fmt.Errorf("%s", refusal)
	}
	removed, err := config.RemoveHomeEnvValue(context.Background(), h, upper)
	if err != nil {
		return false, err
	}
	dropped, err := dropConfigYAMLCopies(ctx, h, key)
	if err != nil {
		return false, err
	}
	return dropped || removed, nil
}

// ReadEnvSetting resolves .env first, then a stale top level config.yaml copy which is reported on stderr
func ReadEnvSetting(h, key string) (string, bool) {
	secrets := config.NewSecrets(h)
	if v, ok := secrets.Get(strings.ToUpper(key)); ok {
		return v, true
	}
	doc, err := config.ReadUser(h)
	if err != nil {
		return "", false
	}
	if v, ok := doc[key]; ok && v != nil {
		fmt.Fprintf(os.Stderr, "  (note: %s is a stale top-level config.yaml copy; `icebath config set %s <value>` "+
			"moves it to .env, `icebath config unset %s` removes it)\n", key, key, key)
		return cfgString(v), true
	}
	return "", false
}

// ---- unified provider catalog (provider_catalog) ----

// Auth types that sign in through an account flow rather than a pasted key
var accountsAuthTypes = map[string]bool{
	AuthTypeOAuthDeviceCode: true, AuthTypeOAuthExternal: true, AuthTypeOAuthMiniMax: true, AuthTypeExternalProcess: true,
}

// CanonicalProviderEntry is one row of the hermes model picker universe
type CanonicalProviderEntry struct {
	Slug    string
	Label   string
	TUIDesc string
}

// ProviderDescriptor is one provider as every surface sees it
type ProviderDescriptor struct {
	Slug          string
	Label         string
	Description   string
	AuthType      string
	Tab           string
	APIKeyEnvVars []string
	BaseURLEnvVar string
	SignupURL     string
	Order         int
}

// TabForAuthType returns the desktop tab a provider's auth maps to
func TabForAuthType(authType string) string {
	if accountsAuthTypes[authType] {
		return "accounts"
	}
	return "keys"
}

func isURLVar(name string) bool {
	return strings.HasSuffix(name, "_BASE_URL") || strings.HasSuffix(name, "_URL")
}

// Splits a profile's env vars into api key vars and the first base URL var
func splitEnvVars(envVars []string) ([]string, string) {
	var keys []string
	baseVar := ""
	for _, v := range envVars {
		if isURLVar(v) {
			if baseVar == "" {
				baseVar = v
			}
			continue
		}
		keys = append(keys, v)
	}
	return keys, baseVar
}

// ProviderCatalog returns one descriptor per listed canonical provider with registry and profile fallbacks
func ProviderCatalog() []ProviderDescriptor {
	FillProviderEnvVars()
	var out []ProviderDescriptor
	for order, entry := range listedCanonicalProviders() {
		prof, hasProf := ProfileFor(entry.Slug)
		authType := ""
		if hasProf {
			authType = prof.AuthType
		}
		if authType == "" {
			authType = AuthTypeAPIKey
		}
		var apiKeyVars []string
		baseURLVar := ""
		if hasProf {
			apiKeyVars, baseURLVar = splitEnvVars(prof.KeyEnv)
			if prof.BaseURLEnv != "" {
				baseURLVar = prof.BaseURLEnv
			}
		}
		label := ""
		signup := ""
		desc := ""
		if hasProf {
			label, signup, desc = prof.DisplayName, prof.SignupURL, prof.Description
		}
		if label == "" {
			label = entry.Label
		}
		if label == "" {
			label = entry.Slug
		}
		if signup == "" && len(apiKeyVars) > 0 {
			signup = config.OptionalEnvVars.Entry(apiKeyVars[0]).URL
		}
		if desc == "" {
			desc = entry.TUIDesc
		}
		if desc == "" {
			desc = label
		}
		out = append(out, ProviderDescriptor{
			Slug: entry.Slug, Label: label, Description: desc, AuthType: authType, Tab: TabForAuthType(authType),
			APIKeyEnvVars: apiKeyVars, BaseURLEnvVar: baseURLVar, SignupURL: signup, Order: order,
		})
	}
	return out
}

// ProviderCatalogBySlug keys the catalog by slug
func ProviderCatalogBySlug() map[string]ProviderDescriptor {
	out := map[string]ProviderDescriptor{}
	for _, d := range ProviderCatalog() {
		out[d.Slug] = d
	}
	return out
}
