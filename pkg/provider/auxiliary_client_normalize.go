package provider

import (
	"reflect"
	"strings"
)

// Callable key passed through uncalled, strings stripped, anything else the empty key
func auxNormalizeAPIKey(raw any) any {
	if raw != nil && reflect.TypeOf(raw).Kind() == reflect.Func {
		return raw
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// Resolved model rewritten into the form the receiving provider expects, unchanged when the rewrite fails
func auxNormalizeResolvedModel(model, provider string) string {
	if model == "" {
		return model
	}
	normalized := model
	if err := auxRecoverRaise(func() { normalized = NormalizeModelForProvider(model, provider) }); err != nil {
		return model
	}
	return normalized
}
