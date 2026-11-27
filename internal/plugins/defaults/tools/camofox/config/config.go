package config

import browserconfig "github.com/nickheyer/icebath/internal/browser/config"

const (
	Key                = browserconfig.Key + ".camofox"
	URL                = Key + ".url"
	APIKey             = Key + ".api_key"
	UserID             = Key + ".user_id"
	SessionKey         = Key + ".session_key"
	AdoptExistingTab   = Key + ".adopt_existing_tab"
	ManagedPersistence = Key + ".managed_persistence"
	RewriteLoopback    = Key + ".rewrite_loopback_urls"
	LoopbackHostAlias  = Key + ".loopback_host_alias"
	CookiesDir         = Key + ".cookies_dir"
)

// Variables of the settings, which a Camofox server's own setup names
const (
	URLEnv               = "CAMOFOX_URL"
	APIKeyEnv            = "CAMOFOX_API_KEY"
	UserIDEnv            = "CAMOFOX_USER_ID"
	SessionKeyEnv        = "CAMOFOX_SESSION_KEY"
	AdoptExistingTabEnv  = "CAMOFOX_ADOPT_EXISTING_TAB"
	RewriteLoopbackEnv   = "CAMOFOX_REWRITE_LOOPBACK_URLS"
	LoopbackHostAliasEnv = "CAMOFOX_LOOPBACK_HOST_ALIAS"
)

// Address a Camofox server listens on by default
const DefaultURL = "http://localhost:9377"

// Toolset of every Camofox tool, and the helper task browser_vision runs on
const (
	Toolset = "camofox"
	Task    = "camofox_vision"
)
