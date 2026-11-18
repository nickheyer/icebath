package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// One required param with the test its value must pass
type requiredParam struct {
	key     string
	present func(v any) bool
}

// Body of a guarded RPC, the live session given when the RPC resolves one
type rpcBody func(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) (map[string]any, error)

// Shape of a guarded RPC, its required params, live session, profile scope and failure prefix
type rpcShape struct {
	required         []requiredParam
	propagateResolve bool
	prefix           string
	unscoped         bool
	liveSession      bool
}

// Whether a value reads as a non blank string
func strippedPresent(v any) bool { return pystr.Strip(strOr(v)) != "" }

// Whether a value is neither None nor the empty string
func nonemptyPresent(v any) bool { return v != nil && pyrepr.Str(v) != "" }

// Required name param of the mcp server RPCs
var nameParam = []requiredParam{{"name", strippedPresent}}

// Required name and session id params of the mcp oauth RPCs
var nameSessionParams = []requiredParam{{"name", strippedPresent}, {"session_id", strippedPresent}}

// Stripped text of a param with None and falsy values read as empty
func strArg(params map[string]any, key string) string { return pystr.Strip(strOr(params[key])) }

// Wraps a body in the optional profile runtime scope, its failures answered under the fail code
func profileScopedRPC(failCode int, shape rpcShape, body rpcBody) Handler {
	return func(ctx context.Context, rid any, params map[string]any) (resp map[string]any) {
		for _, req := range shape.required {
			if !req.present(params[req.key]) {
				return rpcErr(rid, 4063, req.key+" required", nil)
			}
		}
		var session *tuihost.Session
		if shape.liveSession {
			s, errResp := sess(ctx, params, rid)
			if errResp != nil {
				return errResp
			}
			session = s
		}
		if !shape.unscoped {
			profile := strArg(params, "profile")
			var scoped context.Context
			failure := catchPanic(func() {
				h, err := profileHome(profile)
				panicIf(err)
				scoped = profileRuntimeScope(ctx, h, true)
			})
			if failure != nil {
				var unavailable *ProfileUnavailableError
				if err, isErr := failure.(error); isErr && errors.As(err, &unavailable) {
					return rpcErr(rid, 4064, fmt.Sprintf("profile '%s' not found", profile), nil)
				}
				if shape.propagateResolve {
					panic(failure)
				}
				return rpcErr(rid, failCode, panicMessage(failure), nil)
			}
			ctx = scoped
		}
		defer func() {
			if r := recover(); r != nil {
				var unavailable *ProfileUnavailableError
				if err, isErr := r.(error); isErr && errors.As(err, &unavailable) {
					panic(r)
				}
				resp = rpcErr(rid, failCode, shape.prefix+panicMessage(r), nil)
			}
		}()
		out, err := body(ctx, rid, params, session)
		if err != nil {
			panic(err)
		}
		return out
	}
}

// Registers a guarded RPC with no profile scope
func guardedRPC(reg *handlerRegistry, name string, failCode int, shape rpcShape, body rpcBody) {
	shape.unscoped = true
	reg.method(name, profileScopedRPC(failCode, shape, body))
}

// Registers an RPC in the optional profile scope of its params
func scopedRPC(reg *handlerRegistry, name string, failCode int, shape rpcShape, body rpcBody) {
	reg.method(name, profileScopedRPC(failCode, shape, body))
}

// Registers an mcp.servers RPC, profile scoped, its resolve failures left to dispatch and body failures 5024
func mcpRPC(reg *handlerRegistry, name string, required []requiredParam, body rpcBody) {
	scopedRPC(reg, "mcp.servers."+name, 5024, rpcShape{required: required, propagateResolve: true}, body)
}

var toolsRPCRegistry = &handlerRegistry{}

func init() {
	toolsRPCRegistry.method("system.battery", systemBattery)
	guardedRPC(toolsRPCRegistry, "process.stop", 5010, rpcShape{}, processStop)
	guardedRPC(toolsRPCRegistry, "reload.env", 5015, rpcShape{}, reloadEnv)
	guardedRPC(toolsRPCRegistry, "plugins.list", 5032, rpcShape{}, pluginsList)
	guardedRPC(toolsRPCRegistry, "process.list", 5010, rpcShape{liveSession: true}, processList)
	guardedRPC(toolsRPCRegistry, "process.kill", 5010, rpcShape{liveSession: true}, processKill)
	guardedRPC(toolsRPCRegistry, "reload.mcp", 5015, rpcShape{}, reloadMCP)
	guardedRPC(toolsRPCRegistry, "commands.catalog", 5020, rpcShape{}, commandsCatalog)
	toolsRPCRegistry.install()
}

// Enabled and disabled toolsets a session reads back, the built agent's else its profile's, nil for all
func sessionToolsets(ctx context.Context, session *tuihost.Session) ([]string, []string) {
	if a := sessionAgent(session); a != nil {
		return a.EnabledToolsets(), a.DisabledToolsets()
	}
	scoped := sessionProfileRuntimeScope(ctx, session, true)
	return loadEnabledToolsets(scoped, resolveAgentPlatform(sessionSource(session))), loadDisabledToolsets(scoped)
}

// Host battery for the status bar, unavailable on any read failure
func systemBattery(ctx context.Context, rid any, params map[string]any) map[string]any {
	var out map[string]any
	if catchPanic(func() {
		batt := hostinfo.ReadBattery(true)
		var percent, plugged any
		if batt.Percent != nil {
			percent = *batt.Percent
		}
		if batt.Plugged != nil {
			plugged = *batt.Plugged
		}
		out = map[string]any{"available": batt.Available, "percent": percent, "plugged": plugged,
			"category": hostinfo.BatteryCategory(batt)}
	}) != nil {
		out = map[string]any{"available": false, "percent": nil, "plugged": nil, "category": "dim"}
	}
	return ok(rid, out)
}

// Kills every background process of the registry as a deliberate stop
func processStop(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	killed := tools.DefaultProcessRegistry().KillAll(ctx, tools.ToolEnvFrom(ctx), "", nil, "process.stop", false)
	return ok(rid, map[string]any{"killed": killed}), nil
}

// Re-reads the home env file into the process environment
func reloadEnv(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	updated, err := config.ReloadEnv(home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"updated": updated}), nil
}

// Loaded plugins with their version and enabled state in discovery order
func pluginsList(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	rows := []map[string]any{}
	for _, p := range plugins.GetPluginManager(ctx).LoadedPluginsInOrder() {
		rows = append(rows, map[string]any{"name": p.Key, "version": "?", "enabled": p.Plugin.Enabled})
	}
	return ok(rid, map[string]any{"plugins": rows}), nil
}

// Background processes the calling session owns
func processList(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) (map[string]any, error) {
	return ok(rid, map[string]any{"processes": sessionProcesses(ctx, session)}), nil
}

// Kills one background process the calling session owns
func processKill(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) (map[string]any, error) {
	procID := strOr(params["process_id"])
	if procID == "" {
		return rpcErr(rid, 4012, "process_id required", nil), nil
	}
	registry := tools.DefaultProcessRegistry()
	env := tools.ToolEnvFrom(ctx)
	proc := registry.Get(env, procID)
	if proc == nil || proc.SessionKey != strOr(session.Get("session_key")) {
		return rpcErr(rid, 4044, "no such process: "+procID, nil), nil
	}
	return ok(rid, registry.KillProcess(ctx, env, procID, "process.kill", true)), nil
}

// The approvals mcp_reload_confirm flag of the disk config, true on any failure
func mcpReloadConfirmRequired(ctx context.Context) (required bool) {
	required = true
	catchPanic(func() {
		cfg, err := loadConfig(ctx)
		panicIf(err)
		approvals, isDict := map[string]any(cfg)["approvals"].(map[string]any)
		if !isDict {
			return
		}
		flag, present := approvals["mcp_reload_confirm"]
		if !present {
			return
		}
		required = truthy(flag)
	})
	return required
}

// MCP shutdown, discovery and revision hash a reload drives, swapped by tests
var (
	reloadShutdownMCP = func(ctx context.Context) { mcp.ShutdownMCPServers(ctx, nil, nil, 15*time.Second) }
	reloadDiscoverMCP = func(ctx context.Context) error {
		_, err := mcp.ConnectAll(ctx, nil, nil)
		return err
	}
	reloadMCPRev = computeMCPRev
)

// Leader shutdown, rediscovery and refresh of every MCP server, re-hashed until the config is stable
func doFullMCPReload(ctx context.Context) {
	launch := profileRuntimeScope(ctx, "", true)
	loaded := reloadMCPRev(launch)
	for range mcpReloadMaxPasses {
		reloadShutdownMCP(launch)
		mcp.ReprobeToolAvailability()
		panicIf(reloadDiscoverMCP(launch))
		after := reloadMCPRev(launch)
		if after == loaded {
			break
		}
		loaded = after
	}
	sessionsLock.Lock()
	homes := map[string]bool{}
	for _, entry := range tuihost.LiveSessions() {
		if entry.Session.Get("agent") != nil {
			if h, isText := entry.Session.Get("profile_home").(string); isText {
				homes[h] = true
			}
		}
	}
	sessionsLock.Unlock()
	sorted := make([]string, 0, len(homes))
	for h := range homes {
		sorted = append(sorted, h)
	}
	sort.Strings(sorted)
	for _, h := range sorted {
		if failure := catchPanic(func() { panicIf(reloadDiscoverMCP(profileRuntimeScope(ctx, h, true))) }); failure != nil {
			pkgLog().Warn(fmt.Sprintf("MCP rediscovery failed for profile %s: %s", h, panicMessage(failure)))
		}
	}
	panicIf(refreshLiveSessions(ctx, nil, false, nil))
	mcpReloadLoadedRev = loaded
	mcpReloadGen++
}

// Reloads MCP servers behind the prompt cache confirm gate, coalescing a follower onto a completed leader reload
func reloadMCP(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	if !truthy(params["confirm"]) && mcpReloadConfirmRequired(ctx) {
		message := "⚠️  /reload-mcp invalidates the prompt cache (next message re-sends full input tokens). " +
			"Reply `/reload-mcp now` to proceed, or `/reload-mcp always` to proceed and " +
			"silence this prompt permanently."
		return ok(rid, map[string]any{"status": "confirm_required", "message": message}), nil
	}
	reqRev := strOr(params["rev"])
	if mcpReloadLock.TryLock() {
		func() {
			defer mcpReloadLock.Unlock()
			doFullMCPReload(ctx)
		}()
		return finishReload(ctx, rid, params, false), nil
	}
	genBefore := mcpReloadGen
	mcpReloadLock.Lock()
	defer mcpReloadLock.Unlock()
	coalesced := mcpReloadGen > genBefore && (reqRev == "" || reqRev == mcpReloadLoadedRev)
	if coalesced {
		panicIf(refreshLiveSessions(ctx, nil, false, nil))
	} else {
		doFullMCPReload(ctx)
	}
	return finishReload(ctx, rid, params, coalesced), nil
}

// Registry backed slash catalog bound to the profile and workspace of the calling session or draft
func commandsCatalog(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	cwd, err := completionCwd(ctx, params)
	if err != nil {
		return nil, err
	}
	scope := sessionHomeScope(tuihost.SessionRecord(strOr(params["session_id"])), cwd, strOr(params["profile"]))
	catalog, err := CommandCatalog(ctx, scope)
	if err != nil {
		return nil, err
	}
	return ok(rid, catalog), nil
}
