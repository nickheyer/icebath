package agent

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Prompt cache scope view of the agent, one memoizing instance per agent with its fields refreshed
func (a *AIAgent) cacheScopeAgent() *provider.CacheScopeAgent {
	if a.cacheScope == nil {
		a.cacheScope = &provider.CacheScopeAgent{}
	}
	s := a.cacheScope
	s.SessionID, s.GatewaySessionKey, s.PersistDisabled = a.sessionID, a.gatewaySessionKey, a.persistDisabled
	s.InheritedCacheScope, s.Platform, s.Env, s.PromptCacheForkTag = a.inheritedCacheScope, a.platform, a.env, a.promptCacheForkTag
	s.Provider, s.Model, s.BaseURL = a.provider, a.model, a.baseURL
	s.SessionDB = nil
	if a.sessionDB != nil {
		s.SessionDB = provider.CacheScopeSessionStore{DB: a.sessionDB}
	}
	s.CompressionCount = func() int {
		if a.contextCompressor == nil {
			return 0
		}
		return a.contextCompressor.Base().CompressionCount
	}
	return s
}

// Relay outcome for an error or panic value leaving the turn, cancelled for interrupts and timed_out for timeouts
func relayOutcomeForFailure(ctx context.Context, v any) string {
	if _, ok := v.(plugins.UserInterrupt); ok {
		return "cancelled"
	}
	if _, ok := v.(*plugins.UserInterrupt); ok {
		return "cancelled"
	}
	err, ok := v.(error)
	if !ok {
		return "failed"
	}
	if errors.Is(err, context.Canceled) || tools.IsKeyboardInterrupt(ctx, err) {
		return "cancelled"
	}
	var timeout interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "timed_out"
	}
	return "failed"
}

// RunConversation admits one text turn under the durable session lease and relay scopes
func (a *AIAgent) RunConversation(ctx context.Context, userMessage string, o RunConversationOptions) (map[string]any, error) {
	return a.RunConversationContent(ctx, userMessage, o)
}

// RunConversationContent admits one turn of text or content parts, then runs the conversation loop
func (a *AIAgent) RunConversationContent(ctx context.Context, userMessage any, o RunConversationOptions) (result map[string]any, err error) {
	ctx = withLifecycleEnv(ctx, a.env)
	cancelBackgroundReviewForLiveTurn(a)
	effectiveTaskID := o.TaskID
	if effectiveTaskID == "" {
		effectiveTaskID = uuid.NewString()
	}
	sessionID := a.sessionID
	platform := a.platform
	sessionLabel := sessionID
	if sessionLabel == "" {
		sessionLabel = "session"
	}
	relayTurnID := fmt.Sprintf("%s:%s:%s", sessionLabel, effectiveTaskID, uuid.New().String()[:8])
	a.relayPendingTurnID = relayTurnID
	relayParentSessionID := ""
	if platform == "subagent" {
		relayParentSessionID = a.parentSessionID
	}
	var relayLease *tools.ConversationLease
	var relayTurn *tools.RelayTurnContext
	var lease turnLease
	taskStarted, taskFinished := false, false
	relayOutcome := "failed"
	turnCtx := ctx

	defer func() {
		r := recover()
		if r != nil || err != nil {
			failure := r
			if failure == nil {
				failure = err
			}
			relayOutcome = relayOutcomeForFailure(turnCtx, failure)
			if relayTurn != nil {
				tools.RelayCoordinator.FinishLogicalCalls(turnCtx, relayTurn, relayOutcome)
			}
			if taskStarted && !taskFinished {
				taskFinished = true
				failErr, ok := failure.(error)
				if !ok {
					failErr = fmt.Errorf("%v", failure)
				}
				observability.FinishTaskRun(turnCtx, sessionID, effectiveTaskID, platform, nil, failErr)
			}
		}
		func() {
			defer func() {
				defer func() {
					if lease != nil {
						lease.StopRefresher()
						lease.JoinThreads()
						lease.ClearInterrupt()
						lease.Release()
					}
					quietly(func() error { a.resetActivityLabelsAfterTurn(); return nil })
					if a.relayPendingTurnID == relayTurnID {
						a.relayPendingTurnID = ""
					}
					quietly(func() error { noteReviewTurnFinished(); return nil })
				}()
				if relayLease != nil {
					tools.RelayCoordinator.ReleaseConversation(relayLease)
				}
			}()
			if relayTurn != nil {
				tools.RelayCoordinator.EndTurn(turnCtx, relayTurn, relayOutcome)
			}
		}()
		if r != nil {
			panic(r)
		}
	}()

	noteReviewTurnStarted()
	taskContext := map[string]string{"session_id": sessionID, "task_id": effectiveTaskID, "platform": platform}
	admission, err := admitDurableTurnLease(ctx, a, sessionID, relayTurnID, taskContext, o.ConversationHistory)
	if err != nil {
		return nil, err
	}
	if admission.earlyResult != nil {
		carryUnadmittedUserMessage(admission.earlyResult, userMessage, o.PersistUserMessage, o.PersistUserTimestamp,
			o.PersistUserDisplayKind, o.PersistUserDisplayMetadata, o.PersistUserPlatformID)
		if admission.earlyResult["interrupted"] == true {
			relayOutcome = "cancelled"
		} else {
			relayOutcome = "timed_out"
		}
		return admission.earlyResult, nil
	}
	lease = admission.lease
	o.ConversationHistory = admission.conversationHistory
	sessionCwd, turnCwd := resolveRelayScopeCwds(a, effectiveTaskID, sessionID, platform)
	relayLease = tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{
		ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: sessionID, Platform: platform,
		ParentSessionID: relayParentSessionID, Model: a.model, SessionCwd: sessionCwd, TurnCwd: turnCwd,
	})
	var metadata map[string]any
	if len(o.RelayMetadata) > 0 {
		metadata = o.RelayMetadata
	}
	relayTurn, err = tools.RelayCoordinator.BeginTurn(ctx, relayLease, relayTurnID, effectiveTaskID, metadata)
	if err != nil {
		return nil, err
	}
	if relayTurn == nil || relayTurn.RelayEnabled() {
		observability.StartTaskRun(ctx, a.env, sessionID, effectiveTaskID, platform, a.parentSessionID)
		taskStarted = true
	}
	turnCtx = provider.WithPortalConversationContext(turnCtx, a.conversationRootID())
	turnCtx = provider.WithPortalAffinityScope(turnCtx, provider.DeclaredConversationScopeSafe(a.cacheScopeAgent()))
	turnCtx = provider.SetAccountingContext(turnCtx, provider.AuxUsageRecorderFor(a.sessionDB), a.sessionID)
	result, err = func() (map[string]any, error) {
		loopCtx := bindSubagentParent(turnCtx, a)
		loopCtx, _ = provider.ScopedRuntimeMain(loopCtx, map[string]any{})
		loopCtx, untrack := trackInInterruptScope(loopCtx, a)
		defer untrack()
		if lease != nil {
			lease.Start(loopCtx)
			defer lease.StopRefresher()
		}
		return conversationLoopRun(loopCtx, a, userMessage, o, effectiveTaskID)
	}()
	if err != nil {
		return nil, err
	}
	switch {
	case result["interrupted"] == true:
		relayOutcome = "cancelled"
	case result["failed"] == true:
		relayOutcome = "failed"
	default:
		relayOutcome = "success"
	}
	tools.RelayCoordinator.FinishLogicalCalls(turnCtx, relayTurn, relayOutcome)
	if taskStarted {
		taskFinished = true
		observability.FinishTaskRun(turnCtx, sessionID, effectiveTaskID, platform, result, nil)
	}
	return result, nil
}

// Chat returns the final response of one turn, the stream callback receiving each text delta
func (a *AIAgent) Chat(ctx context.Context, message string, streamCallback func(string)) (string, error) {
	res, err := a.RunConversation(ctx, message, RunConversationOptions{StreamCallback: streamCallback})
	if err != nil {
		return "", err
	}
	s, _ := res["final_response"].(string)
	return s, nil
}
