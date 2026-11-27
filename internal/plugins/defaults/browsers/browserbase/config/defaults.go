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
		About:  "Cloud browsers with stealth, proxies and long sessions",
		Order:  20,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Browserbase API key.", Help: "Get one at browserbase.com. Set the project ID as well.", Default: "",
				Env: []string{APIKeyEnv}, Secret: true},
			{Key: ProjectID, Title: "Project ID", About: "The Browserbase project that your browsers run in.",
				Help: "Find the ID in the project settings on browserbase.com.", Default: "", Env: []string{ProjectIDEnv}},
			{Key: Proxies, Title: "Residential proxies", About: "Send the traffic of each session through residential proxies.",
				Help: "Left out by itself when your plan has no proxies.", Default: true, Env: []string{"BROWSERBASE_PROXIES"}},
			{Key: KeepAlive, Title: "Keep sessions alive", About: "Keep a browser session open between steps.",
				Help: "Left out by itself when your plan does not keep sessions alive.", Default: true, Env: []string{"BROWSERBASE_KEEP_ALIVE"}},
			{Key: AdvancedStealth, Title: "Advanced stealth", About: "Ask for the browser that sites find hardest to detect.",
				Help: "Needs a Browserbase plan with advanced stealth.", Default: false, Env: []string{"BROWSERBASE_ADVANCED_STEALTH"}},
			{Key: SessionTimeout, Title: "Session time limit", About: "How long one browser session may run before it ends.", Default: time.Duration(0),
				Zero: "The project's limit", Null: "the project's limit", Env: []string{"BROWSERBASE_SESSION_TIMEOUT", browserconfig.SessionTimeoutEnv},
				Advanced: true},
			{Key: BaseURL, Title: "Base URL", About: "The address of the Browserbase API.", Default: "", Zero: "The standard address",
				Env: []string{"BROWSERBASE_BASE_URL"}, Advanced: true},
		},
	}})
}
