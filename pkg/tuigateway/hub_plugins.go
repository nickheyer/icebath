package tuigateway

// One entry of the curated plugin catalog as the plugins screen lists it
type PluginCatalogRow struct {
	Name        string   `json:"name"`
	Title       string   `json:"title,omitzero"`
	Description string   `json:"description,omitzero"`
	Tier        string   `json:"tier,omitzero"`
	Category    string   `json:"category,omitzero"`
	Version     string   `json:"version,omitzero"`
	Repo        string   `json:"repo,omitzero"`
	Maintainer  string   `json:"maintainer,omitzero"`
	Tools       []string `json:"tools,omitzero"`
	RequiresEnv []string `json:"requires_env,omitzero"`
	KnownIssues []string `json:"known_issues,omitzero"`
	DocsURL     string   `json:"docs_url,omitzero"`
}

// Words the catalog is narrowed to, every entry when empty
type PluginsCatalogParams struct {
	Profile *string `json:"profile,omitzero"`
	Term    string  `json:"term,omitzero"`
}

// Catalog entries the words match
type PluginsCatalogResult struct {
	Entries []PluginCatalogRow `json:"entries"`
}

// One plugins command run without questions: show, info, capabilities, check-updates, trust-update-url, adopt, pack-show, pack-export,
// doctor or validate, with the name, path or source it reads
type PluginsHubRunParams struct {
	Profile *string `json:"profile,omitzero"`
	Verb    string  `json:"verb"`
	Name    string  `json:"name,omitzero"`
	Source  string  `json:"source,omitzero"`
	// Pack export only: the name written into the pack and whether only enabled plugins go in
	PackName    string `json:"pack_name,omitzero"`
	EnabledOnly bool   `json:"enabled_only,omitzero"`
	// Pack export only: full path of a file the pack is written to besides being answered
	Output string `json:"output,omitzero"`
	// Validate only: installs the plugin's declared Python dependencies before the capability probe
	InstallDeps bool `json:"install_deps,omitzero"`
}

// What a plugins command printed and whether it reported success
type PluginsHubRunResult struct {
	OK    bool     `json:"ok"`
	Lines []string `json:"lines"`
}

// Pack whose pinned plugins are installed after the user reviewed it in the screen
type PluginsPackInstallParams struct {
	Profile *string `json:"profile,omitzero"`
	Source  string  `json:"source"`
	Force   bool    `json:"force,omitzero"`
}

var (
	// Curated plugin catalog entries matching the words
	PluginsCatalog = NewMethod[PluginsCatalogParams, PluginsCatalogResult]("plugins.hub.catalog")
	// Runs one plugins command without questions and answers what it printed
	PluginsHubRun = NewMethod[PluginsHubRunParams, PluginsHubRunResult]("plugins.hub.run")
	// Installs every plugin a reviewed pack pins
	PluginsPackInstall = NewMethod[PluginsPackInstallParams, PluginsHubRunResult]("plugins.hub.pack_install")
)
