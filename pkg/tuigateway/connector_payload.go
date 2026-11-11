package tuigateway

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

var connectorSecretHeaders = map[string]bool{"authorization": true, "proxy-authorization": true, "cookie": true,
	"set-cookie": true, "credentials": true}

// Connector value with secret keyed entries replaced and every string redacted for a renderer
func ConnectorUIPayload(value any) any {
	entry := func(key string, item any) any {
		if _, isBool := item.(bool); !isBool && (tools.KeyHasSecretKeyword(key) || connectorSecretHeaders[strings.ToLower(key)]) {
			return "[REDACTED]"
		}
		if s, isStr := item.(string); isStr && key == "connect_url" {
			return s
		}
		return ConnectorUIPayload(item)
	}
	switch typed := value.(type) {
	case *pyjson.Object:
		if typed == nil {
			return value
		}
		out := pyjson.NewObject()
		for _, key := range typed.Keys {
			out.Set(key, entry(key, typed.Vals[key]))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = entry(key, item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = ConnectorUIPayload(item)
		}
		return out
	case string:
		return tools.RedactSensitiveTextWith(nil, typed, tools.RedactOptions{Force: true, URLCredentials: true})
	}
	return value
}
