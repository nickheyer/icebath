package config

import (
	"errors"
	"reflect"
	"slices"

	"gopkg.in/yaml.v3"
)

// Plugin system section, the enabled and disabled lists unset unless written so an empty default never clobbers them
type PluginsConfig struct {
	CloneTimeoutSeconds   int                       `yaml:"clone_timeout_seconds" coerce:"exactint"`
	HookCallbackTimeout   float64                   `yaml:"hook_callback_timeout"`
	LoadTimeoutSeconds    float64                   `yaml:"load_timeout_seconds"`
	AutoUpdateCheckHours  float64                   `yaml:"auto_update_check_hours"`
	AutoApply             bool                      `yaml:"auto_apply" coerce:"bool"`
	Isolation             PluginIsolation           `yaml:"isolation"`
	Host                  PluginHost                `yaml:"host"`
	Enabled               *[]string                 `yaml:"enabled,omitempty" coerce:"exact"`
	Disabled              *[]string                 `yaml:"disabled,omitempty" coerce:"exact"`
	DisabledByUpdate      Map[PluginEvictionRecord] `yaml:"disabled_by_update,omitempty"`
	Entries               Map[PluginEntry]          `yaml:"entries,omitempty"`
	StreamReasoningDeltas *bool                     `yaml:"stream_reasoning_deltas,omitempty" coerce:"bool"`
	rest                  rest
}

type pluginsFields PluginsConfig

// Decodes the mapping and keeps unknown keys
func (p *PluginsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*pluginsFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PluginsConfig) MarshalYAML() (any, error) {
	return encodeMapping(pluginsFields(p), p.rest)
}

// Where third-party plugins run
type PluginIsolation string

// Plugin isolation modes
const (
	IsolationInProcess PluginIsolation = "in_process"
	IsolationHost      PluginIsolation = "host"
)

// Accepted plugin isolation modes, read case-insensitively with hyphens as underscores and any other value in_process
func PluginIsolations() []PluginIsolation {
	return []PluginIsolation{IsolationInProcess, IsolationHost}
}

// Plugin host process launch settings, the launcher an argv list or one shell-split string
type PluginHost struct {
	Launcher StringList `yaml:"launcher"`
	rest     rest
}

type pluginHostFields PluginHost

// Decodes the mapping and keeps unknown keys
func (p *PluginHost) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*pluginHostFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PluginHost) MarshalYAML() (any, error) {
	return encodeMapping(pluginHostFields(p), p.rest)
}

// Per-plugin settings, capability grants and consent, and the deprecated allow gates under plugins.entries
type PluginEntry struct {
	Settings             Map[Raw]           `yaml:"settings,omitempty"`
	GrantedCapabilities  *[]string          `yaml:"granted_capabilities,omitempty" coerce:"exact"`
	CapabilitiesConsent  *CapabilityConsent `yaml:"capabilities_consent,omitempty"`
	AllowToolOverride    *bool              `yaml:"allow_tool_override,omitempty" coerce:"bool"`
	AllowPlatformActions *bool              `yaml:"allow_platform_actions,omitempty" coerce:"bool"`
	LLM                  *PluginLLMGates    `yaml:"llm,omitempty"`
	rest                 rest
}

type pluginEntryFields PluginEntry

// Decodes the mapping and keeps unknown keys
func (p *PluginEntry) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*pluginEntryFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PluginEntry) MarshalYAML() (any, error) {
	return encodeMapping(pluginEntryFields(p), p.rest)
}

// Record of the capability set a user consented to for one plugin
type CapabilityConsent struct {
	Hash      string `yaml:"hash,omitempty" coerce:"exact"`
	GrantedAt string `yaml:"granted_at,omitempty"`
	rest      rest
}

type capabilityConsentFields CapabilityConsent

// Decodes the mapping and keeps unknown keys
func (c *CapabilityConsent) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*capabilityConsentFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c CapabilityConsent) MarshalYAML() (any, error) {
	return encodeMapping(capabilityConsentFields(c), c.rest)
}

// Deprecated per-plugin gates of host-owned LLM calls, each still honored when truthy
type PluginLLMGates struct {
	AllowProviderOverride *bool `yaml:"allow_provider_override,omitempty" coerce:"bool"`
	AllowModelOverride    *bool `yaml:"allow_model_override,omitempty" coerce:"bool"`
	AllowAgentIDOverride  *bool `yaml:"allow_agent_id_override,omitempty" coerce:"bool"`
	AllowProfileOverride  *bool `yaml:"allow_profile_override,omitempty" coerce:"bool"`
	AllowTaskOverride     *bool `yaml:"allow_task_override,omitempty" coerce:"bool"`
	rest                  rest
}

type pluginLLMGatesFields PluginLLMGates

// Decodes the mapping and keeps unknown keys
func (p *PluginLLMGates) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*pluginLLMGatesFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PluginLLMGates) MarshalYAML() (any, error) {
	return encodeMapping(pluginLLMGatesFields(p), p.rest)
}

// What an update undoes once a plugin it disabled fits again
type PluginEvictionRecord struct {
	Enabled bool   `yaml:"enabled" coerce:"bool"`
	Reason  string `yaml:"reason,omitempty"`
	Tried   string `yaml:"tried,omitempty"`
	rest    rest
}

type pluginEvictionRecordFields PluginEvictionRecord

// Decodes the mapping and keeps unknown keys
func (p *PluginEvictionRecord) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*pluginEvictionRecordFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PluginEvictionRecord) MarshalYAML() (any, error) {
	return encodeMapping(pluginEvictionRecordFields(p), p.rest)
}

// Shell hooks by event name plus the reserved output_spill and outbound subsections, written back in file order
type HooksConfig struct {
	OutputSpill *OutputSpill       `yaml:"output_spill,omitempty"`
	Outbound    *[]OutboundWebhook `yaml:"outbound,omitempty"`
	Events      Map[HookList]      `yaml:"-"`
	order       []string
	rest        rest
}

type hooksFields HooksConfig

// Reports a key naming a reserved subsection rather than a hook event
func reservedHookKey(k string) bool {
	return k == "output_spill" || k == "outbound"
}

// Decodes the reserved subsections field by field and every other key as the hook list of one event
func (h *HooksConfig) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n == nil || isNull(n) {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return shapeError(n, yaml.MappingNode)
	}
	reserved := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	events := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	var keys []string
	for _, p := range mappingPairs(n) {
		keys = append(keys, p.key.Value)
		target := events
		if reservedHookKey(p.key.Value) {
			target = reserved
		}
		target.Content = append(target.Content, p.key, p.value)
	}
	h.order = mergeOrder(h.order, keys)
	return errors.Join(decodeMapping(reserved, (*hooksFields)(h), &h.rest), h.Events.UnmarshalYAML(events))
}

// Writes the reserved subsections and the event lists in the order the file held them, new keys last
func (h HooksConfig) MarshalYAML() (any, error) {
	reserved, err := encodeStruct(reflect.ValueOf(hooksFields(h)), h.rest)
	if err != nil {
		return nil, err
	}
	events, err := h.Events.MarshalYAML()
	if err != nil {
		return nil, err
	}
	var pairs []pair
	for _, m := range []*yaml.Node{reserved, events.(*yaml.Node)} {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m != reserved && reservedHookKey(m.Content[i].Value) {
				continue
			}
			pairs = append(pairs, pair{m.Content[i], m.Content[i+1]})
		}
	}
	rank := func(p pair) int {
		if i := slices.Index(h.order, p.key.Value); i >= 0 {
			return i
		}
		return len(h.order)
	}
	slices.SortStableFunc(pairs, func(a, b pair) int { return rank(a) - rank(b) })
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	for _, p := range pairs {
		out.Content = append(out.Content, p.key, p.value)
	}
	return out, nil
}

// Shell hooks of one event, a null or other non-list value kept in Other for the hook loader to report
type HookList struct {
	Hooks []ShellHook
	Other *yaml.Node
}

// Keeps an explicit null as the value of the event
func (HookList) holdsNull() {}

// Accepts a list of hook definitions and keeps any other value as written
func (l *HookList) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n == nil || n.Kind != yaml.SequenceNode {
		*l = HookList{Other: plainCopy(n)}
		return nil
	}
	hooks := []ShellHook{}
	_, err = decodeSlice(n, reflect.ValueOf(&hooks).Elem(), coerceDefault)
	*l = HookList{Hooks: hooks}
	return err
}

// Writes the list, or the other value exactly as read
func (l HookList) MarshalYAML() (any, error) {
	if l.Other != nil {
		return emitted(l.Other), nil
	}
	hooks := l.Hooks
	if hooks == nil {
		hooks = []ShellHook{}
	}
	return encodeValue(reflect.ValueOf(hooks))
}

// One shell hook definition, fail_closed and failClosed kept as written since only a boolean counts and null differs from absent
type ShellHook struct {
	Command         string `yaml:"command,omitempty" coerce:"exact"`
	Matcher         string `yaml:"matcher,omitempty" coerce:"exact"`
	Timeout         *int   `yaml:"timeout,omitempty"`
	FailClosed      Scalar `yaml:"fail_closed,omitempty"`
	FailClosedCamel Scalar `yaml:"failClosed,omitempty"`
	raw             *yaml.Node
	rest            rest
}

type shellHookFields ShellHook

// Decodes a mapping definition keeping unknown keys, and holds a stray non-mapping item as written
func (s *ShellHook) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n == nil || n.Kind != yaml.MappingNode {
		*s = ShellHook{raw: plainCopy(n)}
		return nil
	}
	s.raw = nil
	return decodeEntry(n, (*shellHookFields)(s), &s.rest)
}

// Keeps an explicit null as a stray item
func (ShellHook) holdsNull() {}

// Writes the definition, or the stray item exactly as read
func (s ShellHook) MarshalYAML() (any, error) {
	if s.raw != nil {
		return emitted(s.raw), nil
	}
	return encodeMapping(shellHookFields(s), s.rest)
}

// Reports a mapping definition rather than a stray scalar or list item
func (s ShellHook) Valid() bool {
	return s.raw == nil
}

// Spill of oversized hook output to files, enabled read by truthiness with null keeping the default on
type OutputSpill struct {
	Enabled     *bool  `yaml:"enabled,omitempty" coerce:"boolkeep"`
	MaxChars    *int   `yaml:"max_chars,omitempty"`
	PreviewHead *int   `yaml:"preview_head,omitempty"`
	PreviewTail *int   `yaml:"preview_tail,omitempty"`
	Directory   string `yaml:"directory,omitempty" coerce:"exact"`
	rest        rest
}

type outputSpillFields OutputSpill

// Decodes the mapping and keeps unknown keys
func (o *OutputSpill) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*outputSpillFields)(o), &o.rest)
}

// Writes the mapping with unknown keys after the named ones
func (o OutputSpill) MarshalYAML() (any, error) {
	return encodeMapping(outputSpillFields(o), o.rest)
}

// Outbound webhook target for hook events, the text keys read only when written as text
type OutboundWebhook struct {
	URL       string   `yaml:"url,omitempty" coerce:"exact"`
	Events    []string `yaml:"events,omitempty" coerce:"exact"`
	Matcher   string   `yaml:"matcher,omitempty" coerce:"exact"`
	Timeout   *int     `yaml:"timeout,omitempty"`
	Name      string   `yaml:"name,omitempty" coerce:"exact"`
	SecretEnv string   `yaml:"secret_env,omitempty" coerce:"exact"`
	Secret    string   `yaml:"secret,omitempty" coerce:"exact"`
	raw       *yaml.Node
	rest      rest
}

type outboundWebhookFields OutboundWebhook

// Decodes a mapping target keeping unknown keys, and holds a stray non-mapping item as written
func (o *OutboundWebhook) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n == nil || n.Kind != yaml.MappingNode {
		*o = OutboundWebhook{raw: plainCopy(n)}
		return nil
	}
	o.raw = nil
	return decodeEntry(n, (*outboundWebhookFields)(o), &o.rest)
}

// Keeps an explicit null as a stray item
func (OutboundWebhook) holdsNull() {}

// Writes the target, or the stray item exactly as read
func (o OutboundWebhook) MarshalYAML() (any, error) {
	if o.raw != nil {
		return emitted(o.raw), nil
	}
	return encodeMapping(outboundWebhookFields(o), o.rest)
}

// Reports a mapping target rather than a stray scalar or list item
func (o OutboundWebhook) Valid() bool {
	return o.raw == nil
}

// One MCP server definition under mcp_servers
type MCPServer struct {
	Command                   string         `yaml:"command,omitempty"`
	Args                      []string       `yaml:"args,omitempty"`
	Env                       Map[string]    `yaml:"env,omitempty"`
	Cwd                       string         `yaml:"cwd,omitempty"`
	URL                       string         `yaml:"url,omitempty"`
	Transport                 string         `yaml:"transport,omitempty"`
	Type                      string         `yaml:"type,omitempty"`
	Headers                   Map[string]    `yaml:"headers,omitempty"`
	Timeout                   *float64       `yaml:"timeout,omitempty"`
	ConnectTimeout            *float64       `yaml:"connect_timeout,omitempty"`
	SSLVerify                 Scalar         `yaml:"ssl_verify,omitempty"`
	Enabled                   Scalar         `yaml:"enabled,omitempty"`
	Description               string         `yaml:"description,omitempty"`
	Tools                     *MCPToolFilter `yaml:"tools,omitempty"`
	Auth                      string         `yaml:"auth,omitempty"`
	OAuth                     Map[Raw]       `yaml:"oauth,omitempty"`
	ClientCert                string         `yaml:"client_cert,omitempty"`
	ClientKey                 string         `yaml:"client_key,omitempty"`
	IdentityHeader            Raw            `yaml:"identity_header,omitempty"`
	StrictRedirectHeaders     Scalar         `yaml:"strict_redirect_headers,omitempty"`
	UserAgent                 string         `yaml:"user_agent,omitempty"`
	SupportsParallelToolCalls Scalar         `yaml:"supports_parallel_tool_calls,omitempty"`
	SkipPreflight             Scalar         `yaml:"skip_preflight,omitempty"`
	Sampling                  *MCPSampling   `yaml:"sampling,omitempty"`
	Elicitation               Map[Raw]       `yaml:"elicitation,omitempty"`
	Events                    Scalar         `yaml:"events,omitempty"`
	Lazy                      Scalar         `yaml:"lazy,omitempty"`
	KeepaliveInterval         *float64       `yaml:"keepalive_interval,omitempty"`
	ExposeClientTools         Scalar         `yaml:"expose_client_tools,omitempty"`
	IdleTimeoutSeconds        Scalar         `yaml:"idle_timeout_seconds,omitempty"`
	MaxLifetimeSeconds        Scalar         `yaml:"max_lifetime_seconds,omitempty"`
	Lifecycle                 Map[Raw]       `yaml:"lifecycle,omitempty"`
	ToolCosts                 Map[Raw]       `yaml:"tool_costs,omitempty"`
	SpendLimit                Scalar         `yaml:"spend_limit,omitempty"`
	CostUnit                  string         `yaml:"cost_unit,omitempty"`
	rest                      rest
}

type mcpServerFields MCPServer

// Decodes the mapping and keeps unknown keys
func (m *MCPServer) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*mcpServerFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MCPServer) MarshalYAML() (any, error) {
	return encodeMapping(mcpServerFields(m), m.rest)
}

// Selective tool loading of an MCP server, an explicit empty include registering nothing
type MCPToolFilter struct {
	Include   *StringList `yaml:"include,omitempty"`
	Exclude   *StringList `yaml:"exclude,omitempty"`
	Resources Scalar      `yaml:"resources,omitempty"`
	Prompts   Scalar      `yaml:"prompts,omitempty"`
	rest      rest
}

type mcpToolFilterFields MCPToolFilter

// Decodes the mapping and keeps unknown keys
func (m *MCPToolFilter) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*mcpToolFilterFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MCPToolFilter) MarshalYAML() (any, error) {
	return encodeMapping(mcpToolFilterFields(m), m.rest)
}

// MCP sampling limits for a server
type MCPSampling struct {
	Enabled       Scalar   `yaml:"enabled,omitempty"`
	Model         string   `yaml:"model,omitempty"`
	AllowedModels []string `yaml:"allowed_models,omitempty"`
	MaxRPM        Scalar   `yaml:"max_rpm,omitempty"`
	MaxTokensCap  Scalar   `yaml:"max_tokens_cap,omitempty"`
	MaxToolRounds Scalar   `yaml:"max_tool_rounds,omitempty"`
	LogLevel      string   `yaml:"log_level,omitempty"`
	Timeout       Scalar   `yaml:"timeout,omitempty"`
	rest          rest
}

type mcpSamplingFields MCPSampling

// Decodes the mapping and keeps unknown keys
func (m *MCPSampling) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*mcpSamplingFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MCPSampling) MarshalYAML() (any, error) {
	return encodeMapping(mcpSamplingFields(m), m.rest)
}

// Image or video generation section, plugin backend settings kept by name
type MediaGenConfig struct {
	Provider      string `yaml:"provider,omitempty"`
	Model         string `yaml:"model,omitempty"`
	Backend       string `yaml:"backend,omitempty"`
	CloudProvider string `yaml:"cloud_provider,omitempty"`
	UseGateway    Scalar `yaml:"use_gateway,omitempty"`
	rest          rest
}

type mediaGenFields MediaGenConfig

// Decodes the mapping and keeps plugin settings
func (m *MediaGenConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*mediaGenFields)(m), &m.rest)
}

// Writes the mapping with plugin settings after the named ones
func (m MediaGenConfig) MarshalYAML() (any, error) {
	return encodeMapping(mediaGenFields(m), m.rest)
}

// Plugin backend setting kept by name
func (m MediaGenConfig) Setting(key string) *yaml.Node {
	return m.rest.copyOf(key)
}

// Defaults of the plugin section
func defaultPlugins() PluginsConfig {
	return PluginsConfig{
		CloneTimeoutSeconds:  300,
		HookCallbackTimeout:  30,
		LoadTimeoutSeconds:   10,
		AutoUpdateCheckHours: 24,
		Isolation:            IsolationInProcess,
		Host:                 PluginHost{Launcher: ListOf()},
	}
}

// Documentation of the plugin, hook and MCP server keys
func pluginDocs() map[string]string {
	return map[string]string{
		"plugins":                         "Plugin system. `enabled` and `disabled` (`icebath plugins enable|disable`) and `disabled_by_update` (plugins an update turned off, retried by later updates) are omitted from the defaults so an empty default never clobbers a user's lists.",
		"plugins.clone_timeout_seconds":   "Deadline (seconds) for one plugin Git clone, fetch or checkout. Slow repositories may need more time; each network operation is capped at one hour.",
		"plugins.hook_callback_timeout":   "Wall-clock cap (seconds) for one in-process plugin hook callback; shell hooks keep their own per-entry `timeout`. 0 = no cap (sync call on the agent thread). Max 600.",
		"plugins.load_timeout_seconds":    "Deadline (seconds) for one plugin's load and register. A plugin that overruns it is skipped with the reason \"load timed out\" and the rest keep loading. 0 = no deadline (load inline). Max 600.",
		"plugins.auto_update_check_hours": "Read-only plugin update-check cadence, hours (gateway tick; 0 disables). Applying stays explicit: `icebath plugins update <name>`, or auto_apply below (git-class plugins only, scan-gated by that same pipeline).",
		"plugins.auto_apply":              "Opt-in unattended apply for the cadence check. Git-row plugins ONLY; every apply runs the same security scan and consent pipeline as the manual update command.",
		"plugins.isolation":               "Where third-party plugins run. in_process: loaded into icebath (default). host: one plugin-host process per profile runs them and they reach icebath only through the plugin context (a crashing or hanging plugin takes down its host, which restarts; icebath keeps running). Bundled plugins stay in-process; `icebath plugins validate` says whether a plugin can run in the host.",
		"plugins.host.launcher":           "argv prefix the plugin host runs under, e.g. a sandbox runner. [] = plain subprocess.",
		"plugins.entries":                 "Per-plugin settings (entries.<id>.settings), capability grants and tool-override consent.",
		"hooks":                           "Shell-script hooks: event name (pre_tool_call, post_tool_call, pre_llm_call, subagent_stop, ...) -> list of {matcher, command, timeout}. The first run of a new command prompts for consent; approvals persist in ~/.icebath/shell-hooks-allowlist.json. Reserved subsections: output_spill (oversized hook output spill) and outbound (webhook targets).",
		"mcp_servers":                     "MCP server definitions keyed by name: command + args + env for stdio servers, or url + headers for HTTP servers, plus timeout, tools.include and tools.exclude, auth and oauth, sampling.",
		"image_gen":                       "Image generation provider selection (provider, model) and per-backend plugin settings.",
		"video_gen":                       "Video generation provider selection (provider, model) and per-backend plugin settings.",
	}
}
