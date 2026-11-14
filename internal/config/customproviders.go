package config

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spellings of a wire protocol earlier releases accepted, each with its canonical transport name
func apiModeAliases() map[string]string {
	return map[string]string{
		"openai": "chat_completions", "openai_chat": "chat_completions", "openai-chat": "chat_completions",
		"chat-completions": "chat_completions", "chatcompletions": "chat_completions",
		"responses": "codex_responses", "openai_responses": "codex_responses", "openai-responses": "codex_responses",
		"anthropic": "anthropic_messages", "anthropic-messages": "anthropic_messages", "messages": "anthropic_messages",
		"bedrock": "bedrock_converse", "bedrock-converse": "bedrock_converse",
	}
}

// Canonical transport name for an api_mode spelling, an unknown spelling kept as written
func CanonicalAPIMode(mode string) string {
	cleaned := trimEnvSpace(mode)
	if canonical, ok := apiModeAliases()[strings.ToLower(cleaned)]; ok {
		return canonical
	}
	return cleaned
}

// Provider identity text of a value, empty for null
func providerID(n *yaml.Node) string {
	if n == nil || isNull(n) {
		return ""
	}
	return trimEnvSpace(displayText(n))
}

// Hand-written camelCase keys of a provider entry with the snake_case key each stands for, in mapping order
func providerCamelAliases() [][2]string {
	return [][2]string{
		{"apiKey", "api_key"}, {"baseUrl", "base_url"}, {"apiMode", "api_mode"}, {"keyEnv", "key_env"},
		{"apiKeyEnv", "key_env"}, {"defaultModel", "default_model"}, {"contextLength", "context_length"},
		{"rateLimitDelay", "rate_limit_delay"}, {"sessionAffinityHeader", "session_affinity_header"},
	}
}

// Keys a provider entry may carry
func knownProviderKeys() []string {
	return []string{
		"provider", "name", "api", "url", "base_url", "api_key", "key_env", "api_key_env", "key_cmd",
		"api_mode", "transport", "model", "default_model", "models", "models_discovered",
		"context_length", "rate_limit_delay", "request_timeout_seconds", "stale_timeout_seconds",
		"discover_models", "extra_body", "extra_headers", "capabilities", "ssl_ca_cert", "ssl_verify",
		"catalog_provider", "session_affinity_header",
	}
}

// Logs a provider normalization warning once per provider and signature for the life of the store
func (s *Store) warnProviderOnce(providerKey, signature, text string) {
	if providerKey == "" {
		providerKey = "?"
	}
	key := "provider\x00" + providerKey + "\x00" + signature
	s.cacheMu.Lock()
	seen := s.warned[key]
	s.warned[key] = true
	s.cacheMu.Unlock()
	if !seen {
		slog.Warn(text)
	}
}

// Braced token of a URL template, an env reference or a bare braced name
var urlPlaceholder = regexp.MustCompile(`\{[^}]+\}`)

// Scheme of a URL, letters then letters, digits, plus, dot and dash
var urlSchemeShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// Reports a URL carrying both a scheme and a host, an unbalanced bracket in the host making it invalid
func hasSchemeAndHost(candidate string) bool {
	scheme, rest, found := strings.Cut(candidate, ":")
	if !found || !urlSchemeShape.MatchString(scheme) || !strings.HasPrefix(rest, "//") {
		return false
	}
	host := rest[2:]
	if at := strings.IndexAny(host, "/?#"); at >= 0 {
		host = host[:at]
	}
	if strings.Contains(host, "[") != strings.Contains(host, "]") {
		return false
	}
	return host != ""
}

// First usable URL among base_url, url and api of a provider entry, a templated URL accepted unchecked
func pickProviderBaseURL(entry *yaml.Node, providerKey string) string {
	for _, key := range []string{"base_url", "url", "api"} {
		raw := childOf(entry, key)
		if !isStringNode(raw) || trimEnvSpace(raw.Value) == "" {
			continue
		}
		candidate := trimEnvSpace(raw.Value)
		if urlPlaceholder.MatchString(candidate) || hasSchemeAndHost(candidate) {
			return candidate
		}
		shown := providerKey
		if shown == "" {
			shown = "?"
		}
		slog.Warn(fmt.Sprintf("providers.%s: '%s' value '%s' is not a valid URL (no scheme or host) — skipped", shown, key, candidate))
	}
	return ""
}

// Models of a provider entry as a mapping by id, with whether the mapping carried the discovered marker
func normalizeProviderModels(models *yaml.Node) (*yaml.Node, bool) {
	out := emptyMapping()
	switch {
	case isMapping(models) && len(models.Content) > 0:
		discovered := false
		for i := 0; i+1 < len(models.Content); i += 2 {
			switch models.Content[i].Value {
			case discoveredMarker:
				discovered = isTrue(models.Content[i+1])
			case allowlistMarker:
			default:
				out.Content = append(out.Content, models.Content[i], models.Content[i+1])
			}
		}
		return out, discovered
	case isSequence(models):
		for _, item := range models.Content {
			if isStringNode(item) && trimEnvSpace(item.Value) != "" {
				putKey(out, trimEnvSpace(item.Value), emptyMapping())
				continue
			}
			if !isMapping(item) {
				continue
			}
			id := childOf(item, "id")
			if !isStringNode(id) || trimEnvSpace(id.Value) == "" {
				id = childOf(item, "name")
			}
			if !isStringNode(id) || trimEnvSpace(id.Value) == "" {
				continue
			}
			putKey(out, trimEnvSpace(id.Value), withoutKeys(item, "id", "name"))
		}
	}
	return out, false
}

// Reports an integer or float scalar, booleans counting as the integers they are read as
func isNumberNode(n *yaml.Node, floats bool) bool {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return false
	}
	tag, _ := scalarKind(n)
	return tag == tagInt || tag == tagBool || (floats && tag == tagFloat)
}

// Number a numeric or boolean scalar holds
func numberOf(n *yaml.Node) float64 {
	tag, text := scalarKind(n)
	if tag == tagBool {
		tag, text = boolAsNumber(text, false)
	}
	if v, ok := numericValue(tag, text); ok {
		f, _ := v.Float64()
		return f
	}
	return 0
}

// Provider entry in the shape the runtime reads, nil for an entry without a usable name or endpoint
func (s *Store) normalizeCustomProvider(entry *yaml.Node, providerKey string) *yaml.Node {
	if !isMapping(entry) {
		return nil
	}
	entry = &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap, Content: slices.Clone(entry.Content)}
	has := func(key string) bool { return mappingIndex(entry, key) >= 0 }
	if has("api_key_env") && !has("key_env") {
		putKey(entry, "key_env", childOf(entry, "api_key_env"))
	}
	shown := providerKey
	if shown == "" {
		shown = "?"
	}
	for _, alias := range providerCamelAliases() {
		if has(alias[0]) && !has(alias[1]) {
			s.warnProviderOnce(providerKey, "camel:"+alias[0], fmt.Sprintf(
				"providers.%s: camelCase key '%s' auto-mapped to '%s' (use snake_case to avoid this warning)", shown, alias[0], alias[1]))
			putKey(entry, alias[1], childOf(entry, alias[0]))
		}
	}
	var unknown []string
	for _, key := range mappingKeys(entry) {
		camel := slices.ContainsFunc(providerCamelAliases(), func(a [2]string) bool { return a[0] == key })
		if !camel && !slices.Contains(knownProviderKeys(), key) && !slices.Contains(unknown, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		s.warnProviderOnce(providerKey, "unknown:"+strings.Join(unknown, ","),
			fmt.Sprintf("providers.%s: unknown config keys ignored: %s", shown, strings.Join(unknown, ", ")))
	}
	baseURL := pickProviderBaseURL(entry, providerKey)
	name := providerID(childOf(entry, "name"))
	if name == "" {
		name = providerKey
	}
	if baseURL == "" || name == "" {
		return nil
	}
	out := emptyMapping()
	putKey(out, "name", stringNode(name))
	putKey(out, "base_url", stringNode(baseURL))
	if providerKey != "" {
		putKey(out, "provider_key", stringNode(providerKey))
	}
	stripped := func(keys ...string) string {
		var val *yaml.Node
		for _, k := range keys {
			if val = childOf(entry, k); isSet(val) {
				break
			}
		}
		if isStringNode(val) {
			return trimEnvSpace(val.Value)
		}
		return ""
	}
	putText := func(field, value string) {
		if value != "" {
			putKey(out, field, stringNode(value))
		}
	}
	putText("api_key", stripped("api_key"))
	putText("key_cmd", stripped("key_cmd"))
	keyEnv := stripped("key_env", "api_key_env")
	putText("key_env", keyEnv)
	if keyEnv != "" && isSet(childOf(entry, "api_key_env")) && !isSet(childOf(entry, "key_env")) {
		putKey(out, "api_key_env", stringNode(keyEnv))
	}
	if mode := stripped("api_mode", "transport"); mode != "" {
		putText("api_mode", CanonicalAPIMode(mode))
	}
	putText("model", stripped("model", "default_model"))
	putText("catalog_provider", stripped("catalog_provider"))
	models, discovered := normalizeProviderModels(childOf(entry, "models"))
	if len(models.Content) > 0 {
		putKey(out, "models", models)
	}
	if isTrue(childOf(entry, "models_discovered")) || discovered {
		putKey(out, "models_discovered", &yaml.Node{Kind: yaml.ScalarNode, Tag: tagBool, Value: "true"})
	}
	if caps := childOf(entry, "capabilities"); isMapping(caps) {
		kept := emptyMapping()
		for i := 0; i+1 < len(caps.Content); i += 2 {
			if tag, _ := scalarKind(caps.Content[i+1]); isStringNode(caps.Content[i]) && caps.Content[i+1].Kind == yaml.ScalarNode && tag == tagBool {
				kept.Content = append(kept.Content, caps.Content[i], caps.Content[i+1])
			}
		}
		if len(kept.Content) > 0 {
			putKey(out, "capabilities", kept)
		}
	}
	if v := childOf(entry, "context_length"); isNumberNode(v, false) && numberOf(v) > 0 {
		putKey(out, "context_length", v)
	}
	if v := childOf(entry, "rate_limit_delay"); isNumberNode(v, true) && numberOf(v) >= 0 {
		putKey(out, "rate_limit_delay", v)
	}
	if v := childOf(entry, "discover_models"); v != nil && v.Kind == yaml.ScalarNode {
		if tag, _ := scalarKind(v); tag == tagBool || tag == tagStr {
			putKey(out, "discover_models", v)
		}
	}
	if v := childOf(entry, "extra_body"); isMapping(v) {
		putKey(out, "extra_body", plainCopy(v))
	}
	if headers := normalizeExtraHeaders(childOf(entry, "extra_headers")); len(headers.Content) > 0 {
		putKey(out, "extra_headers", headers)
	}
	putText("session_affinity_header", stripped("session_affinity_header"))
	putText("ssl_ca_cert", stripped("ssl_ca_cert"))
	if v := childOf(entry, "ssl_verify"); v != nil && v.Kind == yaml.ScalarNode {
		switch tag, _ := scalarKind(v); {
		case tag == tagBool:
			putKey(out, "ssl_verify", v)
		case tag == tagStr && trimEnvSpace(v.Value) != "":
			putKey(out, "ssl_verify", stringNode(trimEnvSpace(v.Value)))
		}
	}
	return plainCopy(out)
}

// Extra HTTP headers of a provider entry as text by name, null values dropped
func normalizeExtraHeaders(headers *yaml.Node) *yaml.Node {
	out := emptyMapping()
	if !isMapping(headers) {
		return out
	}
	for i := 0; i+1 < len(headers.Content); i += 2 {
		if !isNull(headers.Content[i+1]) {
			putKey(out, displayText(headers.Content[i]), stringNode(displayText(headers.Content[i+1])))
		}
	}
	return out
}

// Legacy custom provider entry in the shape the keyed providers section stores, nil when it cannot be normalized
func (s *Store) customProviderToProviderConfig(entry *yaml.Node, providerKey string) *yaml.Node {
	normalized := s.normalizeCustomProvider(entry, providerKey)
	if normalized == nil {
		return nil
	}
	out := emptyMapping()
	putKey(out, "api", childOf(normalized, "base_url"))
	for _, field := range []string{
		"name", "api_key", "key_env", "key_cmd", "models", "models_discovered", "context_length",
		"rate_limit_delay", "discover_models", "extra_body", "extra_headers",
		"session_affinity_header", "ssl_ca_cert", "ssl_verify", "catalog_provider",
	} {
		if at := mappingIndex(normalized, field); at >= 0 {
			putKey(out, field, normalized.Content[at])
		}
	}
	if at := mappingIndex(normalized, "model"); at >= 0 {
		putKey(out, "default_model", normalized.Content[at])
	}
	if at := mappingIndex(normalized, "api_mode"); at >= 0 {
		putKey(out, "transport", normalized.Content[at])
	}
	return out
}

// Reports a providers entry the picker and the runtime use, hidden only by an explicit false
func IsProviderEnabled(entry *yaml.Node) bool {
	at := mappingIndex(entry, "enabled")
	if at < 0 {
		return true
	}
	flag := entry.Content[at]
	if isStringNode(flag) {
		switch strings.ToLower(trimEnvSpace(flag.Value)) {
		case "false", "0", "no", "off":
			return false
		}
		return true
	}
	return isSet(flag)
}

// Custom endpoint entry in the shape the runtime reads, from the keyed providers section or the legacy list
type CustomEndpoint struct {
	node *yaml.Node
}

// Copy of the normalized entry as a mapping
func (c CustomEndpoint) Node() *yaml.Node { return plainCopy(c.node) }

// String value of one field, empty when the entry lacks it
func (c CustomEndpoint) Text(field string) string { return orText(childOf(c.node, field)) }

// Canonical wire protocol the entry declares, empty when it declares none
func (c CustomEndpoint) APIMode() string {
	for _, field := range []string{"api_mode", "transport"} {
		if v := childOf(c.node, field); isStringNode(v) && trimEnvSpace(v.Value) != "" {
			return CanonicalAPIMode(v.Value)
		}
	}
	return ""
}

// Certificate bundle path and verification switch the entry declares, verify nil when it declares none
func (c CustomEndpoint) TLS() (caCert string, verify *bool) {
	if v := childOf(c.node, "ssl_ca_cert"); isStringNode(v) {
		caCert = trimEnvSpace(v.Value)
	}
	v := childOf(c.node, "ssl_verify")
	if v == nil || v.Kind != yaml.ScalarNode {
		return caCert, nil
	}
	switch tag, text := scalarKind(v); {
	case tag == tagBool:
		on, _ := resolveTagged(tagBool, text)
		return caCert, new(on == "true")
	case tag == tagStr:
		switch strings.ToLower(trimEnvSpace(text)) {
		case "false", "0", "no", "off":
			return caCert, new(false)
		case "true", "1", "yes", "on":
			return caCert, new(true)
		}
	}
	return caCert, nil
}

// Extra HTTP headers the entry declares in order, values that may carry credentials and must never be logged
func (c CustomEndpoint) ExtraHeaders() []KeyValue {
	headers := normalizeExtraHeaders(childOf(c.node, "extra_headers"))
	out := make([]KeyValue, 0, len(headers.Content)/2)
	for i := 0; i+1 < len(headers.Content); i += 2 {
		out = append(out, KeyValue{Path: headers.Content[i].Value, Value: headers.Content[i+1].Value})
	}
	return out
}

// Header name the entry asks session affinity to be sent under, empty when it asks for none
func (c CustomEndpoint) SessionAffinityHeader() string {
	if v := childOf(c.node, "session_affinity_header"); isStringNode(v) {
		return trimEnvSpace(v.Value)
	}
	return ""
}

// Positive whole number a value spells, ok false otherwise
func positiveInt(n *yaml.Node) (int, bool) {
	if n == nil || isNull(n) || n.Kind != yaml.ScalarNode {
		return 0, false
	}
	tag, text := scalarKind(n)
	var v int64
	switch tag {
	case tagInt, tagFloat, tagBool:
		v = int64(numberOf(n))
	default:
		whole, ok := wholeNumberLiteral(text)
		if !ok || !whole.IsInt64() {
			return 0, false
		}
		v = whole.Int64()
	}
	return int(v), v > 0
}

// Context length the entry declares for one model, ok false when it declares none
func (c CustomEndpoint) ModelContextLength(model string) (int, bool) {
	return positiveInt(treeGet(c.node, "models", model, "context_length"))
}

// Context length the entry declares for every model it serves, ok false when it declares none
func (c CustomEndpoint) ContextLength() (int, bool) {
	return positiveInt(childOf(c.node, "context_length"))
}

// Explicit boolean capability the entry declares for one model, set false when it declares none
func (c CustomEndpoint) ModelCapability(model, capability string) (value, set bool) {
	v := treeGet(c.node, "models", model, capability)
	if v == nil || v.Kind != yaml.ScalarNode {
		return false, false
	}
	tag, text := scalarKind(v)
	if tag != tagBool {
		return false, false
	}
	on, _ := resolveTagged(tagBool, text)
	return on == "true", true
}

// Deduplicated endpoint view over the legacy custom_providers list and the keyed providers section of a config tree
func (s *Store) CustomEndpointsOf(config *yaml.Node) []CustomEndpoint {
	var candidates []*yaml.Node
	switch legacy := childOf(config, "custom_providers"); {
	case isSequence(legacy):
		for _, entry := range legacy.Content {
			candidates = append(candidates, s.normalizeCustomProvider(entry, ""))
		}
	case legacy != nil && !isNull(legacy):
		slog.Warn(fmt.Sprintf("custom_providers is a %s, expected a list — skipping legacy entries; "+
			"'providers:' entries are still used. Move provider configs to the 'providers:' section.", typeName(legacy)))
	}
	if providers := childOf(config, "providers"); isMapping(providers) {
		for i := 0; i+1 < len(providers.Content); i += 2 {
			entry := providers.Content[i+1]
			if isMapping(entry) && !IsProviderEnabled(entry) {
				continue
			}
			candidates = append(candidates, s.normalizeCustomProvider(entry, providerID(providers.Content[i])))
		}
	}
	norm := func(entry *yaml.Node, field string) string {
		return strings.ToLower(trimEnvSpace(orText(childOf(entry, field))))
	}
	var out []CustomEndpoint
	seenKeys, seenPairs := map[string]bool{}, map[string]bool{}
	for _, entry := range candidates {
		if entry == nil {
			continue
		}
		key, name := norm(entry, "provider_key"), norm(entry, "name")
		baseURL := strings.ToLower(strings.TrimRight(trimEnvSpace(orText(childOf(entry, "base_url"))), "/"))
		pair := name + "\x00" + baseURL + "\x00" + norm(entry, "model")
		if (key != "" && seenKeys[key]) || (name != "" && baseURL != "" && seenPairs[pair]) {
			continue
		}
		out = append(out, CustomEndpoint{node: entry})
		if key != "" {
			seenKeys[key] = true
		}
		if name != "" && baseURL != "" {
			seenPairs[pair] = true
		}
	}
	return out
}

// Deduplicated endpoint view of the merged config of ctx
func (s *Store) CustomEndpoints(ctx context.Context) ([]CustomEndpoint, error) {
	tree, err := s.mergedTree(ctx)
	if err != nil {
		return nil, err
	}
	return s.CustomEndpointsOf(tree), nil
}
