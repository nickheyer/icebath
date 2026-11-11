package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Classic CLI banner reads of the session inventory, assigned from pkg/app in cmd/icebath/wiring.go
var InventoryBanner struct {
	ReleaseDate         string
	AvailableSkills     func(ctx context.Context) *pyjson.Object
	UpdateResult        func(timeout time.Duration) *int64
	PrefetchUpdateCheck func()
	ToolsetAvailability func(ctx context.Context, enabled []string) map[string]any
}

// Set once the owner started the passive update check
var updatePrefetchStarted atomic.Bool

// Starts the passive update check once so update_behind reports like the banner instead of staying null
func startUpdatePrefetch() {
	if updatePrefetchStarted.Swap(true) {
		return
	}
	InventoryBanner.PrefetchUpdateCheck()
}

// Runs fn and reports whether it finished without a panic, the contextlib suppress of one builder
func suppressed(fn func()) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	fn()
	return true
}

// Tool names by toolset in load order, a tool no toolset claims under other
func groupedTools(ctx context.Context, defs []map[string]any) *pyjson.Object {
	grouped := pyjson.NewObject()
	scope := tools.ScopeKey(home.Dir(ctx))
	for _, def := range defs {
		function, _ := def["function"].(map[string]any)
		name, _ := function["name"].(string)
		toolset := tools.Default.ToolsetForTool(ctx, scope, name)
		if toolset == "" {
			toolset = "other"
		}
		names, _ := grouped.GetOr(toolset, nil).([]any)
		grouped.Set(toolset, append(names, name))
	}
	return grouped
}

// Toolsets and tools the session's next agent build loads, configured platform and oneshot drops removed
func policyTools(ctx context.Context, policy *LocalSessionPolicy, cfg map[string]any) ([]string, []map[string]any, error) {
	enabled, err := policy.currentToolsets(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	enabled = append([]string{}, enabled...)
	agentSection := cfg["agent"]
	if !pyrepr.Truthy(agentSection) {
		agentSection = map[string]any{}
	}
	raw, err := pyGet(agentSection, "disabled_toolsets", nil)
	if err != nil {
		return nil, nil, err
	}
	configured := tools.ParseConfigStringList(raw)
	if len(configured) == 0 {
		configured = nil
	}
	disabled := tools.SessionDisabledToolsets(configured, policy.Platform, policy.Source == "oneshot")
	defs := tools.Default.ToolDefinitions(ctx, nil, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: true})
	drops := tools.SessionPlatformToolDrops(policy.Platform)
	kept := []map[string]any{}
	for _, def := range defs {
		function, _ := def["function"].(map[string]any)
		if name, _ := function["name"].(string); !drops[name] {
			kept = append(kept, def)
		}
	}
	return enabled, kept, nil
}

// Named project owning the cwd with its id, slug, name and primary path, nil when none or unreadable
func inventoryProject(ctx context.Context, cwd string) any {
	if strings.TrimSpace(cwd) == "" {
		return nil
	}
	db, err := tools.OpenProjectsDB(ctx, &tools.Env{Home: home.Dir(ctx)})
	if err != nil {
		slog.Debug("failed to resolve project for cwd", "error", err)
		return nil
	}
	defer func() { _ = db.Close() }()
	project, err := db.ProjectForPath(cwd, false)
	if err != nil {
		slog.Debug("failed to resolve project for cwd", "error", err)
		return nil
	}
	if project == nil {
		return nil
	}
	var primary any
	if project.PrimaryPath != nil {
		primary = *project.PrimaryPath
	}
	return pyjson.ObjectOf("id", project.ID, "slug", project.Slug, "name", project.Name, "primary_path", primary)
}

// Tool env of the owner scope, the profile home when the context carries none
func inventoryToolEnv(ctx context.Context) *tools.Env {
	if env := tools.ToolEnvFrom(ctx); env != nil {
		return env
	}
	return &tools.Env{Home: home.Dir(ctx)}
}

// Toolset availability warning lines the classic CLI printed under its banner, empty when they cannot be computed
func cliStartupNotices(ctx context.Context, enabled []string) []any {
	lines := []any{}
	suppressed(func() {
		env := inventoryToolEnv(ctx)
		scope := tools.ScopeKey(env.Home)
		_, unavailable := tools.Default.CheckToolAvailability(ctx, env)
		unavailable = tools.FilterToEnabledToolsets(unavailable, enabled, func(name string) ([]string, error) {
			return tools.Default.ResolveToolset(ctx, scope, name), nil
		})
		reason := tools.TerminalBackendUnavailableReason()
		backend, err := tools.CurrentTerminalBackend(env)
		if err != nil {
			panic(err)
		}
		found := []any{}
		for _, line := range tools.ToolAvailabilityWarningLines(unavailable, reason, backend) {
			found = append(found, line)
		}
		lines = found
	})
	return lines
}

// Session toolset names that neither registry, MCP servers, plugin toolsets nor platform bundles can place
func unknownToolsets(ctx context.Context, enabled []string, cfg map[string]any) []any {
	if slices.Contains(enabled, "all") || slices.Contains(enabled, "*") {
		return []any{}
	}
	out := []any{}
	if !suppressed(func() {
		known := tools.SavedToolsetResolver(ctx, config.Tree(cfg))
		for _, name := range enabled {
			if !known(name) {
				out = append(out, name)
			}
		}
	}) {
		return []any{}
	}
	return out
}

// Skill names by category as the classic CLI offers them, kanban skills relevant for this scan only
func cliSkills(ctx context.Context) *pyjson.Object {
	grouped := pyjson.NewObject()
	for _, skill := range skills.NewScope(home.Dir(ctx), nil).FindAllSkillsKanbanScoped() {
		category := "general"
		if c := skill["category"]; pyrepr.Truthy(c) {
			category = pyrepr.Str(c)
		}
		names, _ := grouped.GetOr(category, nil).([]any)
		grouped.Set(category, append(names, skill["name"]))
	}
	return grouped
}

// MCP server status rows as the JSON objects a viewer reads
func inventoryMCPStatus(ctx context.Context) []any {
	raw, err := json.Marshal(mcp.Status(ctx, inventoryToolEnv(ctx), nil, true))
	if err != nil {
		panic(err)
	}
	rows := []any{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		panic(err)
	}
	return rows
}

// Provider identity of an agent's session route, a custom endpoint named by its base url
func sessionProviderOf(ctx context.Context, agent any, cfg map[string]any) string {
	var view SessionRuntimeFacts
	if holder, ok := agent.(interface{ SessionRuntime() SessionRuntimeFacts }); ok {
		view = holder.SessionRuntime()
	}
	var providerName, requested, baseURL, model string
	if view != nil {
		providerName, baseURL = view.Provider(), view.BaseURL()
		if r, ok := view.(interface{ RequestedProvider() string }); ok {
			requested = r.RequestedProvider()
		}
		if m, ok := view.(interface{ Model() string }); ok {
			model = m.Model()
		}
	}
	return provider.SessionProviderIdentity(ctx, providerName, requested, baseURL, model, cfg)
}

// Tool definitions an agent loaded, nil when it exposes none
func agentToolDefs(agent any) []map[string]any {
	if holder, ok := agent.(interface{ ToolDefinitions() []map[string]any }); ok {
		return holder.ToolDefinitions()
	}
	return nil
}

// Cached system prompt of an agent, empty before its first build
func agentCachedSystemPrompt(agent any) string {
	if holder, ok := agent.(interface{ CachedSystemPrompt() string }); ok {
		return holder.CachedSystemPrompt()
	}
	return ""
}

// Context window of an agent's compressor, zero when it has none
func agentContextLength(agent any) int {
	if holder, ok := agent.(interface{ ContextLength() int }); ok {
		return holder.ContextLength()
	}
	return 0
}

// Banner fields only a built agent knows, its provider, tools, cached system prompt and context length
func AgentInventory(ctx context.Context, agent any) *pyjson.Object {
	info := pyjson.NewObject()
	suppressed(func() { info.Set("provider", sessionProviderOf(ctx, agent, nil)) })
	suppressed(func() { info.Set("tools", groupedTools(ctx, agentToolDefs(agent))) })
	info.Set("system_prompt", agentCachedSystemPrompt(agent))
	if length := agentContextLength(agent); length != 0 {
		info.Set("context_length", length)
	}
	return info
}

// Banner and status fields of one session, each section keeping its empty value when its builder fails
func sessionInventory(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, agent any, cwd string) (*pyjson.Object, error) {
	branch := ""
	if cwd != "" {
		branch = tools.GitProbeBranch(cwd)
	}
	info := pyjson.ObjectOf("tools", pyjson.NewObject(), "skills", pyjson.NewObject(), "mcp_servers", []any{},
		"version", "", "release_date", "", "update_behind", nil, "update_command", "",
		"branch", branch, "project", inventoryProject(ctx, cwd))
	scoped := ownerScope(ctx, a, false)
	cfg := map[string]any{}
	if policy != nil {
		loaded, err := policy.config(scoped, a, false)
		if err != nil {
			return nil, err
		}
		cfg = loaded
	}
	var enabled []string
	if !nilAgent(agent) {
		var routeCfg map[string]any
		if policy != nil {
			routeCfg = cfg
		}
		info.Set("provider", sessionProviderOf(scoped, agent, routeCfg))
		info.Set("tools", groupedTools(scoped, agentToolDefs(agent)))
		info.Set("system_prompt", agentCachedSystemPrompt(agent))
		if length := agentContextLength(agent); length != 0 {
			info.Set("context_length", length)
		}
		if holder, ok := agent.(interface{ EnabledToolsets() []string }); ok {
			enabled = append(enabled, holder.EnabledToolsets()...)
		}
	} else {
		route := cfg["model"]
		if !pyrepr.Truthy(route) {
			route = map[string]any{}
		}
		name, err := pyGet(route, "provider", nil)
		if err != nil {
			return nil, err
		}
		info.Set("provider", "")
		if pyrepr.Truthy(name) {
			info.Set("provider", pyrepr.Str(name))
		}
		if policy != nil {
			var loaded []map[string]any
			var err error
			if !suppressed(func() { enabled, loaded, err = policyTools(scoped, policy, cfg) }) || err != nil {
				slog.Warn("session inventory: tool snapshot failed", "error", err)
				enabled = nil
			} else {
				info.Set("tools", groupedTools(scoped, loaded))
			}
		}
	}
	if enabled == nil {
		enabled = []string{}
	}
	sortedNames := slices.Sorted(slices.Values(enabled))
	names := make([]any, len(sortedNames))
	for i, name := range sortedNames {
		names[i] = name
	}
	info.Set("enabled_toolsets", names)
	suppressed(func() { info.Set("tool_availability", InventoryBanner.ToolsetAvailability(scoped, enabled)) })
	if policy != nil && policy.Source == "cli" {
		info.Set("tool_availability_warnings", cliStartupNotices(scoped, enabled))
		info.Set("unknown_toolsets", unknownToolsets(scoped, enabled, cfg))
	}
	suppressed(func() {
		if policy != nil && policy.Source == "cli" {
			info.Set("skills", cliSkills(scoped))
		} else {
			info.Set("skills", InventoryBanner.AvailableSkills(scoped))
		}
	})
	suppressed(func() { info.Set("mcp_servers", inventoryMCPStatus(scoped)) })
	suppressed(func() {
		version, err := updaterecord.GetVersionInfo()
		if err != nil {
			panic(err)
		}
		info.Set("version", version.BaseVersion)
		info.Set("release_date", InventoryBanner.ReleaseDate)
	})
	suppressed(func() {
		startUpdatePrefetch()
		if behind := InventoryBanner.UpdateResult(500 * time.Millisecond); behind != nil {
			info.Set("update_behind", *behind)
		} else {
			info.Set("update_behind", nil)
		}
		command, err := config.RecommendedUpdateCommand(ctx, home.Dir(ctx))
		if err != nil {
			panic(err)
		}
		info.Set("update_command", command)
	})
	return info, nil
}

// Session info plus the banner inventory, the inventory builders run off the owner loop
func snapshotInfo(ctx context.Context, a *SessionAuthority, ref SessionRef) (*pyjson.Object, error) {
	info, err := localSessionInfo(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{key: ref.SessionID}
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	agent, err := a.agent(ref)
	if err != nil {
		return nil, err
	}
	inventory, err := sessionInventory(ctx, a, policy, agent, anyText(info.GetOr("cwd", nil)))
	if err != nil {
		return nil, err
	}
	if info.GetOr("tool_count", nil) == nil {
		total := 0
		if grouped, ok := inventory.GetOr("tools", nil).(*pyjson.Object); ok && grouped != nil {
			for _, key := range grouped.Keys {
				names, _ := grouped.Vals[key].([]any)
				total += len(names)
			}
		}
		info.Set("tool_count", total)
	}
	usage, err := snapshotUsage(ctx, a, ref, agent)
	if err != nil {
		return nil, err
	}
	if usage != nil {
		info.Set("usage", usage)
	}
	merged := inventory.Copy()
	for _, key := range info.Keys {
		merged.Set(key, info.Vals[key])
	}
	return merged, nil
}
