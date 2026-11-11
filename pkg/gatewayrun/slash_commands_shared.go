package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Cached agent of the session peeked without evicting it, nil without a lock unless the lockless read is asked
func (r *GatewayRunner) cachedAgentFor(sessionKey string, locklessFallback bool) *agent.AIAgent {
	if r.agentCache == nil || (r.agentCacheLock == nil && !locklessFallback) {
		return nil
	}
	if r.agentCacheLock != nil {
		r.agentCacheLock.Lock()
		defer r.agentCacheLock.Unlock()
	}
	entry, ok := r.agentCache.Get(sessionKey)
	if !ok {
		return nil
	}
	return entry.agent
}

// Thread and reply metadata of an outbound send anchored on the event
func (r *GatewayRunner) replyMetadata(event *gateway.MessageEvent) map[string]any {
	return r.threadMetadataForSource(event.Source, gateway.ReplyAnchorForEvent(event))
}

// Prefix users can always type to reach commands on the platform, the adapter's own or a slash
func (r *GatewayRunner) typedCommandPrefixFor(platform gateway.Platform) string {
	if r.adapters == nil {
		return "/"
	}
	adapter, ok := r.adapters.Get(platform)
	if !ok || adapter == nil || adapter.Base() == nil {
		return "/"
	}
	return adapter.Base().Traits.TypedCommandPrefix
}

// Runs fn as a fire and forget task held in the background set until it finishes
func (r *GatewayRunner) trackBackgroundTask(ctx context.Context, name string, fn func(ctx context.Context) (any, error)) *gatewayTask {
	return r.retainBackgroundTask(spawnGatewayTask(context.WithoutCancel(ctx), name, fn))
}
