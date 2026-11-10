package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var continuableModes = map[string]bool{"chat_completions": true, "bedrock_converse": true, "anthropic_messages": true, "codex_responses": true}

var thinkTagRE = regexp.MustCompile(`(?i)<(?:think|thinking|reasoning|REASONING_SCRATCHPAD)[^>]*>`)

const contextOverflowPartialFinal = "The request no longer fits the model's context window, so the partial " +
	"response was not continued. Continue in a fresh session (/new; gateway " +
	"chats are reset automatically)."

const (
	lengthFragmentKey = "_length_continuation_fragment"
	lengthNudgeKey    = "_length_continuation_nudge"
)

// One continuation fragment and whether a stream cut mid reply produced it
type TruncatedPart struct {
	Text      string
	CutStream bool
}

// Site copy every truncated turn ends with
func truncatedFinal(ctx context.Context) (string, error) {
	return tools.SiteCopy(ctx, "truncated", nil)
}

// Drops this turn's fragment and nudge rows and appends one assistant row with the joined think stripped partial
func (a *AIAgent) collapseContinuationTrail(ctx context.Context, messages *[]*state.Message, currentTurnUserIdx int, finishReason string, parts *[]TruncatedPart) string {
	rows := *messages
	validIdx := currentTurnUserIdx >= 0
	if parts == nil && !(validIdx && currentTurnUserIdx < len(rows)) {
		return ""
	}
	turnStart := 0
	if validIdx {
		turnStart = min(currentTurnUserIdx+1, len(rows))
	}
	var fragmentParts []TruncatedPart
	var retained []*state.Message
	foundTrail := false
	for _, m := range rows[turnStart:] {
		if m != nil && (pyrepr.Truthy(m.Extra[lengthFragmentKey]) || pyrepr.Truthy(m.Extra[lengthNudgeKey])) {
			foundTrail = true
			if s, ok := m.Content.(string); ok && s != "" && pyrepr.Truthy(m.Extra[lengthFragmentKey]) {
				fragmentParts = append(fragmentParts, TruncatedPart{Text: s})
			}
			continue
		}
		retained = append(retained, m)
	}
	if parts == nil && !foundTrail {
		return ""
	}
	*messages = append(rows[:turnStart:turnStart], retained...)
	raw := fragmentParts
	if parts != nil {
		raw = *parts
	}
	partial := pystr.Strip(a.stripThinkBlocks(joinTruncatedParts(raw)))
	if partial != "" && a.holdStream {
		partial, _, _ = applyLLMOutputTransform(ctx, a, partial, a.currentTurnID, nil)
	}
	if partial != "" {
		appendLiveMessage(messages, map[string]any{"role": "assistant", "content": partial, "finish_reason": finishReason})
	}
	a.sessionMessages = *messages
	return partial
}

var thinkingExhausted = [3]string{
	"💭 Reasoning exhausted the output token budget — no visible response was produced.",
	"⚠️ **Thinking Budget Exhausted**\n\nThe model used all its output tokens on reasoning " +
		"and had none left for the actual response.\n\nTo fix this:\n" +
		"→ Lower reasoning effort: `/reasoning low` or `/reasoning minimal`\n" +
		"→ Or switch to a larger/non-reasoning model with `/model`",
	"Model used all output tokens on reasoning with none left " +
		"for the response. Try lowering reasoning effort or increasing max_tokens.",
}

// Log line, user copy and error for a repetition dominated abort, differing only where the turn stopped
func RepetitionCopy(stopping, outcome, refusal string) [3]string {
	return [3]string{
		"🔁 Response dominated by repeated text — stopping " + stopping + ".",
		"⚠️ **Response Stopped — Repetition Detected**\n\nThe model fell into a repetition loop while " +
			"writing this response, " + outcome + "\n\n→ Switch to a different model with `/model`\n" +
			"→ Or resend your message (your conversation history is preserved)",
		"Model output entered a repetition loop" + refusal + " degenerate response.",
	}
}

var repetitionDominated = RepetitionCopy(
	"instead of continuing a degenerate response",
	"so continuing would only produce more repeated text. The partial response was discarded.",
	" and was truncated mid-loop; refusing to continue a",
)

var repetitionStreamCut = RepetitionCopy(
	"the stream mid-loop",
	"so the stream was stopped instead of running on. The partial response was discarded.",
	" and the stream was cut mid-loop; discarding the",
)

const ceilingNoText = "⚠️ **No visible answer was produced.** The model hit its output-token limit on every " +
	"continuation attempt — its reasoning consumed the entire budget each time.\n\nTo fix this:\n" +
	"→ Lower reasoning effort: `/reasoning low` or `/reasoning none`\n→ Or raise max_tokens for this model"

const minContinuationHeadroom = 512

// Window full notice naming the prompt size and the context length with thousands separators
func windowFilledNotice(prompt, ctxLen int) string {
	return "⚠️ **Context window full.** The prompt used " + groupedInt(prompt) + " of this model's " + groupedInt(ctxLen) + "-token " +
		"context window, leaving no room to answer in. This is a context-window limit, not an " +
		"output-length limit.\n\nTo fix this:\n→ Compress the conversation with `/compress` or start " +
		"a new session\n→ Or raise the model's context window (e.g. Ollama `num_ctx`)"
}

// Prompt tokens and context length when the response usage left less than the continuation headroom
func (a *AIAgent) promptFilledWindow(response any) (int, int, bool) {
	ctxLen := 0
	if a.contextCompressor != nil {
		ctxLen = a.contextCompressor.Base().ContextLength
	}
	usage := state.ToolCallField(response, "usage")
	if ctxLen == 0 || !pyrepr.Truthy(usage) {
		return 0, 0, false
	}
	prompt := provider.NormalizeUsage(usage, a.provider, a.apiMode).PromptTokens()
	if prompt != 0 && ctxLen-prompt < minContinuationHeadroom {
		return prompt, ctxLen, true
	}
	return 0, 0, false
}

// One normalized response from the active transport, Anthropic stripping the OAuth tool prefix
func (a *AIAgent) normalizeResponseForAgent(response any) (*provider.Response, error) {
	t := a.getTransport("")
	if t == nil {
		return nil, fmt.Errorf("no transport registered for api_mode %q", a.apiMode)
	}
	if a.apiMode == "anthropic_messages" {
		return t.NormalizeResponse(response, map[string]any{"strip_tool_prefix": a.isAnthropicOAuth})
	}
	return t.NormalizeResponse(response, nil)
}

// Content of a normalized response message, nil without one
func responseContent(resp *provider.Response) any {
	if resp == nil || resp.Message == nil {
		return nil
	}
	return resp.Message.Content
}

// Whether a normalized response message carries tool calls
func responseHasToolCalls(resp *provider.Response) bool {
	return resp != nil && resp.Message != nil && len(resp.Message.ToolCalls) > 0
}

// Typed incomplete turn result, partial unless failed, the error defaulting to the final response
func PartialResult(messages []*state.Message, apiCallCount int, finalResponse string, errText *string, failed, compressionExhausted bool) map[string]any {
	key := "partial"
	if failed {
		key = "failed"
	}
	errValue := finalResponse
	if errText != nil {
		errValue = *errText
	}
	result := map[string]any{
		"final_response": finalResponse, "messages": messages, "api_calls": apiCallCount,
		"completed": false, key: true, "error": errValue,
	}
	if compressionExhausted {
		result["compression_exhausted"] = true
	}
	return result
}

// Return ends the turn, break restarts after a retry flag, continue reissues the call, the rest are rebound loop locals
type TruncationVerdict struct {
	Action                   string
	Result                   map[string]any
	Messages                 []*state.Message
	LengthContinueRetries    int
	TruncatedResponseParts   []TruncatedPart
	TruncatedToolCallRetries int
	RetryCount               int
	CompressionAttempts      int
}

type truncState struct {
	TruncationVerdict
	a                   *AIAgent
	response            any
	finishReason        string
	conversationHistory []*state.Message
	apiCallCount        int
	effectiveTaskID     string
	currentTurnUserIdx  int
	windowFilled        *[2]int
}

type endTurnOptions struct {
	errText              *string
	resultMessages       []*state.Message
	skipCleanup          bool
	failed               bool
	compressionExhausted bool
	failureReason        string
	notRetryable         bool
}

func (st *truncState) done(action string, result map[string]any) TruncationVerdict {
	st.Action, st.Result = action, result
	return st.TruncationVerdict
}

// Persists and ends the turn as partial or failed with the failure verdict for the UI descriptor
func (st *truncState) endTurn(ctx context.Context, finalResponse string, o endTurnOptions) (TruncationVerdict, error) {
	if !o.skipCleanup {
		st.a.cleanupTaskResources(ctx, st.effectiveTaskID)
	}
	if err := st.a.persistSession(&st.Messages, st.conversationHistory); err != nil {
		return TruncationVerdict{}, err
	}
	rows := st.Messages
	if o.resultMessages != nil {
		rows = o.resultMessages
	}
	reason := o.failureReason
	if reason == "" {
		reason = "truncated"
	}
	return st.done("return", StampFailure(PartialResult(rows, st.apiCallCount, finalResponse, o.errText, o.failed, o.compressionExhausted), reason, !o.notRetryable)), nil
}

func (st *truncState) cutStream() bool {
	id, _ := state.ToolCallField(st.response, "id").(string)
	return id == home.PartialStreamStubID
}

func strPtr(s string) *string { return &s }

// Abort copy when continuing cannot help, thinking ate the budget or a repetition loop dominates the visible text
func (a *AIAgent) truncationAbortReason(content any, hasToolCalls bool) (*[3]string, bool, error) {
	if hasToolCalls {
		return nil, false, nil
	}
	text, isStr := content.(string)
	if !isStr && pyrepr.Truthy(content) {
		return nil, false, fmt.Errorf("expected string or bytes-like object, got '%T'", content)
	}
	if text != "" && thinkTagRE.MatchString(text) && !a.hasContentAfterThinkBlock(text) {
		return &thinkingExhausted, true, nil
	}
	visible := content
	if isStr {
		visible = a.stripThinkBlocks(text)
	}
	if v, ok := visible.(string); ok && v != "" && provider.IsRepetitionDominated(v) {
		return &repetitionDominated, true, nil
	}
	return nil, false, nil
}

// A content filter stream stall escalates to the fallback chain before the primary is retried
func (st *truncState) contentFilterFallback(ctx context.Context, retry *TurnRetryState) (TruncationVerdict, bool) {
	a := st.a
	if !(pyrepr.Truthy(state.ToolCallField(st.response, "_content_filter_terminated")) && a.fallbackIndex < len(a.fallbackChain)) {
		return TruncationVerdict{}, false
	}
	a.vprint(a.logPrefix+"🛡️  Content filter terminated stream — activating fallback provider...", true, true)
	a.emitDiagnosticStatus("Content filter terminated stream; switching to fallback...")
	if a.tryActivateFallback(ctx, "") {
		if len(st.TruncatedResponseParts) > 0 {
			st.Messages = a.getMessagesUpToLastAssistant(st.Messages)
		}
		for _, m := range st.Messages {
			if m != nil {
				delete(m.Extra, lengthFragmentKey)
				delete(m.Extra, lengthNudgeKey)
			}
		}
		a.sessionMessages = st.Messages
		st.LengthContinueRetries = 0
		st.TruncatedResponseParts = nil
		st.RetryCount = 0
		st.CompressionAttempts = 0
		retry.PrimaryRecoveryAttempted = false
		retry.RestartWithRebuiltMessages = true
		return st.done("break", nil), true
	}
	a.vprint(a.logPrefix+"⚠️  No fallback provider configured — retrying with same provider (may re-hit filter)...", true, true)
	return TruncationVerdict{}, false
}

// String items of a list attribute, nil for anything else
func stringItems(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, item := range l {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Appends the fragment and a continuation nudge up to four times, then ends on the stitched partial
func (st *truncState) continueText(ctx context.Context, retry *TurnRetryState, assistantMessage *provider.Response) (TruncationVerdict, error) {
	a := st.a
	st.LengthContinueRetries++
	n := st.LengthContinueRetries
	interim := responseContent(assistantMessage)
	if !pyrepr.Truthy(interim) && !st.cutStream() {
		a.ephemeralReasoningOff = true
	}
	if pyrepr.Truthy(interim) {
		interimMsg := a.buildAssistantMessage(assistantMessage, st.finishReason)
		interimMsg[lengthFragmentKey] = true
		appendLiveMessage(&st.Messages, interimMsg)
		text, _ := interim.(string)
		st.TruncatedResponseParts = append(st.TruncatedResponseParts, TruncatedPart{Text: text, CutStream: st.cutStream()})
	}

	if n < 4 && st.windowFilled == nil {
		dropped := stringItems(state.ToolCallField(st.response, "_dropped_tool_names"))
		switch {
		case st.cutStream() && len(dropped) > 0:
			a.vprint(fmt.Sprintf("%s↻ Stream interrupted mid tool-call (%s) — requesting chunked retry (%d/4)...", a.logPrefix, strings.Join(dropped[:min(3, len(dropped))], ", "), n), false, true)
		case st.cutStream():
			a.vprint(fmt.Sprintf("%s↻ Stream interrupted — requesting continuation (%d/4)...", a.logPrefix, n), false, true)
		default:
			a.vprint(fmt.Sprintf("%s↻ Requesting continuation (%d/4)...", a.logPrefix, n), false, true)
		}
		appendLiveMessage(&st.Messages, map[string]any{
			"role": "user", "content": getContinuationPrompt(st.cutStream(), dropped), lengthNudgeKey: true,
		})
		a.sessionMessages = st.Messages
		retry.RestartWithLengthContinuation = true
		return st.done("break", nil), nil
	}

	parts := st.TruncatedResponseParts
	partialResponse := a.collapseContinuationTrail(ctx, &st.Messages, st.currentTurnUserIdx, "length", &parts)
	a.ephemeralReasoningOff = false
	switch {
	case st.windowFilled != nil:
		a.vprint(a.logPrefix+"⚠️  Not continuing — each attempt would only grow the prompt.", true, true)
	case partialResponse != "":
		a.vprint(fmt.Sprintf("%s⚠️  Response still truncated after %d continuation attempts — keeping the partial response received so far.", a.logPrefix, n), true, true)
	default:
		a.vprint(fmt.Sprintf("%s⚠️  Response still truncated after %d continuation attempts — no visible text was produced.", a.logPrefix, n), true, true)
	}
	if f := st.windowFilled; f != nil {
		notice := windowFilledNotice(f[0], f[1])
		final := notice
		if partialResponse != "" {
			final = partialResponse + "\n\n" + notice
		}
		return st.endTurn(tools.WithEnvHome(ctx, a.env), final, endTurnOptions{errText: strPtr(fmt.Sprintf("Prompt used %d of %d context tokens; no room to answer", f[0], f[1]))})
	}
	final := partialResponse
	if final == "" {
		final = ceilingNoText
	}
	return st.endTurn(tools.WithEnvHome(ctx, a.env), final, endTurnOptions{errText: strPtr("Response remained truncated after 4 continuation attempts")})
}

// The model's known max output tokens on Anthropic Messages, zero when unknown
func (a *AIAgent) modelOutputLimit() int {
	if a.apiMode != "anthropic_messages" {
		return 0
	}
	return provider.AnthropicMaxOutput(a.model)
}

// Output budget for truncation retry n, base doubled n times within the failed cap and the model limit, zero args unset
func (a *AIAgent) boostedOutputCap(requestedCap, n, base int) int {
	if base == 0 {
		base = a.maxTokens
		if base == 0 {
			base = requestedCap
		}
		if base == 0 {
			base = 4096
		}
	}
	anchor := requestedCap
	if anchor == 0 {
		anchor = base
	}
	limit := a.modelOutputLimit()
	if limit != 0 && anchor >= limit {
		return anchor
	}
	boost := min(max(base*(1<<n), requestedCap), max(32768, anchor*2))
	if limit != 0 {
		return min(boost, limit)
	}
	return boost
}

// Appends the closing assistant row when the live transcript ends on a tool result
func closeLiveToolSequence(messages *[]*state.Message, finalResponse string) {
	rows := *messages
	if len(rows) == 0 || rows[len(rows)-1] == nil {
		return
	}
	tail := []map[string]any{{"role": rows[len(rows)-1].Role}}
	if closeInterruptedToolSequence(&tail, finalResponse, false) {
		*messages = append(*messages, state.MessageFromDict(tail[1]))
	}
}

// Reruns a truncated tool call up to four times with a boosted cap, then refuses to execute incomplete arguments
func (st *truncState) retryTruncatedToolCall(ctx context.Context, apiKwargs map[string]any) (TruncationVerdict, error) {
	a := st.a
	if st.TruncatedToolCallRetries < 4 {
		st.TruncatedToolCallRetries++
		n := st.TruncatedToolCallRetries
		if st.cutStream() {
			a.bufferVprint(fmt.Sprintf("⚠️  Stream interrupted mid tool-call — retrying (%d/4)...", n))
		} else {
			a.bufferVprint(fmt.Sprintf("⚠️  Truncated tool call detected — retrying API call (%d/4)...", n))
		}
		a.ephemeralMaxOutputTokens = a.boostedOutputCap(a.requestedOutputCapFromAPIKwargs(apiKwargs), n, 0)
		return st.done("continue", nil), nil
	}
	a.flushStatusBuffer()
	failure := "truncated"
	if st.cutStream() {
		failure = string(provider.ReasonTimeout)
	}
	var final string
	var err error
	switch {
	case st.cutStream() && pyrepr.Truthy(state.ToolCallField(st.response, "_clean_eof")):
		a.vprint(a.logPrefix+"⚠️  Server kept closing the stream mid tool-call after 4 retries — the action was not executed.", true, true)
		final, err = tools.SiteCopy(ctx, "stream_closed_tool_call", map[string]any{"label": ProviderLabelFor(a.provider)})
		failure = "truncated"
	case st.cutStream():
		a.vprint(a.logPrefix+"⚠️  Stream kept dropping mid tool-call after 4 retries — the action was not executed.", true, true)
		final, err = tools.SiteCopy(ctx, "stream_dropped_tool_call", map[string]any{"label": ProviderLabelFor(a.provider)})
	default:
		a.vprint(a.logPrefix+"⚠️  Truncated tool call response detected again — refusing to execute incomplete tool arguments.", true, true)
		final, err = truncatedFinal(ctx)
	}
	if err != nil {
		return TruncationVerdict{}, err
	}
	a.cleanupTaskResources(tools.WithEnvHome(ctx, a.env), st.effectiveTaskID)
	closeLiveToolSequence(&st.Messages, final)
	return st.endTurn(tools.WithEnvHome(ctx, a.env), final, endTurnOptions{skipCleanup: true, failureReason: failure})
}

// Loop locals recover_from_truncation reads and may rebind
type TruncationRequest struct {
	Response                 any
	FinishReason             string
	Retry                    *TurnRetryState
	Messages                 []*state.Message
	ConversationHistory      []*state.Message
	APIKwargs                map[string]any
	APICallCount             int
	EffectiveTaskID          string
	CurrentTurnUserIdx       int
	LengthContinueRetries    int
	TruncatedResponseParts   []TruncatedPart
	TruncatedToolCallRetries int
	RetryCount               int
	CompressionAttempts      int
}

// Aborts on exhausted thinking or repetition, escalates content filter stalls, continues text or retries tool calls, else rolls back
func (a *AIAgent) recoverFromTruncation(ctx context.Context, r TruncationRequest) (TruncationVerdict, error) {
	st := &truncState{
		TruncationVerdict: TruncationVerdict{
			Action: "fallthrough", Messages: r.Messages, LengthContinueRetries: r.LengthContinueRetries,
			TruncatedResponseParts: r.TruncatedResponseParts, TruncatedToolCallRetries: r.TruncatedToolCallRetries,
			RetryCount: r.RetryCount, CompressionAttempts: r.CompressionAttempts,
		},
		a: a, response: r.Response, finishReason: r.FinishReason, conversationHistory: r.ConversationHistory,
		apiCallCount: r.APICallCount, effectiveTaskID: r.EffectiveTaskID, currentTurnUserIdx: r.CurrentTurnUserIdx,
	}
	if pyrepr.Truthy(state.ToolCallField(r.Response, "_runaway_repetition")) {
		a.vprint(a.logPrefix+repetitionStreamCut[0], true, true)
		return st.endTurn(tools.WithEnvHome(ctx, a.env), repetitionStreamCut[1], endTurnOptions{errText: strPtr(repetitionStreamCut[2])})
	}
	if prompt, ctxLen, ok := a.promptFilledWindow(r.Response); ok {
		st.windowFilled = &[2]int{prompt, ctxLen}
	}
	var banner string
	switch {
	case st.cutStream() && pyrepr.Truthy(state.ToolCallField(r.Response, "_clean_eof")):
		banner = "Response truncated — server ended the stream without ever sending finish_reason " +
			"(no transport error — the server or a proxy closed the stream cleanly)"
	case st.cutStream():
		banner = "Response truncated — stream ended before completion"
	case st.windowFilled != nil:
		banner = fmt.Sprintf("Response truncated (finish_reason='length') - the prompt filled the context window (%s/%s tokens)",
			groupedInt(st.windowFilled[0]), groupedInt(st.windowFilled[1]))
	default:
		banner = "Response truncated (finish_reason='length') - model hit max output tokens"
	}
	a.vprint(a.logPrefix+"⚠️  "+banner, true, true)

	if pyrepr.Truthy(state.ToolCallField(st.response, "_overflow_terminal")) {
		a.flushStatusBuffer()
		a.vprint(a.logPrefix+"⚠️ Stream ended on a context-overflow error after "+
			"partial delivery — not continuing (the request no longer fits the model's "+
			"context window).", true, true)
		closeLiveToolSequence(&st.Messages, contextOverflowPartialFinal)
		return st.endTurn(tools.WithEnvHome(ctx, a.env), contextOverflowPartialFinal, endTurnOptions{
			errText: strPtr(contextOverflowPartialFinal), failed: true, compressionExhausted: true,
			failureReason: "context_overflow", notRetryable: true,
		})
	}

	truncMsg, err := a.normalizeResponseForAgent(r.Response)
	if err != nil {
		return TruncationVerdict{}, err
	}
	truncContent := responseContent(truncMsg)
	hasToolCalls := responseHasToolCalls(truncMsg)
	abort, ok, err := a.truncationAbortReason(truncContent, hasToolCalls)
	if err != nil {
		return TruncationVerdict{}, err
	}
	if ok {
		a.vprint(a.logPrefix+abort[0], true, true)
		return st.endTurn(tools.WithEnvHome(ctx, a.env), abort[1], endTurnOptions{errText: strPtr(abort[2])})
	}

	if continuableModes[a.apiMode] {
		if v, ok := st.contentFilterFallback(ctx, r.Retry); ok {
			return v, nil
		}
		if truncMsg != nil {
			if !hasToolCalls {
				return st.continueText(ctx, r.Retry, truncMsg)
			}
			return st.retryTruncatedToolCall(ctx, r.APIKwargs)
		}
	}

	final, err := truncatedFinal(ctx)
	if err != nil {
		return TruncationVerdict{}, err
	}
	if len(r.Messages) > 1 {
		a.vprint(a.logPrefix+"   ⏪ Rolling back to last complete assistant turn", false, true)
		return st.endTurn(tools.WithEnvHome(ctx, a.env), final, endTurnOptions{resultMessages: a.getMessagesUpToLastAssistant(r.Messages)})
	}
	a.flushStatusBuffer()
	a.vprint(a.logPrefix+"❌ First response truncated - cannot recover", true, true)
	return st.endTurn(tools.WithEnvHome(ctx, a.env), final, endTurnOptions{skipCleanup: true, failed: true})
}

var codexReplayKeys = []string{"content", "reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items"}

// Dict items of a decoded list value, a raw JSON list decoded first
func dictItems(v any) []map[string]any {
	if raw, ok := v.(json.RawMessage); ok {
		var decoded []any
		if json.Unmarshal(raw, &decoded) != nil {
			return nil
		}
		v = decoded
	}
	switch l := v.(type) {
	case []map[string]any:
		return l
	case []any:
		out := make([]map[string]any, 0, len(l))
		for _, item := range l {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// The str value of v or "" when falsy, the AttributeError Python raises for any other truthy value
func pyStrOrEmpty(v any) (string, error) {
	if !pyrepr.Truthy(v) {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("'%T' object has no attribute 'strip'", v)
}

func orEmptyString(v any) any {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return v
}

// Integer of a usage count the way int(value or 0) reads it, floats truncated
func pyIntOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	n, _ := pyIntAttr(v)
	return int(n)
}

// Codex incomplete continuation, max three per turn, true when a reasoning only stall moved to a fallback, a result once exhausted
func (a *AIAgent) continueCodexIncomplete(ctx context.Context, assistantMessage *provider.Response, finishReason string, messages *[]*state.Message,
	conversationHistory []*state.Message, apiCallCount int, response any) (map[string]any, bool, error) {
	a.codexIncompleteRetries++
	n := a.codexIncompleteRetries

	interimMsg := a.buildAssistantMessage(assistantMessage, finishReason)
	interimContent, err := pyStrOrEmpty(interimMsg["content"])
	if err != nil {
		return nil, false, err
	}
	hasContent := pystr.Strip(interimContent) != ""
	reasoning, isStr := interimMsg["reasoning"].(string)
	hasReasoning := isStr && pystr.Strip(reasoning) != ""
	hasCodexReasoning := pyrepr.Truthy(interimMsg["codex_reasoning_items"])
	hasCodexMessageItems := pyrepr.Truthy(interimMsg["codex_message_items"])
	reasoningOnly := !hasContent && !responseHasToolCalls(assistantMessage)
	if reasoningOnly {
		a.codexReasoningOnlyStreak++
	} else {
		a.codexReasoningOnlyStreak = 0
	}
	streak := a.codexReasoningOnlyStreak

	if hasContent || hasReasoning || hasCodexReasoning || hasCodexMessageItems {
		var last *state.Message
		var lastDict map[string]any
		if rows := *messages; len(rows) > 0 && rows[len(rows)-1] != nil {
			last = rows[len(rows)-1]
			lastDict = state.MessageToDict(last)
		}
		lastVisible := ""
		if last != nil {
			lastVisible = a.interimAssistantVisibleText(lastDict)
		}
		currentVisible := a.interimAssistantVisibleText(interimMsg)
		var same bool
		if lastVisible != "" || currentVisible != "" {
			same = lastVisible == currentVisible
		} else {
			same = last != nil && reflect.DeepEqual(orEmptyString(lastDict["content"]), orEmptyString(interimMsg["content"])) &&
				reflect.DeepEqual(orEmptyString(lastDict["reasoning"]), orEmptyString(interimMsg["reasoning"]))
		}
		if last != nil && lastDict["role"] == "assistant" && lastDict["finish_reason"] == "incomplete" && same {
			for _, key := range codexReplayKeys {
				value, ok := interimMsg[key]
				if !ok {
					continue
				}
				if key == "codex_reasoning_items" {
					lastDict[key] = provider.MergeInterimReasoningItems(dictItems(lastDict[key]), dictItems(value))
				} else {
					lastDict[key] = value
				}
			}
			*last = *state.MessageFromDict(lastDict)
		} else {
			appendLiveMessage(messages, interimMsg)
			a.emitInterimAssistantMessage(interimMsg)
		}
	}

	if reasoningOnly && streak >= 3 {
		if a.tryActivateFallback(ctx, provider.ReasonIncompleteResponse) {
			if apiCallCount >= a.maxIterations || a.iterationBudget.Remaining() <= 0 {
				a.budgetGraceCall = true
			}
			a.codexIncompleteRetries = 0
			a.codexReasoningOnlyStreak = 0
			if !a.quietMode {
				a.vprint(fmt.Sprintf("%s↻ Codex reasoning-only stall after %d attempts — switching to fallback %s (%s)", a.logPrefix, streak, a.model, a.provider), false, true)
			}
			a.emitDiagnosticWait("↻ model stuck on internal reasoning — switching to fallback provider")
			a.sessionMessages = *messages
			return nil, true, nil
		}
	} else if n < 3 || reasoningOnly {
		replayable := hasContent || hasCodexReasoning || hasCodexMessageItems
		if !replayable || n >= 2 {
			if rows := *messages; len(rows) > 0 && rows[len(rows)-1] != nil {
				lastRow := rows[len(rows)-1]
				alreadyNudged := lastRow.Role == "user" && reflect.DeepEqual(lastRow.Content, codexIncompleteNudge)
				if !alreadyNudged && lastRow.Role == "assistant" {
					appendLiveMessage(messages, map[string]any{"role": "user", "content": codexIncompleteNudge})
				}
			}
		}
		if !hasContent && codexFinishReason(response) == "incomplete" {
			a.ephemeralReasoningOff = true
			observed := pyIntOf(orEmptyString(state.ToolCallField(state.ToolCallField(response, "usage"), "output_tokens")))
			base := a.maxTokens
			if base == 0 {
				base = observed
			}
			if base == 0 {
				base = 4096
			}
			a.ephemeralMaxOutputTokens = a.boostedOutputCap(0, n, base)
		}
		if !a.quietMode {
			a.vprint(fmt.Sprintf("%s↻ Codex response incomplete; continuing turn (%d/3)", a.logPrefix, n), false, true)
		}
		a.emitDiagnosticWait(fmt.Sprintf("↻ model returned reasoning with no final answer — asking it to continue (%d/3)", n))
		a.sessionMessages = *messages
		return nil, false, nil
	}

	a.codexIncompleteRetries = 0
	a.codexReasoningOnlyStreak = 0
	if err := a.persistSession(messages, conversationHistory); err != nil {
		return nil, false, err
	}
	return PartialResult(*messages, apiCallCount, "Codex response remained incomplete after 3 continuation attempts", nil, false, false), false, nil
}

// Break after a fallback activation, else return with the typed content policy result, plus the possibly resynced prompt
type RefusalVerdict struct {
	Action             string
	Result             map[string]any
	ActiveSystemPrompt string
}

// Loop locals the content policy refusal handler reads
type RefusalRequest struct {
	Response            any
	Retry               *TurnRetryState
	ThinkingSpinner     tools.Spinner
	Messages            []*state.Message
	APIMessages         []any
	APIKwargs           map[string]any
	ActiveSystemPrompt  string
	ConversationHistory []*state.Message
	APICallCount        int
	EffectiveTaskID     string
	TurnID              string
	APIRequestID        string
	APIStartTime        float64
	RetryCount          int
	MaxRetries          int
}

// Never retries an HTTP 200 refusal, one configured fallback try else the refusal surfaces with its reasoning or stop details
func (a *AIAgent) handleContentPolicyRefusal(ctx context.Context, r RefusalRequest) (RefusalVerdict, error) {
	refusal, err := a.normalizeResponseForAgent(r.Response)
	if err != nil {
		return RefusalVerdict{}, err
	}
	refusalText, err := pyStrOrEmpty(responseContent(refusal))
	if err != nil {
		return RefusalVerdict{}, err
	}
	refusalText = pystr.Strip(refusalText)
	if refusalText == "" {
		refusalText = pystr.Strip(a.extractReasoning(refusal))
	}
	rawStopDetails := provider.ResponseStopDetails(refusal)
	stopDetails, _ := rawStopDetails.(map[string]any)
	if refusalText == "" && stopDetails != nil {
		refusalText = pystr.Strip(pyrepr.Str(orEmptyString(stopDetails["explanation"])))
		if refusalText == "" && pyrepr.Truthy(stopDetails["category"]) {
			refusalText = "provider refusal category: " + pyrepr.Str(stopDetails["category"])
		}
	}

	hookMessage := refusalText
	if hookMessage == "" {
		hookMessage = "model declined to respond (content_filter)"
	}
	a.invokeAPIRequestErrorHook(ctx, map[string]any{
		"task_id": r.EffectiveTaskID, "turn_id": r.TurnID, "api_request_id": r.APIRequestID,
		"api_call_count": r.APICallCount, "api_start_time": r.APIStartTime, "api_kwargs": r.APIKwargs,
		"error_type": "ContentPolicyBlocked", "error_message": hookMessage, "status_code": nil,
		"retry_count": r.RetryCount, "max_retries": r.MaxRetries, "retryable": false,
		"reason": string(provider.ReasonContentPolicyBlocked),
	})
	stopThinkingSpinner(a, r.ThinkingSpinner)

	if a.hasPendingFallback() {
		a.BufferDiagnosticStatus("⚠️ Model declined to respond (safety refusal) — trying fallback...")
	}
	if a.tryActivateFallback(ctx, "") {
		prompt := armFallbackRestart(ctx, a, r.APIMessages, r.ActiveSystemPrompt, r.Retry)
		return RefusalVerdict{Action: "break", ActiveSystemPrompt: prompt}, nil
	}

	a.flushStatusBuffer()
	refusalLog := refusalText
	if runes := []rune(refusalText); len(runes) > 500 {
		refusalLog = string(runes[:500]) + "..."
	}
	if refusalLog == "" {
		refusalLog = "(no text)"
	}
	nativeStop := pyrepr.Str(orEmptyString(state.ToolCallField(r.Response, "stop_reason")))
	if nativeStop == "" {
		nativeStop = "n/a"
	}
	details := "n/a"
	if pyrepr.Truthy(rawStopDetails) {
		details = pyrepr.Str(rawStopDetails)
	}
	conversationLoopLog().Warn(fmt.Sprintf("%sModel declined to respond (finish_reason=content_filter). model=%s provider=%s native_stop_reason=%s stop_details=%s refusal=%s",
		a.logPrefix, a.model, a.provider, nativeStop, details, refusalLog))
	a.emitDiagnosticStatus("⚠️ The model declined to respond to this request (safety refusal).")
	summary := refusalText
	if summary == "" {
		summary = "the model returned no explanation"
	}
	refusalResponse := "⚠️ " + ContentPolicyCopy(ProviderLabelFor(a.provider), summary)
	a.cleanupTaskResources(tools.WithEnvHome(ctx, a.env), r.EffectiveTaskID)
	if err := a.persistSession(&r.Messages, r.ConversationHistory); err != nil {
		return RefusalVerdict{}, err
	}
	detail := refusalText
	if detail == "" {
		detail = "model declined (content_filter)"
	}
	return RefusalVerdict{Action: "return", Result: contentPolicyBlockedResult(r.Messages, r.APICallCount, refusalResponse, detail), ActiveSystemPrompt: r.ActiveSystemPrompt}, nil
}
