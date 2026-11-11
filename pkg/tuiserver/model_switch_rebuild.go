package tuiserver

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Replaces one completed failed build generation and starts its retry, false when the generation moved on
func restartCompletedFailedAgentBuild(ctx context.Context, sid string, session *tuigateway.Session, failedReady *pythread.Event) bool {
	if failedReady == nil {
		return false
	}
	restarted := func() bool {
		lock := session.SetDefault("agent_build_lock", &sync.Mutex{}).(*sync.Mutex)
		lock.Lock()
		defer lock.Unlock()
		ready, _ := session.Get("agent_ready").(*pythread.Event)
		if session.Get("agent") != nil || session.Get("agent_error") == nil || ready != failedReady || !failedReady.IsSet() {
			return false
		}
		modelOverride, overrideIsDict := session.Get("model_override").(map[string]any)
		resumeOverrides, resumeIsDict := session.Get("resume_runtime_overrides").(map[string]any)
		if overrideIsDict && resumeIsDict {
			merged := copyDict(resumeOverrides)
			merged["model_override"] = modelOverride
			if providerName := modelOverride["provider"]; truthy(providerName) {
				merged["provider_override"] = providerName
			} else {
				delete(merged, "provider_override")
			}
			session.Set("resume_runtime_overrides", merged)
		}
		session.Set("agent_error", nil)
		session.Set("agent_ready", &pythread.Event{})
		session.Pop("agent_build_started")
		session.Pop("_agent_build_thread")
		return true
	}()
	if !restarted {
		return false
	}
	startAgentBuild(ctx, sid, session)
	return true
}
