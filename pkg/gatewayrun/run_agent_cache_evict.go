package gatewayrun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	agentCacheMaxSize     = 128
	agentCacheIdleTTLSecs = 3600.0
)

// Agent of a cache entry, nil for an absent entry
func firstAgent(entry *agentCacheEntry) *agent.AIAgent {
	if entry == nil {
		return nil
	}
	return entry.agent
}

// Agent the running slot of a key holds and whether it is the pending claim sentinel
func (r *GatewayRunner) RunningAgent(key string) (any, bool) {
	state := r.peekSessionState(key)
	if state == nil || state.turn.agent == nil {
		return nil, false
	}
	if state.turn.agent == agentPendingSentinel {
		return nil, true
	}
	return state.turn.agent, false
}

// Removes a cached agent and soft releases it off the lock, a mid turn agent retired after its turn
func (r *GatewayRunner) evictCachedAgent(ctx context.Context, sessionKey string, commitMemory, releaseAfterTurn bool) {
	state := r.peekSessionState(sessionKey)
	if state != nil {
		state.conversation.ephemeralPin = nil
		state.conversation.vcLast = nil
	}
	var evicted *agentCacheEntry
	pop := func() {
		if r.agentCache == nil {
			return
		}
		if entry, ok := r.agentCache.Get(sessionKey); ok {
			r.agentCache.Delete(sessionKey)
			evicted = &entry
		}
	}
	if r.agentCacheLock != nil {
		r.agentCacheLock.Lock()
		pop()
		r.agentCacheLock.Unlock()
	} else {
		pop()
	}
	a := firstAgent(evicted)
	if a == nil || a == agentPendingSentinel {
		return
	}
	if _, running := r.runningAgentIDs()[a]; running {
		if releaseAfterTurn && state != nil {
			state.turn.retiredAgents = append(state.turn.retiredAgents, a)
		}
		return
	}
	var target func(context.Context)
	if commitMemory {
		target = func(ctx context.Context) { r.commitThenReleaseSoft(ctx, a, sessionKey, true) }
	} else {
		target = func(ctx context.Context) { r.releaseEvictedAgentSoft(ctx, a) }
	}
	r.spawnReleaseThread(ctx, target, sessionKey)
}

// Evicts the cached agent of a route, the session retirement surface
func (r *GatewayRunner) EvictCachedAgent(route string) {
	r.evictCachedAgent(context.Background(), route, false, false)
}

// Evicts the cached agent with its memory committed first, reset and compress ending the tracked conversation
func (r *GatewayRunner) evictCachedAgentAtBoundary(ctx context.Context, sessionKey string) {
	r.evictCachedAgent(ctx, sessionKey, true, false)
}

// Evicts the cached agent of a route with its memory committed, the session boundary surface
func (r *GatewayRunner) EvictCachedAgentAtBoundary(route string) {
	r.evictCachedAgentAtBoundary(context.Background(), route)
}

// Runs a release on its own goroutine inside the profile that owns the session key
func (r *GatewayRunner) spawnReleaseThread(ctx context.Context, target func(context.Context), sessionKey string) {
	go r.runReleaseInProfileScope(context.WithoutCancel(ctx), target, sessionKey)
}

// Calls target under the profile owning the session key, entered unless the current scope already is the owner's
func (r *GatewayRunner) runReleaseInProfileScope(ctx context.Context, target func(context.Context), sessionKey string) {
	if provider.IsMultiplexActive(ctx) {
		owner := ""
		if sessionKey != "" && r.sessionStore != nil {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						runLog().Warn(fmt.Sprintf("Could not resolve the owning profile for %s; releasing under the default profile", sessionKey), "error", rec)
					}
				}()
				owner = r.sessionStore.ProfileHomeForKey(sessionKey)
			}()
		}
		ownerHome := owner
		if ownerHome == "" {
			ownerHome = home.Root()
		}
		if provider.CurrentSecretScope(ctx) == nil || home.Key(home.Dir(ctx)) != home.Key(ownerHome) {
			ctx = gateway.ProfileRuntimeScope(ctx, ownerHome, true)
		}
	}
	target(ctx)
}

// Commits the live transcript to memory providers before a resource only eviction, a boundary also ending the engine session
func (r *GatewayRunner) commitMemoryBeforeSoftEvict(ctx context.Context, a *agent.AIAgent, key string, boundary bool) {
	if a == nil {
		return
	}
	hasProvider := a.MemoryManager() != nil
	hasEngine := boundary && a.ContextCompressor() != nil
	if !hasProvider && !hasEngine {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("Pre-evict memory commit failed for %s: %v", key, rec))
		}
	}()
	a.CommitMemorySession(ctx, agent.SessionMessageDicts(a.SessionMessages()))
	runLog().Debug(fmt.Sprintf("Committed on_session_end extraction before soft-evicting session=%s (resource eviction)", key))
}

// Commits end of session memory then soft releases, the commit needing the live memory manager first
func (r *GatewayRunner) commitThenReleaseSoft(ctx context.Context, a *agent.AIAgent, key string, boundary bool) {
	r.commitMemoryBeforeSoftEvict(ctx, a, key, boundary)
	r.releaseEvictedAgentSoft(ctx, a)
}

// Frees an evicted agent's clients and transcript while its sandbox, browser and processes outlive it
func (r *GatewayRunner) releaseEvictedAgentSoft(ctx context.Context, a *agent.AIAgent) {
	if a == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		a.ReleaseClients(ctx)
	}()
	a.ClearSessionMessages()
	a.ClearDBFlushScanPrefix()
}

// Operator configured agent cache bounds resolved once per runner, an unreadable config resolving as empty
func (r *GatewayRunner) agentCacheBounds(ctx context.Context) gateway.AgentCacheBounds {
	if r.agentCacheBoundsCache != nil {
		return *r.agentCacheBoundsCache
	}
	bounds := func() (bounds gateway.AgentCacheBounds) {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("Agent cache bounds config read failed: %v", rec))
				bounds = gateway.ResolveAgentCacheBounds(map[string]any{})
			}
		}()
		return gateway.ResolveAgentCacheBounds(gateway.LoadGatewayConfigMap(ctx, ""))
	}()
	r.agentCacheBoundsCache = &bounds
	return bounds
}

// Effective cache cap, the configured override else the default
func (r *GatewayRunner) agentCacheCap(ctx context.Context) int {
	if b := r.agentCacheBounds(ctx); b.MaxSize != nil && *b.MaxSize != 0 {
		return *b.MaxSize
	}
	return agentCacheMaxSize
}

// Effective idle TTL in seconds, the configured override else the default
func (r *GatewayRunner) agentCacheIdleTTL(ctx context.Context) float64 {
	if b := r.agentCacheBounds(ctx); b.IdleTTLSecs != nil && *b.IdleTTLSecs != 0 {
		return *b.IdleTTLSecs
	}
	return agentCacheIdleTTLSecs
}

// True when the live transcript is fully on disk so a soft eviction can rebuild it
func transcriptPersistenceCaughtUp(a *agent.AIAgent) bool {
	return a != nil && a.LastFlushedDBIdx() >= len(a.SessionMessages())
}

// Sheds least recently used cached transcripts once anonymous RSS crosses the budget, returning the count evicted
func (r *GatewayRunner) sweepAgentCacheUnderPressure(ctx context.Context) int {
	bounds := r.agentCacheBounds(ctx)
	if bounds.MemoryHighMB == nil || *bounds.MemoryHighMB == 0 || r.agentCache == nil || r.agentCache.Len() == 0 || r.agentCacheLock == nil {
		return 0
	}
	rssMB := gateway.ReadAnonRSSMB()
	if rssMB == nil || *rssMB < *bounds.MemoryHighMB {
		return 0
	}
	runningIDs := r.runningAgentIDs()
	isLive := func(a *agent.AIAgent) bool {
		_, running := runningIDs[a]
		return a != nil && a != agentPendingSentinel && !running
	}
	isEvictable := func(_ string, a *agent.AIAgent) bool { return isLive(a) && transcriptPersistenceCaughtUp(a) }
	r.agentCacheLock.Lock()
	ordered := []gateway.PressureEntry[*agent.AIAgent]{}
	for key, entry := range r.agentCache.All() {
		ordered = append(ordered, gateway.PressureEntry[*agent.AIAgent]{Key: key, Agent: entry.agent})
	}
	plan := gateway.PlanPressureEvictions(ordered, isEvictable, bounds.MaxEvictionsPerPass, bounds.ProtectRecent)
	for _, e := range plan {
		r.agentCache.Delete(e.Key)
	}
	r.agentCacheLock.Unlock()
	if len(plan) == 0 {
		midTurn, unflushed := 0, 0
		for _, e := range ordered {
			if _, running := runningIDs[e.Agent]; e.Agent != nil && running {
				midTurn++
			}
			if isLive(e.Agent) && !transcriptPersistenceCaughtUp(e.Agent) {
				unflushed++
			}
		}
		tail := " — memory will keep climbing until those turns finish."
		if unflushed > 0 && midTurn == 0 {
			tail = " — transcripts are not reaching the session DB (session persistence disabled or failing?); the memory valve cannot shed sessions until they persist."
		}
		runLog().Warn(fmt.Sprintf("Agent cache pressure: anon RSS %dMB over budget %dMB but no evictable session (%d cached, %d mid-turn, %d blocked on un-flushed persistence)%s",
			*rssMB, *bounds.MemoryHighMB, len(ordered), midTurn, unflushed, tail))
		return 0
	}
	keys := make([]string, len(plan))
	for i, e := range plan {
		keys[i] = e.Key
	}
	runLog().Warn(fmt.Sprintf("Agent cache pressure: anon RSS %dMB over budget %dMB — evicting %d LRU session(s): %s",
		*rssMB, *bounds.MemoryHighMB, len(plan), strings.Join(keys, ", ")))
	go r.releasePressureBatch(context.WithoutCancel(ctx), plan)
	return len(plan)
}

// Releases a pressure evicted batch in least recently used order on one goroutine, then trims the heap
func (r *GatewayRunner) releasePressureBatch(ctx context.Context, plan []gateway.PressureEntry[*agent.AIAgent]) {
	for len(plan) > 0 {
		entry := plan[0]
		plan[0] = gateway.PressureEntry[*agent.AIAgent]{}
		plan = plan[1:]
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Pressure release failed for %s: %v", entry.Key, rec))
				}
			}()
			r.runReleaseInProfileScope(ctx, func(ctx context.Context) { r.commitThenReleaseSoft(ctx, entry.Agent, entry.Key, false) }, entry.Key)
		}()
	}
	func() {
		defer func() { _ = recover() }()
		memory.TrimMemory(ctx, &tools.Env{Home: home.Dir(ctx)}, memory.TrimOptions{Force: true, Reason: "agent_cache_pressure"})
	}()
}

// Evicts the oldest cached agents past the cap with the cache lock held, mid turn agents skipped
func (r *GatewayRunner) enforceAgentCacheCap(ctx context.Context) {
	if r.agentCache == nil {
		return
	}
	runningIDs := r.runningAgentIDs()
	limit := r.agentCacheCap(ctx)
	keys := r.agentCache.Keys()
	type planned struct {
		key   string
		agent *agent.AIAgent
	}
	var evictPlan []planned
	for _, key := range keys[:max(0, len(keys)-limit)] {
		entry, _ := r.agentCache.Get(key)
		if _, running := runningIDs[entry.agent]; entry.agent == nil || !running {
			evictPlan = append(evictPlan, planned{key, entry.agent})
		}
	}
	for _, p := range evictPlan {
		r.agentCache.Delete(p.key)
	}
	if over := r.agentCache.Len() - limit; over > 0 {
		runLog().Warn(fmt.Sprintf("Agent cache over cap (%d > %d); %d excess slot(s) held by mid-turn agents — will re-check on next insert.",
			r.agentCache.Len(), limit, over))
	}
	for _, p := range evictPlan {
		runLog().Info(fmt.Sprintf("Agent cache at cap; evicting LRU session=%s (cache_size=%d)", p.key, r.agentCache.Len()))
		if p.agent != nil {
			a, key := p.agent, p.key
			r.spawnReleaseThread(ctx, func(ctx context.Context) { r.commitThenReleaseSoft(ctx, a, key, false) }, key)
		}
	}
}

// Evicts cached agents idle past the TTL with mid turn agents skipped, returning the count evicted
func (r *GatewayRunner) sweepIdleCachedAgents(ctx context.Context) int {
	if r.agentCache == nil || r.agentCacheLock == nil {
		return 0
	}
	now := float64(time.Now().UnixNano()) / 1e9
	idleTTL := r.agentCacheIdleTTL(ctx)
	runningIDs := r.runningAgentIDs()
	type planned struct {
		key   string
		agent *agent.AIAgent
	}
	var toEvict []planned
	r.agentCacheLock.Lock()
	for key, entry := range r.agentCache.All() {
		a := entry.agent
		if _, running := runningIDs[a]; a == nil || running {
			continue
		}
		last := a.LastActivityTS()
		if now-last <= idleTTL {
			continue
		}
		toEvict = append(toEvict, planned{key, a})
	}
	for _, p := range toEvict {
		r.agentCache.Delete(p.key)
	}
	r.agentCacheLock.Unlock()
	for _, p := range toEvict {
		runLog().Info(fmt.Sprintf("Agent cache idle-TTL evict: session=%s (idle=%.0fs)", p.key, now-p.agent.LastActivityTS()))
		a, key := p.agent, p.key
		r.spawnReleaseThread(ctx, func(ctx context.Context) { r.commitThenReleaseSoft(ctx, a, key, false) }, key)
	}
	return len(toEvict)
}
