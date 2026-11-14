// Credential store, credential pool and OAuth device and PKCE flows
package auth

import (
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

const (
	StoreVersion = 1
	LockTimeout  = 15 * time.Second

	DefaultPortalURL            = "https://portal.icebath.app"
	DefaultPortalInferenceURL   = "https://inference-api.icebath.app/v1"
	DefaultPortalWelcomeURL     = "https://welcome-api.icebath.app/v1"
	DefaultPortalClientID       = "icebath-cli"
	WelcomeClaimFlag            = "welcome_claim"
	PortalInferenceInvokeScope  = "inference:invoke"
	PortalBillingManageScope    = "billing:manage"
	DefaultPortalScope          = PortalInferenceInvokeScope
	PortalDeviceCodeSource      = "device_code"
	PortalAuthPathInvokeJWT     = "invoke_jwt"
	AccessTokenRefreshSkew      = 120 * time.Second
	PortalInvokeJWTMinTTL       = AccessTokenRefreshSkew
	DevicePollIntervalCap       = time.Second
	DeviceCodeGrantType         = "urn:ietf:params:oauth:grant-type:device_code"
	DefaultCodexBaseURL         = "https://chatgpt.com/backend-api/codex"
	DefaultXAIOAuthBaseURL      = "https://api.x.ai/v1"
	MiniMaxOAuthClientID        = "78257093-7e40-4613-99e0-527b14b39113"
	MiniMaxOAuthScope           = "group_id profile model.completion"
	MiniMaxOAuthGrantType       = "urn:ietf:params:oauth:grant-type:user_code"
	MiniMaxOAuthGlobalBase      = "https://api.minimax.io"
	MiniMaxOAuthCNBase          = "https://api.minimaxi.com"
	MiniMaxOAuthGlobalInference = "https://api.minimax.io/anthropic"
	MiniMaxOAuthCNInference     = "https://api.minimaxi.com/anthropic"
	MiniMaxOAuthRefreshSkew     = 60 * time.Second
	DefaultQwenBaseURL          = "https://portal.qwen.ai/v1"
	DefaultOllamaCloudBaseURL   = "https://ollama.com/v1"
	DefaultActualBaseURL        = "https://api.actual.inc/v1"
	DefaultActualLocalBaseURL   = "http://127.0.0.1:8080/v1"
	StepFunStepPlanIntlBaseURL  = "https://api.stepfun.ai/step_plan/v1"
	StepFunStepPlanCNBaseURL    = "https://api.stepfun.com/step_plan/v1"
	CodexOAuthClientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexOAuthTokenURL          = "https://auth.openai.com/oauth/token"
	CodexAccessTokenRefreshSkew = 120 * time.Second
	XAIOAuthIssuer              = "https://auth.x.ai"
	XAIOAuthDiscoveryURL        = XAIOAuthIssuer + "/.well-known/openid-configuration"
	XAIOAuthClientID            = "b1a00492-073a-47ea-816f-4c329264a828"
	XAIOAuthScope               = "openid profile email offline_access grok-cli:access api:access"
	XAIOAuthDeviceCodeURL       = XAIOAuthIssuer + "/oauth2/device/code"
	XAIAccessTokenRefreshSkew   = 3600 * time.Second
	QwenOAuthClientID           = "f0304373b74a44d2b584a3fb70ca9e56"
	QwenOAuthTokenURL           = "https://chat.qwen.ai/api/v1/oauth2/token"
	QwenAccessTokenRefreshSkew  = 120 * time.Second
	OpenRouterAuthURL           = "https://openrouter.ai/auth"
	OpenRouterAuthKeysURL       = "https://openrouter.ai/api/v1/auth/keys"
	OpenRouterOAuthDocsURL      = "https://openrouter.ai/docs/guides/overview/auth/oauth"
	OAuthOverSSHDocsURL         = "https://icebath.app/docs/guides/oauth-over-ssh"

	LMStudioNoAuthPlaceholder    = "dummy-lm-api-key"
	ActualLocalNoAuthPlaceholder = "dummy-actual-local-api-key"

	CodexRateLimitedCode = "codex_rate_limited"

	ProviderPortal    = "portal"
	ProviderCodex     = "openai-codex"
	ProviderXAIOAuth  = "xai-oauth"
	ProviderQwen      = "qwen-oauth"
	ProviderMiniMax   = "minimax-oauth"
	ProviderAnthropic = "anthropic"
	ProviderOpenRoute = "openrouter"
)

type Error struct {
	Message    string
	Provider   string
	Code       string
	Relogin    bool
	RetryAfter time.Duration
	Retryable  *bool
	Cause      error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Cause }

func providerError(provider, message, code string) *Error {
	return &Error{Message: message, Provider: provider, Code: code}
}

func reloginError(provider, message, code string) *Error {
	return &Error{Message: message, Provider: provider, Code: code, Relogin: true}
}

const redacted = "[redacted]"

type Secret struct{ v string }

func NewSecret(v string) Secret { return Secret{v: v} }

func (s Secret) Reveal() string { return s.v }

func (s Secret) Empty() bool { return s.v == "" }

func (s Secret) String() string {
	if s.v == "" {
		return ""
	}
	return redacted
}

func (s Secret) GoString() string { return s.String() }

func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func decodeJWTClaims(token string) *doc {
	if strings.Count(token, ".") != 2 {
		return nil
	}
	payload := strings.Split(token, ".")[1]
	payload = strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(payload, "="))
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	v, err := parseJSON(raw)
	if err != nil {
		return nil
	}
	claims, _ := v.(*doc)
	return claims
}

func LabelFromToken(token, fallback string) string {
	claims := decodeJWTClaims(token)
	for _, key := range []string{"email", "preferred_username", "upn"} {
		if v := strings.TrimSpace(claims.str(key)); v != "" {
			return v
		}
	}
	return fallback
}
