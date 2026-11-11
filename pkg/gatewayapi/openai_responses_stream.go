package gatewayapi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Recorded SSE event of a keyed responses stream for idempotent replay
type recordedEvent struct {
	eventType string
	data      *pyjson.Object
}

// Open function call of a responses stream
type pendingToolCall struct {
	callID, name, arguments, itemID string
	outputIndex                     int64
}

// Open reasoning item of a responses stream
type openReasoning struct {
	item        *pyjson.Object
	outputIndex int64
	parts       []string
}

// Per request state and event emitters of the responses SSE writer, every event carrying a sequence number
type responsesStream struct {
	mu                        sync.Mutex
	ctx                       context.Context
	adapter                   *APIServerAdapter
	response                  *gateway.WebStreamResponse
	responseID                string
	model                     any
	createdAt                 int64
	conversationHistory       []any
	userMessage               any
	instructions              any
	conversation              string
	store                     bool
	sessionID                 string
	durableKey                string
	durableFingerprint        string
	terminalReplay            bool
	responseStore             *gateway.ResponseStore
	finalTextParts            []string
	pendingToolCalls          []*pendingToolCall
	emittedItems              []*pyjson.Object
	outputIndex               int64
	callCounter               int
	sequenceNumber            int64
	identityCounts            map[gateway.APIItemCounterKey]int
	publishedOutputIDs        []any
	suppressIncremental       bool
	messageItemID             string
	messageOutputIndex        *int64
	messageOpened             bool
	reasoning                 *openReasoning
	finalResponseText         string
	transformedFinal          string
	agentError                string
	usage                     map[string]any
	terminalSnapshotPersisted bool
	recordedEvents            []recordedEvent
	result                    map[string]any
	batchBuf                  []string
	batchTimer                *time.Timer
}

// Responses stream state for one request, the store resolved in the request's profile scope
func newResponsesStream(ctx context.Context, adapter *APIServerAdapter, response *gateway.WebStreamResponse, responseID string, model any, createdAt int64,
	history []any, userMessage, instructions any, conversation string, store bool, sessionID, durableKey, durableFingerprint string, terminalReplay bool) (*responsesStream, error) {
	responseStore, err := adapter.currentResponseStore(ctx)
	if err != nil {
		return nil, err
	}
	s := &responsesStream{ctx: ctx, adapter: adapter, response: response, responseID: responseID, model: model, createdAt: createdAt,
		conversationHistory: history, userMessage: userMessage, instructions: instructions, conversation: conversation, store: store,
		sessionID: sessionID, durableKey: durableKey, durableFingerprint: durableFingerprint, terminalReplay: terminalReplay,
		responseStore: responseStore, identityCounts: map[gateway.APIItemCounterKey]int{},
		usage: map[string]any{"input_tokens": int64(0), "output_tokens": int64(0), "total_tokens": int64(0)}}
	if s.messageItemID, err = s.itemID("message", nil, false); err != nil {
		return nil, err
	}
	return s, nil
}

// Writes one SSE event, a keyed stream keeping the published output prefix consistent
func (s *responsesStream) writeEvent(eventType string, data *pyjson.Object) error {
	if s.durableKey != "" {
		if rawIndex, ok := data.Get("output_index"); ok {
			if s.suppressIncremental {
				return nil
			}
			index := toInt64(rawIndex)
			valid := false
			if eventType == "response.output_item.added" {
				valid = index == int64(len(s.publishedOutputIDs))
				if valid {
					item, _ := data.GetOr("item", nil).(*pyjson.Object)
					s.publishedOutputIDs = append(s.publishedOutputIDs, item.GetOr("id", nil))
				}
			} else {
				identity := data.GetOr("item_id", nil)
				if identity == nil {
					if item, isObj := data.GetOr("item", nil).(*pyjson.Object); isObj {
						identity = item.GetOr("id", nil)
					}
				}
				valid = index >= 0 && index < int64(len(s.publishedOutputIDs)) && (identity == nil || s.publishedOutputIDs[index] == identity)
			}
			if !valid {
				s.suppressIncremental = true
				return nil
			}
		}
	}
	if _, ok := data.Get("sequence_number"); !ok {
		data.Set("sequence_number", s.sequenceNumber)
	}
	s.sequenceNumber++
	if s.durableKey != "" {
		s.recordedEvents = append(s.recordedEvents, recordedEvent{eventType, pyjson.Clone(data).(*pyjson.Object)})
	}
	frame, err := gateway.APISSEFrame(data, eventType, true, nil)
	if err != nil {
		return err
	}
	return s.response.Write(frame)
}

// Stable item id of this response
func (s *responsesStream) itemID(kind string, callID any, commentary bool) (string, error) {
	return gateway.APINextItemID(s.responseID, s.identityCounts, kind, callID, commentary)
}

// Output index of an item, the keyed store's durable order or the next local slot
func (s *responsesStream) itemOutputIndex(itemID string) (int64, error) {
	index := s.outputIndex
	if s.durableKey != "" {
		var err error
		if index, err = s.responseStore.OutputIndex(s.ctx, s.durableKey, itemID); err != nil {
			return 0, err
		}
	}
	s.outputIndex = max(s.outputIndex, index+1)
	return index, nil
}

// Envelope of the response at a status
func (s *responsesStream) envelope(status string) *pyjson.Object {
	return pyjson.ObjectOf("id", s.responseID, "object", "response", "status", status, "created_at", s.createdAt, "model", s.model)
}

// Envelope plus output, error when given, and usage in wire key order
func (s *responsesStream) terminalEnvelope(status string, output []*pyjson.Object, errText *string) *pyjson.Object {
	env := s.envelope(status)
	env.Set("output", objectsToAny(output))
	if errText != nil {
		if s.durableKey != "" {
			env.Set("error", pyjson.ObjectOf("code", "agent_error", "message", *errText))
		} else {
			env.Set("error", pyjson.ObjectOf("message", *errText, "type", "server_error"))
		}
	}
	env.Set("usage", responsesUsagePayload(s.usage))
	return env
}

// History of the request with this turn's user message appended
func (s *responsesStream) historyWithUser() []any {
	out := append([]any(nil), s.conversationHistory...)
	return append(out, pyjson.ObjectOf("role", "user", "content", s.userMessage))
}

// Persists a snapshot of the response when storing is on
func (s *responsesStream) persistSnapshot(env *pyjson.Object, history []any, sessionID string) error {
	if !s.store {
		return nil
	}
	_, err := s.responseStore.Atomically(s.ctx, func(ctx context.Context) (any, error) {
		return nil, s.persistSnapshotLocked(ctx, env, history, sessionID)
	})
	return err
}

// Settled check, body and conversation pointer as one store unit
func (s *responsesStream) persistSnapshotLocked(ctx context.Context, env *pyjson.Object, history []any, sessionID string) error {
	if s.durableKey != "" {
		record, err := s.responseStore.Get(ctx, s.durableKey)
		if err != nil {
			return err
		}
		if obj, _ := record.(*pyjson.Object); settled(obj) {
			return nil
		}
	}
	status, _ := env.Get("status")
	if status == "incomplete" && s.terminalSnapshotPersisted {
		return nil
	}
	advance := s.conversation != ""
	if advance && s.terminalReplay {
		current, err := s.responseStore.GetConversation(ctx, s.conversation)
		if err != nil {
			return err
		}
		advance = current == "" || current == s.responseID
	}
	if history == nil {
		history = s.historyWithUser()
	}
	if sessionID == "" {
		sessionID = s.sessionID
	}
	if err := s.responseStore.Put(ctx, s.responseID, pyjson.ObjectOf("response", env, "conversation_history", history,
		"instructions", s.instructions, "session_id", sessionID)); err != nil {
		return err
	}
	if advance {
		if err := s.responseStore.SetConversation(ctx, s.conversation, s.responseID); err != nil {
			return err
		}
	}
	if status != "in_progress" {
		s.terminalSnapshotPersisted = true
	}
	return nil
}

// Persists an incomplete snapshot when no terminal one was written
func (s *responsesStream) persistIncompleteIfNeeded() error {
	if !s.store || s.terminalSnapshotPersisted {
		return nil
	}
	text := strings.Join(s.finalTextParts, "")
	if text == "" {
		text = s.finalResponseText
	}
	items := append([]*pyjson.Object(nil), s.emittedItems...)
	history := s.historyWithUser()
	if text != "" {
		items = append(items, messageItem(text))
		history = append(history, pyjson.ObjectOf("role", "assistant", "content", text))
	}
	return s.persistSnapshot(s.terminalEnvelope("incomplete", items, nil), history, "")
}

// Emits response.created, stored first so an immediate read back finds it
func (s *responsesStream) emitCreated() error {
	env := s.envelope("in_progress")
	env.Set("output", []any{})
	if !s.terminalReplay {
		if err := s.persistSnapshot(env, nil, ""); err != nil {
			return err
		}
	}
	return s.writeEvent("response.created", pyjson.ObjectOf("type", "response.created", "response", env))
}

// Opens the assistant message item on the first text delta
func (s *responsesStream) openMessageItem() error {
	if s.messageOpened {
		return nil
	}
	s.messageOpened = true
	index, err := s.itemOutputIndex(s.messageItemID)
	if err != nil {
		return err
	}
	s.messageOutputIndex = &index
	return s.writeEvent("response.output_item.added", pyjson.ObjectOf("type", "response.output_item.added", "output_index", index,
		"item", pyjson.ObjectOf("id", s.messageItemID, "type", "message", "status", "in_progress", "role", "assistant", "content", []any{})))
}

// Emits one output text delta
func (s *responsesStream) emitTextDelta(delta string) error {
	if s.terminalReplay {
		s.finalTextParts = append(s.finalTextParts, delta)
		return nil
	}
	if err := s.closeReasoningItem(); err != nil {
		return err
	}
	if err := s.openMessageItem(); err != nil {
		return err
	}
	s.finalTextParts = append(s.finalTextParts, delta)
	return s.writeEvent("response.output_text.delta", pyjson.ObjectOf("type", "response.output_text.delta", "item_id", s.messageItemID,
		"output_index", *s.messageOutputIndex, "content_index", int64(0), "delta", delta, "logprobs", []any{}))
}

// Emits a reasoning summary delta, opening one reasoning item per thinking burst
func (s *responsesStream) emitReasoningDelta(delta string) error {
	if s.reasoning == nil {
		id, err := s.itemID("reasoning", nil, false)
		if err != nil {
			return err
		}
		item := pyjson.ObjectOf("id", id, "type", "reasoning", "status", "in_progress", "summary", []any{})
		index, err := s.itemOutputIndex(id)
		if err != nil {
			return err
		}
		s.reasoning = &openReasoning{item: item, outputIndex: index}
		if err := s.writeEvent("response.output_item.added", pyjson.ObjectOf("type", "response.output_item.added",
			"output_index", index, "item", item)); err != nil {
			return err
		}
		if err := s.writeEvent("response.reasoning_summary_part.added", pyjson.ObjectOf("type", "response.reasoning_summary_part.added",
			"item_id", id, "output_index", index, "summary_index", int64(0), "part", pyjson.ObjectOf("type", "summary_text", "text", ""))); err != nil {
			return err
		}
	}
	rs := s.reasoning
	rs.parts = append(rs.parts, delta)
	id, _ := rs.item.Get("id")
	return s.writeEvent("response.reasoning_summary_text.delta", pyjson.ObjectOf("type", "response.reasoning_summary_text.delta",
		"item_id", id, "output_index", rs.outputIndex, "summary_index", int64(0), "delta", delta))
}

// Closes the open reasoning item with its done events
func (s *responsesStream) closeReasoningItem() error {
	rs := s.reasoning
	s.reasoning = nil
	if rs == nil {
		return nil
	}
	text := strings.Join(rs.parts, "")
	item := rs.item.Copy()
	item.Set("status", "completed")
	item.Set("summary", []any{pyjson.ObjectOf("type", "summary_text", "text", text)})
	id, _ := item.Get("id")
	base := func(pairs ...any) *pyjson.Object {
		o := pyjson.ObjectOf(pairs[:2]...)
		o.Set("item_id", id)
		o.Set("output_index", rs.outputIndex)
		o.Set("summary_index", int64(0))
		for i := 2; i+1 < len(pairs); i += 2 {
			o.Set(pairs[i].(string), pairs[i+1])
		}
		return o
	}
	if err := s.writeEvent("response.reasoning_summary_text.done", base("type", "response.reasoning_summary_text.done", "text", text)); err != nil {
		return err
	}
	if err := s.writeEvent("response.reasoning_summary_part.done", base("type", "response.reasoning_summary_part.done",
		"part", pyjson.ObjectOf("type", "summary_text", "text", text))); err != nil {
		return err
	}
	s.emittedItems = append(s.emittedItems, item)
	return s.writeEvent("response.output_item.done", pyjson.ObjectOf("type", "response.output_item.done", "output_index", rs.outputIndex, "item", item))
}

// Mid turn commentary as its own completed message item, never part of the final text
func (s *responsesStream) emitCommentary(text string) error {
	if err := s.closeReasoningItem(); err != nil {
		return err
	}
	id, err := s.itemID("message", nil, true)
	if err != nil {
		return err
	}
	item := pyjson.ObjectOf("id", id, "status", "completed", "phase", "commentary")
	item.Update(messageItem(text))
	index, err := s.itemOutputIndex(id)
	if err != nil {
		return err
	}
	emitted := pyjson.ObjectOf("phase", "commentary")
	emitted.Update(messageItem(text))
	s.emittedItems = append(s.emittedItems, emitted)
	for _, event := range []string{"response.output_item.added", "response.output_item.done"} {
		if err := s.writeEvent(event, pyjson.ObjectOf("type", event, "output_index", index, "item", item)); err != nil {
			return err
		}
	}
	return nil
}

// Opens a function call item, the agent's tool call id winning over a generated one
func (s *responsesStream) emitToolStarted(payload *pyjson.Object) error {
	if err := s.closeReasoningItem(); err != nil {
		return err
	}
	s.callCounter++
	callID := pyrepr.Str(firstTruthy(payload.GetOr("tool_call_id", nil), fmt.Sprintf("call_%s_%d", s.responseID[5:], s.callCounter)))
	args := payload.GetOr("arguments", pyjson.NewObject())
	argumentsStr := ""
	if obj, isObj := args.(*pyjson.Object); isObj {
		encoded, err := pyjson.Dumps(obj)
		if err != nil {
			return err
		}
		argumentsStr = encoded
	} else {
		argumentsStr = pyrepr.Str(args)
	}
	name := payload.GetOr("name", "")
	id, err := s.itemID("function_call", callID, false)
	if err != nil {
		return err
	}
	item := pyjson.ObjectOf("id", id, "type", "function_call", "status", "in_progress", "name", name, "call_id", callID, "arguments", argumentsStr)
	index, err := s.itemOutputIndex(id)
	if err != nil {
		return err
	}
	s.pendingToolCalls = append(s.pendingToolCalls, &pendingToolCall{callID: callID, name: pyrepr.Str(name), arguments: argumentsStr,
		itemID: id, outputIndex: index})
	s.emittedItems = append(s.emittedItems, pyjson.ObjectOf("type", "function_call", "name", name, "arguments", argumentsStr, "call_id", callID))
	return s.writeEvent("response.output_item.added", pyjson.ObjectOf("type", "response.output_item.added", "output_index", index, "item", item))
}

// Completes a function call and emits its output item, orphan completions skipped
func (s *responsesStream) emitToolCompleted(payload *pyjson.Object) error {
	callID := payload.GetOr("tool_call_id", nil)
	var pending *pendingToolCall
	pos := -1
	for i, p := range s.pendingToolCalls {
		if callID != nil && p.callID == pyrepr.Str(callID) {
			pending, pos = p, i
			break
		}
	}
	if !pyrepr.Truthy(callID) || pending == nil {
		return nil
	}
	s.pendingToolCalls = append(s.pendingToolCalls[:pos], s.pendingToolCalls[pos+1:]...)
	done := pyjson.ObjectOf("id", pending.itemID, "type", "function_call", "status", "completed", "name", pending.name,
		"call_id", pending.callID, "arguments", pending.arguments)
	if err := s.writeEvent("response.output_item.done", pyjson.ObjectOf("type", "response.output_item.done",
		"output_index", pending.outputIndex, "item", done)); err != nil {
		return err
	}
	result := payload.GetOr("result", "")
	resultStr, isStr := result.(string)
	if !isStr {
		encoded, err := pyjson.Dumps(result)
		if err != nil {
			return err
		}
		resultStr = encoded
	}
	parts := []any{pyjson.ObjectOf("type", "input_text", "text", resultStr)}
	id, err := s.itemID("function_call_output", callID, false)
	if err != nil {
		return err
	}
	outputItem := pyjson.ObjectOf("id", id, "type", "function_call_output", "call_id", pending.callID, "output", parts, "status", "completed")
	index, err := s.itemOutputIndex(id)
	if err != nil {
		return err
	}
	s.emittedItems = append(s.emittedItems, pyjson.ObjectOf("type", "function_call_output", "call_id", pending.callID, "output", parts))
	for _, event := range []string{"response.output_item.added", "response.output_item.done"} {
		if err := s.writeEvent(event, pyjson.ObjectOf("type", event, "output_index", index, "item", outputItem)); err != nil {
			return err
		}
	}
	return nil
}

// Lifecycle status as a custom event outside the output items
func (s *responsesStream) emitStatus(payload *pyjson.Object) error {
	frame, err := gateway.APISSEFrame(payload, "hermes.status", true, nil)
	if err != nil {
		return err
	}
	return s.response.Write(frame)
}

// Tagged queue item of the responses agent stream
type streamTagged struct {
	tag     string
	payload *pyjson.Object
}

// Routes one queue item, tagged items emitted at once and text batched for fifty milliseconds
func (s *responsesStream) dispatch(item any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminalReplay {
		return nil
	}
	switch v := item.(type) {
	case streamTagged:
		if err := s.flushBatchLocked(); err != nil {
			return err
		}
		switch v.tag {
		case "__tool_started__":
			return s.emitToolStarted(v.payload)
		case "__tool_completed__":
			return s.emitToolCompleted(v.payload)
		case "__commentary__":
			return s.emitCommentary(pyrepr.Str(v.payload.GetOr("text", "")))
		case "__reasoning__":
			return s.emitReasoningDelta(pyrepr.Str(v.payload.GetOr("text", "")))
		case "__status__":
			return s.emitStatus(v.payload)
		}
	case string:
		s.batchBuf = append(s.batchBuf, v)
		if s.batchTimer == nil {
			s.batchTimer = time.AfterFunc(50*time.Millisecond, func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.batchTimer = nil
				_ = s.flushBatchLocked()
			})
		}
	}
	return nil
}

// Stops a pending batch flush
func (s *responsesStream) cancelBatchTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.batchTimer != nil {
		s.batchTimer.Stop()
		s.batchTimer = nil
	}
}

// Emits one delta for all buffered text
func (s *responsesStream) flushBatch() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushBatchLocked()
}

func (s *responsesStream) flushBatchLocked() error {
	if len(s.batchBuf) == 0 {
		return nil
	}
	combined := strings.Join(s.batchBuf, "")
	s.batchBuf = nil
	return s.emitTextDelta(combined)
}

// Takes the agent outcome, emitting one fallback delta when the provider streamed nothing
func (s *responsesStream) collectResult(result, usage map[string]any, runErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runErr != nil {
		apiLog().Error(fmt.Sprintf("Error running agent for streaming responses: %v", runErr))
		s.agentError = redactAPIErrorText(runErr.Error(), -1)
		return nil
	}
	s.result = result
	if len(usage) > 0 {
		s.usage = usage
	}
	agentFinal, _ := result["final_response"].(string)
	if s.terminalReplay {
		s.finalResponseText = agentFinal
		if s.finalResponseText == "" {
			s.finalResponseText = redactAPIErrorText(firstTruthy(result["error"], "(No response generated)"), -1)
		}
		s.finalTextParts = []string{s.finalResponseText}
		return nil
	}
	tail, appended := postStreamTransform(result)
	if tail != "" && len(s.finalTextParts) > 0 {
		if appended {
			if err := s.emitTextDelta(tail); err != nil {
				return err
			}
		} else {
			s.transformedFinal = agentFinal
		}
	}
	if agentFinal != "" && len(s.finalTextParts) == 0 {
		if err := s.emitTextDelta(agentFinal); err != nil {
			return err
		}
	}
	if agentFinal != "" && s.finalResponseText == "" {
		s.finalResponseText = agentFinal
	}
	if pyrepr.Truthy(result["error"]) && s.finalResponseText == "" {
		s.agentError = redactAPIErrorText(result["error"], -1)
	}
	return nil
}

// Closes the reasoning and message items with their done events
func (s *responsesStream) closeMessageItem() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closeReasoningItem(); err != nil {
		return err
	}
	switch {
	case s.transformedFinal != "":
		s.finalResponseText = s.transformedFinal
	case len(s.finalTextParts) > 0 && strings.Join(s.finalTextParts, "") != "":
		s.finalResponseText = strings.Join(s.finalTextParts, "")
	}
	if !s.messageOpened {
		return nil
	}
	if err := s.writeEvent("response.output_text.done", pyjson.ObjectOf("type", "response.output_text.done", "item_id", s.messageItemID,
		"output_index", *s.messageOutputIndex, "content_index", int64(0), "text", s.finalResponseText, "logprobs", []any{})); err != nil {
		return err
	}
	return s.writeEvent("response.output_item.done", pyjson.ObjectOf("type", "response.output_item.done", "output_index", *s.messageOutputIndex,
		"item", pyjson.ObjectOf("id", s.messageItemID, "type", "message", "status", "completed", "role", "assistant",
			"content", []any{pyjson.ObjectOf("type", "output_text", "text", s.finalResponseText)})))
}

// Emitted items trimmed plus a final message item carrying the assistant text
func (s *responsesStream) finalItems() []*pyjson.Object {
	items := trimToolItems(append([]*pyjson.Object(nil), s.emittedItems...))
	text := s.finalResponseText
	if text == "" && s.agentError != "" {
		text = redactAPIErrorText(s.agentError, -1)
	}
	return append(items, messageItem(text))
}

// Persists and emits the failed terminal envelope
func (s *responsesStream) emitFailed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	errText := redactAPIErrorText(s.agentError, -1)
	env := s.terminalEnvelope("failed", s.finalItems(), &errText)
	history := s.historyWithUser()
	content := s.finalResponseText
	if content == "" {
		content = errText
	}
	history = append(history, pyjson.ObjectOf("role", "assistant", "content", content))
	if err := s.persistSnapshot(env, history, ""); err != nil {
		return err
	}
	return s.writeEvent("response.failed", pyjson.ObjectOf("type", "response.failed", "response", env))
}

// Commits and emits the terminal envelope, a keyed stream recording its replay and adopting a settled one
func (s *responsesStream) emitCompleted() (*pyjson.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.result
	status := responseStatus(result)
	output := s.finalItems()
	if s.durableKey != "" {
		start := responseMessagesTurnStartIndex(s.conversationHistory, s.userMessage, result)
		extracted, err := extractOutputItems(result, start, s.responseID)
		if err != nil {
			return nil, err
		}
		if output, err = s.responseStore.OrderOutput(s.ctx, s.durableKey, extracted); err != nil {
			return nil, err
		}
	}
	var errText *string
	if status == "failed" {
		text := redactAPIErrorText(firstTruthy(result["error"], "The admitted turn failed."), -1)
		errText = &text
	}
	env := s.terminalEnvelope(status, output, errText)
	fullHistory := buildResponseConversationHistory(s.conversationHistory, s.userMessage, result, s.finalResponseText,
		s.adapter.historyToolOutputMaxChars)
	sid, _ := result["session_id"].(string)
	var record *pyjson.Object
	if s.durableKey != "" && !s.terminalReplay {
		headers := pyjson.NewObject()
		for k, values := range s.response.Headers {
			if strings.HasPrefix(k, "X-Hermes-") && len(values) > 0 {
				headers.Set(k, values[0])
			}
		}
		terminal := pyjson.ObjectOf("type", "response."+status, "response", env, "sequence_number", s.sequenceNumber)
		events := make([]any, 0, len(s.recordedEvents)+1)
		for _, ev := range s.recordedEvents {
			events = append(events, []any{ev.eventType, ev.data})
		}
		events = append(events, []any{"response." + status, terminal})
		record = pyjson.ObjectOf("fingerprint", s.durableFingerprint, "response", env, "headers", headers, "events", events)
	}
	stored, err := s.responseStore.Atomically(s.ctx, func(ctx context.Context) (any, error) {
		if s.store {
			if err := s.persistSnapshotLocked(ctx, env, fullHistory, sid); err != nil {
				return nil, err
			}
		}
		if s.durableKey == "" {
			return nil, nil
		}
		if record != nil {
			if err := s.responseStore.Put(ctx, s.durableKey, record); err != nil {
				return nil, err
			}
		}
		return s.responseStore.Get(ctx, s.durableKey)
	})
	if err != nil {
		return nil, err
	}
	if s.durableKey != "" {
		if obj, _ := stored.(*pyjson.Object); settled(obj) {
			env, _ = obj.GetOr("response", nil).(*pyjson.Object)
		}
		status = pyrepr.Str(env.GetOr("status", ""))
	}
	if err := s.writeEvent("response."+status, pyjson.ObjectOf("type", "response."+status, "response", env)); err != nil {
		return nil, err
	}
	return env, nil
}

// Emits the failed envelope of a crashed writer
func (s *responsesStream) emitCrash(crash error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	errText := redactAPIErrorText(crash.Error(), 500)
	env := s.terminalEnvelope("failed", append([]*pyjson.Object(nil), s.emittedItems...), &errText)
	return s.writeEvent("response.failed", pyjson.ObjectOf("type", "response.failed", "response", env))
}

// Integer of a decoded JSON number
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return -1
}

// Object list as a JSON list
func objectsToAny(items []*pyjson.Object) []any {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it
	}
	return out
}
