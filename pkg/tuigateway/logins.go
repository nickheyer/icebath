package tuigateway

type LoginStatus string

func (LoginStatus) Values() []string {
	return []string{"ready", "limited", "needs_sign_in", "dead", "unavailable"}
}

type LoginOrigin string

func (LoginOrigin) Values() []string { return []string{"owned", "linked"} }

type LoginScope string

func (LoginScope) Values() []string { return []string{"session", "profile", "legacy"} }

// One login of a provider as the logins screen lists it, never carrying a secret
type LoginRow struct {
	ID       string      `json:"id"`
	Provider string      `json:"provider"`
	Name     string      `json:"name"`
	Label    string      `json:"label,omitzero"`
	AuthType string      `json:"auth_type"`
	Origin   LoginOrigin `json:"origin"`
	// Raw source, manual or manual:<flow> for Icebath held logins, env:VAR, claude_code, qwen-cli, config:<name>
	Source      string                `json:"source"`
	Account     string                `json:"account,omitzero"`
	Fingerprint string                `json:"fingerprint,omitzero"`
	Status      LoginStatus           `json:"status"`
	ResetsAt    *string               `json:"resets_at,omitzero"`
	Reason      string                `json:"reason,omitzero"`
	Usage       []ProviderUsageWindow `json:"usage,omitzero"`
	Priority    int64                 `json:"priority"`
	// Profile default of the provider
	Default bool `json:"default,omitzero"`
	// In the provider's rotation set
	Rotate bool `json:"rotate,omitzero"`
	// Pinned by the session the request named
	Session bool `json:"session,omitzero"`
	// The login the provider's next request uses for that session, else for the profile
	Active bool `json:"active,omitzero"`
	// Other consumer of the same secret, git for GITHUB_TOKEN and GH_TOKEN
	SharedWith string `json:"shared_with,omitzero"`
}

// Login or rotation set a provider's requests use and the scope the choice came from
type LoginSelection struct {
	Scope  LoginScope `json:"scope"`
	Login  string     `json:"login,omitzero"`
	Name   string     `json:"name,omitzero"`
	Rotate bool       `json:"rotate,omitzero"`
	// Session pin that names a login the provider no longer holds
	Missing string `json:"missing,omitzero"`
}

// One provider's logins with its profile default, rotation strategy and exhaustion policy
type LoginProviderGroup struct {
	// Pool key the logins are stored under, the value session.info.pool_key carries for the same provider
	Provider string `json:"provider"`
	// Provider id the model picker lists, custom:<name> for a custom endpoint
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Profile default, a login id or @rotate, empty when unset
	Default     string         `json:"default,omitzero"`
	Strategy    string         `json:"strategy"`
	OnExhausted string         `json:"on_exhausted"`
	Selection   LoginSelection `json:"selection"`
	Logins      []LoginRow     `json:"logins"`
}

// A session's own login choices and the login its current provider uses
type LoginListSession struct {
	SessionID string `json:"session_id"`
	// Pool key of the session's current route, compare it with a group's provider
	PoolKey string `json:"pool_key,omitzero"`
	// Provider id of the session's current route as the model picker lists it
	Slug string `json:"slug,omitzero"`
	// Login id or @rotate the session pinned per pool key
	Pins map[string]string `json:"pins,omitzero"`
	// rotate or fail the session set per pool key over the profile's on_exhausted
	OnExhausted map[string]string `json:"on_exhausted,omitzero"`
	Login       *SessionLogin     `json:"login,omitzero"`
	Wait        *LoginWait        `json:"wait,omitzero"`
}

// A retry of a session's last turn armed for when its login resets
type LoginWait struct {
	Provider string   `json:"provider"`
	Login    LoginRef `json:"login"`
	RetryAt  string   `json:"retry_at"`
}

type LoginListParams struct {
	Profile   *string `json:"profile,omitzero"`
	SessionID string  `json:"session_id,omitzero"`
	Provider  string  `json:"provider,omitzero"`
}

type LoginListResult struct {
	Providers []LoginProviderGroup `json:"providers"`
	Session   *LoginListSession    `json:"session,omitzero"`
}

type LoginAddKeyParams struct {
	Profile       *string `json:"profile,omitzero"`
	SessionID     string  `json:"session_id,omitzero"`
	Provider      string  `json:"provider"`
	APIKey        string  `json:"api_key"`
	Name          string  `json:"name,omitzero"`
	Rotate        bool    `json:"rotate,omitzero"`
	MakeDefault   bool    `json:"make_default,omitzero"`
	UseForSession bool    `json:"use_for_session,omitzero"`
}

type LoginResult struct {
	Login   LoginRow `json:"login"`
	Created bool     `json:"created,omitzero"`
	// Why the login could not be used for the session at once
	Warning string `json:"warning,omitzero"`
}

type LoginOAuthStartParams struct {
	Profile       *string `json:"profile,omitzero"`
	SessionID     string  `json:"session_id,omitzero"`
	Provider      string  `json:"provider"`
	Name          string  `json:"name,omitzero"`
	Rotate        bool    `json:"rotate,omitzero"`
	MakeDefault   bool    `json:"make_default,omitzero"`
	UseForSession bool    `json:"use_for_session,omitzero"`
	// openai-codex only, the browser PKCE flow instead of the device code
	Browser bool `json:"browser,omitzero"`
}

type LoginOAuthStartResult struct {
	JobID    string `json:"job_id"`
	Provider string `json:"provider"`
	// The sign in finishes through a redirect to the background service machine's localhost, so it works only when the client runs there
	Loopback bool `json:"loopback,omitzero"`
}

type LoginOAuthPollParams struct {
	Profile *string `json:"profile,omitzero"`
	JobID   string  `json:"job_id"`
	// Answer to the prompt the job waits on, the pasted PKCE code
	Input  *string `json:"input,omitzero"`
	Cancel bool    `json:"cancel,omitzero"`
}

type LoginOAuthState string

func (LoginOAuthState) Values() []string {
	return []string{"running", "waiting_input", "done", "failed", "cancelled"}
}

type LoginOAuthPollResult struct {
	JobID string          `json:"job_id"`
	State LoginOAuthState `json:"state"`
	// Every output line of the sign in so far, the device URL and code among them
	Lines    []string  `json:"lines"`
	URL      string    `json:"url,omitzero"`
	UserCode string    `json:"user_code,omitzero"`
	Prompt   string    `json:"prompt,omitzero"`
	Login    *LoginRow `json:"login,omitzero"`
	Error    string    `json:"error,omitzero"`
	// Why the new login could not be used for the session at once
	Warning string `json:"warning,omitzero"`
	// Open url in a browser on the user's machine, once; the background service never opens one
	Open bool `json:"open,omitzero"`
	// The sign in finishes through a redirect to the background service machine's localhost
	Loopback bool `json:"loopback,omitzero"`
}

type LoginLinkParams struct {
	Profile   *string `json:"profile,omitzero"`
	SessionID string  `json:"session_id,omitzero"`
	Provider  string  `json:"provider"`
	// Env var whose value the login reads, left where it is
	EnvVar string `json:"env_var,omitzero"`
	// Another tool's login, claude_code or qwen-cli
	Source        string `json:"source,omitzero"`
	Name          string `json:"name,omitzero"`
	UseForSession bool   `json:"use_for_session,omitzero"`
}

type LoginRenameParams struct {
	Profile  *string `json:"profile,omitzero"`
	Provider string  `json:"provider"`
	Login    string  `json:"login"`
	Name     string  `json:"name"`
}

type LoginRemoveParams struct {
	Profile  *string `json:"profile,omitzero"`
	Provider string  `json:"provider"`
	Login    string  `json:"login"`
}

type LoginRemoveResult struct {
	Login   LoginRow `json:"login"`
	Cleaned []string `json:"cleaned,omitzero"`
	Hints   []string `json:"hints,omitzero"`
	// Profile default the removal left, a login id, empty when none
	Default string `json:"default,omitzero"`
}

type LoginSetDefaultParams struct {
	Profile  *string `json:"profile,omitzero"`
	Provider string  `json:"provider"`
	// Login id or name, @rotate, or empty to clear the default
	Login *string `json:"login,omitzero"`
	// fail or rotate, what a pinned login does when exhausted outside the TUI
	OnExhausted string `json:"on_exhausted,omitzero"`
	Strategy    string `json:"strategy,omitzero"`
}

type LoginSetDefaultResult struct {
	Provider    string `json:"provider"`
	Default     string `json:"default,omitzero"`
	OnExhausted string `json:"on_exhausted"`
	Strategy    string `json:"strategy"`
}

type LoginSetRotationParams struct {
	Profile  *string `json:"profile,omitzero"`
	Provider string  `json:"provider"`
	Login    string  `json:"login"`
	Rotate   bool    `json:"rotate"`
}

type LoginConvertParams struct {
	Profile  *string `json:"profile,omitzero"`
	Provider string  `json:"provider"`
	Login    string  `json:"login"`
}

type SessionSetLoginParams struct {
	SessionID string `json:"session_id"`
	// Pool key or provider slug of the login, the session's current provider when omitted
	Provider string `json:"provider,omitzero"`
	// Login id or name, @rotate, or empty to follow the profile default again; omitted keeps the pin
	Login *string `json:"login,omitzero"`
	// rotate or fail for this session only, empty to follow logins.<p>.on_exhausted again; omitted keeps it
	OnExhausted *string `json:"on_exhausted,omitzero"`
	// Re-runs the session's last turn once the choice is stored
	Retry bool `json:"retry,omitzero"`
}

type SessionSetLoginResult struct {
	SessionID string `json:"session_id"`
	// Pool key the choice is stored under
	Provider string    `json:"provider"`
	Slug     string    `json:"slug"`
	Login    *LoginRow `json:"login,omitzero"`
	Rotate   bool      `json:"rotate,omitzero"`
	Cleared  bool      `json:"cleared,omitzero"`
	// The session's own on_exhausted for the provider, absent when it follows the profile
	OnExhausted string `json:"on_exhausted,omitzero"`
	// The choice is for the session's current provider and applies from its next turn; false keeps it for a later switch
	Current bool `json:"current"`
	// What session.info.login says now
	SessionLogin *SessionLogin `json:"session_login,omitzero"`
	Retried      bool          `json:"retried,omitzero"`
	// Why the choice could not take effect at once, the choice itself kept
	Warning string `json:"warning,omitzero"`
}

type SessionLoginWaitParams struct {
	SessionID string `json:"session_id"`
	// Pool key or provider slug, the session's current provider when omitted
	Provider string `json:"provider,omitzero"`
	// Retries the last turn at once instead of at the reset time
	Now bool `json:"now,omitzero"`
	// Drops an armed wait
	Cancel bool `json:"cancel,omitzero"`
}

type LoginWaitState string

func (LoginWaitState) Values() []string { return []string{"armed", "retried", "cancelled", "idle"} }

type SessionLoginWaitResult struct {
	SessionID string         `json:"session_id"`
	Provider  string         `json:"provider"`
	Login     *LoginRef      `json:"login,omitzero"`
	State     LoginWaitState `json:"state"`
	RetryAt   *string        `json:"retry_at,omitzero"`
}

// One login by id and name
type LoginRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Pinned login of a session that ran out, its reset time and the provider's other ready logins
type LoginExhaustedPayload struct {
	SessionID string `json:"session_id"`
	// Pool key of the login
	Provider string     `json:"provider"`
	Slug     string     `json:"slug"`
	Login    LoginRef   `json:"login"`
	ResetsAt *string    `json:"resets_at,omitzero"`
	Reason   string     `json:"reason,omitzero"`
	Ready    []LoginRef `json:"ready"`
	// The provider has other logins in its rotation set
	Rotation    bool   `json:"rotation"`
	OnExhausted string `json:"on_exhausted"`
}

// Every provider's logins with session, default, rotation and status markers
var LoginList = NewMethod[LoginListParams, LoginListResult]("login.list")

// Stores an API key as an owned login, updating the owned login of that name or key
var LoginAddKey = NewMethod[LoginAddKeyParams, LoginResult]("login.add_key")

// Starts an OAuth sign in that ends in a new owned login, polled with login.oauth_poll
var LoginOAuthStart = NewMethod[LoginOAuthStartParams, LoginOAuthStartResult]("login.oauth_start")

// Progress of an OAuth sign in, answering its prompt or cancelling it
var LoginOAuthPoll = NewMethod[LoginOAuthPollParams, LoginOAuthPollResult]("login.oauth_poll")

// Adds a linked login over an env var or another tool's login
var LoginLink = NewMethod[LoginLinkParams, LoginResult]("login.link")

// Renames a login, references by id staying valid
var LoginRename = NewMethod[LoginRenameParams, LoginResult]("login.rename")

// Removes one login and only its own source
var LoginRemove = NewMethod[LoginRemoveParams, LoginRemoveResult]("login.remove")

// Sets the provider's profile default login, exhaustion policy or rotation strategy
var LoginSetDefault = NewMethod[LoginSetDefaultParams, LoginSetDefaultResult]("login.set_default")

// Puts a login in or out of the provider's rotation set
var LoginSetRotation = NewMethod[LoginSetRotationParams, LoginResult]("login.set_rotation")

// Moves an env linked API key into an owned login of the same name
var LoginConvert = NewMethod[LoginConvertParams, LoginResult]("login.convert")

// Pins a login, or the rotation set, and the exhaustion reaction for one session, per provider
var SessionSetLogin = NewMethod[SessionSetLoginParams, SessionSetLoginResult]("session.set_login")

// Retries a session's last turn when its pinned login resets, at once, or drops the armed retry
var SessionLoginWait = NewMethod[SessionLoginWaitParams, SessionLoginWaitResult]("session.login_wait")

// A session's pinned login ran out and Icebath will not switch accounts on its own
var LoginExhausted = NewEvent[LoginExhaustedPayload]("login.exhausted")

// Login a session's requests go out with, for the status bar's via <login>
type SessionLogin struct {
	// Pool key of the session's current route
	Provider string     `json:"provider"`
	Slug     string     `json:"slug"`
	ID       string     `json:"id,omitzero"`
	Name     string     `json:"name,omitzero"`
	Scope    LoginScope `json:"scope"`
	Rotate   bool       `json:"rotate,omitzero"`
	// Session pin that names a login the provider no longer holds
	Missing string `json:"missing,omitzero"`
	// rotate when the session set its own reaction to an exhausted login, else the profile's
	OnExhausted string `json:"on_exhausted,omitzero"`
}
