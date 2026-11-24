package tuigateway

type SkillsAction string

func (SkillsAction) Values() []string {
	return []string{"list", "search", "install", "browse", "inspect"}
}

// Hub action, query the search text or hub identifier, page and page size for browse
type SkillsManageParams struct {
	Profile  *string      `json:"profile,omitzero"`
	Action   SkillsAction `json:"action,omitzero"`
	Query    string       `json:"query,omitzero"`
	Page     int64        `json:"page,omitzero"`
	PageSize int64        `json:"page_size,omitzero"`
}

type SkillHubHit struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// One skill of a hub browse page with its source and trust level
type SkillBrowseItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Trust       string `json:"trust"`
	Identifier  string `json:"identifier"`
}

// What a skill does, from the hub and the profile's own install, empty when it resolves nowhere
type SkillInspectInfo struct {
	Name           string   `json:"name,omitzero"`
	Description    string   `json:"description,omitzero"`
	Source         string   `json:"source,omitzero"`
	Identifier     string   `json:"identifier,omitzero"`
	Tags           []string `json:"tags,omitzero"`
	SkillMdPreview *string  `json:"skill_md_preview,omitzero"`
	Category       string   `json:"category,omitzero"`
	Path           string   `json:"path,omitzero"`
}

// Answer of a hub action, skills for list, results for search, installed and name, a browse page, info for inspect
type SkillsManageResult struct {
	Skills     map[string][]string `json:"skills,omitzero"`
	Results    []SkillHubHit       `json:"results,omitzero"`
	Installed  bool                `json:"installed,omitzero"`
	Name       string              `json:"name,omitzero"`
	Items      []SkillBrowseItem   `json:"items,omitzero"`
	Page       int64               `json:"page,omitzero"`
	TotalPages int64               `json:"total_pages,omitzero"`
	Total      int64               `json:"total,omitzero"`
	Info       *SkillInspectInfo   `json:"info,omitzero"`
}

// Session whose profile and workspace the rescan binds to, or a profile for a session less rescan
type SkillsReloadParams struct {
	SessionID string  `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

type SkillCommandRef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
}

// Skill commands a rescan added and removed, the unchanged names and the totals
type SkillsReloadDiff struct {
	Added     []SkillCommandRef `json:"added"`
	Removed   []SkillCommandRef `json:"removed"`
	Unchanged []string          `json:"unchanged"`
	Total     int64             `json:"total"`
	Commands  int64             `json:"commands"`
}

// Rescan answer, output the text the reload command prints
type SkillsReloadResult struct {
	Output string           `json:"output"`
	Result SkillsReloadDiff `json:"result"`
}

// Loaded plugin manager entry with its counts and load error
type LegacyPluginRow struct {
	Name     string  `json:"name"`
	Version  string  `json:"version"`
	Enabled  bool    `json:"enabled"`
	Key      *string `json:"key,omitzero"`
	Tools    *int64  `json:"tools,omitzero"`
	Hooks    *int64  `json:"hooks,omitzero"`
	Commands *int64  `json:"commands,omitzero"`
	Error    *string `json:"error,omitzero"`
}

type PluginToolsetRow struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type PluginsListParams struct{}

type PluginsListResult struct {
	Plugins            []LegacyPluginRow  `json:"plugins"`
	Toolsets           []PluginToolsetRow `json:"toolsets,omitzero"`
	PortableMCPServers []string           `json:"portable_mcp_servers,omitzero"`
}

type PluginsAction string

func (PluginsAction) Values() []string {
	return []string{"list", "toggle", "install", "inspect", "update", "update_all", "remove", "settings", "presence"}
}

// Plugin hub action, key or name and enable for toggle, identifier, repo or catalog name for install and inspect
type PluginsManageParams struct {
	Profile            *string        `json:"profile,omitzero"`
	Action             PluginsAction  `json:"action,omitzero"`
	Key                string         `json:"key,omitzero"`
	Name               string         `json:"name,omitzero"`
	Enable             *bool          `json:"enable,omitzero"`
	Identifier         string         `json:"identifier,omitzero"`
	Repo               string         `json:"repo,omitzero"`
	CatalogName        string         `json:"catalog_name,omitzero"`
	Force              bool           `json:"force,omitzero"`
	Ref                string         `json:"ref,omitzero"`
	InspectedSha       string         `json:"inspected_sha,omitzero"`
	AcceptCapabilities bool           `json:"accept_capabilities,omitzero"`
	Values             map[string]any `json:"values,omitzero"`
	Names              []string       `json:"names,omitzero"`
}

type PluginSettingFieldType string

func (PluginSettingFieldType) Values() []string {
	return []string{"string", "number", "boolean", "enum", "secret", "json"}
}

// One config_schema key of a plugin manifest, a secret carrying only its env name and whether it is set
type PluginSettingField struct {
	Key         string                 `json:"key"`
	Type        PluginSettingFieldType `json:"type"`
	Label       string                 `json:"label"`
	Description string                 `json:"description"`
	Required    bool                   `json:"required"`
	Value       any                    `json:"value,omitzero"`
	Default     any                    `json:"default,omitzero"`
	Choices     []string               `json:"choices,omitzero"`
	Env         *string                `json:"env,omitzero"`
	HasValue    *bool                  `json:"has_value,omitzero"`
}

type PluginServerState string

func (PluginServerState) Values() []string {
	return []string{"connected", "app_not_running", "icebath_not_connected", "endpoint_unavailable", "no_interactive_session",
		"version_too_old", "missing_app", "unsupported_gpu", "unknown"}
}

type PluginServerRow struct {
	Name     string            `json:"name"`
	State    PluginServerState `json:"state"`
	Sentence string            `json:"sentence"`
}

// One plugin of the hub with its source, state, servers, catalog pins and settings
type AgentPluginRow struct {
	Name            string               `json:"name"`
	Key             string               `json:"key"`
	Version         string               `json:"version"`
	Description     string               `json:"description"`
	Source          string               `json:"source"`
	Status          string               `json:"status"`
	Portable        bool                 `json:"portable"`
	InstallDir      string               `json:"install_dir"`
	Servers         []PluginServerRow    `json:"servers"`
	CatalogName     *string              `json:"catalog_name,omitzero"`
	CatalogTier     *string              `json:"catalog_tier,omitzero"`
	InstalledSha    *string              `json:"installed_sha,omitzero"`
	CatalogSha      *string              `json:"catalog_sha,omitzero"`
	CatalogVersion  *string              `json:"catalog_version,omitzero"`
	UpdateAvailable *bool                `json:"update_available,omitzero"`
	UpdateTarget    *string              `json:"update_target,omitzero"`
	PinnedSha       *string              `json:"pinned_sha,omitzero"`
	SettingsSchema  []PluginSettingField `json:"settings_schema,omitzero"`
	Tools           []string             `json:"tools,omitzero"`
	// How the plugin came to be in the plugins folder, self-cloned for a git clone made by hand that no install recorded
	Provenance string `json:"provenance,omitzero"`
}

// Whether the plugin is on, the hub's enabled status
func (p AgentPluginRow) Enabled() bool { return p.Status == "enabled" }

type PluginRestartTarget string

func (PluginRestartTarget) Values() []string { return []string{"gateway", "backend"} }

// One plugin MCP server connected at activation, its tools or the reason it did not connect
type PluginLiveServer struct {
	Name      string   `json:"name"`
	Connected bool     `json:"connected"`
	Tools     []string `json:"tools,omitzero"`
	Error     *string  `json:"error,omitzero"`
}

// One plugin skill usable now as plugin:skill
type PluginLiveSkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
}

type PluginLiveNow struct {
	MCPServers []PluginLiveServer `json:"mcp_servers,omitzero"`
	Skills     []PluginLiveSkill  `json:"skills,omitzero"`
}

// What a plugin loaded mid run does now and what waits for a restart, by kind
type PluginActivation struct {
	Name                string                `json:"name"`
	Key                 string                `json:"key"`
	ActivatedNow        map[string][]string   `json:"activated_now,omitzero"`
	LiveNow             *PluginLiveNow        `json:"live_now,omitzero"`
	Deferred            map[string][]string   `json:"deferred,omitzero"`
	MissingDependencies []string              `json:"missing_dependencies,omitzero"`
	MissingIn           []PluginRestartTarget `json:"missing_in,omitzero"`
}

// Catalog plugin of this OS, its app state, the sentence naming what is missing and its disclosure
type CatalogPluginPresence struct {
	Name       string          `json:"name"`
	Title      string          `json:"title"`
	State      CatalogAppState `json:"state"`
	Sentence   string          `json:"sentence"`
	Disclosure string          `json:"disclosure"`
}

// One security scan finding on a plugin tree the scan blocked
type PluginScanFinding struct {
	PatternID   string `json:"pattern_id"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	File        string `json:"file"`
	Line        int64  `json:"line"`
	Description string `json:"description"`
}

// Outcome of one plugin an update of every plugin moved or tried to move
type PluginUpdateStatus string

func (PluginUpdateStatus) Values() []string {
	return []string{"updated", "current", "failed", "needs_review"}
}

// One plugin an update of every plugin handled, with the refusal or failure text when it did not update
type PluginUpdateRow struct {
	Name   string             `json:"name"`
	Status PluginUpdateStatus `json:"status"`
	Detail string             `json:"detail"`
}

// One plugin an update of every plugin left alone and why
type PluginUpdateSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Answer of a plugin hub action, the rows and counts for list, the outcome fields of the others
type PluginsManageResult struct {
	Plugins               []AgentPluginRow        `json:"plugins,omitzero"`
	UserCount             *int64                  `json:"user_count,omitzero"`
	BundledCount          *int64                  `json:"bundled_count,omitzero"`
	Ok                    *bool                   `json:"ok,omitzero"`
	Unchanged             *bool                   `json:"unchanged,omitzero"`
	RestartRequired       *bool                   `json:"restart_required,omitzero"`
	GatewayReloaded       *bool                   `json:"gateway_reloaded,omitzero"`
	Activation            *PluginActivation       `json:"activation,omitzero"`
	ClearedMemoryProvider *bool                   `json:"cleared_memory_provider,omitzero"`
	Name                  *string                 `json:"name,omitzero"`
	Plugin                *AgentPluginRow         `json:"plugin,omitzero"`
	PluginName            *string                 `json:"plugin_name,omitzero"`
	Warnings              []string                `json:"warnings,omitzero"`
	MissingEnv            []string                `json:"missing_env,omitzero"`
	PythonDependencies    []string                `json:"python_dependencies,omitzero"`
	KnownIssues           []string                `json:"known_issues,omitzero"`
	AfterInstallPath      *string                 `json:"after_install_path,omitzero"`
	Enabled               *bool                   `json:"enabled,omitzero"`
	Sha                   *string                 `json:"sha,omitzero"`
	ConsentRequired       *bool                   `json:"consent_required,omitzero"`
	Delta                 map[string][]string     `json:"delta,omitzero"`
	DeltaLines            []string                `json:"delta_lines,omitzero"`
	ScanBlocked           *bool                   `json:"scan_blocked,omitzero"`
	ScanVerdict           *string                 `json:"scan_verdict,omitzero"`
	ScanFindings          []PluginScanFinding     `json:"scan_findings,omitzero"`
	Error                 *string                 `json:"error,omitzero"`
	Written               []string                `json:"written,omitzero"`
	Presence              []CatalogPluginPresence `json:"presence,omitzero"`
	Agent                 *bool                   `json:"agent,omitzero"`
	AgentName             *string                 `json:"agent_name,omitzero"`
	Insecure              *bool                   `json:"insecure,omitzero"`
	Results               []PluginUpdateRow       `json:"results,omitzero"`
	Skipped               []PluginUpdateSkip      `json:"skipped,omitzero"`
	Current               *int64                  `json:"current,omitzero"`
}

// Skills hub, the profile's skills by category or search, browse, inspect and install from the hub
var SkillsManage = NewMethod[SkillsManageParams, SkillsManageResult]("skills.manage")

// Scans the skill directories again and answers what changed with the printed summary
var SkillsReload = NewMethod[SkillsReloadParams, SkillsReloadResult]("skills.reload")

// Loaded plugin manager entries, the flat view the chat commands print
var PluginsList = NewMethod[PluginsListParams, PluginsListResult]("plugins.list")

// Plugins hub, list, toggle, install, inspect, update of one or every plugin, remove, settings and catalog presence
var PluginsManage = NewMethod[PluginsManageParams, PluginsManageResult]("plugins.manage")

// A skill root changed elsewhere, the UI fetches the command catalog again
var SkillsChangedEvent = NewEvent[NoPayload]("skills.changed")
