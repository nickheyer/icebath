package config

import (
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Model key, a bare model id or a mapping of routing settings
type ModelSetting struct {
	ModelConfig
	Bare bool
}

// Accepts a bare model id or a mapping merged over the existing settings, a bare id replacing them all
func (m *ModelSetting) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	switch n.Kind {
	case yaml.ScalarNode:
		*m = ModelSetting{Bare: true, ModelConfig: ModelConfig{Default: ModelRef{ID: scalarText(n)}}}
		return nil
	case yaml.MappingNode:
		m.Bare = false
		return m.ModelConfig.UnmarshalYAML(n)
	}
	return shapeError(n, yaml.MappingNode)
}

// Writes a bare id when the file held one and the mapping otherwise
func (m ModelSetting) MarshalYAML() (any, error) {
	if m.Bare {
		return strNode(m.Default.ID), nil
	}
	return m.ModelConfig.MarshalYAML()
}

// Mapping form of the model key, with Model, Name and APIBase as legacy spellings load normalization folds in
type ModelConfig struct {
	Default                ModelRef       `yaml:"default,omitempty"`
	Provider               string         `yaml:"provider,omitempty"`
	BaseURL                string         `yaml:"base_url,omitempty"`
	APIKey                 string         `yaml:"api_key,omitempty"`
	API                    string         `yaml:"api,omitempty"`
	KeyEnv                 string         `yaml:"key_env,omitempty"`
	APIKeyEnv              string         `yaml:"api_key_env,omitempty"`
	APIMode                string         `yaml:"api_mode,omitempty"`
	ContextLength          int            `yaml:"context_length,omitempty"`
	OllamaNumCtx           int            `yaml:"ollama_num_ctx,omitempty"`
	Streaming              Scalar         `yaml:"streaming,omitempty"`
	DefaultHeaders         Map[string]    `yaml:"default_headers,omitempty"`
	ExtraHeaders           Map[string]    `yaml:"extra_headers,omitempty"`
	AuthMode               string         `yaml:"auth_mode,omitempty"`
	Entra                  *EntraSettings `yaml:"entra,omitempty"`
	OpenAIRuntime          string         `yaml:"openai_runtime,omitempty"`
	CodexBin               string         `yaml:"codex_bin,omitempty"`
	SupportsVision         Scalar         `yaml:"supports_vision,omitempty"`
	SwitchContextConfirm   *int           `yaml:"switch_context_confirm_tokens,omitempty"`
	PersistSwitchByDefault bool           `yaml:"persist_switch_by_default,omitempty" coerce:"bool"`
	ReasoningEcho          *bool          `yaml:"reasoning_echo,omitempty" coerce:"bool"`
	LMStudioLoadMode       string         `yaml:"lmstudio_load_mode,omitempty"`
	Model                  ModelRef       `yaml:"model,omitempty"`
	Name                   ModelRef       `yaml:"name,omitempty"`
	APIBase                string         `yaml:"api_base,omitempty"`
	rest                   rest
}

type modelConfigFields ModelConfig

// Decodes the mapping and keeps unknown keys
func (m *ModelConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*modelConfigFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m ModelConfig) MarshalYAML() (any, error) {
	return encodeMapping(modelConfigFields(m), m.rest)
}

// Entra ID block of an Azure model route
type EntraSettings struct {
	Scope string `yaml:"scope,omitempty"`
	rest  rest
}

type entraFields EntraSettings

// Decodes the mapping and keeps unknown keys
func (e *EntraSettings) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*entraFields)(e), &e.rest)
}

// Writes the mapping with unknown keys after the named ones
func (e EntraSettings) MarshalYAML() (any, error) {
	return encodeMapping(entraFields(e), e.rest)
}

// Model id written as a string or as a mapping pairing it with the provider it routes through
type ModelRef struct {
	ID       string
	Provider string
	Mapping  bool
	idKey    string
	rest     rest
}

// Accepts a model id or a mapping with provider and model or default
func (r *ModelRef) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	switch n.Kind {
	case yaml.ScalarNode:
		*r = ModelRef{ID: scalarText(n)}
		return nil
	case yaml.MappingNode:
		out := ModelRef{Mapping: true}
		var fromDefault string
		for _, p := range mappingPairs(n) {
			switch p.key.Value {
			case "provider":
				out.Provider = scalarText(p.value)
			case "model":
				out.ID, out.idKey = scalarText(p.value), "model"
			case "default":
				fromDefault = scalarText(p.value)
			default:
				out.rest.set(p.key, p.value)
			}
		}
		if out.ID == "" && fromDefault != "" {
			out.ID, out.idKey = fromDefault, "default"
		}
		*r = out
		return nil
	}
	return shapeError(n, yaml.ScalarNode)
}

// Writes the form that was read
func (r ModelRef) MarshalYAML() (any, error) {
	if !r.Mapping {
		return strNode(r.ID), nil
	}
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	if r.Provider != "" {
		n.Content = append(n.Content, strNode("provider"), strNode(r.Provider))
	}
	key := r.idKey
	if key == "" {
		key = "model"
	}
	n.Content = append(n.Content, strNode(key), strNode(r.ID))
	n.Content = append(n.Content, r.rest.emit()...)
	return n, nil
}

// Reports an unset reference so omitempty drops it
func (r ModelRef) IsZero() bool {
	return r.ID == "" && r.Provider == "" && !r.Mapping
}

// Trimmed model id
func (r ModelRef) String() string {
	return strings.TrimSpace(r.ID)
}

// One route of a fallback chain
type FallbackEntry struct {
	Provider      string `yaml:"provider,omitempty"`
	Model         string `yaml:"model,omitempty"`
	BaseURL       string `yaml:"base_url,omitempty"`
	APIKey        string `yaml:"api_key,omitempty"`
	KeyEnv        string `yaml:"key_env,omitempty"`
	APIKeyEnv     string `yaml:"api_key_env,omitempty"`
	APIMode       string `yaml:"api_mode,omitempty"`
	Transport     string `yaml:"transport,omitempty"`
	ReasoningEcho *bool  `yaml:"reasoning_echo,omitempty"`
	raw           *yaml.Node
	rest          rest
}

type fallbackEntryFields FallbackEntry

// Decodes a mapping entry keeping unknown keys, and holds a stray non-mapping item as written
func (f *FallbackEntry) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n.Kind != yaml.MappingNode {
		*f = FallbackEntry{raw: plainCopy(n)}
		return nil
	}
	f.raw = nil
	return decodeEntry(n, (*fallbackEntryFields)(f), &f.rest)
}

// Writes the entry, or the stray item exactly as read
func (f FallbackEntry) MarshalYAML() (any, error) {
	if f.raw != nil {
		return emitted(f.raw), nil
	}
	return encodeMapping(fallbackEntryFields(f), f.rest)
}

// Keeps an explicit null as a stray item
func (FallbackEntry) holdsNull() {}

// Reports a mapping entry rather than a stray scalar or list item
func (f FallbackEntry) Valid() bool {
	return f.raw == nil
}

// Fallback list, also accepted as one mapping, written back in the form that was read
type FallbackChain struct {
	Entries []FallbackEntry
	Single  bool
	Other   *yaml.Node
}

// Accepts a list of entries or a single entry mapping and keeps any other value as written
func (c *FallbackChain) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	switch n.Kind {
	case yaml.MappingNode:
		var e FallbackEntry
		err := e.UnmarshalYAML(n)
		*c = FallbackChain{Entries: []FallbackEntry{e}, Single: true}
		return err
	case yaml.SequenceNode:
		var entries []FallbackEntry
		_, err := decodeValue(n, reflect.ValueOf(&entries).Elem(), coerceDefault)
		*c = FallbackChain{Entries: entries}
		return err
	}
	*c = FallbackChain{Other: plainCopy(n)}
	return nil
}

// Writes the form that was read
func (c FallbackChain) MarshalYAML() (any, error) {
	switch {
	case c.Other != nil:
		return emitted(c.Other), nil
	case c.Single && len(c.Entries) == 1:
		return c.Entries[0], nil
	case c.Entries == nil:
		return []FallbackEntry{}, nil
	}
	return c.Entries, nil
}

// Reports an empty chain so omitempty drops it
func (c FallbackChain) IsZero() bool {
	return len(c.Entries) == 0 && c.Other == nil && !c.Single
}

// Mapping entries that name both a provider and a model, in order
func (c FallbackChain) Routes() []FallbackEntry {
	var out []FallbackEntry
	for _, e := range c.Entries {
		if e.Valid() && strings.TrimSpace(e.Provider) != "" && strings.TrimSpace(e.Model) != "" {
			out = append(out, e)
		}
	}
	return out
}

// Legacy custom_providers list, holding any other shape as written so validation can report it
type CustomProviders struct {
	Entries []ProviderEntry
	Other   *yaml.Node
}

// Accepts the list and keeps any other value as written
func (c *CustomProviders) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n.Kind != yaml.SequenceNode {
		*c = CustomProviders{Other: plainCopy(n)}
		return nil
	}
	var entries []ProviderEntry
	_, err = decodeValue(n, reflect.ValueOf(&entries).Elem(), coerceDefault)
	*c = CustomProviders{Entries: entries}
	return err
}

// Writes the list, or the other value exactly as read
func (c CustomProviders) MarshalYAML() (any, error) {
	if c.Other != nil {
		return emitted(c.Other), nil
	}
	if c.Entries == nil {
		return []ProviderEntry{}, nil
	}
	return c.Entries, nil
}

// Reports an absent list so omitempty drops it
func (c CustomProviders) IsZero() bool {
	return c.Entries == nil && c.Other == nil
}

// How a credential pool picks the next key
type PoolStrategy string

// Credential pool strategies
const (
	PoolFillFirst  PoolStrategy = "fill_first"
	PoolRoundRobin PoolStrategy = "round_robin"
	PoolRandom     PoolStrategy = "random"
	PoolLeastUsed  PoolStrategy = "least_used"
)

// Accepted pool strategy values, any other value reads as fill_first
func PoolStrategies() []PoolStrategy {
	return []PoolStrategy{PoolFillFirst, PoolRoundRobin, PoolRandom, PoolLeastUsed}
}

// Fallback section
type FallbackSettings struct {
	MinSwitchResetSeconds float64 `yaml:"min_switch_reset_seconds"`
	rest                  rest
}

type fallbackSettingsFields FallbackSettings

// Decodes the mapping and keeps unknown keys
func (f *FallbackSettings) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*fallbackSettingsFields)(f), &f.rest)
}

// Writes the mapping with unknown keys after the named ones
func (f FallbackSettings) MarshalYAML() (any, error) {
	return encodeMapping(fallbackSettingsFields(f), f.rest)
}

// Documentation of the model routing keys
func modelDocs() map[string]string {
	return map[string]string{
		"model":                             "Default model id, or a mapping of default, provider, base_url and the other routing keys below.",
		"model.default":                     "Default model to use (can be overridden with --model). model and name are accepted as aliases.",
		"model.provider":                    "Inference provider. auto detects from credentials, or a provider id such as openrouter, portal, anthropic, openai-codex, gemini, custom. Can be overridden with --provider.",
		"model.base_url":                    "API endpoint for the provider. api_base is accepted as an alias.",
		"model.api_key":                     "API key for the provider, instead of setting it in .env.",
		"model.streaming":                   "Stream API responses from the provider (default true). false forces non-streaming requests for the whole session, an escape hatch for self-hosted servers whose streaming tool-call path is broken. Orthogonal to display.streaming.",
		"model.context_length":              "TOTAL context window (input + output tokens). Leave unset to auto-detect from the provider; set it only when detection is wrong, such as a local server with a custom num_ctx.",
		"model.ollama_num_ctx":              "Ollama only. The num_ctx sent on every chat request; auto-detected by default and capped by context_length, while an explicit value is sent as-is to pin VRAM use.",
		"model.default_headers":             "Extra HTTP headers sent on every request to an OpenAI-compatible endpoint; user values take precedence over provider and SDK defaults. OpenAI wire only.",
		"model.extra_headers":               "Alias of default_headers, merged with extra_headers winning when both are set.",
		"model.auth_mode":                   "Azure Foundry auth. api_key (default) or entra_id for keyless DefaultAzureCredential auth.",
		"model.entra.scope":                 "Entra ID token scope; default https://ai.azure.com/.default.",
		"providers":                         "Named provider definitions keyed by provider id (base_url, key_env or key_cmd, api_mode, models, request_timeout_seconds, stale_timeout_seconds, extra_headers, extra_body, session_affinity_header).",
		"fallback_providers":                "Ordered fallback chain of {provider, model} routes used when the primary is unavailable.",
		"fallback.min_switch_reset_seconds": "Opt-in (0 = off). When a rate-limited primary declares a reset sooner than this many seconds, stay on it (the retry backoff rides out the window) instead of switching the turn to a fallback model.",
		"credential_pool_strategies":        "Credential pool selection strategy per provider. fill_first (default), round_robin, least_used, random.",
		"toolsets":                          "Toolsets enabled for the CLI.",
		"custom_providers":                  "Legacy list form of providers; each entry needs name and base_url.",
		"fallback_model":                    "Legacy fallback route, a single {provider, model} mapping or a list of them. Triggers on rate limits (429), overload (529), service errors (503), or connection failures.",
	}
}

// Canonical wire protocols an api_mode or transport key names
func APIModes() []string {
	return []string{"chat_completions", "codex_responses", "anthropic_messages", "bedrock_converse"}
}

// Azure model route auth modes
func AuthModes() []string {
	return []string{"api_key", "entra_id"}
}
