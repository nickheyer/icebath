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
	"github.com/nickheyer/icebath/pkg/state"
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

// Relay conversation and turn coordination the live turn wrapper drives
type relayTurnCoordinator interface {
	AcquireConversation(ctx context.Context, o tools.RelayConversation) *tools.ConversationLease
	BeginTurn(ctx context.Context, lease *tools.ConversationLease, turnID, taskID string, metadata map[string]any) (*tools.RelayTurnContext, error)
	FinishLogicalCalls(ctx context.Context, turn *tools.RelayTurnContext, outcome string)
	EndTurn(ctx context.Context, turn *tools.RelayTurnContext, outcome string)
	ReleaseConversation(lease *tools.ConversationLease)
}

// Relay coordinator of a live turn, replaced by tests recording the relay calls
var relayCoordinatorFn = func() relayTurnCoordinator { return tools.RelayCoordinator }

// Task run start of a live turn, replaced by tests recording its position among the relay calls
var startTaskRunFn = observability.StartTaskRun

// RunConversation admits one text turn under the durable session lease and relay scopes
func (a *AIAgent) RunConversation(ctx context.Context, userMessage string, o RunConversationOptions) (map[string]any, error) {
	return a.RunConversationContent(ctx, userMessage, o)
}

// RunConversationContent admits one turn of text or content parts, then runs the conversation loop
func (a *AIAgent) RunConversationContent(ctx context.Context, userMessage any, o RunConversationOptions) (result map[string]any, err error) {
	ctx = withLifecycleEnv(ctx, a.env)
	defer bindAdmissionFiniteScope(ctx, a.env)()
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
	coord := relayCoordinatorFn()
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
				coord.FinishLogicalCalls(turnCtx, relayTurn, relayOutcome)
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
					coord.ReleaseConversation(relayLease)
				}
			}()
			if relayTurn != nil {
				coord.EndTurn(turnCtx, relayTurn, relayOutcome)
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
		exportResultMessages(admission.earlyResult)
		return admission.earlyResult, nil
	}
	lease = admission.lease
	o.ConversationHistory = admission.conversationHistory
	sessionCwd, turnCwd := resolveRelayScopeCwds(a, effectiveTaskID, sessionID, platform)
	relayLease = coord.AcquireConversation(ctx, tools.RelayConversation{
		ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: sessionID, Platform: platform,
		ParentSessionID: relayParentSessionID, Model: a.model, SessionCwd: sessionCwd, TurnCwd: turnCwd,
	})
	var metadata map[string]any
	if len(o.RelayMetadata) > 0 {
		metadata = o.RelayMetadata
	}
	relayTurn, err = coord.BeginTurn(ctx, relayLease, relayTurnID, effectiveTaskID, metadata)
	if err != nil {
		return nil, err
	}
	if relayTurn == nil || relayTurn.RelayEnabled() {
		startTaskRunFn(ctx, a.env, sessionID, effectiveTaskID, platform, a.parentSessionID)
		taskStarted = true
	}
	turnCtx = provider.WithPortalConversationContext(turnCtx, a.conversationRootID())
	turnCtx = provider.WithPortalAffinityScope(turnCtx, provider.DeclaredConversationScopeSafe(a.cacheScopeAgent()))
	turnCtx = provider.SetAccountingContext(turnCtx, auxUsageRecorderFor(a.sessionDB), a.sessionID)
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
	exportResultMessages(result)
	switch {
	case result["interrupted"] == true:
		relayOutcome = "cancelled"
	case result["failed"] == true:
		relayOutcome = "failed"
	default:
		relayOutcome = "success"
	}
	coord.FinishLogicalCalls(turnCtx, relayTurn, relayOutcome)
	if taskStarted {
		taskFinished = true
		observability.FinishTaskRun(turnCtx, sessionID, effectiveTaskID, platform, result, nil)
	}
	return result, nil
}

// Renders the live messages of a turn result as the list of dicts every caller outside the turn reads
func exportResultMessages(result map[string]any) {
	if live, isLive := result["messages"].([]*state.Message); isLive {
		result["messages"] = messagesToRows(live)
	}
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
