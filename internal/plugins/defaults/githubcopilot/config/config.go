package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	CopilotName         = "copilot"
	CopilotKey          = pluginsconfig.Key + "." + CopilotName
	CopilotBaseURL      = CopilotKey + ".base_url"
	CopilotAPIKey       = CopilotKey + ".api_key"
	CopilotDefaultModel = CopilotKey + ".default_model"
)

const (
	ACPName         = "copilot-acp"
	ACPKey          = pluginsconfig.Key + "." + ACPName
	ACPBaseURL      = ACPKey + ".base_url"
	ACPDefaultModel = ACPKey + ".default_model"
)

const (
	CopilotTokenEnv   = "COPILOT_GITHUB_TOKEN"
	GHTokenEnv        = "GH_TOKEN"
	GitHubTokenEnv    = "GITHUB_TOKEN"
	CopilotBaseURLEnv = "COPILOT_API_BASE_URL"
	ACPBaseURLEnv     = "COPILOT_ACP_BASE_URL"
	ACPMarkerURL      = "acp://copilot"
)
