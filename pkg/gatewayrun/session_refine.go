package gatewayrun

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent constructor every gateway run path builds its agents with
var newAIAgent = agent.NewAIAgent

// Runtime keys a side agent takes from its frozen policy route
var sideRuntimeKeys = []string{"api_key", "base_url", "provider", "requested_provider", "api_mode", "command", "args",
	"credential_pool", "max_tokens", "capabilities"}

// Authority and live session of the local session the source belongs to, nils for a messaging chat
func localSession(r *GatewayRunner, source *gateway.SessionSource) (*gateway.SessionAuthority, *gateway.LiveSession) {
	if source == nil || source.Platform != gateway.PlatformLocal {
		return nil, nil
	}
	return gateway.LocalAuthorityLive(r, source)
}

// Side agent holder slot the ledger reads, set once the side agent is built
type sideAgentSlot struct {
	mu    sync.Mutex
	agent *agent.AIAgent
}

func (s *sideAgentSlot) set(a *agent.AIAgent) {
	s.mu.Lock()
	s.agent = a
	s.mu.Unlock()
}

// True when the held agent reports itself abandoned
func (s *sideAgentSlot) Abandoned() bool {
	s.mu.Lock()
	held := s.agent
	s.mu.Unlock()
	if held == nil {
		return false
	}
	abandoner, ok := any(held).(interface{ Abandoned() bool })
	return ok && abandoner.Abandoned()
}

// Side agent the stop paths interrupt, nil until it is built
func (s *sideAgentSlot) HolderAgent() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil {
		return nil
	}
	return s.agent
}

// Fresh agent on the session's frozen route with its own task session, built inside the side scope
func (r *GatewayRunner) sideAgent(ctx context.Context, a *gateway.SessionAuthority, policy *gateway.LocalSessionPolicy, live *gateway.LiveSession,
	taskID string, env *tools.Env, overrides map[string]any) (*agent.AIAgent, error) {
	model, runtime, err := resolvePolicyAgentRuntime(ctx, r, policy, true)
	if err != nil {
		return nil, err
	}
	config, _, err := policy.EffectiveFor(ctx, a, true)
	if err != nil {
		return nil, err
	}
	pr, err := gateway.SessionRouting(ctx, a, policy)
	if err != nil {
		return nil, err
	}
	kwargs := map[string]any{"model": model}
	add := func(values map[string]any) error {
		for k, v := range values {
			if _, dup := kwargs[k]; dup {
				return &pyTypeError{fmt.Sprintf("AIAgent() got multiple values for keyword argument '%s'", k)}
			}
			kwargs[k] = v
		}
		return nil
	}
	route := map[string]any{}
	for _, key := range sideRuntimeKeys {
		if value, present := runtime[key]; present {
			route[key] = value
		}
	}
	if err := add(route); err != nil {
		return nil, err
	}
	if err := add(gateway.CheckpointAgentKwargs(config)); err != nil {
		return nil, err
	}
	toolsets, err := policy.CurrentToolsets(ctx, config)
	if err != nil {
		return nil, err
	}
	agentCfg, _ := pyrepr.Or(config["agent"], map[string]any{}).(map[string]any)
	var disabled any
	if parsed := tools.ParseConfigStringList(agentCfg["disabled_toolsets"]); len(parsed) > 0 {
		disabled = parsed
	}
	reasoning, err := policy.CurrentReasoningConfig(ctx, r, live.Route)
	if err != nil {
		return nil, err
	}
	var reasoningValue any
	if reasoning != nil {
		reasoningValue = reasoning
	}
	tier, err := policy.CurrentServiceTier(ctx, r, live.Route)
	if err != nil {
		return nil, err
	}
	var tierValue any
	if tier != nil {
		tierValue = *tier
	}
	requestOverrides := map[string]any{}
	if overridesCfg, ok := pyrepr.Or(runtime["request_overrides"], map[string]any{}).(map[string]any); ok {
		for k, v := range overridesCfg {
			requestOverrides[k] = v
		}
	}
	requireParameters, present := pr["require_parameters"]
	if !present {
		requireParameters = false
	}
	if err := add(map[string]any{
		"max_iterations": policy.MaxTurns(), "quiet_mode": true, "verbose_logging": false,
		"enabled_toolsets": append([]string{}, toolsets...), "disabled_toolsets": disabled,
		"reasoning_config": reasoningValue, "service_tier": tierValue, "request_overrides": requestOverrides,
		"providers_allowed": pr["only"], "providers_ignored": pr["ignore"], "providers_order": pr["order"],
		"provider_sort": pr["sort"], "provider_require_parameters": requireParameters, "provider_data_collection": pr["data_collection"],
		"session_id": taskID, "platform": policy.Platform, "session_db": a.DB, "fallback_model": r.refreshFallbackModel(ctx),
		"load_soul_identity": !policy.IgnoreRules(), "skip_memory": policy.IgnoreRules(), "side_agent": true,
	}); err != nil {
		return nil, err
	}
	for k, v := range overrides {
		kwargs[k] = v
	}
	opts, err := agent.AIAgentOptionsFromKwargs(kwargs)
	if err != nil {
		return nil, err
	}
	opts.Env = env
	return newAIAgent(ctx, opts)
}

// Installs a fresh review run on the session's review guard, nil while an earlier review still holds it
func prepareLiveReviewRun(live *gateway.LiveSession) *agent.BackgroundReviewRun {
	run := agent.NewBackgroundReviewRun()
	live.ReviewLock.Lock()
	defer live.ReviewLock.Unlock()
	if current := live.ReviewRun; current != nil && !current.RequestDone() {
		return nil
	}
	live.ReviewRun = run
	return run
}

// Publishes one run's request exit on the session's review guard without clearing a successor
func finishLiveReviewRun(live *gateway.LiveSession, run *agent.BackgroundReviewRun) {
	if run == nil || !run.MarkRequestFinished() {
		return
	}
	live.ReviewLock.Lock()
	if live.ReviewRun == run {
		live.ReviewRun = nil
	}
	live.ReviewLock.Unlock()
	run.SetRequestDone()
}

// Refine reply of a local session reviewing its stored transcript on a side agent, nil for other sources
func (r *GatewayRunner) refineLocalSession(ctx context.Context, source *gateway.SessionSource, focus string) (*string, error) {
	authority, live := localSession(r, source)
	if live == nil {
		return nil, nil
	}
	policy, err := gateway.LocalPolicyForSource(ctx, r, live.Source)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, nil
	}
	ref := gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: source.ChatID}
	target, err := gateway.TranscriptTarget(ctx, authority, ref)
	if err != nil {
		return nil, err
	}
	history, err := authority.DB.GetMessagesAsConversation(ctx, target, state.ConversationOptions{IncludeAncestors: true})
	if err != nil {
		return nil, err
	}
	reply := func(text string) (*string, error) { return &text, nil }
	if len(history) == 0 {
		return reply(i18n.T(ctx, "gateway.refine.nothing_yet", "", nil))
	}
	run := prepareLiveReviewRun(live)
	tail := ""
	if focus != "" {
		tail = i18n.T(ctx, "gateway.refine.focus_suffix", "", map[string]any{"focus": focus})
	}
	if run == nil {
		return reply(i18n.T(ctx, "gateway.refine.started", "", map[string]any{"focus_suffix": tail}))
	}
	taskID := "refine_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
	holder := &sideAgentSlot{}
	publish := func(message string) {
		if current, ok := authority.Live(ref.SessionID); ok && current != nil {
			current.EventStream.Publish(ref.SessionID, pyjson.ObjectOf("text", message), "review.summary", nil)
		}
	}
	var focusArg *string
	if focus != "" {
		focusArg = &focus
	}
	reviewBody := func(bodyCtx context.Context) error {
		return gateway.SideScope(bodyCtx, authority, policy, taskID, func(scoped context.Context, env *tools.Env) error {
			side, err := r.sideAgent(scoped, authority, policy, live, taskID, env, map[string]any{"session_id": target, "skip_memory": true})
			if err != nil {
				return err
			}
			holder.set(side)
			side.SetEndSessionOnClose(false)
			side.SetBackgroundReviewCallback(publish)
			side.SetPrintFn(func(string) {})
			defer func() {
				defer func() { _ = recover() }()
				side.ReleaseClients(scoped)
			}()
			review, _, err := agent.SpawnBackgroundReviewThread(scoped, side, agent.CloneBackgroundReviewMessages(history), true,
				side.ValidToolNames()["skill_manage"], focusArg, run, true)
			if err != nil {
				return err
			}
			review(scoped)
			return nil
		})
	}
	body := func(bodyCtx context.Context) (string, error) {
		defer func() {
			finishLiveReviewRun(live, run)
			go func() {
				if err := gateway.RetireAfterReview(context.WithoutCancel(bodyCtx), authority, ref, live); err != nil {
					runLog().Error("Exception in callback retire_after_review", "error", err)
				}
			}()
		}()
		return "", reviewBody(bodyCtx)
	}
	gateway.SpawnSideAgent(ctx, authority, ref, taskID, "review.summary", body, nil, holder)
	return reply(i18n.T(ctx, "gateway.refine.started", "", map[string]any{"focus_suffix": tail}))
}
