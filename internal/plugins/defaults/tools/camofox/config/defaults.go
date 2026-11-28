package config

import (
	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: browserconfig.Key,
		Title:  "Camofox",
		About:  "A Camofox server, a Firefox that sites find hard to spot",
		Order:  40,
		Settings: []config.Setting{
			{Key: URL, Title: "Server address", About: "The address of your Camofox server.",
				Help: "Such as " + DefaultURL + ". The Camofox tools appear once the address is set.", Default: "", Env: []string{URLEnv}},
			{Key: APIKey, Title: "API key", About: "Your Camofox API key.",
				Help: "The CAMOFOX_API_KEY that the server was started with. Importing cookies needs the key.", Default: "", Env: []string{APIKeyEnv},
				Secret: true},
			{Key: ManagedPersistence, Title: "Keep sign-ins", About: "Run every session of this profile as one Camofox user, so sign-ins and cookies last.",
				Default: false},
			{Key: UserID, Title: "User", About: "The Camofox user that every session of this profile runs as.",
				Help: "Such as a user that a person signed in to sites with.", Default: "", Zero: "One per session", Env: []string{UserIDEnv}, Advanced: true},
			{Key: SessionKey, Title: "Tab group", About: "The tab group of the Camofox user that sessions use.", Help: "Only used while User is set.",
				Default: "", Zero: "One per task", Env: []string{SessionKeyEnv}, Advanced: true},
			{Key: AdoptExistingTab, Title: "Take over open tab", About: "Use the tab that the Camofox user already has open instead of opening one.",
				Default: false, Env: []string{AdoptExistingTabEnv}, Advanced: true},
			{Key: RewriteLoopback, Title: "Rewrite local addresses", About: "Send localhost addresses to the Docker host, for a Camofox server in Docker.",
				Default: false, Env: []string{RewriteLoopbackEnv}, Advanced: true},
			{Key: LoopbackHostAlias, Title: "Docker host name", About: "The name that a Camofox server in Docker uses to reach this computer.",
				Default: "host.docker.internal", Zero: "host.docker.internal", Env: []string{LoopbackHostAliasEnv}, Advanced: true},
			{Key: CookiesDir, Title: "Cookie files folder", About: "The folder that cookies.txt files are imported from.", Default: "~/.camofox/cookies",
				Zero: "~/.camofox/cookies", Path: &config.Path{Kind: config.PathFolder}, Advanced: true},
		},
	}})
}
