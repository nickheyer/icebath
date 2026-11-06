package tools

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var (
	ConnectorActions = []string{"display", "status", "connect", "reconnect", "rename"}
	MCPActions       = []string{"install", "enable", "authorize"}
	AllActions       = append(append([]string{}, ConnectorActions...), MCPActions...)
)

var connTargetFields = map[string]bool{"name": true, "mcp": true, "alias": true, "to": true}

// AliasPattern is the account name shape a hosted connector alias must take
const AliasPattern = `^[a-z0-9][a-z0-9-]{0,31}$`

var connAliasRE = regexp.MustCompile(AliasPattern)

const connAliasFormat = "1-32 lowercase letters, digits or '-', starting with a letter or digit"

// HostedTarget is a hosted connector target, alias naming one account and to the new alias of a rename
type HostedTarget struct {
	Name  string
	Alias *string
	To    *string
}

func connBadAlias(key string, value *string) string {
	if value == nil || connAliasRE.MatchString(*value) {
		return ""
	}
	return fmt.Sprintf("'%s' %s is not a valid account name: use %s.", key, pyrepr.StrRepr(*value), connAliasFormat)
}

// Splits the connectors argument into hosted targets and MCP server names, or the reason it is malformed
func NormalizeTargets(raw any) ([]HostedTarget, []string, string) {
	if raw == nil {
		return nil, nil, ""
	}
	var items []any
	switch v := raw.(type) {
	case string, map[string]any:
		items = []any{v}
	case []any:
		items = v
	default:
		return nil, nil, "'connectors' must be a list of names or {name, mcp} objects."
	}
	managed := []HostedTarget{}
	mcp := []string{}
	for _, item := range items {
		var alias, to *string
		var name string
		isMCP := false
		if obj, ok := item.(map[string]any); ok {
			var unknown []string
			for k := range obj {
				if !connTargetFields[k] {
					unknown = append(unknown, k)
				}
			}
			sort.Strings(unknown)
			if len(unknown) > 0 {
				return nil, nil, fmt.Sprintf("unknown target field(s) %s: a target is "+
					"{\"name\": \"<slug>\"} or {\"name\": \"<server>\", \"mcp\": true}. Transport, "+
					"URLs and credentials come from the catalog manifest, never from the call.", strings.Join(unknown, ", "))
			}
			name = strings.ToLower(strings.TrimSpace(connStrOr(obj["name"])))
			isMCP = connTruthy(obj["mcp"])
			_, hasAlias := obj["alias"]
			_, hasTo := obj["to"]
			if isMCP && (hasAlias || hasTo) {
				return nil, nil, "'alias' and 'to' name hosted connector accounts; an MCP target takes neither."
			}
			if obj["alias"] != nil {
				s := strings.TrimSpace(pyrepr.Str(obj["alias"]))
				alias = &s
			}
			if obj["to"] != nil {
				s := strings.TrimSpace(pyrepr.Str(obj["to"]))
				to = &s
			}
		} else {
			name = strings.ToLower(strings.TrimSpace(connStrOr(item)))
		}
		if name == "" {
			return nil, nil, "every target needs a non-empty 'name'."
		}
		if isMCP {
			if !slices.Contains(mcp, name) {
				mcp = append(mcp, name)
			}
			continue
		}
		dup := false
		for _, t := range managed {
			if t.Name == name && sameOptional(t.Alias, alias) {
				dup = true
				break
			}
		}
		if !dup {
			managed = append(managed, HostedTarget{Name: name, Alias: alias, To: to})
		}
	}
	return managed, mcp, ""
}

// Python str(value or "") of a JSON value
func connStrOr(v any) string {
	if !connTruthy(v) {
		return ""
	}
	return pyrepr.Str(v)
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
func ValidateAction(action string, managed []HostedTarget, mcp []string) string {
	if !slices.Contains(AllActions, action) {
		return fmt.Sprintf("action must be one of %s. %s apply to local MCP servers "+
			"(targets {\"name\": ..., \"mcp\": true}); the rest apply to managed connectors. "+
			"Disconnecting an account is done by the user in the Nous Portal dashboard, not "+
			"through this tool.", strings.Join(AllActions, ", "), strings.Join(MCPActions, ", "))
	}
	if slices.Contains(MCPActions, action) {
		if len(managed) > 0 {
			names := make([]string, len(managed))
			for i, t := range managed {
				names[i] = t.Name
			}
			return fmt.Sprintf("'%s' is an MCP action: every target must carry \"mcp\": true "+
				"(got managed connector(s) %s). Managed connectors use connect / reconnect / status.", action, strings.Join(names, ", "))
		}
		if len(mcp) == 0 {
			return fmt.Sprintf("'%s' requires 'connectors': the MCP server name(s), e.g. "+
				"[{\"name\": \"linear\", \"mcp\": true}].", action)
		}
	} else if len(mcp) > 0 {
		return fmt.Sprintf("'%s' is a managed-connector action; MCP targets (%s) use %s.",
			action, strings.Join(mcp, ", "), strings.Join(MCPActions, ", "))
	}
	return connValidateAccounts(action, managed)
}

func connValidateAccounts(action string, managed []HostedTarget) string {
	anyTo, anyAlias := false, false
	for _, t := range managed {
		anyTo = anyTo || (t.To != nil && *t.To != "")
		anyAlias = anyAlias || (t.Alias != nil && *t.Alias != "")
	}
	if action != "rename" && anyTo {
		return "'to' is the new name of an account and applies to action rename only."
	}
	if action == "display" && anyAlias {
		return "display takes connector slugs to suggest (e.g. [\"gmail\"]); accounts are named on connect."
	}
	if action == "rename" {
		if len(managed) != 1 || managed[0].Alias == nil || *managed[0].Alias == "" || managed[0].To == nil || *managed[0].To == "" {
			return "rename takes exactly one target {\"name\": \"<slug>\", \"alias\": \"<current name>\", " +
				"\"to\": \"<new name>\"}. Use action status to list each connector's accounts."
		}
		return connBadAlias("to", managed[0].To)
	}
	if anyAlias && len(managed) > 1 {
		return "a call with an 'alias' takes that one target only; name other accounts in separate calls."
	}
	if action == "reconnect" {
		return ""
	}
	for _, t := range managed {
		if e := connBadAlias("alias", t.Alias); e != "" {
			return e
		}
	}
	return ""
}

// MCPCatalogNames lists the MCP catalog entry names, installed by pkg/mcp
var MCPCatalogNames func() ([]string, error)

// The MCP catalog names for the routing check, empty when the catalog cannot be read
func CatalogNames() map[string]bool {
	out := map[string]bool{}
	names, err := MCPCatalogNames()
	if err != nil {
		slog.Default().Debug(fmt.Sprintf("MCP catalog for the routing check failed: %s", err))
		return out
	}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Lower cased hosted connector slugs, false when connectors are unavailable or the list fails
func HostedNames(ctx context.Context, env *Env) (map[string]bool, bool) {
	if !ConnectorsAvailable(env) {
		return nil, false
	}
	items, err := NewConnectorClient(env, nil, "").ListConnectors(ctx, 0)
	if err != nil {
		slog.Default().Debug(fmt.Sprintf("connector list for the routing check failed: %s", err))
		return nil, false
	}
	out := map[string]bool{}
	for _, item := range items {
		if item == nil {
			continue
		}
		c, ok := item["connector"]
		if !ok {
			c = ""
		}
		out[strings.ToLower(pyrepr.Str(c))] = true
	}
	return out, true
}

func MisroutedToHostedError(name string) string {
	return fmt.Sprintf("%s is a local MCP server, not a hosted connector account. "+
		"Call manage_connections with action install and connectors "+
		"[{\"name\": \"%s\", \"mcp\": true}].", name, name)
}

func MisroutedToMCPError(action, name string) string {
	return fmt.Sprintf("%s is a hosted connector account, not a local MCP server, so '%s' "+
		"does not apply. Call manage_connections with action connect and connectors [\"%s\"].", name, action, name)
}

// The key an operation registers under, the gateway session key else the agent session id
func OperationSessionKey(env *Env, sessionID string) string {
	return operationSessionKey(env, sessionID)
}

// MCPOperationOptions carries the card callback, session and backend of one MCP manage_connections call
type MCPOperationOptions struct {
	Callback   ConnCallback
	SessionID  string
	ToolCallID string
	Backend    any
}

// MCPOperationRunner runs the MCP half of manage_connections, installed by pkg/mcp
var MCPOperationRunner func(ctx context.Context, env *Env, names []string, action string, opts MCPOperationOptions) string
