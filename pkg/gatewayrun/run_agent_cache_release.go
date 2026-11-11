package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Writes the live in flight agent count to the runtime status and feeds the running chats peak
func (r *GatewayRunner) persistActiveAgents(ctx context.Context) {
	writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{"active_agents": r.activeWorkCount()}})
	var keys []string
	for _, item := range r.runningAgentItems() {
		keys = append(keys, item.sessionKey)
	}
	observability.ObserveRunningChats(ctx, keys)
}

// Republishes the in flight count once owner work the session authority counts ends
func (r *GatewayRunner) PersistActiveAgents() {
	r.persistActiveAgents(context.Background())
}

// Pops every per running agent state of the session, a stale generation leaving a newer run untouched
func (r *GatewayRunner) releaseRunningAgentState(ctx context.Context, sessionKey string, runGeneration *int) bool {
	if sessionKey == "" || (runGeneration != nil && !r.isSessionRunCurrent(sessionKey, *runGeneration)) {
		return false
	}
	if state := r.peekSessionState(sessionKey); state != nil {
		if state.turn.lease != nil {
			gateway.ReleaseSoon(state.turn.lease)
		}
		state.turn.clear()
		retired := state.turn.retiredAgents
		state.turn.retiredAgents = nil
		for _, a := range retired {
			r.spawnReleaseThread(ctx, func(ctx context.Context) { r.releaseEvictedAgentSoft(ctx, a) }, sessionKey)
		}
	}
	r.persistActiveAgents(ctx)
	return true
}

// Releases the running slot, evicts the cached agent and sweeps lease tokens of older generations
func (r *GatewayRunner) dropTurnSlot(ctx context.Context, sessionKey string, runGeneration *int) {
	r.releaseRunningAgentState(ctx, sessionKey, runGeneration)
	r.evictCachedAgent(ctx, sessionKey, false, false)
	state := r.peekSessionState(sessionKey)
	registry := r.turnLeases
	if state == nil || registry == nil {
		return
	}
	current := state.persistent.runGeneration
	tokens := state.turn.leaseTokens
	for gen, token := range tokens {
		if gen >= current {
			continue
		}
		delete(tokens, gen)
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Failed to release displaced turn lease gen %d for %s", gen, sessionKey), "error", rec)
				}
			}()
			registry.Release(token)
		}()
	}
}

// Interrupts the current run and clears the queued session state consistently
func (r *GatewayRunner) interruptAndClearSession(ctx context.Context, sessionKey string, source *gateway.SessionSource, interruptReason, invalidationReason string, releaseRunningState bool) error {
	if sessionKey == "" {
		return nil
	}
	state := r.peekSessionState(sessionKey)
	var running *agent.AIAgent
	if state != nil {
		running = state.turn.agent
	}
	generationAtInterrupt := r.interruptRunningTurn(ctx, sessionKey, interruptReason, invalidationReason, "")
	parentSessionID := ""
	if running != nil && running != agentPendingSentinel {
		parentSessionID = running.SessionID()
	}
	tools.InterruptAsyncDelegationsForSession(sessionKey, "", parentSessionID, invalidationReason)
	if running != nil && running != agentPendingSentinel {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("agent_loop_stopped hook dispatch failed", "error", rec)
				}
			}()
			plugins.InvokeHook(ctx, "agent_loop_stopped", map[string]any{
				"session_key": sessionKey, "platform": string(source.Platform),
				"reason": interruptReason, "invalidation_reason": invalidationReason,
			})
		}()
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	if adapter != nil {
		base := adapter.Base()
		base.InterruptSessionActivity(ctx, sessionKey, source.ChatID, r.threadMetadataForSource(source, nil))
		var wake *gateway.MessageEvent
		if parked := base.GetPendingMessage(sessionKey); parked != nil && parked.Internal {
			wake = parked
		}
		if wake == nil {
			if overflow := r.overflowQueue(sessionKey); overflow != nil {
				for i, e := range *overflow {
					if e != nil && e.Internal {
						wake = e
						*overflow = append((*overflow)[:i:i], (*overflow)[i+1:]...)
						break
					}
				}
			}
		}
		if wake != nil {
			base.SetPendingMessage(sessionKey, wake)
		}
	}
	if state != nil {
		state.persistent.pendingCommandText = nil
	}
	if releaseRunningState {
		r.dropTurnSlot(ctx, sessionKey, &generationAtInterrupt)
	}
	return nil
}

// Rebaselines the cached agent's stored message count after this turn when the same agent is still cached
func (r *GatewayRunner) refreshAgentCacheMessageCount(ctx context.Context, sessionKey, sessionID string) {
	db := r.sessionDB(ctx)
	if db == nil || sessionID == "" || r.agentCacheLock == nil || r.agentCache == nil {
		return
	}
	row, err := db.DB().GetSession(ctx, sessionID)
	if err != nil || row == nil {
		return
	}
	live, ok := pythonIntField(row.GetOr("message_count", int64(0)))
	if !ok {
		return
	}
	r.agentCacheLock.Lock()
	defer r.agentCacheLock.Unlock()
	cached, found := r.agentCache.Get(sessionKey)
	if !found || cached.msgCount == nil || cached.agent == agentPendingSentinel {
		return
	}
	if (cached.sessionID != nil && *cached.sessionID != sessionID) || *cached.msgCount == live {
		return
	}
	cached.msgCount = &live
	r.agentCache.Set(sessionKey, cached)
}

// Rebaselines the cached agent of a route after a completion note wrote its rows
func (r *GatewayRunner) RefreshAgentCacheMessageCount(ctx context.Context, route, target string) error {
	r.refreshAgentCacheMessageCount(ctx, route, target)
	return nil
}

// Integer value of a stored count column, false for a value that is not an integer
func pythonIntField(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), n == float64(int(n))
	}
	return 0, false
}

// Adapter that renders who is in its voice channel for prompt injection
type voiceChannelContextAdapter interface {
	GetVoiceChannelContext(guildID int64) (string, error)
}

// Voice channel now note when the channel state changed, empty when unchanged
func (r *GatewayRunner) voiceChannelSidecarNote(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string) (string, bool, error) {
	if source.Platform != gateway.PlatformDiscord {
		return "", false, nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return "", false, err
	}
	guildID, hasGuild, err := getGuildID(event)
	if err != nil {
		return "", false, err
	}
	voice, capable := adapter.(voiceChannelContextAdapter)
	if !hasGuild || guildID == 0 || adapter == nil || !capable {
		return "", false, nil
	}
	vcNow, err := voice.GetVoiceChannelContext(guildID)
	if err != nil {
		runLog().Debug("voice-channel context read failed", "error", err)
		return "", false, nil
	}
	var vcPrev *string
	if sessionKey != "" {
		state := r.sessionState(sessionKey)
		vcPrev, state.conversation.vcLast = state.conversation.vcLast, &vcNow
	}
	prev := ""
	if vcPrev != nil {
		prev = *vcPrev
	}
	if vcNow == prev {
		return "", false, nil
	}
	shown := vcNow
	if shown == "" {
		shown = "not connected to a voice channel"
	}
	return fmt.Sprintf("[Voice channel now: %s]", shown), true, nil
}
