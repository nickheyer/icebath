package config

import skillsconfig "github.com/nickheyer/icebath/internal/skills/config"

const (
	// Settings of the skills hub
	Hub = skillsconfig.Key + ".hub"
	// Credentials the hub reads GitHub with, beside the gh command's own sign in
	GitHubToken           = Hub + ".github_token"
	GitHubAppID           = Hub + ".github_app_id"
	GitHubAppKey          = Hub + ".github_app_private_key_path"
	GitHubAppInstallation = Hub + ".github_app_installation_id"
)
