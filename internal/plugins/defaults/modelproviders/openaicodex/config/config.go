package config

const (
	Name         = "openai-codex"
	Title        = "ChatGPT or Codex Subscription"
	BaseURL      = "https://chatgpt.com/backend-api/codex"
	DefaultModel = ""
)

const BaseURLEnv = "ICEBATH_CODEX_BASE_URL"

// Fields of an account, below its name
const (
	AccessToken  = "access_token"
	RefreshToken = "refresh_token"
	// When the tokens were last renewed
	LastRefresh = "last_refresh"
)

// Settings of a provider, below its name
const (
	// How the sign-in is done: device_code or browser
	LoginFlow = "login_flow"
	// Seconds a token refresh may take
	RefreshTimeout = "refresh_timeout"
	// Whether the person's own sessions ask Codex for its security research access
	Daybreak = "daybreak"
)

// Ways of signing in
const (
	DeviceCode = "device_code"
	Browser    = "browser"
)

const RefreshTimeoutEnv = "ICEBATH_CODEX_REFRESH_TIMEOUT_SECONDS"
