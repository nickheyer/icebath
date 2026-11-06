package agent

import (
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	LayerProvider  = "provider"
	LayerEndpoint  = "endpoint"
	LayerStreaming = "streaming"
	LayerAuth      = "auth"
	LayerBilling   = "billing"
	LayerGateway   = "gateway"
	LayerDisk      = "disk"
)

var surfaceReasonToLayer = map[string]string{
	"auth": LayerAuth, "auth_permanent": LayerAuth, "billing": LayerBilling, "billing_unverified": LayerBilling,
	"loop_error": LayerGateway, "interpreter_shutdown": LayerGateway, "session_busy": LayerGateway,
	"gateway_connection": LayerGateway, "host_error": LayerGateway, "workspace_unavailable": LayerGateway,
	"launch_credentials_unavailable": LayerGateway,
	"local_runtime_unavailable":      LayerEndpoint,
	"truncated":                      LayerProvider, "empty_response": LayerProvider, "invalid_response": LayerProvider,
	"context_overflow": LayerProvider,
}

var (
	surfaceTransportReasons       = map[string]bool{"timeout": true, "ssl_cert_verification": true}
	surfaceFreeTierRetryableKinds = map[string]bool{"rate_limited": true, "at_capacity": true, "outage": true}
	surfaceNonRetryableReasons    = map[string]bool{
		"auth": true, "auth_permanent": true, "billing": true, "billing_unverified": true, "content_policy_blocked": true,
		"provider_policy_blocked": true, "data_policy_denied": true, "model_not_found": true, "format_error": true,
		"ssl_cert_verification": true, "context_overflow": true, "interpreter_shutdown": true, "upstream_blocked": true,
	}
	surfaceCustomEndpointProviders = map[string]bool{"custom": true, "local": true, "llama.cpp": true, "llamacpp": true,
		"ollama": true, "lmstudio": true, "vllm": true}
	surfaceStreamDropFragments = []string{"stream connection", "peer closed connection", "incomplete chunked read",
		"connection broken", "stream ended prematurely", "sse", "mid-stream"}
)

func errorSurfaceLog() *slog.Logger { return slog.Default().With("logger", "agent.error_surface") }

// True for user configured custom or local endpoint providers
func surfaceIsCustomEndpoint(providerName string) bool {
	p := strings.ToLower(pystr.Strip(providerName))
	return surfaceCustomEndpointProviders[p] || strings.HasPrefix(p, "custom:")
}

// True when the error text reads like an SSE stream dropped mid turn
func surfaceLooksLikeStreamDrop(message string) bool {
	lower := pystr.Lower(message)
	for _, f := range surfaceStreamDropFragments {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return false
}

// Wire descriptor with identity, display label and auth recovery hints for the client card
func errorSurface(layer, code string, retryable bool, providerName, model string) map[string]any {
	surface := map[string]any{"layer": layer, "code": code, "retryable": retryable}
	if providerName != "" {
		surface["provider"] = providerName
	}
	if model != "" {
		surface["model"] = model
	}
	if providerName != "" {
		surface["provider_label"] = provider.ProviderLabel(providerName)
	}
	if layer == LayerAuth && providerName != "" {
		kind := surfaceAuthKind(providerName)
		surface["auth_kind"] = kind
		if kind == "api_key" {
			if env := surfaceAPIKeyEnv(providerName); env != "" {
				surface["api_key_env"] = env
			}
		}
	}
	return surface
}

// Env var holding the provider's API key so the client can open that settings row
func surfaceAPIKeyEnv(providerName string) string {
	d, ok := provider.ProviderCatalogBySlug()[strings.ToLower(pystr.Strip(providerName))]
	if !ok || len(d.APIKeyEnvVars) == 0 {
		return ""
	}
	return d.APIKeyEnvVars[0]
}

// oauth for providers on the desktop Accounts tab, api_key for every other provider
func surfaceAuthKind(providerName string) string {
	if d, ok := provider.ProviderCatalogBySlug()[strings.ToLower(pystr.Strip(providerName))]; ok && d.Tab == "accounts" {
		return "oauth"
	}
	return "api_key"
}

// Layer of a classified failure reason, endpoint for transport faults on custom endpoints
func surfaceResultLayer(reason, errorText, providerName string) string {
	if layer, ok := surfaceReasonToLayer[reason]; ok {
		return layer
	}
	if surfaceTransportReasons[reason] && surfaceIsCustomEndpoint(providerName) {
		return LayerEndpoint
	}
	if surfaceLooksLikeStreamDrop(errorText) {
		return LayerStreaming
	}
	return LayerProvider
}

// Float of a real number value, false for bools and non numbers
func surfaceNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// Descriptor for a failed turn result, nil when the result carries no failure signal
func buildErrorSurfaceFromResult(result any, providerName, model string) (surface map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			errorSurfaceLog().Debug("error_surface: result classification failed", "error", r)
			surface = nil
		}
	}()
	res, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	errorText := surfaceStr(res["error"])
	reason := pystr.Strip(surfaceStr(res["failure_reason"]))
	if errorText == "" && reason == "" {
		return nil
	}
	if errorText != "" && state.IsDiskFullError(errorText) {
		return errorSurface(LayerDisk, "disk_full", false, providerName, model)
	}
	if pyrepr.Truthy(res["billing_block"]) || reason == "billing" || reason == "billing_unverified" {
		if window := surfaceStr(res["usage_window_message"]); window != "" {
			s := errorSurface(LayerBilling, "usage_window", true, providerName, model)
			s["message"] = window
			if resetsAt, ok := surfaceNumber(res["failure_resets_at"]); ok {
				s["resets_at"] = resetsAt
			}
			if limit, ok := res["usage_limit"].(map[string]any); ok {
				s["usage_limit"] = limit
			}
			return s
		}
		code := reason
		if code == "" {
			code = "billing"
		}
		return errorSurface(LayerBilling, code, false, providerName, model)
	}
	if freeTier, ok := res["free_tier"].(map[string]any); ok && pyrepr.Truthy(freeTier["kind"]) {
		kind := pyrepr.Str(freeTier["kind"])
		s := errorSurface(LayerProvider, "free_tier_"+kind, surfaceFreeTierRetryableKinds[kind], providerName, model)
		if message := surfaceStr(freeTier["message"]); message != "" {
			s["message"] = message
		}
		return s
	}
	if reason == "" {
		if surfaceLooksLikeStreamDrop(errorText) {
			return errorSurface(LayerStreaming, "stream_drop", true, providerName, model)
		}
		return errorSurface(LayerProvider, "unknown", true, providerName, model)
	}
	retryable, ok := res["failure_retryable"].(bool)
	if !ok {
		retryable = !surfaceNonRetryableReasons[reason]
	}
	s := errorSurface(surfaceResultLayer(reason, errorText, providerName), reason, retryable, providerName, model)
	if resetsAt, ok := surfaceNumber(res["failure_resets_at"]); ok {
		s["resets_at"] = resetsAt
	}
	return s
}

// Python str of a value or the empty string when the value is falsy
func surfaceStr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}
