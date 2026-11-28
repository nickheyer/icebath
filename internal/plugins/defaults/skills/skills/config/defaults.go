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
		About:  "How the skills hub signs in to GitHub",
		Order:  30,
		Settings: []config.Setting{
			{Key: GitHubToken, Title: "GitHub token", About: "Your GitHub personal access token for the skills hub.",
				Help: "Raises the hub's limit from 60 to 5,000 GitHub requests an hour. " +
					"Without a token, the hub reads GitHub as the gh command or anonymously.", Default: "", Env: []string{"GITHUB_TOKEN", "GH_TOKEN"},
				Secret: true},
			{Key: GitHubAppID, Title: "GitHub App ID", About: "The ID of a GitHub App that the hub signs in as.",
				Help: "Set the key file and installation ID of the App as well.", Default: "", Env: []string{"GITHUB_APP_ID"}, Advanced: true},
			{Key: GitHubAppKey, Title: "GitHub App key file", About: "The file that holds the private key of the GitHub App.", Help: "A .pem file.",
				Default: "", Env: []string{"GITHUB_APP_PRIVATE_KEY_PATH"}, Advanced: true, Path: &config.Path{Kind: config.PathFile, Exists: true}},
			{Key: GitHubAppInstallation, Title: "GitHub App installation", About: "The installation ID of the GitHub App that the hub reads through.",
				Default: "", Env: []string{"GITHUB_APP_INSTALLATION_ID"}, Advanced: true},
		},
	}})
}

func (d declared) Sections(values *config.Config) []config.Section {
	scope := d.registry.Scope(values, "")
	return []config.Section{
		{
			Key:      skillsconfig.Values,
			Parent:   skillsconfig.Key,
			Title:    "Skill options",
			About:    "Values that your installed skills read",
			Order:    20,
			Settings: scope.ConfigSettings(),
		},
		{
			Key:      skillsconfig.Variables,
			Parent:   skillsconfig.Key,
			Title:    "Skill secrets",
			About:    "API keys and other secrets that your skills need",
			Order:    10,
			Settings: scope.VariableSettings(),
		},
	}
}
