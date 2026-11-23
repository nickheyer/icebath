package tuigateway

// Keys kept in the home's .env a setup form asks for: the names wanted, or every key of a category
type SetupKeysParams struct {
	Names []string `json:"names,omitzero"`
	// provider, tool, skill, messaging or setting
	Category string `json:"category,omitzero"`
	// Only the keys that hold no value yet
	Missing bool `json:"missing,omitzero"`
	// The keys hidden from setup too
	Advanced bool `json:"advanced,omitzero"`
}

// One key kept in the home's .env, never its value
type SetupKey struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Help  string `json:"help,omitzero"`
	URL   string `json:"url,omitzero"`
	// Whether the key holds a value
	Set    bool `json:"set"`
	Secret bool `json:"secret"`
	// provider, tool, skill, messaging or setting
	Category string `json:"category"`
	// Chat app the key belongs to, empty for a key of no one app
	App      string   `json:"app,omitzero"`
	Tools    []string `json:"tools,omitzero"`
	Required bool     `json:"required,omitzero"`
	Advanced bool     `json:"advanced,omitzero"`
}

// Keys in the order the catalog lists them
type SetupKeysResult struct {
	Keys []SetupKey `json:"keys"`
}

// Value of a key of the catalog to keep in the home's .env, an empty value taking it out
type SetupSaveKeyParams struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// The key after the write
type SetupSaveKeyResult struct {
	Key SetupKey `json:"key"`
}

// Facts a setup form reads before it asks: env:NAME, extra:NAME, module:NAME, binary:NAME, xai.oauth
type SetupFactsParams struct {
	Names []string `json:"names"`
}

// Whether each fact holds, by name
type SetupFactsResult struct {
	Facts map[string]bool `json:"facts"`
}

// Platform whose tool groups the setup form lists, the terminal when empty
type SetupToolsetsParams struct {
	Platform string `json:"platform,omitzero"`
}

// One tool group a person may turn on or off
type SetupToolset struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitzero"`
	On     bool   `json:"on"`
	// Whether the keys the group needs are there
	Ready bool `json:"ready"`
	// Keys of the home's .env that unlock the group's tools
	Keys []string `json:"keys,omitzero"`
}

// Tool groups in the order the catalog lists them
type SetupToolsetsResult struct {
	Toolsets []SetupToolset `json:"toolsets"`
}

var (
	// Keys of the home's .env the catalog knows, with whether each holds a value
	SetupKeys = NewMethod[SetupKeysParams, SetupKeysResult]("setup.keys")
	// Keeps one key of the catalog in the home's .env
	SetupSaveKey = NewMethod[SetupSaveKeyParams, SetupSaveKeyResult]("setup.save_key")
	// Reads facts of this computer and home a setup form needs, such as whether an engine is installed
	SetupFacts = NewMethod[SetupFactsParams, SetupFactsResult]("setup.facts")
	// Tool groups of a platform, which are on and whether their keys are there
	SetupToolsets = NewMethod[SetupToolsetsParams, SetupToolsetsResult]("setup.toolsets")
)
