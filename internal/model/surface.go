package model

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
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

var reasonLayers = map[string]string{
	"auth": LayerAuth, "auth_permanent": LayerAuth, "billing": LayerBilling, "billing_unverified": LayerBilling,
	"loop_error": LayerGateway, "interpreter_shutdown": LayerGateway, "session_busy": LayerGateway,
	"gateway_connection": LayerGateway, "host_error": LayerGateway, "workspace_unavailable": LayerGateway,
	"launch_credentials_unavailable": LayerGateway,
	"local_runtime_unavailable":      LayerEndpoint,
	"truncated":                      LayerProvider, "empty_response": LayerProvider, "invalid_response": LayerProvider,
	"context_overflow": LayerProvider,
}

var transportReasons = setOf("timeout", "ssl_cert_verification")

var freeTierRetryableKinds = setOf("rate_limited", "at_capacity", "outage")

var nonRetryableReasons = setOf(
	"auth", "auth_permanent", "billing", "billing_unverified", "content_policy_blocked",
	"provider_policy_blocked", "data_policy_denied", "model_not_found", "format_error", "ssl_cert_verification",
	"context_overflow", "interpreter_shutdown", "upstream_blocked",
)

var customEndpointProviders = setOf("custom", "local", "llama.cpp", "llamacpp", "ollama", "lmstudio", "vllm")

var streamDropFragments = []string{
	"stream connection", "peer closed connection", "incomplete chunked read",
	"connection broken", "stream ended prematurely", "sse", "mid-stream",
}

type Surface struct {
	Layer         string          `json:"layer"`
	Code          string          `json:"code"`
	Retryable     bool            `json:"retryable"`
	Provider      string          `json:"provider,omitempty"`
	Model         string          `json:"model,omitempty"`
	ProviderLabel string          `json:"provider_label,omitempty"`
	AuthKind      string          `json:"auth_kind,omitempty"`
	APIKeyEnv     string          `json:"api_key_env,omitempty"`
	Message       string          `json:"message,omitempty"`
	ResetsAt      *float64        `json:"resets_at,omitempty"`
	UsageLimit    json.RawMessage `json:"usage_limit,omitempty"`
}

type TurnFailure struct {
	Error              string
	Reason             string
	Retryable          *bool
	BillingBlock       bool
	UsageWindowMessage string
	ResetsAt           time.Time
	UsageLimit         json.RawMessage
	FreeTierKind       string
	FreeTierMessage    string
}

func customEndpoint(provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	return customEndpointProviders[p] || strings.HasPrefix(p, "custom:")
}

func looksLikeStreamDrop(message string) bool {
	return containsAny(strings.ToLower(message), streamDropFragments)
}

func (c *Classifier) AuthKind(ctx context.Context, provider string) string {
	if d, ok := c.o.Descriptor(ctx, strings.ToLower(strings.TrimSpace(provider))); ok && d.Tab == providers.TabAccounts {
		return "oauth"
	}
	return "api_key"
}

func (c *Classifier) surface(ctx context.Context, layer, code string, retryable bool, provider, model string) *Surface {
	s := &Surface{Layer: layer, Code: code, Retryable: retryable, Provider: provider, Model: model}
	if provider == "" {
		return s
	}
	s.ProviderLabel = c.o.Label(ctx, provider)
	if layer == LayerAuth {
		s.AuthKind = c.AuthKind(ctx, provider)
		if s.AuthKind == "api_key" {
			if d, ok := c.o.Descriptor(ctx, strings.ToLower(strings.TrimSpace(provider))); ok && len(d.APIKeyEnvVars) > 0 {
				s.APIKeyEnv = d.APIKeyEnvVars[0]
			}
		}
	}
	return s
}

func resultLayer(reason, errorText, provider string) string {
	if layer, ok := reasonLayers[reason]; ok {
		return layer
	}
	if transportReasons[reason] && customEndpoint(provider) {
		return LayerEndpoint
	}
	if looksLikeStreamDrop(errorText) {
		return LayerStreaming
	}
	return LayerProvider
}

func epochPointer(t time.Time) *float64 {
	if t.IsZero() {
		return nil
	}
	f := epochSeconds(t)
	return &f
}

func (c *Classifier) SurfaceFromResult(ctx context.Context, r TurnFailure, provider, model string) *Surface {
	reason := strings.TrimSpace(r.Reason)
	if r.Error == "" && reason == "" {
		return nil
	}
	if r.Error != "" && c.o.DiskFull(errors.New(r.Error)) {
		return c.surface(ctx, LayerDisk, "disk_full", false, provider, model)
	}
	if r.BillingBlock || reason == "billing" || reason == "billing_unverified" {
		if r.UsageWindowMessage != "" {
			s := c.surface(ctx, LayerBilling, "usage_window", true, provider, model)
			s.Message = r.UsageWindowMessage
			s.ResetsAt = epochPointer(r.ResetsAt)
			if len(r.UsageLimit) > 0 && strings.HasPrefix(strings.TrimSpace(string(r.UsageLimit)), "{") {
				s.UsageLimit = r.UsageLimit
			}
			return s
		}
		code := reason
		if code == "" {
			code = "billing"
		}
		return c.surface(ctx, LayerBilling, code, false, provider, model)
	}
	if r.FreeTierKind != "" {
		s := c.surface(ctx, LayerProvider, "free_tier_"+r.FreeTierKind, freeTierRetryableKinds[r.FreeTierKind], provider, model)
		s.Message = r.FreeTierMessage
		return s
	}
	if reason == "" {
		if looksLikeStreamDrop(r.Error) {
			return c.surface(ctx, LayerStreaming, "stream_drop", true, provider, model)
		}
		return c.surface(ctx, LayerProvider, "unknown", true, provider, model)
	}
	retryable := !nonRetryableReasons[reason]
	if r.Retryable != nil {
		retryable = *r.Retryable
	}
	s := c.surface(ctx, resultLayer(reason, r.Error, provider), reason, retryable, provider, model)
	s.ResetsAt = epochPointer(r.ResetsAt)
	return s
}

func apiLike(err error) bool {
	var ae *wire.APIError
	var se *wire.StreamError
	var classified *Error
	var lifetime *StreamLifetimeError
	if errors.As(err, &ae) || errors.As(err, &se) || errors.As(err, &classified) {
		return true
	}
	return !errors.As(err, &lifetime) && transportFailure(err)
}

func typeName(err error) string {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

func (c *Classifier) SurfaceFromError(ctx context.Context, err error, provider, model string, apiKey auth.Secret) *Surface {
	if err == nil {
		return nil
	}
	if c.o.DiskFull(err) {
		return c.surface(ctx, LayerDisk, "disk_full", false, provider, model)
	}
	if !apiLike(err) {
		return c.surface(ctx, LayerGateway, typeName(err), true, provider, model)
	}
	classified := c.Classify(ctx, err, Facts{Provider: provider, Model: model, APIKey: apiKey})
	message := classified.Message
	if message == "" {
		message = safeText(err)
		if message == "" {
			message = typeName(err)
		}
	}
	s := c.SurfaceFromResult(ctx, TurnFailure{
		Error: message, Reason: classified.Class.String(), ResetsAt: ContextOf(err).ResetAt,
	}, provider, model)
	if s != nil {
		s.Retryable = classified.Retryable
	}
	return s
}
