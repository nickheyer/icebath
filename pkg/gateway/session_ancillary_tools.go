package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Saved outcome of one tools enable or disable before the calling session adopts it
type toolsChange struct {
	changed, enabled, missing, unknown []string
}

// Enables or disables toolsets and MCP tools in the owner profile and commits the selection into the calling session
func configureTools(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	rawAction, isStr := params.ValueOr("action", nil).(string)
	rawNames, isList := params.ValueOr("names", nil).([]any)
	if paramsOutside(params, "session_id", "profile", "action", "names") || !isStr || !isList {
		return nil, runtimeRefusal("invalid_params")
	}
	action := strings.ToLower(strings.TrimSpace(rawAction))
	targets := []string{}
	for _, raw := range rawNames {
		name, isName := raw.(string)
		if !isName {
			return nil, runtimeRefusal("invalid_params")
		}
		if name = strings.TrimSpace(name); name != "" {
			targets = append(targets, name)
		}
	}
	if action != "disable" && action != "enable" {
		return nil, refuseText("action", "unknown tools action: "+action)
	}
	if len(targets) == 0 {
		return nil, refuseText("names", "names required")
	}
	a := c.authority
	platform, local := "cli", false
	if ref.SessionID == "" {
		if !c.actor.Capabilities["session:control"] {
			return nil, runtimeRefusal("permission_denied")
		}
	} else {
		live, ok := a.live(ref.SessionID)
		if !ok {
			return nil, runtimeRefusal("not_found")
		}
		if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:control"); err != nil {
			return nil, err
		}
		policy, err := policyForSource(ctx, a.Runner, live.Source)
		if err != nil {
			return nil, err
		}
		local = live.Source != nil && live.Source.Platform == PlatformLocal
		switch {
		case local && policy != nil && policy.Source == "acp":
			platform = "acp"
		case !local && policy != nil:
			platform = policy.Platform
		}
	}
	change, err := forwardOffThread(ctx, func() (toolsChange, error) {
		return applyToolsChange(ownerScope(ctx, a, false), action, platform, targets)
	})
	if err != nil {
		return nil, err
	}
	var info any
	if local {
		if _, err := commitRefresh(ctx, a, ref, false, false); err != nil {
			return nil, err
		}
		sessionInfo, err := localSessionInfo(ctx, a, ref)
		if err != nil {
			return nil, err
		}
		info = sessionInfo
	}
	return jsonutil.ObjectOf("changed", change.changed, "enabled_toolsets", change.enabled, "info", info,
		"missing_servers", change.missing, "reset", local, "unknown", change.unknown), nil
}

// Validates the targets, writes the platform selection and the MCP tool filters, and reports what changed
func applyToolsChange(ctx context.Context, action, platform string, targets []string) (toolsChange, error) {
	out := toolsChange{changed: []string{}, enabled: []string{}, missing: []string{}, unknown: []string{}}
	h := home.Dir(ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return out, err
	}
	valid := map[string]bool{}
	for _, ts := range tools.ConfigurableToolsets {
		valid[ts.Key] = true
	}
	for key := range tools.PluginToolsetKeys(ctx) {
		valid[key] = true
	}
	var mcpTargets, toolsetTargets []string
	unknown := map[string]bool{}
	for _, name := range targets {
		switch {
		case strings.Contains(name, ":"):
			mcpTargets = append(mcpTargets, name)
		case !valid[name]:
			if line, refused := tools.ToolsetTierRefusal(name); refused {
				return out, refuseText("names", line)
			}
			out.unknown = append(out.unknown, name)
			unknown[name] = true
		case !tools.ToolsetAllowedForPlatform(name, platform):
			allowed := []string{}
			for p := range tools.ToolsetPlatformRestrictions[name] {
				allowed = append(allowed, p)
			}
			sort.Strings(allowed)
			return out, refuseText("names", fmt.Sprintf("Toolset '%s' is not available on platform '%s' (only: %s)", name, platform, strings.Join(allowed, ", ")))
		default:
			toolsetTargets = append(toolsetTargets, name)
		}
	}
	_, owners := mcp.ServerConfigsWithSources(ctx, mcp.StoredServers(ctx))
	for _, target := range mcpTargets {
		server, _, _ := strings.Cut(target, ":")
		if plugin := owners[server]; plugin != nil {
			return out, refuseText("names", fmt.Sprintf("server '%s' is provided by plugin '%s' and cannot be modified", server, *plugin))
		}
	}
	if len(toolsetTargets) > 0 {
		if err := mcp.ApplyToolsetChange(ctx, cfg, platform, toolsetTargets, action); err != nil {
			return out, err
		}
	}
	missing := map[string]bool{}
	if len(mcpTargets) > 0 {
		missing = mcp.ApplyMCPChange(cfg, mcpTargets, action)
	}
	if err := config.Save(h, cfg); err != nil {
		return out, err
	}
	fresh, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return out, err
	}
	for name := range tools.Default.PlatformTools(ctx, "", fresh, platform, tools.PlatformToolsOptions{}) {
		out.enabled = append(out.enabled, name)
	}
	sort.Strings(out.enabled)
	for _, name := range targets {
		server, _, isMCP := strings.Cut(name, ":")
		if !unknown[name] && (!isMCP || !missing[server]) {
			out.changed = append(out.changed, name)
		}
	}
	for name := range missing {
		out.missing = append(out.missing, name)
	}
	sort.Strings(out.missing)
	return out, nil
}
