package tuigateway

// One profile as the profiles screen shows it: where it lives, its model, skills and description, and how it is reached
type ProfileOverviewRow struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// The default profile living in the root home
	IsDefault bool `json:"is_default,omitzero"`
	// The profile icebath opens with when none is named
	Active          bool   `json:"active,omitzero"`
	DisplayName     string `json:"display_name,omitzero"`
	Model           string `json:"model,omitzero"`
	Provider        string `json:"provider,omitzero"`
	Description     string `json:"description,omitzero"`
	DescriptionAuto bool   `json:"description_auto,omitzero"`
	SkillCount      int64  `json:"skill_count"`
	// Shell command that opens the profile and the script behind it, empty without one
	Alias     string `json:"alias,omitzero"`
	AliasPath string `json:"alias_path,omitzero"`
	// Distribution the profile was installed from, its name and version, and where it came from
	Distribution       string   `json:"distribution,omitzero"`
	DistributionSource string   `json:"distribution_source,omitzero"`
	PreviousNames      []string `json:"previous_names,omitzero"`
	// Whether a background service of its own runs for the profile
	ServiceRunning bool `json:"service_running,omitzero"`
	HasEnv         bool `json:"has_env,omitzero"`
}

type ProfilesOverviewParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Every profile of the install and the one icebath opens with
type ProfilesOverviewResult struct {
	Active   string               `json:"active"`
	Profiles []ProfileOverviewRow `json:"profiles"`
}

// Description of a profile to set, or to have written by the auxiliary model when Auto
type ProfilesDescribeParams struct {
	Profile *string `json:"profile,omitzero"`
	Name    string  `json:"name"`
	Text    string  `json:"text,omitzero"`
	Auto    bool    `json:"auto,omitzero"`
	// With Auto, replaces a description the user wrote too
	Overwrite bool `json:"overwrite,omitzero"`
}

// Description a profile holds after the change, and why an automatic one was not written
type ProfilesDescribeResult struct {
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
	Auto        bool   `json:"auto,omitzero"`
	Reason      string `json:"reason,omitzero"`
}

// Shell alias of a profile to write or remove
type ProfilesAliasParams struct {
	Profile *string `json:"profile,omitzero"`
	Name    string  `json:"name"`
	Remove  bool    `json:"remove,omitzero"`
}

// Script written or removed, and the command an alias would have shadowed
type ProfilesAliasResult struct {
	Name      string `json:"name"`
	Path      string `json:"path,omitzero"`
	Removed   bool   `json:"removed,omitzero"`
	Collision string `json:"collision,omitzero"`
}

// Every profile with the one icebath opens with, the sticky default
var ProfilesOverview = NewMethod[ProfilesOverviewParams, ProfilesOverviewResult]("profiles.overview")

// Sets a profile's description, or has the auxiliary model write one
var ProfilesDescribe = NewMethod[ProfilesDescribeParams, ProfilesDescribeResult]("profiles.describe")

// Writes or removes the shell alias that opens a profile
var ProfilesAlias = NewMethod[ProfilesAliasParams, ProfilesAliasResult]("profiles.alias")
