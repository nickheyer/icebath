package tuigateway

type ConfigFileLock string

func (ConfigFileLock) Values() []string { return []string{"managed", "no_file"} }

type ConfigFilesParams struct {
	Profile *string `json:"profile,omitzero"`
	// Makes config.yaml from the example when it is missing, so an editor opens a file that holds every setting
	Seed bool `json:"seed,omitzero"`
}

// Files the profile's settings and keys are kept in, and why they are not edited by hand when they are not
type ConfigFilesResult struct {
	Config       string `json:"config"`
	ConfigExists bool   `json:"config_exists,omitzero"`
	Env          string `json:"env"`
	EnvExists    bool   `json:"env_exists,omitzero"`
	// config.yaml was made from the example by this request
	Seeded bool `json:"seeded,omitzero"`
	// managed for an install a package manager keeps, no_file for a settings store without a file
	Locked ConfigFileLock `json:"locked,omitzero"`
	// Package manager that keeps a managed install
	Manager string `json:"manager,omitzero"`
}

// Settings and keys files of the profile, config.yaml made first when asked
var ConfigFiles = NewMethod[ConfigFilesParams, ConfigFilesResult]("config.files")
