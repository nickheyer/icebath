package provider

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const (
	AuthStoreVersion             = 1
	AuthLockTimeout              = 15 * time.Second
	DefaultNousPortalURL         = "https://portal.nousresearch.com"
	DefaultNousInferenceURL      = "https://inference-api.nousresearch.com/v1"
	DefaultNousWelcomeURL        = "https://welcome-api.nousresearch.com/v1"
	DefaultNousClientID          = "hermes-cli"
	WelcomeClaimFlag             = "welcome_claim"
	NousInferenceInvokeScope     = "inference:invoke"
	NousBillingManageScope       = "billing:manage"
	DefaultNousScope             = NousInferenceInvokeScope
	NousDeviceCodeSource         = "device_code"
	NousAuthPathInvokeJWT        = "invoke_jwt"
	AccessTokenRefreshSkew       = 120 * time.Second
	NousInvokeJWTMinTTL          = AccessTokenRefreshSkew
	DeviceAuthPollIntervalCap    = 1 * time.Second
	DeviceCodeGrantType          = "urn:ietf:params:oauth:grant-type:device_code"
	DefaultCodexBaseURL          = "https://chatgpt.com/backend-api/codex"
	DefaultXAIOAuthBaseURL       = "https://api.x.ai/v1"
	MinimaxOAuthClientID         = "78257093-7e40-4613-99e0-527b14b39113"
	MinimaxOAuthScope            = "group_id profile model.completion"
	MinimaxOAuthGrantType        = "urn:ietf:params:oauth:grant-type:user_code"
	MinimaxOAuthGlobalBase       = "https://api.minimax.io"
	MinimaxOAuthCNBase           = "https://api.minimaxi.com"
	MinimaxOAuthGlobalInference  = "https://api.minimax.io/anthropic"
	MinimaxOAuthCNInference      = "https://api.minimaxi.com/anthropic"
	MinimaxOAuthRefreshSkew      = 60 * time.Second
	DefaultQwenBaseURL           = "https://portal.qwen.ai/v1"
	DefaultOllamaCloudBaseURL    = "https://ollama.com/v1"
	DefaultActualBaseURL         = "https://api.actual.inc/v1"
	DefaultActualLocalBaseURL    = "http://127.0.0.1:8080/v1"
	StepfunStepPlanIntlBaseURL   = "https://api.stepfun.ai/step_plan/v1"
	StepfunStepPlanCNBaseURL     = "https://api.stepfun.com/step_plan/v1"
	CodexOAuthClientID           = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexOAuthTokenURL           = "https://auth.openai.com/oauth/token"
	CodexAccessTokenRefreshSkew  = 120 * time.Second
	XAIOAuthIssuer               = "https://auth.x.ai"
	XAIOAuthDiscoveryURL         = XAIOAuthIssuer + "/.well-known/openid-configuration"
	XAIOAuthClientID             = "b1a00492-073a-47ea-816f-4c329264a828"
	XAIOAuthScope                = "openid profile email offline_access grok-cli:access api:access"
	XAIOAuthDeviceCodeURL        = XAIOAuthIssuer + "/oauth2/device/code"
	XAIAccessTokenRefreshSkew    = 3600 * time.Second
	QwenOAuthClientID            = "f0304373b74a44d2b584a3fb70ca9e56"
	QwenOAuthTokenURL            = "https://chat.qwen.ai/api/v1/oauth2/token"
	QwenAccessTokenRefreshSkew   = 120 * time.Second
	OpenRouterAuthURL            = "https://openrouter.ai/auth"
	OpenRouterAuthKeysURL        = "https://openrouter.ai/api/v1/auth/keys"
	OpenRouterOAuthDocsURL       = "https://openrouter.ai/docs/guides/overview/auth/oauth"
	OAuthOverSSHDocsURL          = "https://hermes-agent.nousresearch.com/docs/guides/oauth-over-ssh"
	LMStudioNoAuthPlaceholder    = "dummy-lm-api-key"
	ActualLocalNoAuthPlaceholder = "dummy-actual-local-api-key"
	CodexRateLimitedCode         = "codex_rate_limited"
)

var formJSONHeaders = map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"}

// AuthError is a structured credential failure carrying relogin and retry hints
type AuthError struct {
	Message         string
	Provider        string
	Code            string
	ReloginRequired bool
	RetryAfter      time.Duration
	HasRetryAfter   bool
	Retryable       *bool
	Err             error
}

func (e *AuthError) Error() string { return e.Message }

func (e *AuthError) Unwrap() error { return e.Err }

func newAuthErr(provider, message, code string, relogin bool) *AuthError {
	return &AuthError{Message: message, Provider: provider, Code: code, ReloginRequired: relogin}
}

func nousErr(message, code string, relogin bool) *AuthError {
	return newAuthErr("nous", message, code, relogin)
}
func xaiErr(message, code string, relogin bool) *AuthError {
	return newAuthErr("xai-oauth", message, code, relogin)
}
func codexErr(message, code string, relogin bool) *AuthError {
	return newAuthErr("openai-codex", message, code, relogin)
}
func qwenErr(message, code string, relogin bool) *AuthError {
	return newAuthErr("qwen-oauth", message, code, relogin)
}
func minimaxErr(message, code string, relogin bool) *AuthError {
	return newAuthErr("minimax-oauth", message, code, relogin)
}
func openrouterErr(message, code string, relogin bool) *AuthError {
	return newAuthErr("openrouter", message, code, relogin)
}

// DecodeJWTClaims returns the unverified payload object of a three part JWT or an empty map
func DecodeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	payload := parts[1]
	if pad := (4 - len(payload)%4) % 4; pad > 0 {
		payload += strings.Repeat("=", pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}
