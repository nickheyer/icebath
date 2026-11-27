package config

import browserconfig "github.com/nickheyer/icebath/internal/browser/config"

const (
	Key       = browserconfig.Key + ".browserbase"
	APIKey    = Key + ".api_key"
	ProjectID = Key + ".project_id"
	BaseURL   = Key + ".base_url"
	// Paid features asked for each session, dropped by themselves when the plan has none
	AdvancedStealth = Key + ".advanced_stealth"
	KeepAlive       = Key + ".keep_alive"
	Proxies         = Key + ".proxies"
	// Seconds a session may run, zero for the project's own limit
	SessionTimeout = Key + ".session_timeout"

	APIKeyEnv    = "BROWSERBASE_API_KEY"
	ProjectIDEnv = "BROWSERBASE_PROJECT_ID"
)
