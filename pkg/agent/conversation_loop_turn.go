package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// LoopState holds every local the turn loop threads through the phase helpers of the turn modules
type LoopState struct {
	UserMessage                          any
	SystemMessage                        *string
	MoaConfig                            map[string]any
	OriginalUserMessage                  any
	ConversationHistory                  []*state.Message
	EffectiveTaskID                      string
	TurnID                               string
	ShouldReviewMemory                   bool
	PluginUserContext                    string
	ExtPrefetchCache                     string
	Messages                             []*state.Message
	ActiveSystemPrompt                   string
	CurrentTurnUserIdx                   int
	PreflightCompressionBlocked          bool
	MaxCompressionAttempts               int
	APICallCount                         int
	FinalResponse                        any
	Interrupted                          bool
	Failed                               bool
	CodexAckContinuations                int
	LengthContinueRetries                int
	RestartCount                         int
	OuterErrorCount                      int
	TruncatedToolCallRetries             int
	TruncatedResponseParts               []TruncatedPart
	CompressionAttempts                  int
	LastPreflightPressure                *int
	ProviderOverflowRecoveryPending      bool
	CompressionTimeoutExhausted          bool
	TurnExitReason                       any
	PendingVerificationResponse          any
	PendingVerificationResponsePreviewed bool
	PendingMoaPreparedRequest            map[string]any
	RequestLogger                        any
	APIMessages                          []any
	ToolsForAPI                          []map[string]any
	MoaPreparedRequest                   map[string]any
	ApproxTokens                         int
	RequestPressureTokens                int
	TotalChars                           int
	ThinkingSpinner                      tools.Spinner
	APIStartTime                         float64
	RetryCount                           int
	MaxRetries                           int
	Retry                                *TurnRetryState
	FinishReason                         string
	Response                             any
	APIKwargs                            map[string]any
	APIRequestID                         string
	OriginalAPIKwargs                    map[string]any
	LLMMiddlewareTrace                   []any
	APIDuration                          float64
	AssistantMessage                     *provider.Response
}

// PhaseVerdict is a phase helper's action, return carrying the turn result, break, continue or empty to proceed
type PhaseVerdict struct {
	Action string
	Result map[string]any
}

// Loop state seeded from the built turn context with the dataclass defaults
func newLoopState(tc *TurnContext, systemMessage *string, moaConfig map[string]any, maxCompressionAttempts int) *LoopState {
	return &LoopState{
		UserMessage: tc.UserMessage, SystemMessage: systemMessage, MoaConfig: moaConfig,
		OriginalUserMessage: tc.OriginalUserMessage, ConversationHistory: tc.ConversationHistory,
		EffectiveTaskID: tc.EffectiveTaskID, TurnID: tc.TurnID, ShouldReviewMemory: tc.ShouldReviewMemory,
		PluginUserContext: tc.PluginUserContext, ExtPrefetchCache: tc.ExtPrefetchCache, Messages: tc.Messages,
		ActiveSystemPrompt: tc.ActiveSystemPrompt, CurrentTurnUserIdx: tc.CurrentTurnUserIdx,
		PreflightCompressionBlocked: tc.PreflightCompressionBlocked, MaxCompressionAttempts: maxCompressionAttempts,
		TurnExitReason: "unknown", FinishReason: "stop", TruncatedResponseParts: []TruncatedPart{},
	}
}

// Runs a phase, turning a panic other than a user interrupt into the error the except clause catches
func guardPhase(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			err = RecoveredOuterLoopError(r)
		}
	}()
	return fn()
}

func (a *AIAgent) runPerformAPICall(ctx context.Context, s *LoopState) (string, error) {
	v, err := a.performAPICall(ctx, APICallRequest{
		APIKwargs: s.APIKwargs, OriginalAPIKwargs: s.OriginalAPIKwargs, LLMMiddlewareTrace: s.LLMMiddlewareTrace,
		Retry: s.Retry, ThinkingSpinner: s.ThinkingSpinner, RetryCount: s.RetryCount, APICallCount: s.APICallCount,
		APIRequestID: s.APIRequestID, EffectiveTaskID: s.EffectiveTaskID, TurnID: s.TurnID, Interrupted: s.Interrupted,
	})
	if err != nil {
		return "", err
	}
	s.Response, s.ThinkingSpinner, s.Interrupted = v.Response, v.ThinkingSpinner, v.Interrupted
	return v.Action, nil
}

// One API call with its retry and recovery loop, a turn result when a phase ends the turn
func runAPIRetryLoop(ctx context.Context, a *AIAgent, s *LoopState) (map[string]any, error) {
	for s.RetryCount < s.MaxRetries {
		var verdict PhaseVerdict
		err := guardPhase(func() error {
			if err := buildAPIRequest(ctx, a, s); err != nil {
				return err
			}
			action, err := a.runPerformAPICall(ctx, s)
			if err != nil || action == "break" {
				verdict.Action = action
				return err
			}
			verdict, err = checkAPIResponse(ctx, a, s)
			return err
		})
		if err == nil {
			switch verdict.Action {
			case "return":
				return verdict.Result, nil
			case "break":
				return nil, nil
			}
			continue
		}
		var rateLimited *provider.DoError
		if errors.As(err, &rateLimited) && rateLimited.Failure == provider.FailNousRateLimited {
			return a.nousRateLimitResult(ctx, rateLimited, s.Messages, s.ConversationHistory, s.APICallCount)
		}
		var interrupted *provider.InterruptedError
		if errors.As(err, &interrupted) {
			v, ierr := a.handleAPIInterrupt(s.Retry, s.ThinkingSpinner, &s.Messages, s.ConversationHistory, s.APIStartTime, s.Interrupted, s.FinalResponse)
			if ierr != nil {
				return nil, ierr
			}
			s.ThinkingSpinner, s.Interrupted, s.FinalResponse = v.ThinkingSpinner, v.Interrupted, v.FinalResponse
			if v.Action == "break" {
				return nil, nil
			}
			continue
		}
		latched := s.ProviderOverflowRecoveryPending
		v, aerr := handleAPIError(ctx, a, s, err)
		s.ProviderOverflowRecoveryPending = s.ProviderOverflowRecoveryPending || latched
		if aerr != nil {
			return nil, aerr
		}
		switch v.Action {
		case "return":
			return v.Result, nil
		case "break":
			return nil, nil
		}
	}
	return nil, nil
}

// The turn's result when it never enters the generic loop, nil otherwise
func turnWithoutLoop(ctx context.Context, a *AIAgent, s *LoopState, blockMessage string) (map[string]any, error) {
	if blockMessage != "" {
		s.FinalResponse, s.TurnExitReason = blockMessage, preLLMBlockExitReason
		return finalizeTurn(ctx, a, s)
	}
	if a.apiMode == "codex_app_server" {
		return codexAppServerTurn(ctx, a, s)
	}
	return nil, nil
}

// Text of a user message for the codex app server, text parts joined for a content part list
func userMessageText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var parts []string
	for _, p := range anyRows(v) {
		if m, ok := p.(map[string]any); ok && m["type"] == "text" {
			if t, ok := m["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// The codex app server's turn result, nil when its failure activated a fallback the generic loop retries
func codexAppServerTurn(ctx context.Context, a *AIAgent, s *LoopState) (map[string]any, error) {
	var turnID any
	if s.TurnID != "" {
		turnID = s.TurnID
	}
	rows := messagesToRows(s.Messages)
	result, err := provider.RunCodexAppServerTurn(a, userMessageText(s.UserMessage), s.OriginalUserMessage, rows,
		s.EffectiveTaskID, s.ShouldReviewMemory, turnID)
	if err != nil {
		return nil, err
	}
	if !activateCodexAppServerFallback(ctx, a, result) {
		return result, nil
	}
	switch m := result["messages"].(type) {
	case []map[string]any:
		s.Messages = rowsToMessages(m)
	case []*state.Message:
		s.Messages = m
	}
	s.APICallCount = pyInt(result["api_calls"])
	s.ActiveSystemPrompt = syncFailoverSystemMessage(a, nil, s.ActiveSystemPrompt)
	return nil, nil
}

func pyInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case bool:
		if n {
			return 1
		}
	}
	return 0
}

// Runs one complete turn with tool calling until a phase ends it, returning the result dict
func runConversationTurn(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, taskID string) (map[string]any, error) {
	moaConfig, persistUserMessage := o.MoaConfig, o.PersistUserMessage
	if moaConfig == nil {
		userMessage, moaConfig, persistUserMessage = decodeInlineMoaTurn(userMessage, persistUserMessage)
	}
	a.lastCompactionInPlace, a.lastCompressionAttemptRecorded = false, false
	a.lastCompressionAttemptInPlace = nil
	beginFastModeTurn(a, o.ConversationHistory)
	if err := a.tryRefreshEnvClientCredentials(ctx); err != nil {
		conversationLoopLog().Debug("per-turn env credential refresh failed", "error", err)
	}
	held := plugins.HeldStreamTransforms(ctx)
	a.heldTransforms, a.holdStream = held, len(held) > 0

	tc, err := buildTurnContext(ctx, a, TurnContextRequest{
		UserMessage: userMessage, SystemMessage: o.SystemMessage, ConversationHistory: o.ConversationHistory, TaskID: taskID,
		StreamCallback: o.StreamCallback, PersistUserMessage: persistUserMessage, PersistUserTimestamp: o.PersistUserTimestamp,
		PersistUserDisplayKind: o.PersistUserDisplayKind, PersistUserDisplayMetadata: o.PersistUserDisplayMetadata,
		PersistUserPlatformID: o.PersistUserPlatformID, TurnAuthor: o.TurnAuthor, MoaActive: len(moaConfig) > 0,
		TitleUserMessage: o.TitleUserMessage,
	})
	if err != nil {
		var timedOut *PreflightCompressionTimedOut
		if errors.As(err, &timedOut) {
			return preflightTimeoutResult(a, timedOut, o.ConversationHistory), nil
		}
		return nil, err
	}
	tc.ActiveSystemPrompt = beginVoiceTurnRoute(ctx, a, tc.Messages, tc.ActiveSystemPrompt)

	a.deliveredInterimTexts = map[string]bool{}
	a.incrementalPersistenceFailed = false
	a.lastPersistenceErrorCause = nil
	a.compressionAdoptionFailed = false
	a.ephemeralReasoningOff = false
	a.authPoolRefreshCounts = map[string]int{}
	a.lastTurnUsage = nil

	maxAttempts := a.maxCompressionAttempts
	s := newLoopState(tc, o.SystemMessage, moaConfig, maxAttempts)
	early, err := turnWithoutLoop(ctx, a, s, tc.PluginBlockMessage)
	if err != nil || early != nil {
		return early, err
	}

	prelude, err := playPrelude(ctx, a, s, o.Prelude)
	if err != nil {
		return nil, err
	}
	if prelude.Action == "return" {
		return prelude.Result, nil
	}
	for prelude.Action != "break" && ((s.APICallCount < a.maxIterations && a.iterationBudget.Remaining() > 0) || a.budgetGraceCall) {
		action, result, err := runIteration(ctx, a, s)
		if err != nil {
			return nil, err
		}
		if action == "return" {
			return result, nil
		}
		if action == "break" {
			break
		}
	}

	result, err := finalizeTurn(ctx, a, s)
	if err != nil {
		return nil, err
	}
	if s.CompressionTimeoutExhausted {
		result["error"], result["partial"], result["compression_exhausted"] = compressionTimeoutFinalResponse, true, true
	}
	return result, nil
}

// One loop iteration, returning the action that ends, restarts or continues the turn
func runIteration(ctx context.Context, a *AIAgent, s *LoopState) (string, map[string]any, error) {
	begin, err := beginIteration(ctx, a, s)
	if err != nil || begin.Action == "break" {
		return begin.Action, nil, err
	}
	if err := prepareIteration(ctx, a, s); err != nil {
		return "", nil, err
	}
	if err := assembleAPIRequest(ctx, a, s); err != nil {
		return "", nil, err
	}
	gate, err := runPreflightGate(ctx, a, s)
	if err != nil {
		return "", nil, err
	}
	switch gate.Action {
	case "return", "break", "continue":
		return gate.Action, gate.Result, nil
	}
	if err := announceAPICall(ctx, a, s); err != nil {
		return "", nil, err
	}

	s.APIStartTime, s.RetryCount, s.MaxRetries = loopNow(), 0, a.apiMaxRetries
	s.Retry, s.FinishReason, s.Response, s.APIKwargs = &TurnRetryState{}, "stop", nil, nil
	s.APIRequestID = fmt.Sprintf("%s:api:%d", s.TurnID, s.APICallCount)
	a.currentAPIRequestID = s.APIRequestID

	early, err := runAPIRetryLoop(ctx, a, s)
	if err != nil {
		return "", nil, err
	}
	if early != nil {
		return "return", early, nil
	}
	restarts, err := applyRetryRestarts(ctx, a, s)
	if err != nil {
		return "", nil, err
	}
	if restarts.Action == "break" || restarts.Action == "continue" {
		return restarts.Action, nil, nil
	}

	var verdict PhaseVerdict
	perr := guardPhase(func() error {
		intake, err := normalizeModelResponse(ctx, a, s)
		if err != nil || intake.Action == "return" || intake.Action == "continue" {
			verdict = intake
			return err
		}
		if responseHasToolCalls(s.AssistantMessage) {
			verdict, err = runToolRound(ctx, a, s)
		} else {
			verdict, err = finishTextResponse(ctx, a, s)
		}
		return err
	})
	if perr != nil {
		v, err := a.handleOuterLoopError(ctx, perr, s.OuterErrorCount, s.APICallCount, &s.Messages, s.ConversationHistory,
			s.TurnExitReason, s.Failed, s.FinalResponse)
		if err != nil {
			return "", nil, err
		}
		s.OuterErrorCount, s.TurnExitReason, s.Failed, s.FinalResponse = v.OuterErrorCount, v.TurnExitReason, v.Failed, v.FinalResponse
		return v.Action, nil, nil
	}
	return verdict.Action, verdict.Result, nil
}

// Runs one turn and stamps the current turn boundary beside the exact messages it addresses
func runConversationLoop(ctx context.Context, a *AIAgent, userMessage string, o RunConversationOptions, effectiveTaskID string) (map[string]any, error) {
	a.turnUserIntervened = false
	result, err := func() (map[string]any, error) {
		restoreImages := tools.NativeTurnImages(a.env, userMessage)
		defer restoreImages()
		defer endVoiceTurnRoute(a)
		return runConversationTurn(ctx, a, userMessage, o, effectiveTaskID)
	}()
	if err != nil {
		return nil, err
	}
	settleHeldFinal(ctx, a, result)
	result = exportCurrentTurnBoundary(a, result, userMessage)
	if result != nil {
		result["user_intervened"] = a.turnUserIntervened
	}
	closeDurableFailedTurn(ctx, a, result)
	return result, nil
}

const failedTurnErrorMaxChars = 2000

// Error text and error surface a client needs to redraw a failed turn's card, every string force redacted
func failedTurnDisplayMetadata(a *AIAgent, result map[string]any) map[string]any {
	surface, err := buildErrorSurfaceFromResult(result, a.provider, a.model)
	if err != nil {
		conversationLoopLog().Debug("failed-turn error surface unavailable", "error", err)
		surface = nil
	}
	errText := ""
	if v := result["error"]; pyTruthy(v) {
		errText = fmt.Sprint(v)
	}
	runes := []rune(strings.TrimSpace(errText))
	if len(runes) > failedTurnErrorMaxChars {
		runes = runes[:failedTurnErrorMaxChars]
	}
	metadata := map[string]any{}
	if len(runes) > 0 {
		metadata["error"] = string(runes)
	}
	if len(surface) > 0 {
		metadata["error_surface"] = surface
	}
	return redactDisplayMetadata(a, metadata)
}

// Force redacts every string of a display metadata payload, nested maps and lists included
func redactDisplayMetadata(a *AIAgent, metadata map[string]any) map[string]any {
	var redact func(v any) any
	redact = func(v any) any {
		switch x := v.(type) {
		case string:
			return tools.RedactSensitiveTextWith(a.env, x, tools.RedactOptions{Force: true})
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, val := range x {
				out[k] = redact(val)
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, val := range x {
				out[i] = redact(val)
			}
			return out
		case []string:
			out := make([]any, len(x))
			for i, val := range x {
				out[i] = redact(val)
			}
			return out
		}
		return v
	}
	out := make(map[string]any, len(metadata))
	for k, v := range metadata {
		out[k] = redact(v)
	}
	return out
}

// Appends the failed turn boundary when a failed turn left its durable tail open, in place and in the session DB
func closeDurableFailedTurn(ctx context.Context, a *AIAgent, result map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			conversationLoopLog().Debug("failed-turn boundary not written", "error", fmt.Sprint(r))
		}
	}()
	if result == nil || result["completed"] == true {
		return
	}
	if pyTruthy(result["compression_exhausted"]) || pyTruthy(result["compression_deferred"]) || result["failure_reason"] == "context_overflow" {
		return
	}
	messages, ok := result["messages"].([]*state.Message)
	if !ok || len(messages) == 0 || a.sessionDB == nil || a.sessionID == "" || a.persistDisabled {
		return
	}
	role, err := a.sessionDB.LatestConversationRole(ctx, a.sessionID)
	if err != nil {
		conversationLoopLog().Debug("failed-turn boundary not written", "error", err)
		return
	}
	if !OpenTurnTailRoles[role] {
		return
	}
	turnMessages := messages
	if start, ok := result["current_turn_user_idx"].(int); ok && 0 <= start && start < len(messages) {
		turnMessages = messages[start:]
	}
	boundary := map[string]any{"role": "assistant", "content": FailedTurnNoticeFor(messagesToRows(turnMessages)), "display_kind": FailedTurnDisplayKind}
	if failure := failedTurnDisplayMetadata(a, result); len(failure) > 0 {
		boundary["display_metadata"] = failure
	}
	appendLiveMessage(&messages, boundary)
	result["messages"] = messages
	rows := messagesToRows(messages)
	if _, err := a.flushMessagesToSessionDB(rows); err != nil {
		conversationLoopLog().Debug("failed-turn boundary not written", "error", err)
		return
	}
	for i, row := range rows {
		if marker, ok := row[dbPersistedMarker]; ok && messages[i] != nil {
			if messages[i].Extra == nil {
				messages[i].Extra = map[string]any{}
			}
			messages[i].Extra[dbPersistedMarker] = marker
		}
	}
}
