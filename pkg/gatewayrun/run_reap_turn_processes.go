package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Kills only the background processes one abandoned turn started, skipped when a newer turn claimed the session
func reapGatewayTurnProcesses(ctx context.Context, taskID string, processBaseline map[string]bool, source string, isStillCurrent func() bool) int {
	if taskID == "" {
		return 0
	}
	if isStillCurrent != nil {
		current, failed := func() (current, failed bool) {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("is_still_current check failed for turn %s (%s); reaping anyway", taskID, source), "error", rec)
					failed = true
				}
			}()
			return isStillCurrent(), false
		}()
		if !failed && !current {
			runLog().Debug(fmt.Sprintf("Skipping reap for turn %s (%s): a newer turn already claimed this session; it owns its own baseline.", taskID, source))
			return 0
		}
	}
	killed, ok := func() (killed int, ok bool) {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Warn(fmt.Sprintf("Failed to reap background processes for turn %s (%s)", taskID, source), "error", rec)
				ok = false
			}
		}()
		env := &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
		return killStartedSince(ctx, env, taskID, processBaseline, source), true
	}()
	if !ok {
		return 0
	}
	if killed > 0 {
		runLog().Warn(fmt.Sprintf("Reaped %d background process(es) created by abandoned turn %s (%s)", killed, taskID, source))
	}
	return killed
}

// Process registry kill of processes a turn started past its baseline
var killStartedSince = func(ctx context.Context, env *tools.Env, taskID string, baseline map[string]bool, source string) int {
	return tools.DefaultProcessRegistry().KillStartedSince(ctx, env, taskID, baseline, source)
}
