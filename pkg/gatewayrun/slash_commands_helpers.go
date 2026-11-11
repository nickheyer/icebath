package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Live running agent of the session, else its cached one, never the pending sentinel
func (r *GatewayRunner) residentAgentFor(sessionKey string) *agent.AIAgent {
	if running, ok := r.runningAgents().Get(sessionKey); ok && running != nil && running != agentPendingSentinel {
		return running
	}
	return r.cachedAgentFor(sessionKey, false)
}

// Session history unavailable line with its cause gloss and action under the gateway prefix
func sessionDBUnavailableReply(ctx context.Context) string {
	return state.FormatSessionDBUnavailable(ctx, i18n.T(ctx, "gateway.shared.session_db_unavailable_prefix", "", nil), false)
}

// Delivery adapter and session key of the event source, both empty when the event has no source
func (r *GatewayRunner) adapterAndKeyFor(ctx context.Context, event *gateway.MessageEvent) (gateway.PlatformAdapter, string, error) {
	if event.Source == nil {
		return nil, "", nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil {
		return nil, "", err
	}
	key, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		return nil, "", err
	}
	return adapter, key, nil
}

// Reply text with its slash command mentions rewritten for the event platform
func telegramizedCommandReply(event *gateway.MessageEvent, text string) string {
	var platform gateway.Platform
	if event.Source != nil {
		platform = event.Source.Platform
	}
	return telegramizeCommandMentions(text, platform)
}

// Checkpoint manager from a local session's frozen policy else the gateway config, nil when disabled
func (r *GatewayRunner) checkpointManager(ctx context.Context, source *gateway.SessionSource) (*tools.CheckpointManager, error) {
	var policy *gateway.LocalSessionPolicy
	if source != nil {
		p, err := gateway.SessionPolicyForSource(ctx, r, source)
		if err != nil {
			return nil, err
		}
		policy = p
	}
	var cfg map[string]any
	if policy != nil {
		authority, err := gateway.ActiveAuthority(ctx, r)
		if err != nil {
			return nil, err
		}
		if cfg, _, err = policy.EffectiveFor(ctx, authority, false); err != nil {
			return nil, err
		}
	} else {
		cfg = gateway.LoadGatewayConfigMap(ctx, "")
	}
	cp := gateway.CheckpointAgentKwargs(cfg)
	if !pyrepr.Truthy(cp["checkpoints_enabled"]) {
		return nil, nil
	}
	field := func(key string) (int, error) {
		n, err := pyint.Int64(cp[key])
		return int(n), err
	}
	snapshots, err := field("checkpoint_max_snapshots")
	if err != nil {
		return nil, err
	}
	totalMB, err := field("checkpoint_max_total_size_mb")
	if err != nil {
		return nil, err
	}
	fileMB, err := field("checkpoint_max_file_size_mb")
	if err != nil {
		return nil, err
	}
	env := &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
	return tools.NewCheckpointManager(env, tools.CheckpointManagerOptions{Enabled: true, MaxSnapshots: snapshots,
		MaxTotalSizeMB: totalMB, MaxFileSizeMB: fileMB}), nil
}

// Setter persisting the section write_approval flag raw and dropping the session's cached agent
func (r *GatewayRunner) writeApprovalSetter(ctx context.Context, section string, event *gateway.MessageEvent) (func(enabled bool) error, error) {
	configHome := home.Dir(ctx)
	configPath := filepath.Join(configHome, "config.yaml")
	sessionKey, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		return nil, err
	}
	return func(enabled bool) error {
		userConfig, err := config.ReadUserConfigRaw(ctx, configPath)
		if err != nil {
			return err
		}
		if userConfig == nil {
			userConfig = map[string]any{}
		}
		if _, present := userConfig[section]; !present {
			userConfig[section] = map[string]any{}
		}
		sectionMap, ok := userConfig[section].(map[string]any)
		if !ok {
			return fmt.Errorf("TypeError: '%s' object does not support item assignment", pyrepr.TypeName(userConfig[section]))
		}
		sectionMap["write_approval"] = enabled
		if err := config.AtomicConfigWrite(ctx, configHome, configPath, userConfig, ""); err != nil {
			return err
		}
		r.evictCachedAgent(ctx, sessionKey, false, false)
		return nil
	}, nil
}

// Working directory the terminal scope reports, the user home when unset
func terminalCwd(ctx context.Context) (string, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return terminalScopeCwd(ctx, userHome)
}

// Gateway config.yaml path and the platform key of the event's per platform display settings
func displayConfigTarget(ctx context.Context, event *gateway.MessageEvent) (string, string) {
	return filepath.Join(home.Dir(ctx), "config.yaml"), platformConfigKey(event.Source.Platform)
}
