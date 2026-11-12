package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent a cache lookup reused and the one it evicted under the lock for an off lock release
type cachedAgentLookup struct {
	agent   *agent.AIAgent
	reused  bool
	evicted *agent.AIAgent
}

// Whether the platform opts out of filesystem context file discovery, a local policy deciding by its ignore rules
func (t *TurnRunner) skipContextFiles(ctx context.Context, platformKey string) (bool, error) {
	policy, err := gateway.LocalPolicyForSource(ctx, t.runner, t.ctx.source)
	if err != nil {
		return false, err
	}
	if policy != nil {
		return policy.IgnoreRules(), nil
	}
	gw, _ := t.ctx.userConfig["gateway"].(map[string]any)
	platforms, ok := gw["platforms"].(map[string]any)
	if !ok {
		return false, nil
	}
	entry, _ := platforms[platformKey].(map[string]any)
	return pyrepr.Truthy(entry["skip_context_files"]), nil
}

// Session id the cached agent was taken for and whether that session is ended in its store
func (t *TurnRunner) cachedSidIsDead(ctx context.Context) (*string, bool) {
	tc := t.ctx
	var peek *string
	if t.runner.agentCacheLock != nil && t.runner.agentCache != nil {
		t.runner.agentCacheLock.Lock()
		entry, found := t.runner.agentCache.Get(tc.sessionKey)
		t.runner.agentCacheLock.Unlock()
		if found {
			peek = entry.sessionID
		}
	}
	dead := false
	if peek != nil && *peek != tc.sessionID && t.runner.sessionStore != nil {
		func() {
			defer func() { _ = recover() }()
			dead = t.runner.sessionStore.IsSessionEndedInDB(ctx, *peek, tc.sessionKey)
		}()
	}
	return peek, dead
}

// Message count the session row holds now, nil without a database or session id
func (t *TurnRunner) currentMessageCount(ctx context.Context) *int {
	db := t.runner.sessionDB(ctx)
	if db == nil || t.ctx.sessionID == "" {
		return nil
	}
	row, err := db.DB().GetSession(ctx, t.ctx.sessionID)
	if err != nil || row == nil || row.Len() == 0 {
		return nil
	}
	count := 0
	if value, present := row.Get("message_count"); present {
		n, isInt := pyIntValue(value)
		if !isInt {
			return nil
		}
		count = n
	}
	return &count
}

// Integer value of a database cell
func pyIntValue(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// Evicts this session's cache entry under the held lock and returns its agent for a deferred soft release
func (t *TurnRunner) popCachedAgentForEviction() *agent.AIAgent {
	entry, found := t.runner.agentCache.Get(t.ctx.sessionKey)
	if !found {
		return nil
	}
	t.runner.agentCache.Delete(t.ctx.sessionKey)
	if entry.agent == nil || entry.agent == agentPendingSentinel {
		return nil
	}
	return entry.agent
}

// Cached agent with a matching signature unless a dead session or a cross process write invalidated it
func (t *TurnRunner) lookupCachedAgent(sig string, maxIterations int, peekSid *string, dead bool, msgCount *int) cachedAgentLookup {
	tc := t.ctx
	var out cachedAgentLookup
	if t.runner.agentCacheLock == nil || t.runner.agentCache == nil {
		return out
	}
	t.runner.agentCacheLock.Lock()
	defer t.runner.agentCacheLock.Unlock()
	cached, found := t.runner.agentCache.Get(tc.sessionKey)
	if !found || cached.signature != sig {
		return out
	}
	sidMismatch := cached.sessionID != nil && *cached.sessionID != tc.sessionID
	switch {
	case sidMismatch && dead && peekSid != nil && *cached.sessionID == *peekSid:
		runLog().Info(fmt.Sprintf("Agent cache invalidated for session %s: cached agent's session_id %s is ended in "+
			"state.db (stale self-heal artifact, #54878 x #54947) — discarding instead of reusing across the routing recovery",
			tc.sessionKey, *cached.sessionID))
	case !sidMismatch && cached.msgCount != nil && msgCount != nil && *msgCount != *cached.msgCount:
		runLog().Info(fmt.Sprintf("Agent cache invalidated for session %s: message_count changed (%d -> %d), possible cross-process write",
			tc.sessionKey, *cached.msgCount, *msgCount))
	default:
		out.agent = cached.agent
		t.runner.agentCache.Delete(tc.sessionKey)
		t.runner.agentCache.Set(tc.sessionKey, cached)
		initCachedAgentForTurn(out.agent, tc.interruptDepth)
		out.agent.SetMaxIterations(maxIterations)
		runLog().Debug("Reusing cached agent for session " + tc.sessionKey)
		out.reused = true
		return out
	}
	out.evicted = t.popCachedAgentForEviction()
	return out
}

// Soft releases an evicted agent off the gateway loop
func (t *TurnRunner) releaseEvictedAgent(ctx context.Context, a *agent.AIAgent) {
	t.runner.spawnReleaseThread(ctx, func(rctx context.Context) { t.runner.releaseEvictedAgentSoft(rctx, a) }, t.ctx.sessionKey)
}

// Fresh agent for the turn route with the session's toolsets, prompt, routing and policy bounds
// Session env of the turn read through the policy terminal scope, so the prompt and tools see the client cwd
func turnAgentEnv(ctx context.Context, tc *TurnContext) *tools.Env {
	return tools.EnvInTerminalScope(ctx, tc.sessionEnv)
}

func (t *TurnRunner) buildFreshAgent(ctx context.Context, route turnRoute, platformKey, combinedEphemeral string, maxIterations int,
	reasoningConfig map[string]any, pr map[string]any, skipContextFiles bool) (*agent.AIAgent, error) {
	tc, runner, src := t.ctx, t.runner, t.ctx.source
	policy, err := gateway.LocalPolicyForSource(ctx, runner, src)
	if err != nil {
		return nil, err
	}
	kwargs := map[string]any{"model": route.model}
	add := func(values map[string]any) error {
		for k, v := range values {
			if _, dup := kwargs[k]; dup {
				return &pyTypeError{fmt.Sprintf("AIAgent() got multiple values for keyword argument '%s'", k)}
			}
			kwargs[k] = v
		}
		return nil
	}
	if err := add(route.runtime); err != nil {
		return nil, err
	}
	if err := add(gateway.CheckpointAgentKwargs(tc.userConfig)); err != nil {
		return nil, err
	}
	var prefill []any
	if policy != nil {
		authority, err := gateway.ActiveAuthority(ctx, runner)
		if err != nil {
			return nil, err
		}
		if prefill, err = gateway.SessionPrefill(ctx, authority, policy, src.ChatID); err != nil {
			return nil, err
		}
	} else {
		prefill = runner.prefillMessages
	}
	var prefillValue any
	if len(prefill) > 0 {
		prefillValue = prefill
	}
	var ephemeral any
	if combinedEphemeral != "" {
		ephemeral = combinedEphemeral
	}
	locked, err := gateway.APIRuntimeLocked(ctx)
	if err != nil {
		return nil, err
	}
	var fallback any
	if locked == nil {
		fallback = runner.refreshFallbackModel(ctx)
	}
	var runBudget any
	if policy != nil {
		budget, err := policy.RunBudgetSeconds(ctx)
		if err != nil {
			return nil, err
		}
		if budget != nil {
			runBudget = *budget
		}
	}
	var serviceTier any
	if runner.serviceTier != nil {
		serviceTier = *runner.serviceTier
	}
	var sessionDB any
	if db := runner.sessionDB(ctx); db != nil {
		sessionDB = db.DB()
	}
	requireParameters, present := pr["require_parameters"]
	if !present {
		requireParameters = false
	}
	ignoreRules := policy != nil && policy.IgnoreRules()
	if err := add(map[string]any{
		"max_iterations": maxIterations, "quiet_mode": !(policy != nil && policy.Verbose()), "verbose_logging": false,
		"enabled_toolsets": tc.enabledToolsets, "disabled_toolsets": tc.disabledToolsets, "ephemeral_system_prompt": ephemeral,
		"prefill_messages": prefillValue, "reasoning_config": reasoningConfig, "service_tier": serviceTier,
		"request_overrides": route.requestOverrides, "providers_allowed": pr["only"], "providers_ignored": pr["ignore"],
		"providers_order": pr["order"], "provider_sort": pr["sort"], "provider_require_parameters": requireParameters,
		"provider_data_collection": pr["data_collection"], "session_id": tc.sessionID, "platform": platformKey,
		"user_id": optAny(src.UserID), "user_id_alt": optAny(src.UserIDAlt), "user_name": optAny(src.UserName),
		"chat_id": src.ChatID, "chat_name": optAny(src.ChatName), "chat_type": src.ChatType, "thread_id": optAny(src.ThreadID),
		"gateway_session_key": tc.sessionKey, "session_db": sessionDB, "fallback_model": fallback,
		"skip_context_files": skipContextFiles, "load_soul_identity": !ignoreRules, "skip_memory": ignoreRules,
		"pass_session_id": policy != nil && policy.PassSessionID(), "run_budget_seconds": runBudget,
	}); err != nil {
		return nil, err
	}
	opts, err := agent.AIAgentOptionsFromKwargs(kwargs)
	if err != nil {
		return nil, err
	}
	opts.Env = turnAgentEnv(ctx, tc)
	return newAIAgent(ctx, opts)
}

// Reuses this session's cached agent or builds a fresh one, the session env applied either way
func (t *TurnRunner) resolveTurnAgent(ctx context.Context, route turnRoute, platformKey, combinedEphemeral string, maxIterations int,
	reasoningConfig map[string]any, pr map[string]any) (*agent.AIAgent, bool, error) {
	tc, runner := t.ctx, t.runner
	skip, err := t.skipContextFiles(ctx, platformKey)
	if err != nil {
		return nil, false, err
	}
	userID, userIDAlt := "", ""
	if tc.source != nil {
		userID, userIDAlt = optText(tc.source.UserID), optText(tc.source.UserIDAlt)
	}
	sig := agentConfigSignature(route.model, route.runtime, tc.enabledToolsets, combinedEphemeral,
		gateway.ExtractCacheBustingConfig(ctx, tc.userConfig), userID, userIDAlt, skip)
	peek, dead := t.cachedSidIsDead(ctx)
	msgCount := t.currentMessageCount(ctx)
	found := t.lookupCachedAgent(sig, maxIterations, peek, dead, msgCount)
	a := found.agent
	locked, err := gateway.APIRuntimeLocked(ctx)
	if err != nil {
		return nil, false, err
	}
	if found.reused && a != nil && locked != nil && a.FallbackActivated() {
		found.evicted, found.reused, a = a, false, nil
		runner.agentCacheLock.Lock()
		runner.agentCache.Delete(tc.sessionKey)
		runner.agentCacheLock.Unlock()
	}
	if found.reused && a != nil {
		var chain []map[string]any
		if locked == nil {
			chain = runner.refreshFallbackModel(ctx)
		}
		applyFallbackChainToAgent(a, chain)
		a.SetToolEnv(turnAgentEnv(ctx, tc))
	}
	if found.evicted != nil {
		t.releaseEvictedAgent(ctx, found.evicted)
	}
	if a == nil {
		inline := tools.GatewayDelegationInline(tc.userConfig, platformKey, tc.interimAssistantMessagesEnabled)
		if a, err = t.buildFreshAgent(tools.GatewayDelegationInlineScope(ctx, inline), route, platformKey, combinedEphemeral,
			maxIterations, reasoningConfig, pr, skip); err != nil {
			return nil, false, err
		}
		if runner.agentCacheLock != nil && runner.agentCache != nil {
			runner.agentCacheLock.Lock()
			sessionID := tc.sessionID
			runner.agentCache.Set(tc.sessionKey, agentCacheEntry{agent: a, signature: sig, msgCount: msgCount, sessionID: &sessionID})
			runner.enforceAgentCacheCap(ctx)
			runner.agentCacheLock.Unlock()
		}
		runLog().Debug(fmt.Sprintf("Created new agent for session %s (sig=%s)", tc.sessionKey, sig))
	}
	return a, found.reused, nil
}
