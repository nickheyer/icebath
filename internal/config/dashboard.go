package config

import "gopkg.in/yaml.v3"

// Browser webapp listener of the gateway
type WebappConfig struct {
	Enabled   bool   `yaml:"enabled" coerce:"exact"`
	Host      string `yaml:"host" coerce:"exact"`
	Port      int    `yaml:"port" coerce:"exactint"`
	PublicURL string `yaml:"public_url" coerce:"exact"`
	rest      rest
}

type webappFields WebappConfig

// Decodes the mapping and keeps unknown keys
func (w *WebappConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*webappFields)(w), &w.rest)
}

// Writes the mapping with unknown keys after the named ones
func (w WebappConfig) MarshalYAML() (any, error) {
	return encodeMapping(webappFields(w), w.rest)
}

// Web dashboard section
type DashboardConfig struct {
	Theme                    DashboardTheme   `yaml:"theme"`
	TurnIsolation            bool             `yaml:"turn_isolation"`
	ComputeHostHeartbeatSecs int              `yaml:"compute_host_heartbeat_secs"`
	ComputeHostRespawnMax    int              `yaml:"compute_host_respawn_max"`
	ShowTokenAnalytics       bool             `yaml:"show_token_analytics"`
	TrustedProxies           StringList       `yaml:"trusted_proxies"`
	WSPingInterval           float64          `yaml:"ws_ping_interval"`
	WSPingTimeout            float64          `yaml:"ws_ping_timeout"`
	WSOrphanReapGraceS       float64          `yaml:"ws_orphan_reap_grace_s"`
	WSOrphanActivityStaleS   float64          `yaml:"ws_orphan_activity_stale_s"`
	StartupOrphanSweep       bool             `yaml:"startup_orphan_sweep"`
	OAuth                    DashboardOAuth   `yaml:"oauth"`
	BasicAuth                BasicAuth        `yaml:"basic_auth"`
	SessionTokenAuth         SessionTokenAuth `yaml:"session_token_auth"`
	DrainAuth                DrainAuth        `yaml:"drain_auth"`
	PublicURL                string           `yaml:"public_url"`
	rest                     rest
}

type dashboardFields DashboardConfig

// Decodes the mapping and keeps unknown keys
func (d *DashboardConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*dashboardFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DashboardConfig) MarshalYAML() (any, error) {
	return encodeMapping(dashboardFields(d), d.rest)
}

// Dashboard visual theme
type DashboardTheme string

// Dashboard themes
const (
	ThemeDefault   DashboardTheme = "default"
	ThemeMidnight  DashboardTheme = "midnight"
	ThemeEmber     DashboardTheme = "ember"
	ThemeMono      DashboardTheme = "mono"
	ThemeCyberpunk DashboardTheme = "cyberpunk"
	ThemeRose      DashboardTheme = "rose"
)

// Accepted dashboard themes
func DashboardThemes() []DashboardTheme {
	return []DashboardTheme{ThemeDefault, ThemeMidnight, ThemeEmber, ThemeMono, ThemeCyberpunk, ThemeRose}
}

// Portal OAuth gate of the dashboard
type DashboardOAuth struct {
	ClientID  string `yaml:"client_id"`
	PortalURL string `yaml:"portal_url"`
	rest      rest
}

type dashboardOAuthFields DashboardOAuth

// Decodes the mapping and keeps unknown keys
func (d *DashboardOAuth) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*dashboardOAuthFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DashboardOAuth) MarshalYAML() (any, error) {
	return encodeMapping(dashboardOAuthFields(d), d.rest)
}

// Username and password gate of the dashboard
type BasicAuth struct {
	Username          string `yaml:"username"`
	PasswordHash      string `yaml:"password_hash"`
	Password          string `yaml:"password"`
	Secret            string `yaml:"secret"`
	SessionTTLSeconds int    `yaml:"session_ttl_seconds"`
	rest              rest
}

type basicAuthFields BasicAuth

// Decodes the mapping and keeps unknown keys
func (b *BasicAuth) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*basicAuthFields)(b), &b.rest)
}

// Writes the mapping with unknown keys after the named ones
func (b BasicAuth) MarshalYAML() (any, error) {
	return encodeMapping(basicAuthFields(b), b.rest)
}

// Provisioned session token gate of the dashboard
type SessionTokenAuth struct {
	Enabled bool `yaml:"enabled"`
	rest    rest
}

type sessionTokenAuthFields SessionTokenAuth

// Decodes the mapping and keeps unknown keys
func (s *SessionTokenAuth) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sessionTokenAuthFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SessionTokenAuth) MarshalYAML() (any, error) {
	return encodeMapping(sessionTokenAuthFields(s), s.rest)
}

// Drain-control token gate of the dashboard
type DrainAuth struct {
	Scope          string `yaml:"scope"`
	MinSecretChars int    `yaml:"min_secret_chars"`
	rest           rest
}

type drainAuthFields DrainAuth

// Decodes the mapping and keeps unknown keys
func (d *DrainAuth) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*drainAuthFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DrainAuth) MarshalYAML() (any, error) {
	return encodeMapping(drainAuthFields(d), d.rest)
}

// Privacy section
type PrivacyConfig struct {
	RedactPII bool `yaml:"redact_pii" coerce:"bool"`
	rest      rest
}

type privacyFields PrivacyConfig

// Decodes the mapping and keeps unknown keys
func (p *PrivacyConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*privacyFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PrivacyConfig) MarshalYAML() (any, error) {
	return encodeMapping(privacyFields(p), p.rest)
}

// Defaults of the dashboard section
func defaultDashboard() DashboardConfig {
	return DashboardConfig{
		Theme:                    ThemeDefault,
		ComputeHostHeartbeatSecs: 15,
		ComputeHostRespawnMax:    3,
		TrustedProxies:           ListOf(),
		WSPingInterval:           20.0,
		WSPingTimeout:            20.0,
		WSOrphanReapGraceS:       20.0,
		WSOrphanActivityStaleS:   600.0,
		StartupOrphanSweep:       true,
		SessionTokenAuth:         SessionTokenAuth{Enabled: true},
		DrainAuth:                DrainAuth{Scope: "drain", MinSecretChars: 43},
	}
}

// Documentation of the webapp, dashboard and privacy sections
func dashboardDocs() map[string]string {
	return map[string]string{
		"webapp":                                   "Browser webapp listener of the gateway.",
		"dashboard.theme":                          "Visual theme: \"default\" | \"midnight\" | \"ember\" | \"mono\" | \"cyberpunk\" | \"rose\".",
		"dashboard.turn_isolation":                 "Process-isolation rollout control, read through the raw config loader.",
		"dashboard.compute_host_heartbeat_secs":    "Process-isolation compute host heartbeat interval (seconds).",
		"dashboard.compute_host_respawn_max":       "Process-isolation compute host respawn limit.",
		"dashboard.show_token_analytics":           "Token and cost analytics surfaces are hidden by default: the numbers are a local LOWER-BOUND estimate, not billing. Only successful main-agent responses with a response.usage count; auxiliary calls, retries, fallbacks and cache writes are missed, so the total can be 10x-100x under the provider bill.",
		"dashboard.trusted_proxies":                "IPs or bounded CIDRs of reverse proxies trusted to supply X-Forwarded-Proto and -For. Loopback is always trusted; wildcards and /0 are rejected (spoofing guard).",
		"dashboard.ws_ping_interval":               "WebSocket keepalive (seconds), NON-loopback binds only: loopback always disables the protocol ping so an event-loop stall never kills a healthy local connection.",
		"dashboard.ws_ping_timeout":                "WebSocket keepalive timeout (seconds), non-loopback binds only.",
		"dashboard.ws_orphan_reap_grace_s":         "Grace (seconds) before a WS-orphaned gateway session is interrupted and reaped after its client disconnects. 0 = park forever. Env: ICEBATH_TUI_WS_ORPHAN_REAP_GRACE_S.",
		"dashboard.ws_orphan_activity_stale_s":     "A detached RUNNING turn is only interrupted once its activity clock (API waits, stream tokens, tool heartbeats) has been idle this many seconds; an active turn runs to completion. Default = agent.turn_liveness.timeout_s. 0 = interrupt at grace.",
		"dashboard.startup_orphan_sweep":           "On gateway boot, close tui, desktop and subagent rows orphaned by a dead gateway (start AND newest message older than ICEBATH_TUI_SESSION_TTL_S, default 6h) with end_reason='startup_orphan_reap'; otherwise they stay phantom \"active\" forever. Messaging-gateway and live sessions are never touched; swept rows stay resumable.",
		"dashboard.oauth":                          "OAuth gate (engaged when --host is set and --insecure is not), read by the Icebath Portal plugin. Env ICEBATH_DASHBOARD_OAUTH_CLIENT_ID and ICEBATH_DASHBOARD_PORTAL_URL win when non-empty. Empty client_id = no provider; empty portal_url = production.",
		"dashboard.oauth.client_id":                "agent:{instance_id}, provisioned by the Portal.",
		"dashboard.basic_auth":                     "Username and password gate (no OAuth IDP). Active when username plus password_hash (preferred) or password (hashed in-memory) are set; empty username = no-op. Env ICEBATH_DASHBOARD_BASIC_AUTH_USERNAME, _PASSWORD_HASH, _PASSWORD, _SECRET, _TTL_SECONDS win when non-empty. secret signs session tokens; empty = a random per-process key (sessions die on restart, no multi-worker), so set 32+ random bytes.",
		"dashboard.basic_auth.password_hash":       "scrypt$... hash of the password.",
		"dashboard.basic_auth.session_ttl_seconds": "0 -> the plugin default (12h).",
		"dashboard.session_token_auth.enabled":     "A provisioned ICEBATH_DASHBOARD_SESSION_TOKEN (literal or file:<path>) authorizes the gated dashboard; false opts out.",
		"dashboard.drain_auth":                     "Drain-control token auth: the secret is NOT here (env ICEBATH_DASHBOARD_DRAIN_SECRET); no-op unless >=256-bit, weak secrets rejected (fail-closed).",
		"dashboard.public_url":                     "Public URL (env ICEBATH_DASHBOARD_PUBLIC_URL): full authority (scheme + host + optional prefix, e.g. https://example.com/icebath) for the OAuth redirect_uri; its hostname is trusted by Host and Origin guards and engages the auth gate when non-loopback. For proxies that don't forward X-Forwarded-Host, -Proto and -Prefix; X-Forwarded-Prefix is then IGNORED on the OAuth path. Empty or malformed (no http(s):// + host, or quote, angle or whitespace chars) = reconstruct from headers.",
		"privacy.redact_pii":                       "Hash user IDs and strip phone numbers from LLM context.",
	}
}
