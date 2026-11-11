package gateway

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Palette catalog and slash completion builders of pkg/tuiserver, assigned in cmd/icebath/wiring.go
var (
	CommandCatalog   func(ctx context.Context, scope func(context.Context) (context.Context, error)) (map[string]any, error)
	SlashCompletions func(ctx context.Context, text string, scope func(context.Context) (context.Context, error)) (map[string]any, error)
)

// Command catalog or slash completions for this profile, skills read in the session workspace when one is named
func discoverCommands(ctx context.Context, a *SessionAuthority, actor Principal, params *pyjson.Object, completion bool) (any, error) {
	if !actor.Capabilities["session:read"] || actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("permission_denied")
	}
	allowed := []string{"profile", "session_id"}
	if completion {
		allowed = append(allowed, "text")
	}
	if paramsOutside(params, allowed...) {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, key := range params.Keys {
		if _, isStr := params.GetOr(key, nil).(string); !isStr {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	var scope func(context.Context) (context.Context, error)
	if sessionID, _ := params.GetOr("session_id", "").(string); sessionID != "" {
		ref := SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}
		if err := a.Authorize(ctx, actor, ref, "session:read"); err != nil {
			return nil, err
		}
		live, err := liveSession(a, ref.SessionID)
		if err != nil {
			return nil, err
		}
		scope = func(ctx context.Context) (context.Context, error) {
			return sessionWorkspace(ctx, a.Runner, live.Source)
		}
	}
	if profile, _ := params.GetOr("profile", "").(string); profile != "" && !profiles.ProfileMatchesHome(ctx, profile, a.ProfileID) {
		return nil, runtimeRefusal("profile_mismatch")
	}
	scoped := profileRuntimeScope(ctx, a.ProfileID, true)
	if completion {
		text, _ := params.GetOr("text", "").(string)
		return SlashCompletions(scoped, text, scope)
	}
	return CommandCatalog(scoped, scope)
}

// Plugins this profile's manager loaded with their counts and errors, plugin toolsets and portable MCP servers
func listPlugins(ctx context.Context, a *SessionAuthority, actor Principal, params *pyjson.Object) (any, error) {
	if !actor.Capabilities["session:read"] || actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("permission_denied")
	}
	if params.Len() > 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	scoped := profileRuntimeScope(ctx, a.ProfileID, true)
	plugins.DiscoverPlugins(scoped, false)
	manager := plugins.GetPluginManager(scoped)
	loaded, toolsets := manager.ListPlugins(), plugins.GetPluginToolsets(scoped)
	portable := make([]string, 0)
	for name := range manager.GetPortableMCPServers() {
		portable = append(portable, name)
	}
	sort.Strings(portable)
	rows := make([]any, 0, len(loaded))
	for _, p := range loaded {
		rows = append(rows, map[string]any{"name": pluginStr(p["name"]), "key": pluginStr(p["key"]),
			"version": pluginStr(p["version"]), "enabled": p["enabled"] == true, "tools": p["tools"],
			"hooks": p["hooks"], "commands": p["commands"], "error": pluginStr(p["error"])})
	}
	sets := make([]any, 0, len(toolsets))
	for _, ts := range toolsets {
		sets = append(sets, map[string]any{"key": ts.Key, "label": ts.Label, "description": ts.Description})
	}
	names := make([]any, len(portable))
	for i, name := range portable {
		names[i] = name
	}
	return map[string]any{"plugins": rows, "toolsets": sets, "portable_mcp_servers": names}, nil
}

// Python str of a plugin row field, empty for a missing or empty value
func pluginStr(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

// Commands catalog verb of this profile
func (c *AuthorityConnection) commandCatalog(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return discoverCommands(ctx, c.authority, c.actor, params, false)
}

// Slash completion verb of this profile
func (c *AuthorityConnection) slashCompletions(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return discoverCommands(ctx, c.authority, c.actor, params, true)
}

// Plugins list verb of this profile
func (c *AuthorityConnection) pluginsList(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return listPlugins(ctx, c.authority, c.actor, params)
}
