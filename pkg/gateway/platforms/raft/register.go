package raft

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Whether the config enables the platform or carries a bridge token
func isConnected(config *gateway.PlatformConfig) bool {
	if config == nil {
		return false
	}
	return pyrepr.Truthy(config.Extra["enabled"]) || pyrepr.Truthy(config.Extra["bridge_token"])
}

// Enables the platform during gateway config load when the scope aware RAFT_PROFILE is set
func envEnablement(ctx context.Context) map[string]any {
	if resolveRaftProfile(ctx) == "" {
		return nil
	}
	return map[string]any{"enabled": true}
}

// Model facing instructions naming the raft profile every CLI call must pass
const platformHintTemplate = "You are connected to Raft via an external-agent channel. " +
	"Run `raft --profile {profile} profile show` to confirm which agent profile is active. " +
	"Run `raft --profile {profile} manual get raft-cli-overview` to learn available Raft commands. " +
	"Always pass `--profile {profile}` to every raft CLI call."

// Raft platform plugin compiled into the binary under the bundled platforms/raft key
type raftPlatformPlugin struct{}

func (raftPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "raft-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/raft",
		Author: "botiverse",
		Description: "Raft gateway adapter for Icebath Agent. Connects to a Raft workspace as an external agent via a local " +
			"wake-channel bridge. The adapter starts a loopback HTTP endpoint that receives content-free wake hints from the " +
			"bridge, then injects them into the Icebath gateway session pipeline. The agent reads and sends messages through " +
			"the Raft CLI — the adapter never touches message bodies or delivery cursors.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "RAFT_PROFILE", Description: "Raft agent profile slug — auto-enables the adapter when set"}}}
}

// Adapter class of the Raft wake channel endpoint
var raftAdapterClass = &gateway.AdapterClass{Name: "RaftAdapter", Traits: gateway.DefaultAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewRaftAdapter(ctx, config)
	}}

// Registers the Raft platform entry and the session, model and tool hooks that feed activity telemetry
func (raftPlatformPlugin) Register(ctx *plugins.Context) error {
	profile := resolveRaftProfile(ctx.HomeContext())
	if profile == "" {
		profile = "your-agent-profile"
	}
	if _, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "raft", Label: "Raft",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewRaftAdapter(fctx, cfg)
		}),
		Check:       checkRaftRequirements,
		RequiredEnv: []string{"RAFT_PROFILE"},
		InstallHint: "Install the Raft CLI from https://raft.build",
		Extra: map[string]any{
			"dashboard_description": "Join a Raft workspace as an external agent.",
			"docs_url":              "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/raft",
			"is_connected":          isConnected,
			"setup_fn":              InteractiveSetup,
			"env_enablement_fn":     envEnablement,
			"emoji":                 "🔔",
			"platform_hint":         strings.ReplaceAll(platformHintTemplate, "{profile}", profile),
		},
	}); err != nil {
		return err
	}
	for _, hook := range []struct {
		name string
		fn   plugins.HookFunc
	}{{"on_session_start", onSessionStart}, {"pre_llm_call", onPreLLMCall}, {"pre_tool_call", onPreToolCall},
		{"post_tool_call", onPostToolCall}, {"post_llm_call", onPostLLMCall}, {"on_session_end", onSessionEnd},
		{"on_session_finalize", onSessionFinalize}} {
		ctx.RegisterHook(hook.name, hook.fn, false)
	}
	return nil
}

func init() {
	gateway.RegisterAdapterModule(raftAdapterClass)
	plugins.RegisterBundled(raftPlatformPlugin{})
}
