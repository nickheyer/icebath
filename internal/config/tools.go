package config

import (
	"errors"
	"reflect"
	"slices"

	"gopkg.in/yaml.v3"
)

// Tool surface section
type ToolsConfig struct {
	ToolSearch ToolSearchConfig `yaml:"tool_search"`
	Connectors ConnectorsConfig `yaml:"connectors"`
	rest       rest
}

type toolsFields ToolsConfig

// Decodes the mapping and keeps unknown keys
func (t *ToolsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*toolsFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t ToolsConfig) MarshalYAML() (any, error) {
	return encodeMapping(toolsFields(t), t.rest)
}

// Deferred tool bridge settings
type ToolSearchConfig struct {
	Enabled            Scalar   `yaml:"enabled"`
	ThresholdPct       float64  `yaml:"threshold_pct"`
	SearchDefaultLimit int      `yaml:"search_default_limit"`
	MaxSearchLimit     int      `yaml:"max_search_limit"`
	Listing            Scalar   `yaml:"listing"`
	ListingMaxTokens   int      `yaml:"listing_max_tokens"`
	Defer              []string `yaml:"defer"`
	bare               bareForm
	rest               rest
}

type toolSearchFields ToolSearchConfig

// Decodes the mapping and keeps unknown keys, a bare false meaning off and any other scalar auto
func (t *ToolSearchConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeSwitchable(n, (*toolSearchFields)(t), &t.rest, &t.bare, func(s *yaml.Node) {
		t.Enabled = Text("auto")
		if s.ShortTag() == tagBool && s.Value == "false" {
			t.Enabled = Text("off")
		}
	})
}

// Writes the bare scalar the file held while nothing changed, else the mapping with unknown keys last
func (t ToolSearchConfig) MarshalYAML() (any, error) {
	return encodeSwitchable(toolSearchFields(t), t.rest, t.bare)
}

// Tool search activation and listing words with their aliases, other values reading as auto
func ToolSearchModes() []string {
	return []string{"auto", "on", "true", "1", "yes", "off", "false", "0", "no"}
}

// Remote connector discovery switch
type ConnectorsConfig struct {
	Enabled bool `yaml:"enabled" coerce:"notfalse"`
	bare    bareForm
	rest    rest
}

type connectorsFields ConnectorsConfig

// Decodes the mapping and keeps unknown keys, a bare boolean standing for enabled and any other scalar for defaults
func (c *ConnectorsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeSwitchable(n, (*connectorsFields)(c), &c.rest, &c.bare, func(s *yaml.Node) {
		if s.ShortTag() == tagBool {
			c.Enabled = s.Value == "true"
		}
	})
}

// Writes the bare scalar the file held while nothing changed, else the mapping with unknown keys last
func (c ConnectorsConfig) MarshalYAML() (any, error) {
	return encodeSwitchable(connectorsFields(c), c.rest, c.bare)
}

// File logging section
type LoggingConfig struct {
	Level       LogLevel `yaml:"level"`
	MaxSizeMB   float64  `yaml:"max_size_mb"`
	BackupCount int      `yaml:"backup_count"`
	rest        rest
}

type loggingFields LoggingConfig

// Decodes the mapping and keeps unknown keys
func (l *LoggingConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*loggingFields)(l), &l.rest)
}

// Writes the mapping with unknown keys after the named ones
func (l LoggingConfig) MarshalYAML() (any, error) {
	return encodeMapping(loggingFields(l), l.rest)
}

// Minimum level written to agent.log
type LogLevel string

// Log levels
const (
	LogDebug   LogLevel = "DEBUG"
	LogInfo    LogLevel = "INFO"
	LogWarning LogLevel = "WARNING"
	LogError   LogLevel = "ERROR"
)

// Accepted log levels
func LogLevels() []LogLevel {
	return []LogLevel{LogDebug, LogInfo, LogWarning, LogError}
}

// Remote model catalog manifest section, with the ttl_hours key of older configs
type ModelCatalogConfig struct {
	Enabled    bool                 `yaml:"enabled" coerce:"bool"`
	URL        string               `yaml:"url"`
	TTLMinutes float64              `yaml:"ttl_minutes"`
	Providers  Map[CatalogProvider] `yaml:"providers"`
	TTLHours   *float64             `yaml:"ttl_hours,omitempty"`
	rest       rest
}

type modelCatalogFields ModelCatalogConfig

// Decodes the mapping and keeps unknown keys
func (m *ModelCatalogConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*modelCatalogFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m ModelCatalogConfig) MarshalYAML() (any, error) {
	return encodeMapping(modelCatalogFields(m), m.rest)
}

// Per-provider curation list override
type CatalogProvider struct {
	URL  string `yaml:"url,omitempty"`
	rest rest
}

type catalogProviderFields CatalogProvider

// Decodes the mapping and keeps unknown keys
func (c *CatalogProvider) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*catalogProviderFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c CatalogProvider) MarshalYAML() (any, error) {
	return encodeMapping(catalogProviderFields(c), c.rest)
}

// Per-model metadata overrides by provider and model id, with the top-level _default gap filler
type ModelOverrides struct {
	Default    *ModelOverride
	Providers  Map[Map[ModelOverride]]
	defaultRaw *yaml.Node
	order      []string
}

const overrideDefaultKey = "_default"

// Accepts provider mappings of model id to override and the top-level _default override
func (m *ModelOverrides) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if isNull(n) {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return shapeError(n, yaml.MappingNode)
	}
	providers := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	var errs []error
	for _, p := range mappingPairs(n) {
		if !slices.Contains(m.order, p.key.Value) {
			m.order = append(m.order, p.key.Value)
		}
		if p.key.Value != overrideDefaultKey {
			providers.Content = append(providers.Content, p.key, p.value)
			continue
		}
		if isNull(p.value) {
			if m.Default == nil {
				m.defaultRaw = plainCopy(p.value)
			}
			continue
		}
		if m.Default == nil {
			m.Default = &ModelOverride{}
		}
		m.defaultRaw = nil
		hold, err := decodeValue(p.value, reflect.ValueOf(m.Default).Elem(), coerceDefault)
		if hold {
			m.Default, m.defaultRaw = nil, plainCopy(p.value)
		}
		errs = append(errs, underKey(p.key.Value, err))
	}
	errs = append(errs, m.Providers.UnmarshalYAML(providers))
	if slices.Equal(m.order, canonicalOrder(m.order, []string{overrideDefaultKey}, m.Providers.keys)) {
		m.order = nil
	}
	return errors.Join(errs...)
}

// Writes the overrides in file order
func (m ModelOverrides) MarshalYAML() (any, error) {
	out, err := m.Providers.MarshalYAML()
	if err != nil {
		return nil, err
	}
	n := out.(*yaml.Node)
	var def *yaml.Node
	switch {
	case m.Default != nil:
		if def, err = encodeValue(reflect.ValueOf(m.Default)); err != nil {
			return nil, err
		}
	case m.defaultRaw != nil:
		def = emitted(m.defaultRaw)
	default:
		return n, nil
	}
	at := 0
	if i := slices.Index(m.order, overrideDefaultKey); i > 0 {
		for j := 0; j+1 < len(n.Content); j += 2 {
			if k := slices.Index(m.order, n.Content[j].Value); k >= 0 && k < i {
				at = j + 2
			}
		}
	}
	n.Content = slices.Insert(n.Content, at, strNode(overrideDefaultKey), def)
	return n, nil
}

// Metadata fields one model override may set, unset fields keeping the catalog value
type ModelOverride struct {
	ContextWindow     *int   `yaml:"context_window,omitempty"`
	SupportsTools     *bool  `yaml:"supports_tools,omitempty" coerce:"bool"`
	SupportsVision    *bool  `yaml:"supports_vision,omitempty" coerce:"bool"`
	SupportsReasoning *bool  `yaml:"supports_reasoning,omitempty" coerce:"bool"`
	ModelFamily       string `yaml:"model_family,omitempty"`
	rest              rest
}

type modelOverrideFields ModelOverride

// Decodes the mapping and keeps unknown keys
func (m *ModelOverride) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*modelOverrideFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m ModelOverride) MarshalYAML() (any, error) {
	return encodeMapping(modelOverrideFields(m), m.rest)
}

// models.dev registry mirror section
type ModelsDevConfig struct {
	URL  string `yaml:"url"`
	rest rest
}

type modelsDevFields ModelsDevConfig

// Decodes the mapping and keeps unknown keys
func (m *ModelsDevConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*modelsDevFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m ModelsDevConfig) MarshalYAML() (any, error) {
	return encodeMapping(modelsDevFields(m), m.rest)
}

// Network section
type NetworkConfig struct {
	ForceIPv4 bool `yaml:"force_ipv4" coerce:"bool"`
	rest      rest
}

type networkFields NetworkConfig

// Decodes the mapping and keeps unknown keys
func (c *NetworkConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*networkFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c NetworkConfig) MarshalYAML() (any, error) {
	return encodeMapping(networkFields(c), c.rest)
}

// Gateway health monitoring section
type MonitoringConfig struct {
	InstallID           string              `yaml:"install_id"`
	GatewayHealthExport GatewayHealthExport `yaml:"gateway_health_export"`
	Export              MonitoringExport    `yaml:"export"`
	rest                rest
}

type monitoringFields MonitoringConfig

// Decodes the mapping and keeps unknown keys
func (m *MonitoringConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*monitoringFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MonitoringConfig) MarshalYAML() (any, error) {
	return encodeMapping(monitoringFields(m), m.rest)
}

// Gateway health and diagnostics export
type GatewayHealthExport struct {
	Enabled                   bool        `yaml:"enabled" coerce:"bool"`
	MetricsEnabled            bool        `yaml:"metrics_enabled" coerce:"bool"`
	DiagnosticEventsEnabled   bool        `yaml:"diagnostic_events_enabled" coerce:"bool"`
	WarningErrorEventsEnabled bool        `yaml:"warning_error_events_enabled" coerce:"bool"`
	ExportIntervalSeconds     int         `yaml:"export_interval_seconds"`
	LogsExportIntervalSeconds int         `yaml:"logs_export_interval_seconds"`
	ResourceAttributes        Map[string] `yaml:"resource_attributes"`
	rest                      rest
}

type gatewayHealthExportFields GatewayHealthExport

// Decodes the mapping and keeps unknown keys
func (g *GatewayHealthExport) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*gatewayHealthExportFields)(g), &g.rest)
}

// Writes the mapping with unknown keys after the named ones
func (g GatewayHealthExport) MarshalYAML() (any, error) {
	return encodeMapping(gatewayHealthExportFields(g), g.rest)
}

// Monitoring export destinations
type MonitoringExport struct {
	OTLP OTLPExport `yaml:"otlp"`
	rest rest
}

type monitoringExportFields MonitoringExport

// Decodes the mapping and keeps unknown keys
func (m *MonitoringExport) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*monitoringExportFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MonitoringExport) MarshalYAML() (any, error) {
	return encodeMapping(monitoringExportFields(m), m.rest)
}

// OTLP destination, headers named by the environment variables holding their values
type OTLPExport struct {
	Enabled    bool        `yaml:"enabled"`
	Endpoint   string      `yaml:"endpoint"`
	HeadersEnv Map[string] `yaml:"headers_env"`
	rest       rest
}

type otlpExportFields OTLPExport

// Decodes the mapping and keeps unknown keys
func (o *OTLPExport) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*otlpExportFields)(o), &o.rest)
}

// Writes the mapping with unknown keys after the named ones
func (o OTLPExport) MarshalYAML() (any, error) {
	return encodeMapping(otlpExportFields(o), o.rest)
}

// Hosted room limits section
type HostedRoomsConfig struct {
	Discussion RoomDiscussion `yaml:"discussion"`
	rest       rest
}

type hostedRoomsFields HostedRoomsConfig

// Decodes the mapping and keeps unknown keys
func (h *HostedRoomsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*hostedRoomsFields)(h), &h.rest)
}

// Writes the mapping with unknown keys after the named ones
func (h HostedRoomsConfig) MarshalYAML() (any, error) {
	return encodeMapping(hostedRoomsFields(h), h.rest)
}

// Discussion ceilings shared by all members
type RoomDiscussion struct {
	MaxRounds   int `yaml:"max_rounds"`
	MaxMessages int `yaml:"max_messages"`
	rest        rest
}

type roomDiscussionFields RoomDiscussion

// Decodes the mapping and keeps unknown keys
func (r *RoomDiscussion) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*roomDiscussionFields)(r), &r.rest)
}

// Writes the mapping with unknown keys after the named ones
func (r RoomDiscussion) MarshalYAML() (any, error) {
	return encodeMapping(roomDiscussionFields(r), r.rest)
}

// Defaults of the tools section
func defaultTools() ToolsConfig {
	return ToolsConfig{
		ToolSearch: ToolSearchConfig{
			Enabled:            Text("auto"),
			ThresholdPct:       5,
			SearchDefaultLimit: 5,
			MaxSearchLimit:     25,
			Listing:            Text("auto"),
			ListingMaxTokens:   4000,
			Defer: []string{
				"computer_use", "session_search", "image_generate",
				"todo_list", "process_manage", "cronjob_manage", "browser_vault",
				"drive_preview", "gui_tour", "desktop_preview", "annotate_preview", "manage_catalog",
				"show_tip", "desktop_project", "close_terminal",
				"set_theme", "read_terminal", "read_window_below", "focus_pane",
			},
		},
		Connectors: ConnectorsConfig{Enabled: true},
	}
}

// Defaults of the monitoring section
func defaultMonitoring() MonitoringConfig {
	return MonitoringConfig{
		GatewayHealthExport: GatewayHealthExport{
			MetricsEnabled:            true,
			DiagnosticEventsEnabled:   true,
			WarningErrorEventsEnabled: true,
			ExportIntervalSeconds:     60,
			LogsExportIntervalSeconds: 5,
			ResourceAttributes:        MapOf([]string{"service.name", "deployment.environment.name"}, []string{"icebath-gateway", "production"}),
		},
	}
}

// Documentation of the tools, logging, catalog, network, monitoring and hosted room sections
func toolsDocs() map[string]string {
	return map[string]string{
		"tools":                                  "Tool Search replaces deferred tools in the model-facing array with the tool_search, tool_describe and tool_call bridges and surfaces them on demand. Working-set core tools stay eager, while the explicit `defer` list may include cold, event-triggered built-ins as well as plugin and MCP tools.",
		"tools.tool_search.enabled":              "Tiered: tier 0 (no deferred tools) = everything eager; tier 1 = bridge + a name and description manifest when it fits the budget (degrades to names-only); tier 2 (over budget even names-only, e.g. ~3,300-tool APIs) = bare bridge + a one-line-per-server summary (name + tool count). \"auto\" or \"on\" = activate when at least one deferred tool exists (\"auto\" is an alias of \"on\" and stays the default so explicit \"on\" and \"off\" pins are unaffected). \"off\" = pass-through, no bridge.",
		"tools.tool_search.threshold_pct":        "Listing budget as % of the model's context length; effective budget = min(this % of context, listing_max_tokens). Range 0..100.",
		"tools.tool_search.search_default_limit": "Hits per query when the model omits `limit`. Range 1..max_search_limit.",
		"tools.tool_search.max_search_limit":     "Hard upper bound the model may request via `limit` (per query). Range 1..50.",
		"tools.tool_search.listing":              "Catalog listing embedded in the bridge description (name + first sentence up to 60 chars, grouped by server or toolset). \"auto\" = include when it fits (falls back to names-only, then the bare tier-2 bridge); \"on\" = same rendering, explicit intent; \"off\" = always the bare bridge.",
		"tools.tool_search.listing_max_tokens":   "Absolute cap on the embedded listing in tokens (chars/4), regardless of context size. Range 200..60000.",
		"tools.tool_search.defer":                "Tools replaced by the bridge by default. This list intentionally includes cold, event-triggered built-ins; an explicit list replaces it wholesale and [] keeps every tool eager.",
		"tools.connectors":                       "Remote connector discovery and lifecycle through the Icebath Portal tool gateway. The flag is the user's off switch; availability additionally requires the portal sign-in every managed tool gates on.",
		"logging":                                "File logging to ~/.icebath/logs/: agent.log captures INFO+, errors.log WARNING+.",
		"logging.level":                          "Minimum level for agent.log: DEBUG, INFO, WARNING.",
		"logging.max_size_mb":                    "Max size per log file before rotation.",
		"logging.backup_count":                   "Rotated backups to keep.",
		"model_catalog":                          "Remote model-catalog manifest: curated OpenRouter and Icebath Portal model lists fetched from this URL (falls back to the built-in snapshot on network failure), so picker lists update without a release.",
		"model_catalog.ttl_minutes":              "Disk cache TTL in minutes. The gateway refreshes in the background on this cadence; the CLI refetches on the next /model or `icebath model` once the cache is older. Network failures silently use the stale cache. Legacy `ttl_hours` is honoured if set.",
		"model_catalog.providers":                "Per-provider override URLs for self-hosted curation lists using the same schema, e.g. providers: {openrouter: {url: https://example.com/my-curation.json}}.",
		"model_overrides":                        "Per-model metadata overrides. Fields: context_window, supports_tools, supports_vision, supports_reasoning, model_family. <provider>.<model_id> wins over models.dev, OpenRouter and hardcoded defaults for the fields it sets. <provider>._default and top-level _default fill gaps ONLY for models the catalog does not know, so they never clamp known models. Unknown ids start from safe defaults (200K context, tools on) and get patched; supports_vision and supports_reasoning stay UNKNOWN (fail-open) unless the override sets them, so a context_window-only entry never turns into \"text-only\". Provider keys: an icebath or models.dev id; model ids match case-insensitively. Example: {\"custom:my-local-vllm\": {\"my-llava-model\": {\"context_window\": 8192}}}. An explicit model.context_length and a custom provider per-model context_length are user settings at other layers, consulted in resolution chain order.",
		"models_dev":                             "models.dev registry (context windows, capabilities, pricing, modalities): fetched on startup, served from cache, refreshed by a background daemon with ETag conditional GET. Override `url` to point at a mirror (e.g. behind a corporate proxy).",
		"models_dev.url":                         "Empty = default https://models.dev/api.json.",
		"network.force_ipv4":                     "Force IPv4. With broken or unreachable IPv6, AAAA records are tried first and hang for the full TCP timeout before falling back. true skips IPv6 entirely.",
		"monitoring":                             "Gateway monitoring: service health + redacted operational diagnostics exported over OTLP to an operator endpoint (OTEL Collector, DataDog, ...). Content-free by construction: no prompts, messages, tool args or results, session history, usage analytics, audit logs, or trajectories. Nothing is collected or sent until enabled with an endpoint.",
		"monitoring.install_id":                  "Stable install identifier on exported signals so operators can tell instances apart. \"\" = mint a fresh UUID on first use; clear it to rotate. Carries no account identity.",
		"monitoring.gateway_health_export":       "Gateway health and diagnostics export.",
		"monitoring.export":                      "OTLP destination. headers_env maps header names to ENVIRONMENT VARIABLE NAMES (never secret values); values are read from the environment at export time.",
		"hosted_rooms":                           "Native hosted-room Discussion ceilings, shared by all members per discussion. Positive integers only (no unlimited or 0). Read from the root owner's config at service startup for all hosted rooms; restart the owning service after changing them.",
	}
}
