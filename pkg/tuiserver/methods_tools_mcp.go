package tuiserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var toolsMCPRegistry = &handlerRegistry{}

func init() {
	scopedRPC(toolsMCPRegistry, "mcp.catalog", 5024, rpcShape{}, mcpCatalog)
	mcpRPC(toolsMCPRegistry, "list", nil, mcpServersList)
	mcpRPC(toolsMCPRegistry, "status", nil, mcpServersStatus)
	mcpRPC(toolsMCPRegistry, "add", nameParam, mcpServersAdd)
	mcpRPC(toolsMCPRegistry, "set_api_key", []requiredParam{{"name", strippedPresent}, {"value", nonemptyPresent}}, mcpServersSetAPIKey)
	mcpRPC(toolsMCPRegistry, "test", nameParam, mcpServersTest)
	mcpRPC(toolsMCPRegistry, "remove", nameParam, mcpServersRemove)
	mcpRPC(toolsMCPRegistry, "oauth.start", nameParam, mcpOAuthStart)
	mcpRPC(toolsMCPRegistry, "oauth.poll", nameSessionParams, mcpOAuthPoll)
	mcpRPC(toolsMCPRegistry, "oauth.cancel", nameSessionParams, mcpOAuthCancel)
	mcpRPC(toolsMCPRegistry, "oauth.callback", nameSessionParams, mcpOAuthCallback)
	toolsMCPRegistry.install()
}

// Configured servers plus the safe plugin portable ones of the bound profile, each mapped to its owning plugin or nil
func mcpServerRows(ctx context.Context) (map[string]map[string]any, map[string]*string) {
	configServers, err := mcp.GetMCPServers(ctx)
	panicIf(err)
	return mcp.ServerConfigsWithSources(ctx, configServers)
}

// Name and rows of a configured server, the 4064 error when the name is not configured
func mcpNamedServer(ctx context.Context, rid any, params map[string]any) (string, map[string]map[string]any, map[string]any) {
	name := strArg(params, "name")
	servers, _ := mcpServerRows(ctx)
	if _, found := servers[name]; !found {
		return name, servers, rpcErr(rid, 4064, fmt.Sprintf("server '%s' not found", name), nil)
	}
	return name, servers, nil
}

// The 4090 refusal for a server a plugin provides, nil for a config server
func mcpPluginWriteError(rid any, name string, owners map[string]*string) map[string]any {
	if plugin := owners[name]; plugin != nil {
		return rpcErr(rid, 4090, fmt.Sprintf("server '%s' is provided by plugin '%s' and cannot be modified", name, *plugin), nil)
	}
	return nil
}

// Name and rows of a configured server the config owns, else the not found or plugin owned error
func mcpConfigServerOrError(ctx context.Context, rid any, params map[string]any) (string, map[string]map[string]any, map[string]any) {
	name, servers, errResp := mcpNamedServer(ctx, rid, params)
	if errResp != nil {
		return name, servers, errResp
	}
	_, owners := mcpServerRows(ctx)
	return name, servers, mcpPluginWriteError(rid, name, owners)
}

// Items a Python list call yields for a config value, characters of a string and keys of a dict
func pyListItems(v any) []any {
	switch x := v.(type) {
	case nil:
		return []any{}
	case []any:
		return append([]any{}, x...)
	case string:
		out := []any{}
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	case map[string]any:
		out := []any{}
		for k := range x {
			out = append(out, k)
		}
		return out
	}
	panic(fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v)))
}

// Display summary of one server with its transport, env key names, auth and source
func summarizeMCPServer(ctx context.Context, name string, cfg map[string]any, plugin *string) map[string]any {
	if cfg == nil {
		cfg = map[string]any{}
	}
	transport := "unknown"
	if truthy(cfg["url"]) {
		transport = "http"
	} else if truthy(cfg["command"]) {
		transport = "stdio"
	}
	auth := cfg["auth"]
	if headers, isDict := cfg["headers"].(map[string]any); !truthy(auth) && isDict {
		for key := range headers {
			if strings.ToLower(key) == "authorization" {
				auth = "header"
				break
			}
		}
	}
	args := []any{}
	if truthy(cfg["args"]) {
		args = pyListItems(cfg["args"])
	}
	envKeys := []string{}
	if truthy(cfg["env"]) {
		for _, k := range pyListItems(cfg["env"]) {
			envKeys = append(envKeys, pyrepr.Str(k))
		}
	}
	sort.Strings(envKeys)
	var tokens any
	if auth == "oauth" {
		tokens = mcp.OAuthTokensPresent(ctx, name)
	}
	source := "config"
	var pluginName any
	if plugin != nil {
		source, pluginName = "plugin", *plugin
	}
	return map[string]any{"name": name, "transport": transport, "url": cfg["url"], "command": cfg["command"], "args": args,
		"env": envKeys, "auth": auth, "oauth_tokens_present": tokens, "enabled": tools.MCPServerEnabled(cfg),
		"tools": cfg["tools"], "source": source, "plugin": pluginName}
}

// Counts an mcp.servers.add as an MCP extension install, failed when the save rejected the config
func recordMCPAdd(ctx context.Context, entry *mcp.CatalogEntry, serverConfig map[string]any, saved bool) {
	source := "local"
	if entry != nil {
		source = "catalog"
	} else if truthy(serverConfig["url"]) {
		source = "url"
	}
	name := ""
	if entry != nil {
		name = entry.Name
	}
	outcome, failureClass := "success", ""
	if !saved {
		outcome, failureClass = "failed", "config_rejected"
	}
	mcp.RecordMCPInstall(ctx, source, name, outcome, failureClass, nil)
}

// Python dataclass repr of a catalog transport spec
func transportSpecRepr(t mcp.TransportSpec) string {
	args := make([]any, len(t.Args))
	for i, a := range t.Args {
		args[i] = a
	}
	keys := make([]string, 0, len(t.Env))
	for k := range t.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := pyjson.NewObject()
	for _, k := range keys {
		env.Set(k, t.Env[k])
	}
	return fmt.Sprintf("TransportSpec(type=%s, command=%s, args=%s, url=%s, version=%s, env=%s)", pyrepr.Repr(t.Type),
		pyrepr.Repr(t.Command), pyrepr.Repr(args), pyrepr.Repr(t.URL), pyrepr.Repr(t.Version), pyrepr.Repr(env))
}

// Catalog servers of the profile with their install and enable state
func mcpCatalog(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	entries, err := mcp.ListCatalog()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, entry := range entries {
		installed, err := mcp.IsInstalled(ctx, entry.Name)
		if err != nil {
			return nil, err
		}
		enabled, err := mcp.IsEnabled(ctx, entry.Name)
		if err != nil {
			return nil, err
		}
		var slug any
		if entry.ConnectorSlug != "" {
			slug = entry.ConnectorSlug
		}
		out = append(out, map[string]any{"name": entry.Name, "description": entry.Description, "connector_slug": slug,
			"installed": installed, "enabled": enabled, "requires": []string{}, "transport": transportSpecRepr(entry.Transport)})
	}
	return ok(rid, map[string]any{"servers": out}), nil
}

// Summaries of every configured and plugin server sorted by name
func mcpServersList(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	servers, owners := mcpServerRows(ctx)
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := []map[string]any{}
	for _, name := range names {
		rows = append(rows, summarizeMCPServer(ctx, name, servers[name], owners[name]))
	}
	return ok(rid, map[string]any{"servers": rows}), nil
}

// Cached runtime state of each server, never connecting, the runtime view only for the served profile
func mcpServersStatus(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	configured, owners := mcpServerRows(ctx)
	includeRuntime := provider.IsMultiplexActive(ctx) || home.Key(home.Dir(ctx)) == home.Key(home.Process())
	rendered := make(map[string]*mcp.Rendered, len(configured))
	for name, cfg := range configured {
		rendered[name] = &mcp.Rendered{Config: cfg}
	}
	rows := []map[string]any{}
	for _, e := range mcp.Status(ctx, configEnv(ctx), rendered, includeRuntime) {
		source := "config"
		var plugin any
		if owner := owners[e.Name]; owner != nil {
			source, plugin = "plugin", *owner
		}
		rows = append(rows, map[string]any{"name": e.Name, "transport": e.Transport, "tools": e.Tools, "connected": e.Connected,
			"disabled": e.Disabled, "status": e.Status, "source": source, "plugin": plugin})
	}
	return ok(rid, map[string]any{"servers": rows, "checked_at": time.Now().UnixMilli()}), nil
}

// Adds a server from a catalog id, preset or explicit config, saving its bearer token to the profile env
func mcpServersAdd(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	name, preset := strArg(params, "name"), strArg(params, "preset")
	servers, owners := mcpServerRows(ctx)
	if refusal := mcpPluginWriteError(rid, name, owners); refusal != nil {
		return refusal, nil
	}
	if _, exists := servers[name]; exists {
		return rpcErr(rid, 4090, fmt.Sprintf("server '%s' already exists", name), nil), nil
	}
	serverConfig := map[string]any{}
	if raw, isDict := params["config"].(map[string]any); isDict {
		for k, v := range raw {
			serverConfig[k] = v
		}
	}
	var entry *mcp.CatalogEntry
	if preset != "" && !(truthy(serverConfig["url"]) || truthy(serverConfig["command"])) {
		found, err := mcp.GetEntry(preset)
		if err != nil {
			return nil, err
		}
		entry = found
		if entry != nil {
			built, err := mcp.BuildServerConfig(entry, "")
			if err != nil {
				return nil, err
			}
			for k, v := range built {
				if _, set := serverConfig[k]; !set {
					serverConfig[k] = v
				}
			}
		} else {
			var cmdArgs []string
			for _, a := range pyListItems(serverConfig["args"]) {
				cmdArgs = append(cmdArgs, pyrepr.Str(a))
			}
			if err := mcp.ApplyMCPPreset(preset, strOr(serverConfig["url"]), strOr(serverConfig["command"]), cmdArgs, serverConfig); err != nil {
				return rpcErr(rid, 4063, "Unknown MCP catalog entry or preset: "+preset, nil), nil
			}
		}
	}
	if !truthy(serverConfig["url"]) && !truthy(serverConfig["command"]) {
		return rpcErr(rid, 4063, "config must specify a 'url' (http) or 'command' (stdio), or a valid 'preset'", nil), nil
	}
	if token := params["bearer_token"]; truthy(token) {
		headers, err := mcp.SaveBearerAuthToken(ctx, name, pyrepr.Str(token))
		if err != nil {
			return nil, err
		}
		serverConfig["headers"] = headers
	}
	savedOK, err := mcp.SaveServer(ctx, mcp.HeadlessUI(), name, serverConfig)
	if err != nil {
		return nil, err
	}
	recordMCPAdd(ctx, entry, serverConfig, savedOK)
	if !savedOK {
		return rpcErr(rid, 4001, fmt.Sprintf("server '%s' rejected: suspicious command/args configuration", name), nil), nil
	}
	current, err := mcp.GetMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	saved := serverConfig
	if raw, found := current[name]; found {
		saved, _ = raw.(map[string]any)
	}
	return ok(rid, map[string]any{"ok": true, "name": name, "server": summarizeMCPServer(ctx, name, saved, nil)}), nil
}

// Saves a server credential to the profile env and points the config at it by reference
func mcpServersSetAPIKey(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	name, servers, errResp := mcpConfigServerOrError(ctx, rid, params)
	if errResp != nil {
		return errResp, nil
	}
	value := params["value"]
	envVar := strArg(params, "env_var")
	if envVar == "" {
		envVar = mcp.EnvKeyForServer(name)
	}
	entry := servers[name]
	if entry == nil {
		return rpcErr(rid, 4001, "malformed server config", nil), nil
	}
	h := home.Dir(ctx)
	if truthy(entry["url"]) {
		normalized := mcp.StripBearerPrefix(pyrepr.Str(value))
		if normalized == "" || strings.ToLower(normalized) == "bearer" {
			return rpcErr(rid, 4063, "value is not a valid credential", nil), nil
		}
		if err := config.SaveEnvValueContext(ctx, h, envVar, normalized); err != nil {
			return nil, err
		}
		if envVar == mcp.EnvKeyForServer(name) {
			entry["headers"] = mcp.BearerAuthHeaders(name)
		} else {
			entry["headers"] = map[string]any{"Authorization": "Bearer ${" + envVar + "}"}
		}
	} else {
		if err := config.SaveEnvValueContext(ctx, h, envVar, pyrepr.Str(value)); err != nil {
			return nil, err
		}
		envBlock, isDict := entry["env"].(map[string]any)
		if !isDict {
			envBlock = map[string]any{}
		}
		envBlock[envVar] = "${" + envVar + "}"
		entry["env"] = envBlock
	}
	cfg, err := loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	block, isDict := cfg["mcp_servers"].(map[string]any)
	if _, present := cfg["mcp_servers"]; !present {
		block, isDict = map[string]any{}, true
		cfg["mcp_servers"] = block
	}
	if !isDict {
		return nil, fmt.Errorf("'%s' object does not support item assignment", pyrepr.TypeName(cfg["mcp_servers"]))
	}
	block[name] = entry
	if err := config.Save(h, cfg); err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"ok": true, "name": name, "env_var": envVar, "server": summarizeMCPServer(ctx, name, entry, nil)}), nil
}

// Connects to one server, lists its tools and disconnects, an oauth server also needing a stored token
func mcpServersTest(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	name, servers, errResp := mcpNamedServer(ctx, rid, params)
	if errResp != nil {
		return errResp, nil
	}
	cfg := servers[name]
	needsOAuthToken := cfg["auth"] == "oauth"
	failure := func(errText string, oauthNeeded bool, tokensPresent any) map[string]any {
		return ok(rid, map[string]any{"ok": false, "error": errText, "tools": []any{}, "oauth_needed": oauthNeeded,
			"oauth_tokens_present": tokensPresent})
	}
	details := &mcp.ProbeDetails{}
	summaries, err := mcp.ProbeSingleServerDetails(ctx, configEnv(ctx), name, cfg, details)
	if err != nil {
		var present any
		if needsOAuthToken {
			present = mcp.OAuthTokensPresent(ctx, name)
		}
		return failure(errorMessage(err), needsOAuthToken, present), nil
	}
	if needsOAuthToken && !mcp.OAuthTokensPresent(ctx, name) {
		return failure("OAuth authentication required — no token found.", true, false), nil
	}
	toolRows := []map[string]any{}
	for _, t := range summaries {
		toolRows = append(toolRows, map[string]any{"name": t.Name, "description": t.Description})
	}
	prompts, resources := 0, 0
	if details.Prompts != nil {
		prompts = *details.Prompts
	}
	if details.Resources != nil {
		resources = *details.Resources
	}
	var tokens any
	if needsOAuthToken {
		tokens = true
	}
	return ok(rid, map[string]any{"ok": true, "tools": toolRows, "prompts": prompts, "resources": resources,
		"oauth_needed": needsOAuthToken, "oauth_tokens_present": tokens}), nil
}

// Removes a config server from the profile config
func mcpServersRemove(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	name := strArg(params, "name")
	_, owners := mcpServerRows(ctx)
	if refusal := mcpPluginWriteError(rid, name, owners); refusal != nil {
		return refusal, nil
	}
	removed, err := mcp.RemoveServer(ctx, name)
	if err != nil {
		return nil, err
	}
	if !removed {
		return rpcErr(rid, 4064, fmt.Sprintf("server '%s' not found", name), nil), nil
	}
	return ok(rid, map[string]any{"ok": true, "removed": true}), nil
}

// Begins a session backed OAuth flow for an http server and answers its authorization URL
func mcpOAuthStart(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	var clientRedirectURI *string
	if uri := strArg(params, "client_redirect_uri"); uri != "" {
		if _, err := mcp.ValidateClientRedirectURI(uri); err != nil {
			return rpcErr(rid, 4001, err.Error(), nil), nil
		}
		clientRedirectURI = &uri
	}
	name, servers, errResp := mcpConfigServerOrError(ctx, rid, params)
	if errResp != nil {
		return errResp, nil
	}
	cfg := map[string]any{}
	for k, v := range servers[name] {
		cfg[k] = v
	}
	if !truthy(cfg["url"]) {
		return rpcErr(rid, 4001, "stdio servers authenticate via env keys, not OAuth", nil), nil
	}
	if truthy(cfg["headers"]) && cfg["auth"] != "oauth" {
		return rpcErr(rid, 4001, "this server uses header/API-key auth, not OAuth", nil), nil
	}
	cfg["auth"] = "oauth"
	homeDir, err := pypath.Resolve(pypath.Expanduser(home.Dir(ctx)))
	if err != nil {
		return nil, err
	}
	result, err := mcp.StartOAuthFlow(ctx, configEnv(ctx), homeDir, name, cfg, false, 30*time.Second, clientRedirectURI)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"ok": true}
	for _, k := range []string{"session_id", "auth_url", "flow"} {
		out[k] = result.GetOr(k, nil)
	}
	return ok(rid, out), nil
}

// Polls a flow for pending, approved or error, approved tokens persisted per profile
func mcpOAuthPoll(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	out := pyjson.ObjectOf("ok", true)
	out.Update(mcp.PollOAuthFlow(ctx, strArg(params, "session_id"), strArg(params, "name")))
	return ok(rid, out), nil
}

// Cancels a flow the resolved profile owns, waking its callback worker
func mcpOAuthCancel(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	homeDir, err := pypath.Resolve(pypath.Expanduser(home.Dir(ctx)))
	if err != nil {
		return nil, err
	}
	return ok(rid, mcp.CancelOAuthFlow(ctx, strArg(params, "session_id"), strArg(params, "name"), homeDir)), nil
}

// Relays a client captured redirect into a client redirect flow
func mcpOAuthCallback(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	return ok(rid, mcp.DeliverOAuthCallbackFlow(ctx, strArg(params, "session_id"), strArg(params, "name"), strOr(params["code"]),
		strOr(params["state"]), strOr(params["error"]), strOr(params["iss"]))), nil
}
