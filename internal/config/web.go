package config

import "gopkg.in/yaml.v3"

// Web section
type WebConfig struct {
	Backend          WebBackend        `yaml:"backend"`
	SearchBackend    WebBackend        `yaml:"search_backend"`
	ExtractBackend   WebBackend        `yaml:"extract_backend"`
	ExtractCharLimit int               `yaml:"extract_char_limit"`
	KeylessFallback  bool              `yaml:"keyless_fallback" coerce:"bool"`
	KeylessRescue    bool              `yaml:"keyless_rescue" coerce:"bool"`
	ProviderTier     Map[ProviderTier] `yaml:"provider_tier"`
	CacheEnabled     bool              `yaml:"cache_enabled" coerce:"boolkeep"`
	CacheTTLMinutes  float64           `yaml:"cache_ttl_minutes"`
	CacheExemptHosts []string          `yaml:"cache_exempt_hosts"`
	UseGateway       *bool             `yaml:"use_gateway,omitempty"`
	rest             rest
}

type webFields WebConfig

// Decodes the mapping and keeps unknown keys
func (w *WebConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*webFields)(w), &w.rest)
}

// Writes the mapping with unknown keys after the named ones
func (w WebConfig) MarshalYAML() (any, error) {
	return encodeMapping(webFields(w), w.rest)
}

// Web search or extract backend, a built-in vendor, the managed portal gateway, or a plugin name
type WebBackend string

// Built-in web backends
const (
	WebUnset      WebBackend = ""
	WebExa        WebBackend = "exa"
	WebParallel   WebBackend = "parallel"
	WebKeenable   WebBackend = "keenable"
	WebFirecrawl  WebBackend = "firecrawl"
	WebTavily     WebBackend = "tavily"
	WebPerplexity WebBackend = "perplexity"
	WebSearxng    WebBackend = "searxng"
	WebBraveFree  WebBackend = "brave-free"
	WebDDGS       WebBackend = "ddgs"
	WebXAI        WebBackend = "xai"
	WebNative     WebBackend = "native"
	WebPortal     WebBackend = "portal"
)

// Built-in web backends, plugins add their own names
func WebBackends() []WebBackend {
	return []WebBackend{WebUnset, WebExa, WebParallel, WebKeenable, WebFirecrawl, WebTavily, WebPerplexity, WebSearxng, WebBraveFree, WebDDGS, WebXAI, WebNative, WebPortal}
}

// Keyless or keyed path of a web vendor
type ProviderTier string

// Vendor tiers
const (
	TierFree ProviderTier = "free"
	TierPaid ProviderTier = "paid"
)

// Accepted vendor tiers, unset means keyed when the key is present
func ProviderTiers() []ProviderTier {
	return []ProviderTier{TierFree, TierPaid}
}

// Browser section
type BrowserConfig struct {
	InactivityTimeout       int           `yaml:"inactivity_timeout"`
	CommandTimeout          int           `yaml:"command_timeout"`
	Headed                  bool          `yaml:"headed"`
	AllowPrivateURLs        bool          `yaml:"allow_private_urls"`
	Engine                  BrowserEngine `yaml:"engine"`
	ExecutablePath          string        `yaml:"executable_path"`
	UserDataDir             string        `yaml:"user_data_dir"`
	ChromiumArgs            StringList    `yaml:"chromium_args"`
	AutoLocalForPrivateURLs bool          `yaml:"auto_local_for_private_urls" coerce:"bool"`
	CDPURL                  string        `yaml:"cdp_url"`
	UseRealProfile          bool          `yaml:"use_real_profile" coerce:"bool"`
	RealProfileAutoclose    bool          `yaml:"real_profile_autoclose" coerce:"bool"`
	RealProfilePin          string        `yaml:"real_profile_pin"`
	DialogPolicy            DialogPolicy  `yaml:"dialog_policy"`
	DialogTimeoutS          float64       `yaml:"dialog_timeout_s"`
	Camofox                 Map[Raw]      `yaml:"camofox"`
	CloudProvider           string        `yaml:"cloud_provider,omitempty"`
	UseGateway              *bool         `yaml:"use_gateway,omitempty"`
	rest                    rest
}

type browserFields BrowserConfig

// Decodes the mapping and keeps unknown keys, including settings the removed built-in browser tools left behind
func (b *BrowserConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*browserFields)(b), &b.rest)
}

// Writes the mapping with unknown keys after the named ones
func (b BrowserConfig) MarshalYAML() (any, error) {
	return encodeMapping(browserFields(b), b.rest)
}

// Local browser engine
type BrowserEngine string

// Browser engines
const (
	EngineAuto       BrowserEngine = "auto"
	EngineLightpanda BrowserEngine = "lightpanda"
	EngineChrome     BrowserEngine = "chrome"
)

// Accepted browser engines, chrome reading as auto
func BrowserEngines() []BrowserEngine {
	return []BrowserEngine{EngineAuto, EngineLightpanda, EngineChrome}
}

// How the CDP supervisor answers page dialogs
type DialogPolicy string

// Dialog policies
const (
	DialogMustRespond DialogPolicy = "must_respond"
	DialogAutoDismiss DialogPolicy = "auto_dismiss"
	DialogAutoAccept  DialogPolicy = "auto_accept"
)

// Accepted dialog policies, any other value reads as must_respond
func DialogPolicies() []DialogPolicy {
	return []DialogPolicy{DialogMustRespond, DialogAutoDismiss, DialogAutoAccept}
}

// Defaults of the web section
func defaultWeb() WebConfig {
	return WebConfig{
		ExtractCharLimit: 15000,
		KeylessFallback:  true,
		KeylessRescue:    true,
		CacheEnabled:     true,
		CacheTTLMinutes:  20,
		CacheExemptHosts: []string{},
	}
}

// Defaults of the browser section
func defaultBrowser() BrowserConfig {
	return BrowserConfig{
		InactivityTimeout:       120,
		CommandTimeout:          30,
		Engine:                  EngineAuto,
		ChromiumArgs:            ListOf(),
		AutoLocalForPrivateURLs: true,
		DialogPolicy:            DialogMustRespond,
		DialogTimeoutS:          300,
	}
}

// Documentation of the web and browser sections
func webDocs() map[string]string {
	return map[string]string{
		"web.backend":                         "Shared fallback, applies to both search and extract.",
		"web.search_backend":                  "Per-capability override for web_search (e.g. \"searxng\").",
		"web.extract_backend":                 "Per-capability override for web_extract (e.g. \"native\").",
		"web.extract_char_limit":              "Per-page char budget for web_extract; larger pages truncate, full text kept in cache/web.",
		"web.keyless_fallback":                "Keyless free-tier ring: with NO web backend configured or keyed, web_search and web_extract rotate round-robin across exa, parallel, firecrawl, keenable public free tiers, failing over on rate limits. Never pre-empts a configured or keyed backend. false = disable.",
		"web.keyless_rescue":                  "One-shot rescue: when the chosen or keyed backend fails a call, THAT call retries once on the keyless ring; the next call tries the chosen backend again (no sticky failover). Off when keyless_fallback is false.",
		"web.provider_tier":                   "Per-vendor tier for vendors with both a keyless free endpoint and a keyed paid path (exa, parallel, firecrawl, keenable; tavily is opt-in keyless via `icebath tools`, not a ring member). Set by the `icebath tools` picker. \"free\" = always the anonymous endpoint even with a key; \"paid\" = always keyed (missing key = error; vendor excluded from the ring); unset = keyed when the key is present, else the ring.",
		"web.cache_enabled":                   "TTL caching for web_search + web_extract: repeat searches (same query + provider) within the TTL come from an in-process memo; repeat extracts from the cache/web store. Concurrent identical searches coalesce into one vendor request. Only successes cached.",
		"web.cache_ttl_minutes":               "Cache TTL in minutes (clamped 1 to 1440).",
		"web.cache_exempt_hosts":              "Hosts always fetched live, never from the extract cache (staging deploys, tunnel URLs, preview builds). Entries match exactly, as \"*.wildcard\", or as a domain suffix (\"mysite.dev\" also covers \"preview.mysite.dev\"). localhost and private IPs are always exempt.",
		"web.use_gateway":                     "Legacy managed tool gateway selection; true reads as the portal provider.",
		"browser":                             "The built-in browser_* tools left core; browser_exec is the only core browser. Config v52 drops `backend` (an `off` gets a one-time notice) and copies AGENT_BROWSER_* from .env to the keys below; other user values stay untouched.",
		"browser.inactivity_timeout":          "Idle seconds before a browser session is reaped.",
		"browser.command_timeout":             "Seconds a local Chromium launch may take to open its DevTools port; a launch always waits at least 120 s (a cold first start unpacks the profile).",
		"browser.headed":                      "Visible Chromium window (local); skips per-turn cleanup, the idle reaper still applies.",
		"browser.allow_private_urls":          "Legacy security.allow_private_urls; local and sidecar Chromium ignore it.",
		"browser.engine":                      "Local browser engine. \"auto\" = Chromium; \"lightpanda\" = `lightpanda serve` per session (faster navigation, no screenshots); \"chrome\" is read as \"auto\". Ignored while a cloud provider, cdp_url or use_real_profile is active.",
		"browser.executable_path":             "Chromium binary for local sessions. \"\" = the managed chromium (`icebath pm install chromium`), the Docker image's build, else a system chromium where none is managed (Termux).",
		"browser.user_data_dir":               "Persistent Chromium profile directory shared by local sessions; \"\" = a fresh throwaway profile per session.",
		"browser.chromium_args":               "Extra Chromium flags for local sessions, e.g. [\"--lang=de\"].",
		"browser.auto_local_for_private_urls": "Cloud provider: LAN and localhost URLs open in a local Chromium (ignores allow_private_urls; false refuses).",
		"browser.cdp_url":                     "Persistent CDP endpoint for attaching to an existing Chromium or Chrome.",
		"browser.use_real_profile":            "Consent to browse with the user's REAL logins locally: runs on an icebath-managed SNAPSHOT of the ACTIVE default-Chromium profile (Local State -> profile.last_used; cookies, logins, prefs copied and re-synced per fresh session) driven by icebath's packaged Chromium. The snapshot dir sidesteps Chrome 136+'s default-profile debugging block and never contends with the running browser. Turning off deletes ~/.icebath/browser-profile/ so credentials don't outlive consent. Chromium-family only; a profile that can't be copied (Firefox default, Windows browser holding its cookies) gives a fresh, signed-out browser plus a message saying why. Also gates the browser_exec `local` arg (real-profile session under a cloud backend). Desktop Settings -> Browser.",
		"browser.real_profile_autoclose":      "Windows only: a running Chrome/Edge/Brave locks its cookie DB (the last snapshot is reused; with none yet, a fresh browser runs). When on, the agent ASKS first; on approval it runs `icebath browser close-profile` (kills that profile's browser tree, unsaved tabs lost) so a new session can copy it; no auto-kill. No effect on macOS and Linux, where a running browser only locks the password and autofill DBs, which are skipped (cookies still copy).",
		"browser.real_profile_pin":            "Pin WHICH source profile directory is snapshotted for real-profile browsing (e.g. \"Profile 2\"). Empty = the browser's last-used profile, which on multi-profile machines can hand the agent the wrong identity. A pin naming a missing directory never falls back to another profile: the session gets a fresh browser without the user's logins.",
		"browser.dialog_policy":               "CDP supervisor dialog handling, active only with a CDP-capable backend (Browserbase, or local Chrome via /browser connect): must_respond | auto_dismiss | auto_accept.",
		"browser.dialog_timeout_s":            "Safety auto-dismiss after N seconds under must_respond.",
		"browser.camofox":                     "Read by the camofox catalog plugin. An empty free-form mapping, so `icebath config set browser.camofox.<key>` is accepted and user values are never stripped.",
		"browser.cloud_provider":              "Cloud browser provider picked in `icebath tools`; portal = the managed tool gateway.",
		"browser.use_gateway":                 "Legacy managed tool gateway selection; true reads as the portal provider.",
	}
}
