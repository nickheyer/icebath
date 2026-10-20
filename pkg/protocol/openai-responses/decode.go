package openairesponses

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/sse"
	"github.com/nickheyer/icebath/pkg/protocol"
)

const (
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"
	statusFailed     = "failed"
	partSeparator    = "\n\n"
	doneSentinel     = "[DONE]"
)

// Parses a complete Response body
func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return protocol.Response{}, fmt.Errorf("openai-responses: response body: %w", err)
	}
	return convert(&resp)
}

func convert(resp *response) (protocol.Response, error) {
	if err := terminal(resp); err != nil {
		return protocol.Response{}, err
	}
	out := protocol.Response{ID: resp.ID, Model: resp.Model, Usage: usageOf(resp.Usage)}
	toolCall := false
	for i, item := range resp.Output {
		parts, err := itemParts(item)
		if err != nil {
			return protocol.Response{}, fmt.Errorf("openai-responses: output item %d: %w", i, err)
		}
		toolCall = toolCall || item.Type == itemFunctionCall
		out.Content = append(out.Content, parts...)
	}
	out.StopReason, out.RawStopReason = stopReason(resp, toolCall)
	return out, nil
}

// Rejects statuses that cannot end a turn, surfacing a failed response as an error
func terminal(resp *response) error {
	switch resp.Status {
	case statusCompleted, statusIncomplete:
		return nil
	case statusFailed:
		e := &protocol.Error{Status: 200, Message: "response failed"}
		if resp.Error != nil {
			e.Type, e.Message = resp.Error.Code, resp.Error.Message
		}
		return e
	}
	return fmt.Errorf("openai-responses: response status %q is not terminal", resp.Status)
}

func itemParts(item outputItem) ([]protocol.Part, error) {
	switch item.Type {
	case itemMessage:
		parts := make([]protocol.Part, 0, len(item.Content))
		for _, c := range item.Content {
			switch c.Type {
			case partOutputText:
				parts = append(parts, protocol.Text{Text: c.Text})
			case partRefusal:
				parts = append(parts, protocol.Text{Text: c.Refusal})
			default:
				return nil, fmt.Errorf("message part type %q has no neutral form", c.Type)
			}
		}
		return parts, nil
	case itemFunctionCall:
		args, err := protocol.ParseArgs(item.Arguments)
		if err != nil {
			return nil, fmt.Errorf("function call %s: %w", item.Name, err)
		}
		return []protocol.Part{protocol.ToolCall{ID: item.CallID, Name: item.Name, Args: args}}, nil
	case itemReasoning:
		return []protocol.Part{protocol.Thinking{Text: reasoningText(item), Signature: signatureOf(item)}}, nil
	}
	return nil, fmt.Errorf("output item type %q has no neutral form", item.Type)
}

// Joins summary and reasoning texts of a reasoning item with blank lines
func reasoningText(item outputItem) string {
	texts := make([]string, 0, len(item.Summary)+len(item.Content))
	for _, s := range item.Summary {
		texts = append(texts, s.Text)
	}
	for _, c := range item.Content {
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, partSeparator)
}

func signatureOf(item outputItem) string {
	raw, _ := json.Marshal(reasoningSignature{ID: item.ID, EncryptedContent: item.EncryptedContent})
	return string(raw)
}

func stopReason(resp *response, toolCall bool) (protocol.StopReason, string) {
	if resp.Status == statusCompleted {
		if toolCall {
			return protocol.StopToolUse, statusCompleted
		}
		return protocol.StopEnd, statusCompleted
	}
	reason := ""
	if resp.IncompleteDetails != nil {
		reason = resp.IncompleteDetails.Reason
	}
	raw := statusIncomplete
	if reason != "" {
		raw += "/" + reason
	}
	switch reason {
	case "max_output_tokens":
		return protocol.StopLength, raw
	case "content_filter":
		return protocol.StopContentFilter, raw
	}
	return protocol.StopOther, raw
}

func usageOf(u *usage) protocol.Usage {
	if u == nil {
		return protocol.Usage{}
	}
	return protocol.Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.InputTokensDetails.CachedTokens,
		CacheWrite: u.InputTokensDetails.CacheWriteTokens,
		Reasoning:  u.OutputTokensDetails.ReasoningTokens,
	}
}

// Parses an error body, falling back to the raw text when it is not the wire error shape
func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Body: body}
	var wire errorBody
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error.Message != "" {
		e.Type = wire.Error.Code
		if e.Type == "" {
			e.Type = wire.Error.Type
		}
		e.Message = wire.Error.Message
		return e
	}
	e.Message = strings.TrimSpace(string(body))
	return e
}

// Turns the SSE event stream into neutral events, ending at the terminal response event
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		st := streamState{acc: map[string]string{}, parts: map[string]int{}}
		for ev, err := range sse.Read(r) {
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			if ev.Data == doneSentinel {
				continue
			}
			var e streamEvent
			if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
				yield(protocol.Event{}, fmt.Errorf("openai-responses: event %q: %w", ev.Name, err))
				return
			}
			done, err := st.apply(&e)
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			for _, out := range st.b.Events() {
				if !yield(out, nil) {
					return
				}
			}
			if done {
				return
			}
		}
		yield(protocol.Event{}, errors.New("openai-responses: stream ended before a terminal response event"))
	}
}

// Stream decoder state keyed by item id, with accumulated text per builder key
type streamState struct {
	b        protocol.Builder
	acc      map[string]string
	parts    map[string]int
	toolCall bool
}

func textKey(itemID string, index int) string {
	return fmt.Sprintf("%s/%d", itemID, index)
}

func (st *streamState) apply(e *streamEvent) (bool, error) {
	switch e.Type {
	case "response.created", "response.in_progress", "response.queued":
		if e.Response != nil {
			st.b.Start(e.Response.ID, e.Response.Model)
		}
	case "response.output_item.added":
		return false, st.itemAdded(e.Item)
	case "response.content_part.added":
		return false, st.partAdded(e)
	case "response.output_text.delta", "response.refusal.delta":
		return false, st.delta(textKey(e.ItemID, e.ContentIndex), e.Delta)
	case "response.output_text.done":
		return false, st.reconcile(textKey(e.ItemID, e.ContentIndex), e.Text)
	case "response.refusal.done":
		return false, st.reconcile(textKey(e.ItemID, e.ContentIndex), e.Refusal)
	case "response.content_part.done":
		return false, st.partDone(e)
	case "response.reasoning_summary_part.added":
		return false, st.startPart(e.ItemID, "summary", e.SummaryIndex)
	case "response.reasoning_summary_text.delta":
		return false, st.partDelta(e.ItemID, "summary", e.SummaryIndex, e.Delta)
	case "response.reasoning_summary_text.done":
		return false, st.partFinal(e.ItemID, "summary", e.SummaryIndex, e.Text)
	case "response.reasoning_summary_part.done":
		if e.Part == nil {
			return false, errors.New("openai-responses: reasoning_summary_part.done without a part")
		}
		return false, st.partFinal(e.ItemID, "summary", e.SummaryIndex, e.Part.Text)
	case "response.reasoning_text.delta":
		return false, st.partDelta(e.ItemID, "content", e.ContentIndex, e.Delta)
	case "response.reasoning_text.done":
		return false, st.partFinal(e.ItemID, "content", e.ContentIndex, e.Text)
	case "response.function_call_arguments.delta":
		return false, st.delta(e.ItemID, e.Delta)
	case "response.function_call_arguments.done":
		return false, st.reconcile(e.ItemID, e.Arguments)
	case "response.output_item.done":
		return false, st.itemDone(e.Item)
	case "response.completed", "response.incomplete":
		return true, st.finish(e.Response)
	case "response.failed":
		err := &protocol.Error{Status: 200, Message: "response failed"}
		if e.Response != nil && e.Response.Error != nil {
			err.Type, err.Message = e.Response.Error.Code, e.Response.Error.Message
		}
		return false, err
	case "error":
		return false, &protocol.Error{Status: 200, Type: e.Code, Message: e.Message}
	}
	return false, nil
}

func (st *streamState) itemAdded(item *outputItem) error {
	if item == nil {
		return errors.New("openai-responses: output_item.added without an item")
	}
	switch item.Type {
	case itemFunctionCall:
		st.b.OpenToolCall(item.ID, item.CallID, item.Name)
	case itemReasoning:
		st.b.OpenThinking(item.ID, false)
	case itemMessage:
	default:
		return fmt.Errorf("openai-responses: output item type %q has no neutral form", item.Type)
	}
	return nil
}

func (st *streamState) partAdded(e *streamEvent) error {
	if e.Part == nil {
		return errors.New("openai-responses: content_part.added without a part")
	}
	switch e.Part.Type {
	case partOutputText, partRefusal:
		st.b.OpenText(textKey(e.ItemID, e.ContentIndex))
	case partReasoningText:
	default:
		return fmt.Errorf("openai-responses: content part type %q has no neutral form", e.Part.Type)
	}
	return nil
}

func (st *streamState) partDone(e *streamEvent) error {
	if e.Part == nil {
		return errors.New("openai-responses: content_part.done without a part")
	}
	key := textKey(e.ItemID, e.ContentIndex)
	switch e.Part.Type {
	case partOutputText:
		if err := st.reconcile(key, e.Part.Text); err != nil {
			return err
		}
	case partRefusal:
		if err := st.reconcile(key, e.Part.Refusal); err != nil {
			return err
		}
	case partReasoningText:
		return nil
	default:
		return fmt.Errorf("openai-responses: content part type %q has no neutral form", e.Part.Type)
	}
	return st.b.Close(key)
}

// Appends a fragment to the block under key and remembers it for later reconciliation
func (st *streamState) delta(key, s string) error {
	if err := st.b.Delta(key, s); err != nil {
		return err
	}
	st.acc[key] += s
	return nil
}

// Fills in any tail the deltas missed and rejects a final text that contradicts them
func (st *streamState) reconcile(key, full string) error {
	cur := st.acc[key]
	if !strings.HasPrefix(full, cur) {
		return fmt.Errorf("openai-responses: final text of %s diverges from its streamed deltas", key)
	}
	if len(full) == len(cur) {
		return nil
	}
	return st.delta(key, full[len(cur):])
}

// Separates reasoning parts of one item with a blank line the first time each part appears
func (st *streamState) startPart(itemID, kind string, index int) error {
	key := partKey(itemID, kind, index)
	if _, seen := st.acc[key]; seen {
		return nil
	}
	st.acc[key] = ""
	if st.parts[itemID] > 0 {
		if err := st.delta(itemID, partSeparator); err != nil {
			return err
		}
	}
	st.parts[itemID]++
	return nil
}

func partKey(itemID, kind string, index int) string {
	return fmt.Sprintf("%s/%s/%d", itemID, kind, index)
}

// Appends a reasoning fragment to its item and to the part it belongs to
func (st *streamState) partDelta(itemID, kind string, index int, s string) error {
	if err := st.startPart(itemID, kind, index); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	if err := st.delta(itemID, s); err != nil {
		return err
	}
	st.acc[partKey(itemID, kind, index)] += s
	return nil
}

// Fills in the tail of a reasoning part its deltas missed and rejects a contradicting final text
func (st *streamState) partFinal(itemID, kind string, index int, full string) error {
	if err := st.startPart(itemID, kind, index); err != nil {
		return err
	}
	cur := st.acc[partKey(itemID, kind, index)]
	if !strings.HasPrefix(full, cur) {
		return fmt.Errorf("openai-responses: final text of %s diverges from its streamed deltas", partKey(itemID, kind, index))
	}
	return st.partDelta(itemID, kind, index, full[len(cur):])
}

func (st *streamState) itemDone(item *outputItem) error {
	if item == nil {
		return errors.New("openai-responses: output_item.done without an item")
	}
	switch item.Type {
	case itemFunctionCall:
		st.toolCall = true
		if err := st.reconcile(item.ID, item.Arguments); err != nil {
			return err
		}
		return st.b.Close(item.ID)
	case itemReasoning:
		if err := st.reconcile(item.ID, reasoningText(*item)); err != nil {
			return err
		}
		if err := st.b.Signature(item.ID, signatureOf(*item)); err != nil {
			return err
		}
		return st.b.Close(item.ID)
	case itemMessage:
		return nil
	}
	return fmt.Errorf("openai-responses: output item type %q has no neutral form", item.Type)
}

func (st *streamState) finish(resp *response) error {
	if resp == nil {
		return errors.New("openai-responses: terminal event without a response")
	}
	st.b.Start(resp.ID, resp.Model)
	toolCall := st.toolCall
	for _, item := range resp.Output {
		toolCall = toolCall || item.Type == itemFunctionCall
	}
	stop, raw := stopReason(resp, toolCall)
	return st.b.Finish(stop, raw, usageOf(resp.Usage))
}
