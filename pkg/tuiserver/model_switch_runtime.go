package tuiserver

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
)

// Runtime fields a one turn override captures and restores
var runtimeKeys = []string{"model", "provider", "api_key", "base_url", "api_mode"}

// Live model runtime of an agent captured for a one turn restore
func snapshotAgentModelRuntime(a *agent.AIAgent) map[string]any {
	var reasoning, primary any
	if rc := a.ReasoningConfig(); rc != nil {
		reasoning = pycopy.DeepCopy(rc)
	}
	if pr := a.PrimaryRuntime(); pr != nil {
		primary = pycopy.DeepCopy(pr)
	}
	return map[string]any{"model": a.Model(), "provider": a.Provider(), "api_key": a.APIKey(), "base_url": a.BaseURL(),
		"api_mode": a.APIMode(), "reasoning_config": reasoning, "primary_runtime": primary}
}

// Reasoning config value of a snapshot as the agent stores it
func snapshotReasoning(snapshot map[string]any) map[string]any {
	rc, _ := snapshot["reasoning_config"].(map[string]any)
	return rc
}

// Puts back the runtime captured before a one turn override, primary runtime first, else a plain switch
func restoreAgentModelRuntime(ctx context.Context, a *agent.AIAgent, snapshot map[string]any) {
	if len(snapshot) == 0 || a == nil {
		return
	}
	_, hasReasoning := snapshot["reasoning_config"]
	if hasReasoning {
		a.SetReasoningConfig(snapshotReasoning(snapshot))
	}
	if primary, _ := snapshot["primary_runtime"].(map[string]any); len(primary) > 0 {
		restored, err := func() (restored bool, err error) {
			defer func() {
				if r := recover(); r != nil {
					restored, err = false, fmt.Errorf("%s", panicMessage(r))
				}
			}()
			return a.RestorePrimarySnapshot(ctx, primary)
		}()
		if err != nil {
			logger().Debug("TUI one-turn model restore via primary runtime failed", "error", err)
		} else if restored {
			if hasReasoning {
				a.SetReasoningConfig(snapshotReasoning(snapshot))
			}
			return
		}
	}
	field := func(key string) string { return strOr(snapshot[key]) }
	capabilities, _ := snapshot["capabilities"].(map[string]bool)
	if err := a.SwitchModel(ctx, field("model"), field("provider"), field("api_key"), field("base_url"), field("api_mode"),
		capabilities, false, false, ""); err != nil {
		panic(err)
	}
	if hasReasoning {
		a.SetReasoningConfig(snapshotReasoning(snapshot))
	}
}
