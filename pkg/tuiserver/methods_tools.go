package tuiserver

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Tool snapshot rebuild of one live agent, the registry refresh the reload and plugin activation share
var refreshAgentMCPTools = mcp.RefreshAgentMCPTools

// Rebuilds every live agent tool snapshot of a home from the registry, queues the plugin note and pushes session info
func refreshLiveSessions(ctx context.Context, h *string, preservePrefix bool, pluginNote *[2]string) error {
	want := ""
	if h != nil {
		want = home.Key(*h)
	}
	var live []tuigateway.LiveSession
	for _, entry := range tuigateway.LiveSessions() {
		if sessionAgent(entry.Session) == nil {
			continue
		}
		if h != nil {
			owner := strOr(entry.Session.Get("profile_home"))
			if owner == "" {
				owner = home.Process()
			}
			if home.Key(owner) != want {
				continue
			}
		}
		live = append(live, entry)
	}
	for _, entry := range live {
		a := sessionAgent(entry.Session)
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger().Warn(fmt.Sprintf("Failed to refresh cached agent tools (session %s): %v", entry.SID, r))
				}
			}()
			scoped := sessionProfileRuntimeScope(ctx, entry.Session, true)
			enabled := tools.CarryOnboardingToolset(scoped, loadEnabledToolsets(scoped, a.Platform()), a)
			disabled := loadDisabledToolsets(scoped)
			refreshAgentMCPTools(scoped, a, mcp.RefreshOptions{EnabledOverride: enabled, DisabledOverride: disabled,
				Quiet: true, PreservePrefix: preservePrefix})
		}()
		if pluginNote != nil {
			a.QueuePluginNotice(pluginNote[0], pluginNote[1])
		}
		tuigateway.Emit(ctx, "session.info", entry.SID, sessionInfo(ctx, a, entry.Session))
	}
	return nil
}

// Rebuilds live session tool snapshots of a home for the gateway plugin activation hook
func RefreshLiveSessions(ctx context.Context, h *string, preservePrefix bool, pluginNote *[2]string) error {
	return refreshLiveSessions(ctx, h, preservePrefix, pluginNote)
}
