package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	toolRoundHousekeepingTools = map[string]bool{"memory": true, "todo_list": true, "skill_manage": true, "session_search": true}
	toolRoundLineIsReplyTools  = map[string]bool{"memory": true, "todo_list": true, "skill_manage": true, "session_search": true, "setup_beat": true}
	toolCallStaleMarkerRE      = regexp.MustCompile(`^\[[A-Za-z_][A-Za-z0-9_.-]*\]$`)
)

// Agent and loop state the tool call validation reads and mutates for one assistant response
type toolRoundValidationHost struct {
	ctx              context.Context
	a                *AIAgent
	s                *LoopState
	assistantMessage *provider.Response
	finishReason     string
	retries          tools.TurnValidationRetries
	err              error
}

func (h *toolRoundValidationHost) ValidToolNames() map[string]bool { return h.a.validToolNames }

func (h *toolRoundValidationHost) RepairToolName(name string) string {
	repaired, _ := h.a.repairToolCall(name)
	return repaired
}

func (h *toolRoundValidationHost) Messages() []*state.Message { return h.s.Messages }

func (h *toolRoundValidationHost) AppendAssistantMessage() {
	appendLiveMessage(&h.s.Messages, h.a.buildAssistantMessage(h.assistantMessage, h.finishReason))
}

// One tool result per call so every tool call keeps a matching result
func (h *toolRoundValidationHost) AppendToolErrorResults(contentFor func(tc state.ToolCall) string) {
	for _, tc := range h.assistantMessage.Message.ToolCalls {
		appendLiveMessage(&h.s.Messages, map[string]any{
			"role": "tool", "name": tc.Function.Name, "tool_call_id": state.CoalesceToolCallID(tc), "content": contentFor(tc),
		})
	}
}

// Closes a tool result tail, persists, and returns the stamped terminal partial result
func (h *toolRoundValidationHost) PartialExit(finalResponse string) map[string]any {
	closeLiveToolSequence(&h.s.Messages, finalResponse)
	if err := h.a.persistSession(&h.s.Messages, h.s.ConversationHistory); err != nil && h.err == nil {
		h.err = err
	}
	return StampFailure(map[string]any{
		"final_response": finalResponse, "messages": h.s.Messages, "api_calls": h.s.APICallCount,
		"completed": false, "partial": true, "error": finalResponse,
	}, "truncated", true)
}

func (h *toolRoundValidationHost) CleanupTaskResources() {
	h.a.cleanupTaskResources(h.ctx, h.s.EffectiveTaskID)
}

func (h *toolRoundValidationHost) RecordToolCallQuality(toolCalls []state.ToolCall, repaired map[int]bool) {
	observability.RecordValidatedToolCalls(h.ctx, h.a, toolCalls, repaired)
}

func (h *toolRoundValidationHost) Print(text string, force bool) { h.a.vprint(text, force, true) }

func (h *toolRoundValidationHost) BufferPrint(text string) { h.a.bufferVprint(text) }

func (h *toolRoundValidationHost) FlushStatusBuffer() { h.a.flushStatusBuffer() }

// Failure copy for a code, the lookup error kept for the round to raise
func (h *toolRoundValidationHost) SiteCopy(code string) string {
	text, err := tools.SiteCopy(h.ctx, code, nil)
	if err != nil && h.err == nil {
		h.err = err
	}
	return text
}

func (h *toolRoundValidationHost) LogPrefix() string { return h.a.logPrefix }

func (h *toolRoundValidationHost) Retries() *tools.TurnValidationRetries { return &h.retries }

// Validates the response tool calls and writes the strike counters back to the agent
func validateRoundToolCalls(ctx context.Context, a *AIAgent, s *LoopState, assistantMessage *provider.Response, finishReason string) (tools.ToolValidationVerdict, error) {
	h := &toolRoundValidationHost{ctx: ctx, a: a, s: s, assistantMessage: assistantMessage, finishReason: finishReason,
		retries: tools.TurnValidationRetries{InvalidTool: a.invalidToolRetries, InvalidJSON: a.invalidJSONRetries}}
	verdict := tools.ValidateToolCalls(h, assistantMessage.Message.ToolCalls, finishReason)
	a.invalidToolRetries, a.invalidJSONRetries = h.retries.InvalidTool, h.retries.InvalidJSON
	return verdict, h.err
}

// Flushes the tool call turn before execution, a raised failure classified and logged as a failed append
func flushToolTurn(a *AIAgent, s *LoopState) (persisted bool) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			exc := RecoveredOuterLoopError(r)
			persisted = false
			a.lastPersistenceErrorCause = state.ClassifyPersistenceError(exc)
			sessionID := a.sessionID
			if sessionID == "" {
				sessionID = "none"
			}
			conversationLoopLog().Warn(fmt.Sprintf("Incremental tool-call persistence failed before execution (session=%s): %s", sessionID, exc))
		}
	}()
	_, err := a.flushLiveMessages(s.Messages, s.ConversationHistory)
	return err == nil
}

// Runs one tool round in order, persisting the tool call turn before any tool side effect
func runToolRound(ctx context.Context, a *AIAgent, s *LoopState) (PhaseVerdict, error) {
	assistantMessage, finishReason := s.AssistantMessage, s.FinishReason
	msg := assistantMessage.Message
	if !a.quietMode {
		a.vprint(fmt.Sprintf("%s🔧 Processing %d tool call(s)...", a.logPrefix, len(msg.ToolCalls)), false, false)
	}
	if a.verboseLogging {
		for _, tc := range msg.ToolCalls {
			slog.Debug(fmt.Sprintf("Tool call: %s with args: %s...", tc.Function.Name, pystr.Slice(tc.Function.Arguments, 0, 200)))
		}
	}

	tvv, err := validateRoundToolCalls(ctx, a, s, assistantMessage, finishReason)
	if err != nil {
		return PhaseVerdict{}, err
	}
	switch tvv.Action {
	case "return":
		return PhaseVerdict{Action: "return", Result: tvv.Result}, nil
	case "continue":
		return PhaseVerdict{Action: "continue"}, nil
	}

	expandRoundToolBatches(msg)
	msg.ToolCalls = deduplicateToolCalls(capDelegateTaskCalls(ctx, msg.ToolCalls))
	msg.ToolCalls = tools.NormalizeProviderToolCallIDs(msg.ToolCalls)

	var invalidBatchCalls []state.ToolCall
	if tvv.MixedInvalidBatch {
		for _, tc := range msg.ToolCalls {
			if !a.validToolNames[tc.Function.Name] {
				invalidBatchCalls = append(invalidBatchCalls, tc)
			}
		}
	}

	assistantMsg, duplicatePreviousInterim, err := stageToolCallMessage(a, assistantMessage, finishReason, &s.Messages)
	if err != nil {
		return PhaseVerdict{}, err
	}
	appendLiveMessage(&s.Messages, assistantMsg)

	if len(invalidBatchCalls) > 0 {
		for _, tc := range invalidBatchCalls {
			appendLiveMessage(&s.Messages, map[string]any{
				"role": "tool", "name": tc.Function.Name, "tool_call_id": state.CoalesceToolCallID(tc),
				"content": tools.InvalidToolNameErrorContent(tc.Function.Name, a.validToolNames),
			})
		}
		kept := make([]state.ToolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			if a.validToolNames[tc.Function.Name] {
				kept = append(kept, tc)
			}
		}
		msg.ToolCalls = kept
	}

	if !flushToolTurn(a, s) {
		if a.lastPersistenceErrorCause == nil {
			a.lastPersistenceErrorCause = "unknown"
		}
		s.TurnExitReason, s.FinalResponse, s.Failed = "session_persistence_failed", "", true
		return PhaseVerdict{Action: "break"}, nil
	}

	if !duplicatePreviousInterim {
		a.emitInterimAssistantMessage(assistantMsg)
	}
	if a.streamDeltaCallback != nil {
		callQuietly(a.streamDeltaCallback, nil)
	}
	MaybePrepareCompaction(ctx, a, liveMessageDicts(s.Messages), "tool-batch")

	if err := a.executeToolCalls(ctx, msg, &s.Messages, s.EffectiveTaskID); err != nil {
		return PhaseVerdict{}, err
	}
	observability.FinishToolRound(ctx, a)

	if a.incrementalPersistenceFailed {
		s.TurnExitReason, s.FinalResponse, s.Failed = "session_persistence_failed", "", true
		return PhaseVerdict{Action: "break"}, nil
	}

	if decision := a.toolGuardrailHaltDecision; decision != nil {
		finalResponse := toolguardControlledHaltResponse(*decision)
		s.TurnExitReason, s.FinalResponse = "guardrail_halt", finalResponse
		a.emitDiagnosticStatus(fmt.Sprintf("⚠️ Tool guardrail halted %s: %s", decision.ToolName, decision.Code))
		appendLiveMessage(&s.Messages, map[string]any{"role": "assistant", "content": finalResponse})
		if finalResponse != "" {
			a.safePrint("\n"+finalResponse+"\n", false)
			if a.streamDeltaCallback != nil && !a.holdStream {
				if callQuietly(a.streamDeltaCallback, finalResponse) {
					callQuietly(a.streamDeltaCallback, nil)
				}
			}
		}
		return PhaseVerdict{Action: "break"}, nil
	}

	a.streamNeedsBreak = true
	if onlyExecuteCode(msg.ToolCalls) {
		a.iterationBudget.Refund()
	}

	endTurn, err := compressAfterToolResults(ctx, a, s)
	if err != nil {
		return PhaseVerdict{}, err
	}
	s.TruncatedToolCallRetries = 0
	if endTurn {
		return PhaseVerdict{Action: "break"}, nil
	}
	a.sessionMessages = s.Messages
	a.TouchActivity(fmt.Sprintf("tool results posted, continuing iteration #%d", s.APICallCount))
	return PhaseVerdict{Action: "continue"}, nil
}

// Splits local tool batches, rewriting the ordered Anthropic and Bedrock content block carriers the provider data holds
func expandRoundToolBatches(msg *state.Message) {
	providerData := map[string]any{}
	anthropicBlocks, hasAnthropic := provider.AnthropicLiveBlocks(msg)
	if hasAnthropic {
		providerData["anthropic_content_blocks"] = anthropicBlocks
	}
	if blocks, ok := msg.Extra["bedrock_content_blocks"]; ok {
		providerData["bedrock_content_blocks"] = blocks
	}
	if len(providerData) == 0 {
		providerData = nil
	}
	msg.ToolCalls = tools.ExpandLocalToolBatches(msg.ToolCalls, providerData)
	if rewritten, ok := providerData["anthropic_content_blocks"].([]any); ok && hasAnthropic && !sameBlockList(rewritten, anthropicBlocks) {
		provider.SetAnthropicLiveBlocks(msg, rewritten)
	}
	if rewritten, ok := providerData["bedrock_content_blocks"]; ok {
		msg.Extra["bedrock_content_blocks"] = rewritten
	}
}

// Whether two block lists share one backing array and length
func sameBlockList(a, b []any) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// Whether the distinct tool names of the round are exactly execute_code
func onlyExecuteCode(toolCalls []state.ToolCall) bool {
	if len(toolCalls) == 0 {
		return false
	}
	for _, tc := range toolCalls {
		if tc.Function.Name != "execute_code" {
			return false
		}
	}
	return true
}

// Dict views of the live transcript rows
func liveMessageDicts(messages []*state.Message) []map[string]any {
	rows := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		if m != nil {
			rows = append(rows, state.MessageToDict(m))
		}
	}
	return rows
}

// Builds the tool call row and updates fallback, mute and retry state, true when it repeats the prior interim
var stageToolCallMessage = func(a *AIAgent, assistantMessage *provider.Response, finishReason string, messages *[]*state.Message) (map[string]any, bool, error) {
	assistantMsg := a.buildAssistantMessage(assistantMessage, finishReason)
	msg := assistantMessage.Message
	turnContent, isText := msg.Content.(string)
	if !isText && pyrepr.Truthy(msg.Content) {
		return nil, false, &pyrepr.FormatError{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(msg.Content))}
	}

	if len(msg.ToolCalls) > 0 && toolCallStaleMarkerRE.MatchString(pystr.Strip(turnContent)) {
		conversationLoopLog().Warn(fmt.Sprintf("Discarding bare tool-call marker from assistant content: %s", turnContent))
		turnContent = ""
		assistantMsg["content"] = ""
	}

	allHousekeeping, lineIsReply := true, true
	for _, tc := range msg.ToolCalls {
		allHousekeeping = allHousekeeping && toolRoundHousekeepingTools[tc.Function.Name]
		lineIsReply = lineIsReply && toolRoundLineIsReplyTools[tc.Function.Name]
	}
	if len(msg.ToolCalls) > 0 && !allHousekeeping {
		a.lastContentWithTools, a.lastContentToolsAllHousekeeping, a.mutePostResponse = nil, false, false
	}

	if turnContent != "" && a.hasContentAfterThinkBlock(turnContent) {
		a.lastContentWithTools, a.lastContentToolsAllHousekeeping = turnContent, lineIsReply
		if allHousekeeping && a.hasStreamConsumers() {
			a.mutePostResponse = true
		} else if a.shouldEmitQuietToolMessages() && !a.holdStream {
			if clean := pystr.Strip(a.stripThinkBlocks(turnContent)); clean != "" {
				a.vprint("  ┊ 💬 "+clean, false, false)
			}
		}
	}

	hadPrefill := false
	for len(*messages) > 0 {
		last := (*messages)[len(*messages)-1]
		if last == nil || !pyrepr.Truthy(last.Extra["_thinking_prefill"]) {
			break
		}
		*messages = (*messages)[:len(*messages)-1]
		hadPrefill = true
	}
	if hadPrefill {
		a.thinkingPrefillRetries, a.emptyContentRetries = 0, 0
	}
	a.postToolEmptyRetried = false
	a.droppedToolcallRetries = 0

	var previous *state.Message
	if n := len(*messages); n > 0 {
		previous = (*messages)[n-1]
	}
	currentVisible := a.interimAssistantVisibleText(assistantMsg)
	duplicate := currentVisible != "" && previous != nil && previous.Role == "assistant" &&
		previous.FinishReason == "incomplete" && a.interimAssistantVisibleText(state.MessageToDict(previous)) == currentVisible
	return assistantMsg, duplicate, nil
}
