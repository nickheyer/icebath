package config

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// One named provider under providers or one item of the legacy custom_providers list
type ProviderEntry struct {
	Enabled               Scalar          `yaml:"enabled,omitempty"`
	Provider              string          `yaml:"provider,omitempty" coerce:"exact"`
	Name                  string          `yaml:"name,omitempty"`
	API                   string          `yaml:"api,omitempty" coerce:"exact"`
	URL                   string          `yaml:"url,omitempty" coerce:"exact"`
	BaseURL               string          `yaml:"base_url,omitempty" coerce:"exact"`
	APIKey                string          `yaml:"api_key,omitempty" coerce:"exact"`
	KeyEnv                string          `yaml:"key_env,omitempty" coerce:"exact"`
	APIKeyEnv             string          `yaml:"api_key_env,omitempty" coerce:"exact"`
	KeyCmd                string          `yaml:"key_cmd,omitempty" coerce:"exact"`
	APIMode               string          `yaml:"api_mode,omitempty" coerce:"exact"`
	Transport             string          `yaml:"transport,omitempty" coerce:"exact"`
	Model                 string          `yaml:"model,omitempty" coerce:"exact"`
	DefaultModel          string          `yaml:"default_model,omitempty" coerce:"exact"`
	Models                ProviderModels  `yaml:"models,omitempty"`
	ModelsDiscovered      bool            `yaml:"models_discovered,omitempty" coerce:"strict"`
	ContextLength         int             `yaml:"context_length,omitempty" coerce:"exactint"`
	RateLimitDelay        *float64        `yaml:"rate_limit_delay,omitempty" coerce:"exact"`
	RequestTimeoutSeconds *float64        `yaml:"request_timeout_seconds,omitempty"`
	StaleTimeoutSeconds   *float64        `yaml:"stale_timeout_seconds,omitempty"`
	DiscoverModels        Scalar          `yaml:"discover_models,omitempty"`
	ExtraBody             Map[Raw]        `yaml:"extra_body,omitempty"`
	ExtraHeaders          Map[string]     `yaml:"extra_headers,omitempty"`
	Capabilities          Map[Raw]        `yaml:"capabilities,omitempty"`
	SSLCACert             string          `yaml:"ssl_ca_cert,omitempty" coerce:"exact"`
	SSLVerify             Scalar          `yaml:"ssl_verify,omitempty"`
	CatalogProvider       string          `yaml:"catalog_provider,omitempty" coerce:"exact"`
	SessionAffinityHeader string          `yaml:"session_affinity_header,omitempty" coerce:"exact"`
	ReasoningEcho         *bool           `yaml:"reasoning_echo,omitempty" coerce:"bool"`
	CamelAliases          providerAliases `yaml:",inline"`
	raw                   *yaml.Node
	rest                  rest
}

// Hand-written camelCase spellings that load normalization maps onto the snake_case fields
type providerAliases struct {
	APIKey                string   `yaml:"apiKey,omitempty" coerce:"exact"`
	BaseURL               string   `yaml:"baseUrl,omitempty" coerce:"exact"`
	APIMode               string   `yaml:"apiMode,omitempty" coerce:"exact"`
	KeyEnv                string   `yaml:"keyEnv,omitempty" coerce:"exact"`
	APIKeyEnv             string   `yaml:"apiKeyEnv,omitempty" coerce:"exact"`
	DefaultModel          string   `yaml:"defaultModel,omitempty" coerce:"exact"`
	ContextLength         int      `yaml:"contextLength,omitempty" coerce:"exactint"`
	RateLimitDelay        *float64 `yaml:"rateLimitDelay,omitempty" coerce:"exact"`
	SessionAffinityHeader string   `yaml:"sessionAffinityHeader,omitempty" coerce:"exact"`
}

type providerEntryFields ProviderEntry

// Decodes a mapping entry keeping unknown keys, and holds a stray non-mapping list item as written
func (p *ProviderEntry) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n.Kind != yaml.MappingNode {
		*p = ProviderEntry{raw: plainCopy(n)}
		return nil
	}
	p.raw = nil
	return decodeEntry(n, (*providerEntryFields)(p), &p.rest)
}

// Writes the entry, or the stray item exactly as read
func (p ProviderEntry) MarshalYAML() (any, error) {
	if p.raw != nil {
		return emitted(p.raw), nil
	}
	return encodeMapping(providerEntryFields(p), p.rest)
}

// Keeps an explicit null as a stray value
func (ProviderEntry) holdsNull() {}

// Reports a mapping entry rather than a stray scalar or list item
func (p ProviderEntry) Valid() bool {
	return p.raw == nil
}

// Keys of the entry the schema does not name
func (p ProviderEntry) Unknown() []string {
	return p.rest.names()
}

// Models of a provider entry, a mapping by id or a list of ids and id mappings
type ProviderModels struct {
	entries           Map[ProviderModel]
	Discovered        bool
	ExplicitAllowlist bool
	list              bool
	items             []modelItem
}

// How one list item was written so a save reproduces it, a stray item without an id kept as written
type modelItem struct {
	id    string
	bare  bool
	idKey string
	raw   *yaml.Node
}

const (
	discoveredMarker = "__discovered_model_catalog__"
	allowlistMarker  = "__explicit_model_allowlist__"
)

// Settings of one model id
func (m ProviderModels) Get(id string) (ProviderModel, bool) {
	return m.entries.Get(id)
}

// Stores the settings of one model id
func (m *ProviderModels) Set(id string, v ProviderModel) {
	m.entries.Set(id, v)
}

// Removes one model id
func (m *ProviderModels) Delete(id string) {
	m.entries.Delete(id)
}

// Model ids in file order
func (m ProviderModels) IDs() []string {
	return m.entries.Keys()
}

// Reports whether the file held the list form
func (m ProviderModels) IsList() bool {
	return m.list
}

// Reports an empty value so omitempty drops it
func (m ProviderModels) IsZero() bool {
	return m.entries.IsZero() && !m.Discovered && !m.ExplicitAllowlist
}

// Accepts a mapping of id to settings or a list of ids and id mappings
func (m *ProviderModels) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	switch n.Kind {
	case yaml.MappingNode:
		m.list = false
		body := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
		for _, p := range mappingPairs(n) {
			switch p.key.Value {
			case discoveredMarker:
				m.Discovered = isTrue(p.value)
			case allowlistMarker:
				m.ExplicitAllowlist = isTrue(p.value)
			default:
				body.Content = append(body.Content, p.key, p.value)
			}
		}
		return m.entries.UnmarshalYAML(body)
	case yaml.SequenceNode:
		*m = ProviderModels{list: true}
		var errs []error
		for i, item := range n.Content {
			if item.Kind == yaml.ScalarNode && item.ShortTag() == tagStr && strings.TrimSpace(item.Value) != "" {
				id := strings.TrimSpace(item.Value)
				m.entries.Set(id, ProviderModel{})
				m.items = append(m.items, modelItem{id: id, bare: true})
				continue
			}
			id, key := listItemID(item)
			if id == "" {
				m.items = append(m.items, modelItem{raw: plainCopy(item)})
				continue
			}
			var v ProviderModel
			errs = append(errs, underKey("["+strconv.Itoa(i)+"]", v.UnmarshalYAML(withoutKeys(item, "id", "name"))))
			m.entries.Set(id, v)
			m.items = append(m.items, modelItem{id: id, idKey: key})
		}
		return errors.Join(errs...)
	}
	return shapeError(n, yaml.MappingNode)
}

// Reports a boolean true scalar
func isTrue(n *yaml.Node) bool {
	n = resolveAlias(n)
	return n != nil && n.Kind == yaml.ScalarNode && n.ShortTag() == tagBool && n.Value == "true"
}

// Id of a list item mapping from its id key, else its name key, with the key used
func listItemID(item *yaml.Node) (id, key string) {
	if item.Kind != yaml.MappingNode {
		return "", ""
	}
	pairs := mappingPairs(item)
	for _, want := range []string{"id", "name"} {
		for _, p := range pairs {
			if p.key.Value == want {
				if v := strings.TrimSpace(scalarText(p.value)); v != "" {
					return v, want
				}
			}
		}
	}
	return "", ""
}

// Mapping node without the named keys
func withoutKeys(n *yaml.Node, skip ...string) *yaml.Node {
	body := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	for _, p := range mappingPairs(n) {
		if !slices.Contains(skip, p.key.Value) {
			body.Content = append(body.Content, p.key, p.value)
		}
	}
	return body
}

// Writes the form that was read
func (m ProviderModels) MarshalYAML() (any, error) {
	if !m.list {
		out, err := m.entries.MarshalYAML()
		if err != nil {
			return nil, err
		}
		n := out.(*yaml.Node)
		for _, marker := range []struct {
			on  bool
			key string
		}{{m.Discovered, discoveredMarker}, {m.ExplicitAllowlist, allowlistMarker}} {
			if marker.on {
				n.Content = append(n.Content, strNode(marker.key), &yaml.Node{Kind: yaml.ScalarNode, Tag: tagBool, Value: "true"})
			}
		}
		return n, nil
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq}
	written := map[string]bool{}
	emit := func(it modelItem) error {
		v, ok := m.entries.Get(it.id)
		if !ok || written[it.id] {
			return nil
		}
		written[it.id] = true
		body, err := encodeValue(reflect.ValueOf(v))
		if err != nil {
			return err
		}
		if it.bare && body.Kind == yaml.MappingNode && len(body.Content) == 0 {
			seq.Content = append(seq.Content, strNode(it.id))
			return nil
		}
		if body.Kind != yaml.MappingNode {
			seq.Content = append(seq.Content, body)
			return nil
		}
		key := it.idKey
		if key == "" {
			key = "id"
		}
		item := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap, Content: []*yaml.Node{strNode(key), strNode(it.id)}}
		item.Content = append(item.Content, body.Content...)
		seq.Content = append(seq.Content, item)
		return nil
	}
	for _, it := range m.items {
		if it.raw != nil {
			seq.Content = append(seq.Content, emitted(it.raw))
			continue
		}
		if err := emit(it); err != nil {
			return nil, err
		}
	}
	for _, id := range m.entries.Keys() {
		if err := emit(modelItem{id: id, bare: true}); err != nil {
			return nil, err
		}
	}
	return seq, nil
}

// Per-model settings of a provider entry, capability flags kept among the unknown keys
type ProviderModel struct {
	ContextLength       int      `yaml:"context_length,omitempty"`
	TimeoutSeconds      *float64 `yaml:"timeout_seconds,omitempty"`
	StaleTimeoutSeconds *float64 `yaml:"stale_timeout_seconds,omitempty"`
	raw                 *yaml.Node
	rest                rest
}

type providerModelFields ProviderModel

// Decodes the mapping keeping capability flags and unknown keys, and holds a non-mapping value as written
func (p *ProviderModel) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n.Kind != yaml.MappingNode {
		*p = ProviderModel{raw: plainCopy(n)}
		return nil
	}
	p.raw = nil
	return decodeEntry(n, (*providerModelFields)(p), &p.rest)
}

// Keeps an explicit null as a model without settings
func (ProviderModel) holdsNull() {}

// Writes the mapping, or the non-mapping value exactly as read
func (p ProviderModel) MarshalYAML() (any, error) {
	if p.raw != nil {
		return emitted(p.raw), nil
	}
	return encodeMapping(providerModelFields(p), p.rest)
}

// Explicit boolean flag such as supports_vision, prompt_caching or answer_in_reasoning, set false when absent
func (p ProviderModel) Capability(name string) (value, set bool) {
	n := p.rest.get(name)
	if n == nil || n.Kind != yaml.ScalarNode || n.ShortTag() != tagBool {
		return false, false
	}
	return n.Value == "true", true
}
