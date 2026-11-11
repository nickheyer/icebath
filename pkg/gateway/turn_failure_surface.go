package gateway

import (
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Process hook classifying a failed turn result into its client error descriptor, bound by the binary
var ErrorSurfaceFromResult func(result any, providerName, model string) map[string]any

// Key order the error descriptor is built in
var errorSurfaceKeyOrder = []string{"layer", "code", "retryable", "provider", "model", "provider_label", "auth_kind",
	"api_key_env", "message", "resets_at", "usage_limit"}

// Stamps the UI failure verdict fields on a terminal result in place
func stampTurnFailure(result *pyjson.Object, reason string, retryable bool) {
	result.Set("failure_reason", reason)
	result.Set("failure_retryable", retryable)
}

// Client error descriptor of a failed turn result, nil when it carries no failure signal
func buildErrorSurfaceFromResult(result *pyjson.Object, providerName, model string) any {
	surface := ErrorSurfaceFromResult(pyjson.Plain(result), providerName, model)
	if surface == nil {
		return nil
	}
	descriptor := pyjson.NewObject()
	for _, key := range errorSurfaceKeyOrder {
		if value, ok := surface[key]; ok {
			descriptor.Set(key, value)
		}
	}
	rest := make([]string, 0, len(surface))
	for key := range surface {
		if _, placed := descriptor.Get(key); !placed {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		descriptor.Set(key, surface[key])
	}
	return descriptor
}
