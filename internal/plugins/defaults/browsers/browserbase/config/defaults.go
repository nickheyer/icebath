package config

import (
	"time"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: browserconfig.Key,
		Title:  "Browserbase",
		About:  "Browserbase (https://browserbase.com) cloud browser backend with stealth, proxies and keep-alive sessions",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Browserbase key", About: "Browserbase API key", Default: "", Env: []string{APIKeyEnv}, Secret: true},
			{Key: ProjectID, Title: "Browserbase project", About: "Browserbase project ID, from the project settings", Default: "", Env: []string{ProjectIDEnv}},
			{Key: BaseURL, Title: "Browserbase address", About: "Browserbase API address, https://api.browserbase.com when empty", Default: "",
				Env: []string{"BROWSERBASE_BASE_URL"}},
			{Key: AdvancedStealth, Title: "Browserbase stealth", About: "Ask Browserbase for its advanced stealth browser; needs a Browserbase plan that " +
				"includes it", Default: false, Env: []string{"BROWSERBASE_ADVANCED_STEALTH"}},
			{Key: KeepAlive, Title: "Browserbase keep alive", About: "Keep a Browserbase session alive between steps; dropped by itself when the plan " +
				"does not include it", Default: true, Env: []string{"BROWSERBASE_KEEP_ALIVE"}},
			{Key: Proxies, Title: "Browserbase proxies", About: "Route Browserbase sessions through its residential proxies; dropped by itself when the " +
				"plan does not include them", Default: true, Env: []string{"BROWSERBASE_PROXIES"}},
			{Key: SessionTimeout, Title: "Browserbase session time", About: "Seconds a Browserbase session may run before it ends; zero uses the " +
				"project's own limit", Default: time.Duration(0), Null: "the project's own limit", Env: []string{"BROWSERBASE_SESSION_TIMEOUT", browserconfig.SessionTimeoutEnv}},
		},
	}})
}
