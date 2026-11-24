package tools

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// MCPActions are the manage_connections actions, each over MCP servers of the bundled catalog or the config
var MCPActions = []string{"install", "enable", "authorize"}

var connTargetFields = map[string]bool{"name": true}

// MCP server names of the connectors argument, or the reason it is malformed
func NormalizeTargets(raw any) ([]string, string) {
	if raw == nil {
		return nil, ""
	}
	var items []any
	switch v := raw.(type) {
	case string, map[string]any:
		items = []any{v}
	case []any:
		items = v
	default:
		return nil, "'connectors' must be a list of MCP server names or {name} objects."
	}
	names := []string{}
	for _, item := range items {
		var name string
		if obj, ok := item.(map[string]any); ok {
			var unknown []string
			for k := range obj {
				if !connTargetFields[k] {
					unknown = append(unknown, k)
				}
			}
			sort.Strings(unknown)
			if len(unknown) > 0 {
				return nil, fmt.Sprintf("unknown target field(s) %s: a target is a server name or "+
					"{\"name\": \"<server>\"}. Transport, URLs and credentials come from the catalog manifest, "+
					"never from the call.", strings.Join(unknown, ", "))
			}
			name = strings.ToLower(strings.TrimSpace(connStrOr(obj["name"])))
		} else {
			name = strings.ToLower(strings.TrimSpace(connStrOr(item)))
		}
		if name == "" {
			return nil, "every target needs a non-empty 'name'."
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, ""
}

// Python str(value or "") of a JSON value
func connStrOr(v any) string {
	if !connTruthy(v) {
		return ""
	}
	return anyutil.String(v)
}

// Python truthiness of a decoded JSON value
func connTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// The reason action does not fit these targets, empty when it does
func ValidateAction(action string, names []string) string {
	if !slices.Contains(MCPActions, action) {
		return fmt.Sprintf("action must be one of %s.", strings.Join(MCPActions, ", "))
	}
	if len(names) == 0 {
		return fmt.Sprintf("'%s' requires 'connectors': the MCP server name(s), e.g. [\"linear\"].", action)
	}
	return ""
}

// The key an operation registers under, the gateway session key else the agent session id
func OperationSessionKey(env *Env, sessionID string) string {
	if key := sessionValue(env, "key", "ICEBATH_SESSION_KEY"); key != "" {
		return key
	}
	return sessionID
}

// MCPOperationOptions carries the card callback, session and backend of one MCP manage_connections call
type MCPOperationOptions struct {
	Callback   ConnCallback
	SessionID  string
	ToolCallID string
	Backend    any
}

// MCPOperationRunner runs manage_connections over MCP servers, installed by pkg/mcp
var MCPOperationRunner func(ctx context.Context, env *Env, names []string, action string, opts MCPOperationOptions) string
