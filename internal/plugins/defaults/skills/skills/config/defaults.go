package config

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/skills"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
)

// Sections of what the profile's skills declare: the values they read, and the variables they need
type declared struct {
	registry *skills.Registry
}

func Register(cfg *config.Config, registry *skills.Registry) {
	cfg.Register(declared{registry: registry})
	cfg.Register(config.Sections{{
		Key:    Hub,
		Parent: skillsconfig.Key,
		Title:  "Skills hub",
		About:  "How the skills hub signs in to GitHub, where it searches, installs and publishes skills; without these it reads GitHub anonymously, or as the gh command is signed in",
		Settings: []config.Setting{
			{Key: GitHubToken, Title: "GitHub token", About: "Personal access token the hub reads GitHub with, which raises its limit from 60 to 5,000 requests an hour",
				Default: "", Env: []string{"GITHUB_TOKEN", "GH_TOKEN"}, Secret: true},
			{Key: GitHubAppID, Title: "GitHub App id", About: "Id of a GitHub App the hub signs in as, with its private key and installation", Default: "",
				Env: []string{"GITHUB_APP_ID"}},
			{Key: GitHubAppKey, Title: "GitHub App key file", About: "File of the GitHub App's private key, in PEM", Default: "",
				Env: []string{"GITHUB_APP_PRIVATE_KEY_PATH"}},
			{Key: GitHubAppInstallation, Title: "GitHub App installation", About: "Id of the GitHub App's installation the hub reads through", Default: "",
				Env: []string{"GITHUB_APP_INSTALLATION_ID"}},
		},
	}})
}

func (d declared) Sections(values *config.Config) []config.Section {
	scope := d.registry.Scope(values, "")
	return []config.Section{
		{
			Key:      skillsconfig.Values,
			Parent:   skillsconfig.Key,
			Title:    "Skill settings",
			About:    "Values the profile's skills read, each declared by a skill",
			Settings: scope.ConfigSettings(),
		},
		{
			Key:      skillsconfig.Variables,
			Parent:   skillsconfig.Key,
			Title:    "Skill secrets",
			About:    "Environment variables the profile's skills need, kept here when this process has none of the name",
			Settings: scope.VariableSettings(),
		},
	}
}
