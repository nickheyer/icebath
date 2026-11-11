package dashboard

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Validated name, token free config and bearer token of an MCP create body, every error a ValueError
func normalizeMCPServerCreate(body *pyjson.Object) (string, map[string]any, *string, error) {
	name := pystr.Strip(optText(body.GetOr("name", nil)))
	if name == "" {
		return "", nil, nil, errors.New("Server name is required")
	}
	url := pystr.Strip(optText(body.GetOr("url", nil)))
	command := pystr.Strip(optText(body.GetOr("command", nil)))
	authRaw := optText(body.GetOr("auth", nil))
	if authRaw == "" {
		authRaw = "none"
	}
	auth := pystr.Lower(pystr.Strip(authRaw))
	var bearerToken *string
	if secret, ok := body.GetOr("bearer_token", nil).(SecretStr); ok {
		value := secret.Value
		bearerToken = &value
	}
	if (url != "") == (command != "") {
		return "", nil, nil, errors.New("Provide exactly one of URL (HTTP/SSE) or command (stdio)")
	}
	if auth != "none" && auth != "header" && auth != "oauth" {
		return "", nil, nil, fmt.Errorf("Unsupported auth mode: %s", auth)
	}
	args, _ := body.GetOr("args", nil).([]any)
	env, _ := body.GetOr("env", nil).(*pyjson.Object)
	serverConfig := map[string]any{}
	if url != "" {
		if len(args) > 0 {
			return "", nil, nil, errors.New("Arguments are only supported for stdio MCP servers")
		}
		if env != nil && env.Len() > 0 {
			return "", nil, nil, errors.New("Environment variables are only supported for stdio MCP servers")
		}
		if auth == "header" {
			normalized := ""
			if bearerToken != nil && *bearerToken != "" {
				normalized = mcp.StripBearerPrefix(*bearerToken)
			}
			if normalized == "" || pystr.Lower(normalized) == "bearer" {
				return "", nil, nil, errors.New("Bearer token is required")
			}
			serverConfig["headers"] = mcp.BearerAuthHeaders(name)
		} else if bearerToken != nil {
			return "", nil, nil, errors.New("Bearer token requires header authentication")
		}
		serverConfig["url"] = url
		if auth == "oauth" {
			serverConfig["auth"] = "oauth"
		}
	} else {
		if auth != "none" || bearerToken != nil {
			return "", nil, nil, errors.New("HTTP authentication is not supported for stdio MCP servers")
		}
		serverConfig["command"] = command
		if len(args) > 0 {
			serverConfig["args"] = append([]any(nil), args...)
		}
		if env != nil && env.Len() > 0 {
			copied := make(map[string]any, env.Len())
			for _, k := range env.Keys {
				copied[k] = env.Vals[k]
			}
			serverConfig["env"] = copied
		}
	}
	if issues := mcp.ValidateServerEntry(name, serverConfig); len(issues) > 0 {
		return "", nil, nil, fmt.Errorf("Server '%s' rejected: %s", name, strings.Join(issues, "; "))
	}
	return name, serverConfig, bearerToken, nil
}

// Text of an optional string field, empty for None
func optText(v any) string {
	s, _ := v.(string)
	return s
}

// Env values masked for read responses, empty text for a falsy value
func redactMCPEnv(env any) *pyjson.Object {
	out := pyjson.NewObject()
	for k, v := range pyItems(env) {
		if pyrepr.Truthy(v) {
			out.Set(pyrepr.Str(k), config.RedactKey(pyrepr.Str(v)))
		} else {
			out.Set(pyrepr.Str(k), "")
		}
	}
	return out
}

// Key value pairs of a mapping in its own order, sorted for a Go map, none for anything else
func pyItems(v any) func(yield func(any, any) bool) {
	return func(yield func(any, any) bool) {
		switch m := v.(type) {
		case *pyjson.Object:
			if m == nil {
				return
			}
			for _, k := range m.Keys {
				if !yield(k, m.Vals[k]) {
					return
				}
			}
		case map[string]any:
			for _, k := range sortedMapKeys(m) {
				if !yield(k, m[k]) {
					return
				}
			}
		}
	}
}

// Keys of a Go map in ascending order
func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Python list() of a config value, characters of a string and keys of a mapping
func pyListOf(v any) []any {
	switch t := v.(type) {
	case []any:
		return append([]any{}, t...)
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	case map[string]any, *pyjson.Object:
		var out []any
		for k := range pyItems(t) {
			out = append(out, k)
		}
		return out
	}
	return []any{}
}

// Dashboard row of one MCP server with its transport, redacted env, auth mode and source
func mcpServerSummary(name string, cfg map[string]any, plugin *string) *pyjson.Object {
	transport := "unknown"
	if pyrepr.Truthy(cfg["url"]) {
		transport = "http"
	} else if pyrepr.Truthy(cfg["command"]) {
		transport = "stdio"
	}
	auth := cfg["auth"]
	headers := cfg["headers"]
	if !pyrepr.Truthy(auth) {
		for k := range pyItems(headers) {
			if pystr.Lower(pyrepr.Str(k)) == "authorization" {
				auth = "header"
				break
			}
		}
	}
	args := cfg["args"]
	if !pyrepr.Truthy(args) {
		args = []any{}
	}
	source := "config"
	var pluginValue any
	if plugin != nil {
		source = "plugin"
		pluginValue = *plugin
	}
	return pyjson.ObjectOf(
		"name", name,
		"transport", transport,
		"url", cfg["url"],
		"command", cfg["command"],
		"args", pyListOf(args),
		"env", redactMCPEnv(cfg["env"]),
		"auth", auth,
		"enabled", tools.MCPServerEnabled(cfg),
		"tools", cfg["tools"],
		"source", source,
		"plugin", pluginValue,
	)
}
