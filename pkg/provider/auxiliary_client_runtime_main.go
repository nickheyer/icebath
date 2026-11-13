package provider

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

type auxRuntimeMainKey struct{}

// Live main runtime bound to the context, nil outside a binding
func auxRuntimeMain(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}
	runtime, _ := ctx.Value(auxRuntimeMainKey{}).(map[string]any)
	return runtime
}

// One field of the bound main runtime, empty when unbound or unset
func auxRuntimeMainValue(ctx context.Context, field string) any {
	runtime := auxRuntimeMain(ctx)
	if runtime == nil {
		return ""
	}
	if value := runtime[field]; pyrepr.Truthy(value) {
		return value
	}
	return ""
}

// Optional fields of a main runtime binding
type RuntimeMainOptions struct {
	RequestedProvider string
	BaseURL           string
	APIKey            any
	APIMode           string
	AuthMode          string
	SessionID         string
	CacheScope        string
}

// Binds the live main runtime for auxiliary routing under the returned context
func SetRuntimeMain(ctx context.Context, provider, model string, opts RuntimeMainOptions) context.Context {
	runtime := map[string]any{
		"provider":           strings.ToLower(pyStripSpace(provider)),
		"requested_provider": strings.ToLower(pyStripSpace(opts.RequestedProvider)),
		"model":              pyStripSpace(model),
		"base_url":           pyStripSpace(opts.BaseURL),
		"api_key":            auxNormalizeAPIKey(opts.APIKey),
		"api_mode":           pyStripSpace(opts.APIMode),
		"auth_mode":          strings.ToLower(pyStripSpace(opts.AuthMode)),
		"session_id":         pyStripSpace(opts.SessionID),
		"cache_scope":        pyStripSpace(opts.CacheScope),
	}
	return context.WithValue(ctx, auxRuntimeMainKey{}, runtime)
}

// Binds a sanitized copy of an explicit runtime, an empty one clearing the binding
func ScopedRuntimeMain(ctx context.Context, mainRuntime map[string]any) (context.Context, map[string]any) {
	runtime := auxNormalizeMainRuntime(ctx, mainRuntime)
	if len(runtime) == 0 {
		return context.WithValue(ctx, auxRuntimeMainKey{}, map[string]any(nil)), runtime
	}
	return context.WithValue(ctx, auxRuntimeMainKey{}, runtime), runtime
}

// Drops the runtime binding under the returned context
func ClearRuntimeMain(ctx context.Context) context.Context {
	return context.WithValue(ctx, auxRuntimeMainKey{}, map[string]any(nil))
}

// Main runtime fields and the context only fields a binding carries
var (
	auxMainRuntimeFields        = []string{"provider", "model", "base_url", "api_key", "api_mode", "auth_mode"}
	auxMainRuntimeContextFields = append(append([]string{}, auxMainRuntimeFields...), "requested_provider", "session_id", "cache_scope")
)

// Sanitized copy of a main runtime or the bound one, callable keys kept and identities lowercased
func auxNormalizeMainRuntime(ctx context.Context, mainRuntime map[string]any) map[string]any {
	if mainRuntime == nil {
		mainRuntime = auxRuntimeMain(ctx)
	}
	normalized := map[string]any{}
	if mainRuntime == nil {
		return normalized
	}
	for _, field := range auxMainRuntimeContextFields {
		value := mainRuntime[field]
		if _, callable := auxCallableIdentity(value); callable {
			if field == "api_key" {
				normalized[field] = value
			}
			continue
		}
		switch v := value.(type) {
		case string:
			if s := pyStripSpace(v); s != "" {
				normalized[field] = s
			}
		}
	}
	for _, identity := range []string{"provider", "requested_provider"} {
		if s, ok := normalized[identity].(string); ok {
			normalized[identity] = strings.ToLower(s)
		}
	}
	return normalized
}
