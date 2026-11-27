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
		About:  "A camofox-browser server (Camoufox, a Firefox fork with fingerprint spoofing) the camofox browser tools drive over its REST API",
		Settings: []config.Setting{
			{Key: URL, Title: "Camofox server", About: "Address of your camofox-browser server, such as " + DefaultURL + "; the camofox tools appear once it " +
				"is set", Default: "", Env: []string{URLEnv}},
			{Key: APIKey, Title: "Camofox key", About: "Bearer token of a Camofox server started with CAMOFOX_API_KEY; cookie import needs it", Default: "",
				Env: []string{APIKeyEnv}, Secret: true},
			{Key: UserID, Title: "Camofox user", About: "Camofox user every session of the profile runs as, such as one a person signed in; empty gives " +
				"each session its own", Default: "", Env: []string{UserIDEnv}},
			{Key: SessionKey, Title: "Camofox session key", About: "Tab group of the Camofox user the sessions use; empty names one per task", Default: "",
				Env: []string{SessionKeyEnv}},
			{Key: AdoptExistingTab, Title: "Adopt open tab", About: "Take over the tab the Camofox user already has open for the session key instead of " +
				"opening a new one", Default: false, Env: []string{AdoptExistingTabEnv}},
			{Key: ManagedPersistence, Title: "Keep Camofox profile", About: "Run every session of this profile as one stable Camofox user, so its logins " +
				"and cookies last between sessions", Default: false},
			{Key: RewriteLoopback, Title: "Rewrite local addresses", About: "Send localhost addresses to the Docker host alias, for a Camofox server in a " +
				"container", Default: false, Env: []string{RewriteLoopbackEnv}},
			{Key: LoopbackHostAlias, Title: "Docker host name", About: "Name a containerized Camofox server reaches this machine by", Default: "host.docker.internal",
				Env: []string{LoopbackHostAliasEnv}},
			{Key: CookiesDir, Title: "Cookie files folder", About: "Folder browser_import_cookies reads cookies.txt files from", Default: "~/.camofox/cookies"},
		},
	}})
}
