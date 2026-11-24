package tuigateway

// One setting a memory provider asks for
type MemoryField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Value in force, never that of a secret
	Value  string `json:"value,omitzero"`
	Secret bool   `json:"secret,omitzero"`
	// Whether the secret already holds a value
	SecretSet bool `json:"secret_set,omitzero"`
	// Only values the setting takes, empty for free text
	Choices []string `json:"choices,omitzero"`
	// Where a key for the setting is handed out
	URL string `json:"url,omitzero"`
	// Settings that must hold these values for this one to count
	When map[string]string `json:"when,omitzero"`
}

// One memory provider that can be set up
type MemoryProviderRow struct {
	Name string `json:"name"`
	// The provider new sessions use
	Active bool `json:"active,omitzero"`
	// Brings a setup of its own that takes the place of its settings
	OwnSetup bool          `json:"own_setup,omitzero"`
	Fields   []MemoryField `json:"fields"`
}

type MemoryOverviewParams struct {
	Profile *string `json:"profile,omitzero"`
}

// State of memory for the profile: the built-in stores, the provider in use and every provider that can be set up
type MemoryOverviewResult struct {
	// Provider new sessions use, empty for the built-in memory alone
	Provider           string `json:"provider,omitzero"`
	MemoryEnabled      bool   `json:"memory_enabled,omitzero"`
	UserProfileEnabled bool   `json:"user_profile_enabled,omitzero"`
	ToolEnabled        bool   `json:"tool_enabled,omitzero"`
	// Whether the plugin of the provider in use is installed, and whether the provider has what it needs
	Installed bool `json:"installed,omitzero"`
	Ready     bool `json:"ready,omitzero"`
	// Why the provider in use is not ready
	Problem   string              `json:"problem,omitzero"`
	Providers []MemoryProviderRow `json:"providers"`
}

// One change to memory: use a provider with the values of its settings, off for the built-in memory alone, or erase a built-in store
type MemoryChangeParams struct {
	Profile  *string           `json:"profile,omitzero"`
	Verb     string            `json:"verb"`
	Provider string            `json:"provider,omitzero"`
	Values   map[string]string `json:"values,omitzero"`
	// Store an erase empties: all, memory for the agent's notes or user for what it knows about the user
	Target string `json:"target,omitzero"`
}

// Memory after a change, and the files an erase deleted
type MemoryChangeResult struct {
	Erased []string             `json:"erased,omitzero"`
	State  MemoryOverviewResult `json:"state"`
}

// State of memory for the memory provider screen
var MemoryOverview = NewMethod[MemoryOverviewParams, MemoryOverviewResult]("memory.overview")

// One change to memory, answered with its state after it
var MemoryChange = NewMethod[MemoryChangeParams, MemoryChangeResult]("memory.change")
