package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session id and transcript of the session the source belongs to, a local session's physical transcript first
func (r *GatewayRunner) coldReviewHistory(ctx context.Context, source *gateway.SessionSource) (string, []map[string]any, error) {
	authority, live := localSession(r, source)
	if live != nil {
		target, err := gateway.TranscriptTarget(ctx, authority, gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: source.ChatID})
		if err != nil {
			return "", nil, err
		}
		history, err := authority.DB.GetMessagesAsConversation(ctx, target, state.ConversationOptions{IncludeAncestors: true})
		if err != nil {
			return "", nil, err
		}
		return target, history, nil
	}
	entry, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", nil, err
	}
	history, err := r.slashStore().LoadTranscript(ctx, entry.SessionID)
	if err != nil {
		return "", nil, err
	}
	return entry.SessionID, transcriptMaps(history), nil
}

// Session id and transcript of a messaging session without creating one, empty with no user or assistant turn
func (r *GatewayRunner) storedTranscript(ctx context.Context, sessionKey string) (string, []map[string]any, error) {
	sessionID, _ := r.slashStore().PeekSessionID(sessionKey)
	var history []map[string]any
	if sessionID != "" {
		loaded, err := r.slashStore().LoadTranscript(ctx, sessionID)
		if err != nil {
			return "", nil, err
		}
		history = transcriptMaps(loaded)
	}
	for _, message := range history {
		if role := message["role"]; role == "user" || role == "assistant" {
			return sessionID, append([]map[string]any{}, history...), nil
		}
	}
	return sessionID, []map[string]any{}, nil
}

// Runs body with a fresh agent on the session's next turn route in its policy scope, releasing its own clients
func (r *GatewayRunner) coldReviewParent(ctx context.Context, source *gateway.SessionSource, sessionKey, sessionID string,
	body func(ctx context.Context, parent *agent.AIAgent) error) error {
	policy, err := gateway.LocalPolicyForSource(ctx, r, source)
	if err != nil {
		return err
	}
	var authority *gateway.SessionAuthority
	if policy != nil {
		if authority, err = gateway.ActiveAuthority(ctx, r); err != nil {
			return err
		}
	}
	var userConfig map[string]any
	platformKey := ""
	if policy != nil {
		if userConfig, _, err = policy.EffectiveFor(ctx, authority, false); err != nil {
			return err
		}
		platformKey = policy.Platform
	} else {
		userConfig = gateway.LoadGatewayConfigMap(ctx, "")
		platformKey = platformConfigKey(source.Platform)
	}
	enabled, disabled := r.resolveTurnToolsets(ctx, userConfig, source, platformKey)
	pr := r.providerRouting
	prefill := r.prefillMessages
	var tier *string
	if policy != nil {
		if enabled, err = gateway.PolicyTurnToolsets(ctx, authority, policy, userConfig, source.ChatID); err != nil {
			return err
		}
		if pr, err = gateway.SessionRouting(ctx, authority, policy); err != nil {
			return err
		}
		if prefill, err = gateway.SessionPrefill(ctx, authority, policy, source.ChatID); err != nil {
			return err
		}
		if tier, err = policy.CurrentServiceTier(ctx, r, sessionKey); err != nil {
			return err
		}
	} else if tier, err = r.resolveSessionServiceTier(ctx, source, sessionKey); err != nil {
		return err
	}
	return gateway.PolicyScope(ctx, policy, authority, func(scoped context.Context) error {
		model, runtime, err := r.resolveSessionAgentRuntime(scoped, source, sessionKey, userConfig, true)
		if err != nil {
			return err
		}
		route := r.resolveTurnAgentConfig(model, runtime, turnTier{value: tier})
		kwargs := map[string]any{"model": route.model}
		for k, v := range route.runtime {
			kwargs[k] = v
		}
		reasoning, err := r.resolveSessionReasoningConfig(scoped, source, sessionKey, model)
		if err != nil {
			return err
		}
		var ephemeral, prefillValue, tierValue, reasoningValue, sessionDB, fallback, sessionIDValue any
		if policy != nil && policy.SkillsPrompt != nil && *policy.SkillsPrompt != "" {
			ephemeral = *policy.SkillsPrompt
		}
		if len(prefill) > 0 {
			prefillValue = prefill
		}
		if tier != nil {
			tierValue = *tier
		}
		if reasoning != nil {
			reasoningValue = reasoning
		}
		if db := r.sessionDB(scoped); db != nil {
			sessionDB = db.DB()
		}
		locked, err := gateway.APIRuntimeLocked(scoped)
		if err != nil {
			return err
		}
		if locked == nil {
			fallback = r.refreshFallbackModel(scoped)
		}
		if sessionID != "" {
			sessionIDValue = sessionID
		}
		requireParameters, present := pr["require_parameters"]
		if !present {
			requireParameters = false
		}
		for k, v := range map[string]any{
			"quiet_mode": true, "verbose_logging": false, "enabled_toolsets": enabled, "disabled_toolsets": disabled,
			"ephemeral_system_prompt": ephemeral, "prefill_messages": prefillValue, "reasoning_config": reasoningValue,
			"service_tier": tierValue, "request_overrides": route.requestOverrides,
			"providers_allowed": pr["only"], "providers_ignored": pr["ignore"], "providers_order": pr["order"],
			"provider_sort": pr["sort"], "provider_require_parameters": requireParameters, "provider_data_collection": pr["data_collection"],
			"session_id": sessionIDValue, "platform": platformKey, "gateway_session_key": sessionKey, "session_db": sessionDB,
			"fallback_model": fallback, "skip_memory": true,
		} {
			if _, dup := kwargs[k]; dup {
				return &pyTypeError{fmt.Sprintf("AIAgent() got multiple values for keyword argument '%s'", k)}
			}
			kwargs[k] = v
		}
		opts, err := agent.AIAgentOptionsFromKwargs(kwargs)
		if err != nil {
			return err
		}
		opts.Env = tools.ToolEnvFrom(scoped)
		parent, err := agent.NewAIAgent(scoped, opts)
		if err != nil {
			return err
		}
		parent.SetEndSessionOnClose(false)
		defer func() {
			defer func() { _ = recover() }()
			parent.ReleaseClients(scoped)
		}()
		return body(scoped, parent)
	})
}

// Runs body converting a panic into its error, the exception a worker thread catches
func catchColdReview(body func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if failure, ok := recovered.(error); ok {
				err = failure
			} else {
				err = fmt.Errorf("%v", recovered)
			}
		}
	}()
	return body()
}

// Refine of a messaging session whose cached agent is gone, returning once the review spawned or failed to start
func (r *GatewayRunner) startColdRefine(ctx context.Context, source *gateway.SessionSource, sessionKey, sessionID string, history []map[string]any, focus string) error {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	metadata := interimMetadata(nonConversationalMetadata(r.threadMetadataForSource(source, nil), source.Platform))
	threadCtx := context.WithoutCancel(ctx)
	deliver := func(message string) {
		if adapter != nil {
			go func() { _, _ = adapter.Send(threadCtx, source.ChatID, message, "", metadata) }()
		}
	}
	var focusArg *string
	if focus != "" {
		focusArg = &focus
	}
	started := make(chan error, 1)
	go func() {
		startedSet := false
		err := catchColdReview(func() error {
			return r.coldReviewParent(threadCtx, source, sessionKey, sessionID, func(parentCtx context.Context, parent *agent.AIAgent) error {
				parent.SetBackgroundReviewCallback(deliver)
				parent.SpawnBackgroundReview(parentCtx, history, true, parent.ValidToolNames()["skill_manage"], focusArg, true)
				startedSet = true
				started <- nil
				if run := parent.CurrentBackgroundReviewRun(); run != nil {
					run.WaitRequestDone()
				}
				return nil
			})
		})
		if err == nil {
			return
		}
		if startedSet {
			runLog().Warn("cold /refine failed after it started", "error", pyrepr.ExceptionMessage(err))
			return
		}
		started <- err
	}()
	select {
	case err := <-started:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
