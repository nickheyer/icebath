package tuiserver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var toolsConfigRegistry = &handlerRegistry{}

func init() {
	guardedRPC(toolsConfigRegistry, "tools.show", 5034, rpcShape{}, toolsShow)
	guardedRPC(toolsConfigRegistry, "tools.configure", 5035, rpcShape{}, toolsConfigure)
	guardedRPC(toolsConfigRegistry, "learning.frames", 5000, rpcShape{prefix: "learning.frames failed: "}, learningFrames)
	toolsConfigRegistry.install()
}

// Every tool the session would load, deferred ones included, grouped by toolset with its first sentence
func toolsShow(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	session := tuihost.SessionRecord(strOr(params["session_id"]))
	enabled, disabled := sessionToolsets(ctx, session)
	env := configEnv(ctx)
	defs := tools.Default.ToolDefinitions(ctx, env, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: true,
		SkipToolSearchAssembly: true})
	toolName := func(def map[string]any) string {
		fn, _ := pyjson.Dict(def["function"])
		return pyrepr.Str(fn["name"])
	}
	sorted := append([]map[string]any(nil), defs...)
	sort.SliceStable(sorted, func(i, j int) bool { return toolName(sorted[i]) < toolName(sorted[j]) })
	scope := tools.ScopeKey(home.Dir(ctx))
	sections := map[string][]map[string]any{}
	for _, def := range sorted {
		name := toolName(def)
		fn, _ := pyjson.Dict(def["function"])
		desc, _, _ := strings.Cut(strOr(fn["description"]), "\n")
		if i := strings.Index(desc, ". "); i >= 0 {
			desc = desc[:i+1]
		}
		toolset := tools.Default.ToolsetForTool(ctx, scope, name)
		if toolset == "" {
			toolset = "unknown"
		}
		sections[toolset] = append(sections[toolset], map[string]any{"name": name, "description": desc})
	}
	names := make([]string, 0, len(sections))
	for n := range sections {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []map[string]any{}
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "tools": sections[n]})
	}
	return ok(rid, map[string]any{"sections": out, "total": len(defs)}), nil
}

// Enables or disables toolsets and MCP tools for the cli platform in the session profile, rebuilding its agent
func toolsConfigure(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	sid := strOr(params["session_id"])
	var session *tuihost.Session
	if sid != "" {
		s, errResp := sessNowait(ctx, params, rid)
		if errResp != nil {
			return errResp, nil
		}
		if busy := busyError(rid, s, "tools"); busy != nil {
			return busy, nil
		}
		session = s
	}
	scoped := bindBuildProfileScopes(ctx, strOr(session.Get("profile_home")))
	return configureSessionTools(scoped, rid, params, sid, session)
}

// Applies a tools enable or disable to the saved cli selection and answers what changed
func configureSessionTools(ctx context.Context, rid any, params map[string]any, sid string, session *tuihost.Session) (map[string]any, error) {
	action := strings.ToLower(pystr.Strip(strOr(params["action"])))
	var targets []string
	var rawNames []any
	if truthy(params["names"]) {
		switch names := params["names"].(type) {
		case []any:
			rawNames = names
		case string:
			for _, r := range names {
				rawNames = append(rawNames, string(r))
			}
		case map[string]any:
			for k := range names {
				rawNames = append(rawNames, k)
			}
		default:
			return nil, fmt.Errorf("the tool names must be a list, got %s", errchain.Kind(params["names"]))
		}
	}
	for _, name := range rawNames {
		if text := pystr.Strip(pyrepr.Str(name)); text != "" {
			targets = append(targets, text)
		}
	}
	if action != "disable" && action != "enable" {
		return rpcErr(rid, 4017, "unknown tools action: "+action, nil), nil
	}
	if len(targets) == 0 {
		return rpcErr(rid, 4018, "names required", nil), nil
	}
	cfg, err := loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	pluginKeys := tools.PluginToolsetKeys(ctx)
	valid := map[string]bool{}
	for _, ts := range tools.ConfigurableToolsets {
		valid[ts.Key] = true
	}
	for k := range pluginKeys {
		valid[k] = true
	}
	var mcpTargets, toolsetTargets []string
	unknown := []string{}
	unknownSet := map[string]bool{}
	for _, name := range targets {
		switch {
		case strings.Contains(name, ":"):
			mcpTargets = append(mcpTargets, name)
		case valid[name]:
			toolsetTargets = append(toolsetTargets, name)
		default:
			if line, refused := tools.ToolsetTierRefusal(name); refused {
				return rpcErr(rid, 4015, line, nil), nil
			}
			unknown = append(unknown, name)
			unknownSet[name] = true
		}
	}
	if len(toolsetTargets) > 0 {
		if err := mcp.ApplyToolsetChange(ctx, cfg, "cli", toolsetTargets, action); err != nil {
			return nil, err
		}
	}
	_, owners := mcpServerRows(ctx)
	for _, target := range mcpTargets {
		serverName, _, _ := strings.Cut(target, ":")
		if refusal := mcpPluginWriteError(rid, serverName, owners); refusal != nil {
			return refusal, nil
		}
	}
	missing := map[string]bool{}
	if len(mcpTargets) > 0 {
		missing = mcp.ApplyMCPChange(cfg, mcpTargets, action)
	}
	if err := config.Save(home.Dir(ctx), cfg); err != nil {
		return nil, err
	}
	var info any
	if session != nil {
		resetInfo, err := resetSessionAgent(ctx, sid, session)
		if err != nil {
			return nil, err
		}
		info = resetInfo
	}
	fresh, err := loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	enabledSet := tools.Default.PlatformTools(ctx, "", fresh, "cli", tools.PlatformToolsOptions{})
	enabled := make([]string, 0, len(enabledSet))
	for name := range enabledSet {
		enabled = append(enabled, name)
	}
	sort.Strings(enabled)
	changed := []string{}
	for _, name := range targets {
		server, _, isMCP := strings.Cut(name, ":")
		if !unknownSet[name] && (!isMCP || !missing[server]) {
			changed = append(changed, name)
		}
	}
	missingServers := make([]string, 0, len(missing))
	for name := range missing {
		missingServers = append(missingServers, name)
	}
	sort.Strings(missingServers)
	return ok(rid, map[string]any{"changed": changed, "enabled_toolsets": enabled, "info": info,
		"missing_servers": missingServers, "reset": session != nil, "unknown": unknown}), nil
}

// Pre rendered journey timeline frames with their legend and summary metadata
func learningFrames(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	sizes := []int64{80, 24, 48}
	for i, key := range []string{"cols", "rows", "frames"} {
		raw, present := params[key]
		if !present || !truthy(raw) {
			continue
		}
		n, err := pyint.Int64(raw)
		if err != nil {
			sizes = []int64{80, 24, 48}
			break
		}
		sizes[i] = n
	}
	graph, err := memory.BuildLearningGraph(ctx, configEnv(ctx))
	if err != nil {
		return nil, err
	}
	rendered, err := memory.RenderFrames(graph, int(max(20, sizes[0])), int(max(10, sizes[1])), int(sizes[2]))
	if err != nil {
		return nil, err
	}
	return ok(rid, rendered), nil
}
