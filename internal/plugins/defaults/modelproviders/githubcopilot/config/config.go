package config

const (
	CopilotName         = "copilot"
	CopilotTitle        = "GitHub Copilot"
	CopilotBaseURL      = "https://api.githubcopilot.com"
	CopilotDefaultModel = ""
)

const (
	ACPName         = "copilot-acp"
	ACPTitle        = "GitHub Copilot ACP"
	ACPBaseURL      = ACPMarkerURL
	ACPDefaultModel = ""
)

const (
	CopilotTokenEnv   = "COPILOT_GITHUB_TOKEN"
	GHTokenEnv        = "GH_TOKEN"
	GitHubTokenEnv    = "GITHUB_TOKEN"
	CopilotBaseURLEnv = "COPILOT_API_BASE_URL"
	ACPBaseURLEnv     = "COPILOT_ACP_BASE_URL"
	ACPMarkerURL      = "acp://copilot"
)
