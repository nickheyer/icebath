package gatewayrun

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears a session's pending slash confirm, the tools slash_confirm module the command layer owns
var SlashConfirmClear func(sessionKey string)

// Records the process task id and baseline a turn spawned its processes under on the agent running it
func (r *GatewayRunner) stampTurnProcess(a *agent.AIAgent, taskID string, baseline map[string]bool) {
	r.turnProcessMu.Lock()
	defer r.turnProcessMu.Unlock()
	if r.turnProcessStamps == nil {
		r.turnProcessStamps = map[*agent.AIAgent]turnProcessStamp{}
	}
	r.turnProcessStamps[a] = turnProcessStamp{taskID: taskID, baseline: baseline}
}

// Clears a finished turn's process stamp so a later interrupt reaps nothing of it
func (r *GatewayRunner) clearTurnProcess(a *agent.AIAgent) {
	r.turnProcessMu.Lock()
	defer r.turnProcessMu.Unlock()
	delete(r.turnProcessStamps, a)
}

// Process task id and baseline stamped on an agent, empty and nil when none
func (r *GatewayRunner) turnProcessOf(a *agent.AIAgent) (string, map[string]bool) {
	r.turnProcessMu.Lock()
	defer r.turnProcessMu.Unlock()
	stamp, ok := r.turnProcessStamps[a]
	if !ok {
		return "", nil
	}
	return stamp.taskID, stamp.baseline
}

// The single conversation boundary funnel clearing conversation scoped state and boundary security state
func (r *GatewayRunner) clearConversationScope(ctx context.Context, sessionKey, reason string) {
	r.conversationBoundary(ctx, sessionKey, reason, false)
}

// Clears conversation scoped and boundary security state, the overflow FIFO kept within the same hold when asked
func (r *GatewayRunner) conversationBoundary(ctx context.Context, sessionKey, reason string, keepOverflow bool) {
	if sessionKey == "" {
		return
	}
	r.withPeekedSessionState(sessionKey, func(s *SessionState) {
		queued := s.conversation.queuedEvents
		s.conversation.clear()
		if keepOverflow {
			s.conversation.queuedEvents = queued
		}
	})
	delete(r.pendingModelNotes, sessionKey)
	delete(r.sessionStallNotified, sessionKey)
	delete(r.transcriptLagStreaks, sessionKey)
	r.clearSessionBoundarySecurityState(ctx, sessionKey)
	runLog().Debug(fmt.Sprintf("Cleared conversation scope for %s (%s)", sessionKey, reason))
}

// Clears per session control state that must not survive a boundary, approvals, confirms and the persisted yolo
func (r *GatewayRunner) clearSessionBoundarySecurityState(ctx context.Context, sessionKey string) {
	if sessionKey == "" {
		return
	}
	delete(r.pendingSkillsReloadNotes, sessionKey)
	r.withPeekedSessionState(sessionKey, func(s *SessionState) {
		s.persistent.approvals = nil
		s.persistent.updatePromptPending = false
	})
	for _, clear := range []struct {
		what string
		fn   func(string)
	}{{"slash-confirm", SlashConfirmClear}, {"approval", tools.ClearSession}} {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Failed to clear %s state for session boundary %s: %v", clear.what, sessionKey, rec))
				}
			}()
			clear.fn(sessionKey)
		}()
	}
	if r.sessionStore != nil {
		if _, err := r.sessionStore.SetSessionYolo(ctx, sessionKey, nil); err != nil {
			runLog().Warn(fmt.Sprintf("Failed to clear persisted yolo for session boundary %s", sessionKey), "error", err)
		}
	}
}

// Hard interrupts the in flight agent, invalidates its generation and reaps the processes its turn spawned
func (r *GatewayRunner) interruptRunningTurn(ctx context.Context, sessionKey, interruptReason, invalidationReason, toolReason string) int {
	running := r.runningSlotAgent(sessionKey)
	taskID, baseline := "", map[string]bool(nil)
	if running != nil && running != agentPendingSentinel {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Warn(fmt.Sprintf("Failed to interrupt running agent for %s; continuing", sessionKey), "error", rec)
				}
			}()
			agent.RequestHardInterrupt(running, interruptReason, toolReason)
		}()
		taskID, baseline = r.turnProcessOf(running)
	}
	generation := r.invalidateSessionRunGeneration(ctx, sessionKey, invalidationReason)
	if taskID != "" && baseline != nil {
		go reapGatewayTurnProcesses(context.WithoutCancel(ctx), taskID, baseline, "gateway_turn_interrupt",
			func() bool { return r.isSessionRunCurrent(sessionKey, generation) })
	}
	return generation
}

// Restores a persisted model override after a restart, its credentials resolved again since they are never stored
func (r *GatewayRunner) rehydrateSessionModelOverride(ctx context.Context, sessionKey string) {
	store := r.sessionStore
	if r.sessionModelOverride(sessionKey) != nil || store == nil {
		return
	}
	persisted, failed := func() (obj map[string]any, failed bool) {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("Failed to read persisted session model override", "error", rec)
				failed = true
			}
		}()
		raw := store.GetModelOverride(ctx, sessionKey)
		if raw == nil {
			return nil, false
		}
		out := map[string]any{}
		for _, k := range raw.Keys {
			out[k], _ = raw.Get(k)
		}
		return out, false
	}()
	if failed || len(persisted) == 0 {
		return
	}
	override := map[string]any{"model": persisted["model"], "provider": persisted["provider"], "base_url": persisted["base_url"]}
	providerName, _ := persisted["provider"].(string)
	baseURL, _ := override["base_url"].(string)
	if provider.IsForeignProviderEndpoint(providerName, baseURL) {
		override["base_url"] = nil
	}
	if providerName != "" {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Credential re-resolution failed for persisted override (provider=%s); using credential-less override", providerName), "error", rec)
				}
			}()
			targetModel, _ := persisted["model"].(string)
			runtime, err := resolveRuntimeAgentKwargsForProvider(ctx, providerName, targetModel)
			if err != nil {
				runLog().Debug(fmt.Sprintf("Credential re-resolution failed for persisted override (provider=%s); using credential-less override", providerName), "error", err)
				return
			}
			for _, k := range []string{"api_key", "api_mode", "credential_pool", "requested_provider", "max_tokens"} {
				override[k] = runtime[k]
			}
			requestOverrides, _ := runtime["request_overrides"].(map[string]any)
			override["request_overrides"] = maps.Clone(requestOverrides)
			if override["request_overrides"] == nil {
				override["request_overrides"] = map[string]any{}
			}
			override["capabilities"] = capabilityMap(runtime["capabilities"])
			if override["capabilities"] == nil {
				override["capabilities"] = map[string]any{}
			}
			if !pyrepr.Truthy(override["base_url"]) || provider.LlamacppAliases[strings.ToLower(strings.TrimSpace(providerName))] {
				override["base_url"] = runtime["base_url"]
			}
			if provider.OpenCodeFamily(providerName) != "" && pyrepr.Truthy(override["base_url"]) {
				apiMode, _ := override["api_mode"].(string)
				override["base_url"] = provider.NormalizeOpenCodeBaseURL(providerName, provider.APIMode(apiMode), pyrepr.Str(override["base_url"]))
			}
		}()
	}
	adopted := false
	r.withSessionState(sessionKey, func(s *SessionState) {
		if s.conversation.modelOverride == nil {
			s.conversation.modelOverride, adopted = override, true
		}
	})
	if !adopted {
		return
	}
	runLog().Info(fmt.Sprintf("Rehydrated persisted /model override for session=%s: model=%s provider=%s", sessionKey, pyrepr.Str(override["model"]), providerName))
}

// Layers a session model override onto the model and runtime kwargs, skipping unset fields
func (r *GatewayRunner) applySessionModelOverride(ctx context.Context, sessionKey, model string, runtimeKwargs map[string]any) (string, map[string]any) {
	override := r.sessionModelOverride(sessionKey)
	if len(override) == 0 {
		return model, runtimeKwargs
	}
	if m, ok := override["model"]; ok {
		model, _ = m.(string)
	}
	for _, key := range overrideApplyKeys {
		if v := override[key]; v != nil {
			runtimeKwargs[key] = v
		}
	}
	if ro, ok := override["request_overrides"]; ok {
		if m, isMap := ro.(map[string]any); isMap && len(m) > 0 {
			runtimeKwargs["request_overrides"] = maps.Clone(m)
		} else {
			runtimeKwargs["request_overrides"] = ro
		}
	}
	if pyrepr.Truthy(runtimeKwargs["api_key"]) && runtimeKwargs["credential_pool"] == nil && pyrepr.Truthy(override["provider"]) {
		if pool := credentialPoolForProvider(ctx, pyrepr.Str(override["provider"])); pool != nil {
			runtimeKwargs["credential_pool"] = pool
		} else {
			runtimeKwargs["credential_pool"] = nil
		}
	}
	return model, runtimeKwargs
}

// Model the agent was built to run, canonicalized on its primary route, for the post turn fallback check
func (r *GatewayRunner) fallbackBaselineModel(ctx context.Context, sessionKey string, source *gateway.SessionSource, a *agent.AIAgent, policyModel string) string {
	primary := a.PrimaryRuntime()
	providerName := a.Provider()
	if v, ok := primary["provider"]; ok {
		providerName, _ = v.(string)
	}
	override := r.sessionModelOverride(sessionKey)
	overrideProvider, hasProvider := override["provider"]
	var model string
	switch {
	case pyrepr.Truthy(override["model"]) && (!hasProvider || overrideProvider == nil || overrideProvider == "" || overrideProvider == providerName):
		model = pyrepr.Str(override["model"])
	case policyModel != "":
		model = policyModel
	case source != nil:
		model = r.resolveModelForChannel(ctx, source.Platform, source.ChatID, nil, nonEmpty(source.ThreadID), nonEmpty(source.ParentChatID))
	default:
		model = gateway.ResolveGatewayModel(ctx, nil)
	}
	func() {
		defer func() { _ = recover() }()
		if providerName != "" && !provider.IsAggregatorModelProvider(providerName) {
			model = provider.NormalizeModelForProvider(model, providerName)
		}
	}()
	func() {
		defer func() { _ = recover() }()
		baseURL := a.BaseURL()
		if v, ok := primary["base_url"]; ok {
			baseURL, _ = v.(string)
		}
		model = provider.PinModelForRoute(providerName, baseURL, model)
	}()
	return model
}

// The pointer itself when it names a non empty string, else nil
func nonEmpty(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}
