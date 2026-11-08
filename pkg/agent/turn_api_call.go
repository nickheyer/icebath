package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// A client that reports itself a test double takes the non streaming path like a unittest Mock
type mockClient interface {
	IsMock() bool
}

// Wall clock seconds the interrupt path measures the elapsed call against
var apiCallWallNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Stamps a dict row and appends it to the live transcript, returning the stamped row
func appendLiveMessage(messages *[]*state.Message, row map[string]any) map[string]any {
	scratch := []map[string]any{}
	stamped := state.AppendMessage(&scratch, row, nil)
	*messages = append(*messages, state.MessageFromDict(stamped))
	return stamped
}

// Stops the spinner silently and clears the thinking callback, returning nil for the caller to rebind
func stopThinkingSpinner(a *AIAgent, spinner tools.Spinner) tools.Spinner {
	if spinner != nil {
		spinner.Stop("")
	}
	a.callThinking("")
	return nil
}

// Fallthrough hands the response to verification, break means a redirect crossed it
type APICallVerdict struct {
	Action          string
	Response        any
	ThinkingSpinner tools.Spinner
	Interrupted     bool
}

// Streaming for liveness unless disabled, an ACP transport, an unopted process provider or a consumerless moa or mock
func shouldStream(ctx context.Context, a *AIAgent) bool {
	if a.RetrySession().DisableStreaming {
		return false
	}
	base := strings.ToLower(a.baseURL)
	if strings.HasPrefix(base, "acp://") || strings.HasPrefix(base, "acp+tcp://") {
		return false
	}
	var streaming *bool
	if profile, ok := provider.GetProviderProfile(ctx, a.provider); ok {
		streaming = profile.SupportsStreaming
	}
	if streaming != nil && !*streaming {
		return false
	}
	if provider.IsExternalProcessProvider(ctx, a.provider) && (streaming == nil || !*streaming) {
		return false
	}
	if !a.hasStreamConsumers() {
		if a.provider == "moa" {
			return false
		}
		if m, ok := a.client.(mockClient); ok && m.IsMock() {
			return false
		}
	}
	return true
}

// Loop locals one provider attempt reads
type APICallRequest struct {
	APIKwargs          map[string]any
	OriginalAPIKwargs  map[string]any
	LLMMiddlewareTrace []any
	Retry              *TurnRetryState
	ThinkingSpinner    tools.Spinner
	RetryCount         int
	APICallCount       int
	APIRequestID       string
	EffectiveTaskID    string
	TurnID             string
	Interrupted        bool
}

// Relay call role of this agent's attempt
func (a *AIAgent) relayCallRole() string {
	if a.isSubagent {
		return "delegated"
	}
	if a.fallbackIndex > 0 {
		return "fallback"
	}
	return "primary"
}

// Issues the request through the LLM execution middleware inside the model request bracket, discarding a response a redirect crossed
func (a *AIAgent) performAPICall(ctx context.Context, r APICallRequest) (APICallVerdict, error) {
	spinner := r.ThinkingSpinner
	interrupted := r.Interrupted
	stopSpinner := func() { spinner = stopThinkingSpinner(a, spinner) }
	useStreaming := shouldStream(ctx, a)

	call := func(payload any) (any, error) {
		next, ok := payload.(map[string]any)
		if !ok && payload != nil {
			return nil, fmt.Errorf("llm execution middleware passed %T where the request dict belongs", payload)
		}
		if a.apiMode == "codex_responses" {
			var err error
			next, err = provider.PreflightKwargs(a.getTransport(""), next, false, provider.DropsReplayItemIDs(a.baseURL), a.isCodexBackend())
			if err != nil {
				return nil, err
			}
		}
		if useStreaming {
			return a.interruptibleStreamingAPICall(ctx, next, stopSpinner)
		}
		sessionID := a.sessionID
		name := a.provider
		if name == "" {
			name = "provider"
		}
		return provider.RelayExecute(ctx, next, a.interruptibleAPICall, provider.RelayExecuteOptions{
			Name: name, ModelName: a.model, SessionID: &sessionID, DeferLogicalCompletion: true,
			Metadata: map[string]any{
				"api_mode": a.apiMode, "api_request_id": r.APIRequestID, "call_role": a.relayCallRole(), "retry_count": r.RetryCount,
			},
		})
	}

	lockBracket := func() {
		if a.pendingRedirectLock != nil {
			a.pendingRedirectLock.Lock()
		}
	}
	unlockBracket := func() {
		if a.pendingRedirectLock != nil {
			a.pendingRedirectLock.Unlock()
		}
	}
	lockBracket()
	if a.modelRequestActive != nil {
		a.modelRequestActive.Set()
	}
	unlockBracket()

	crossed := false
	response, err := func() (resp any, err error) {
		defer func() {
			lockBracket()
			defer unlockBracket()
			if a.modelRequestActive != nil {
				a.modelRequestActive.Clear()
			}
			if a.pendingRedirectLock != nil {
				crossed = pyrepr.Truthy(a.pendingRedirect)
			} else {
				crossed = a.hasPendingRedirect()
			}
		}()
		trace := append([]any{}, r.LLMMiddlewareTrace...)
		return plugins.GetPluginManager(ctx).RunLLMExecutionMiddleware(r.APIKwargs, call, map[string]any{
			"original_request": r.OriginalAPIKwargs, "task_id": r.EffectiveTaskID, "turn_id": r.TurnID,
			"api_request_id": r.APIRequestID, "session_id": a.sessionID, "platform": a.platform, "model": a.model,
			"provider": a.provider, "base_url": a.baseURL, "api_mode": a.apiMode, "api_call_count": r.APICallCount,
			"middleware_trace": trace,
		})
	}()
	if err != nil {
		return APICallVerdict{}, err
	}
	if crossed {
		spinner = stopThinkingSpinner(a, spinner)
		if a.clearInterrupt(true) {
			r.Retry.RestartWithRedirectedMessages = true
		} else {
			interrupted = true
		}
		return APICallVerdict{Action: "break", Response: response, ThinkingSpinner: spinner, Interrupted: interrupted}, nil
	}
	return APICallVerdict{Action: "fallthrough", Response: response, ThinkingSpinner: spinner, Interrupted: interrupted}, nil
}

// Always break, with a redirect restart armed or the turn interrupted and its final response set
type APIInterruptVerdict struct {
	Action          string
	ThinkingSpinner tools.Spinner
	Interrupted     bool
	FinalResponse   any
}

// Keeps a pending redirect queued for the rebuild, else records the streamed partial or the waiting notice and persists
func (a *AIAgent) handleAPIInterrupt(retry *TurnRetryState, spinner tools.Spinner, messages *[]*state.Message,
	conversationHistory []*state.Message, apiStartTime float64, interrupted bool, finalResponse any) (APIInterruptVerdict, error) {
	spinner = stopThinkingSpinner(a, spinner)
	if a.hasPendingRedirect() && a.clearInterrupt(true) {
		retry.RestartWithRedirectedMessages = true
		return APIInterruptVerdict{"break", spinner, interrupted, finalResponse}, nil
	}
	apiElapsed := apiCallWallNow() - apiStartTime
	a.vprint(a.logPrefix+"⚡ Interrupted during API call.", true, false)
	interrupted = true
	a.dropTrailingEmptyResponseScaffolding(messages)
	partial := pystr.Strip(a.stripThinkBlocks(a.currentStreamedAssistantText()))
	switch {
	case partial != "" && provider.IsRunawayRepetition(partial):
		appendLiveMessage(messages, HiddenInterruptPlaceholderRow())
		finalResponse = provider.RepetitionLoopInterrupted
	case partial != "":
		appendLiveMessage(messages, map[string]any{"role": "assistant", "content": partial, "display_metadata": map[string]any{"interrupted": true}})
		finalResponse = partial
	default:
		finalResponse = fmt.Sprintf("%s%.1fs elapsed).", interruptWaitingForModelPrefix, apiElapsed)
	}
	if err := a.persistSession(messages, conversationHistory); err != nil {
		return APIInterruptVerdict{}, err
	}
	return APIInterruptVerdict{"break", spinner, interrupted, finalResponse}, nil
}

// Turn result for a Nous Portal rate limit the provider guard hit with no fallback left, persisted first
func (a *AIAgent) nousRateLimitResult(ctx context.Context, de *provider.DoError, messages, conversationHistory []*state.Message, apiCallCount int) (map[string]any, error) {
	if err := a.persistSession(&messages, conversationHistory); err != nil {
		return nil, err
	}
	anonymous := de.FreeTierKind == "rate_limited"
	finalResponse := "⏳ " + de.Message
	if !anonymous {
		advice, err := tools.SiteCopy(ctx, "nous_rate_limit", nil)
		if err != nil {
			return nil, err
		}
		finalResponse += "\n\n" + advice
	}
	result := map[string]any{
		"final_response": finalResponse, "messages": messages, "api_calls": apiCallCount,
		"completed": false, "failed": true, "error": de.Message,
	}
	if anonymous {
		result["free_tier"] = map[string]any{"kind": "rate_limited", "message": de.WelcomeHint}
	}
	return StampFailure(result, string(provider.ReasonRateLimit), true), nil
}
