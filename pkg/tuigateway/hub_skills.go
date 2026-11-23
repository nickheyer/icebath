package tuigateway

// One installed skill as the skills screen lists it
type SkillHubRow struct {
	Name        string `json:"name"`
	Category    string `json:"category,omitzero"`
	Description string `json:"description,omitzero"`
	// hub, builtin or local
	Source string `json:"source"`
	// Registry a hub skill came from
	Origin string `json:"origin,omitzero"`
	Trust  string `json:"trust,omitzero"`
	// Whether the agent may use it, the profile's disabled list leaving it out
	Enabled bool `json:"enabled"`
	// Always loaded, never disabled
	Essential bool `json:"essential,omitzero"`
}

// One extra source the skill hub searches beside the default ones
type SkillTapRow struct {
	Name string `json:"name"`
	Path string `json:"path,omitzero"`
}

// One skill bundle, several skills loaded under one slash command
type SkillBundleRow struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitzero"`
	Instruction string   `json:"instruction,omitzero"`
	Skills      []string `json:"skills"`
	Path        string   `json:"path,omitzero"`
}

// Profile whose skills the screen reads
type SkillsHubParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Everything the skills screens list: installed skills, extra sources, bundles and trusted projects
type SkillsHubResult struct {
	Skills  []SkillHubRow    `json:"skills"`
	Taps    []SkillTapRow    `json:"taps"`
	Bundles []SkillBundleRow `json:"bundles"`
	// Project folders whose own skills load
	Trusted []string `json:"trusted"`
	// Whether bundled skills stopped being copied into this profile
	OptedOut bool `json:"opted_out,omitzero"`
}

// One skills command run without questions, the verb and the arguments it reads
type SkillsHubRunParams struct {
	Profile *string `json:"profile,omitzero"`
	// install, check, update, audit, uninstall, reset, list-modified, diff, opt-out, opt-in, repair-official, publish,
	// snapshot-export, snapshot-import, tap-add, tap-remove, trust, untrust
	Action     string `json:"action"`
	Name       string `json:"name,omitzero"`
	Identifier string `json:"identifier,omitzero"`
	Category   string `json:"category,omitzero"`
	Repo       string `json:"repo,omitzero"`
	Path       string `json:"path,omitzero"`
	To         string `json:"to,omitzero"`
	Force      bool   `json:"force,omitzero"`
	Restore    bool   `json:"restore,omitzero"`
	Sync       bool   `json:"sync,omitzero"`
	Remove     bool   `json:"remove,omitzero"`
	Deep       bool   `json:"deep,omitzero"`
}

// What a skills command printed and whether it reported success
type SkillsHubRunResult struct {
	OK    bool     `json:"ok"`
	Lines []string `json:"lines"`
}

// Bundle to write, replacing one of the same name only when force is set
type SkillsBundleSaveParams struct {
	Profile     *string  `json:"profile,omitzero"`
	Name        string   `json:"name"`
	Skills      []string `json:"skills"`
	Description string   `json:"description,omitzero"`
	Instruction string   `json:"instruction,omitzero"`
	Force       bool     `json:"force,omitzero"`
}

// Bundle a removal names
type SkillsBundleNameParams struct {
	Profile *string `json:"profile,omitzero"`
	Name    string  `json:"name"`
}

// Bundle file a write or removal touched
type SkillsBundleResult struct {
	Path string `json:"path"`
}

// Bundles a rescan found added and removed
type SkillsBundleReloadResult struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Total   int64    `json:"total"`
}

var (
	// Installed skills, sources, bundles and trusted projects of the profile
	SkillsHubList = NewMethod[SkillsHubParams, SkillsHubResult]("skills.hub.list")
	// Runs one skills command without questions and answers what it printed
	SkillsHubRun = NewMethod[SkillsHubRunParams, SkillsHubRunResult]("skills.hub.run")
	// Writes a skill bundle
	SkillsBundleSave = NewMethod[SkillsBundleSaveParams, SkillsBundleResult]("skills.bundles.save")
	// Removes a skill bundle
	SkillsBundleDelete = NewMethod[SkillsBundleNameParams, SkillsBundleResult]("skills.bundles.delete")
	// Reads the bundles folder again
	SkillsBundleReload = NewMethod[SkillsHubParams, SkillsBundleReloadResult]("skills.bundles.reload")
)
