package agent

import (
	"fmt"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"reflect"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Per response stream delivery flags plus the per goroutine writer token a claim stores
type streamDeliveryState struct {
	nativeReasoningStreamed     bool
	streamReasoningHooksEnabled *bool
	streamWriterTLS             *pythread.Local
}

// Calls a callback with the arguments, true when it ran without panicking
func callQuietly(cb any, args ...any) (ran bool) {
	if cb == nil {
		return false
	}
	fn := reflect.ValueOf(cb)
	if fn.Kind() == reflect.Func && fn.IsNil() {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			ran = false
		}
	}()
	if fn.Kind() != reflect.Func {
		return false
	}
	callDynamic(fn, args)
	return true
}

// Sends text to the display and TTS delta callbacks, nothing while a plugin holds the reply
func (a *AIAgent) deliverToStreamCallbacks(text string) bool {
	if a.holdStream {
		return false
	}
	display := callQuietly(a.streamDeltaCallback, text)
	tts := callQuietly(a.streamCallback, text)
	return display || tts
}

// Queues a plugin stream hook with the base payload, a failure only logged
func (a *AIAgent) enqueueStreamHook(event string, fields map[string]any) {
	a.enqueueStreamHookLabeled(event, "", fields)
}

// Queues a plugin stream hook, the label naming the hook in the failure log
func (a *AIAgent) enqueueStreamHookLabeled(event, label string, fields map[string]any) {
	name := label
	if name == "" {
		name = event
	}
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			runAgentLog().Debug(fmt.Sprintf("%s plugin hook enqueue failed", name), "error", r)
		}
	}()
	payload := a.streamHookBasePayload()
	for k, v := range fields {
		if _, dup := payload[k]; dup {
			panic(fmt.Sprintf("TypeError: enqueue_plugin_stream_hook() got multiple values for keyword argument '%s'", k))
		}
		payload[k] = v
	}
	plugins.EnqueuePluginStreamHook(a.homeContext(), event, payload)
}

// Turn, iteration, session, model, provider and surface every plugin stream hook carries
func (a *AIAgent) streamHookBasePayload() map[string]any {
	surface := a.platform
	if surface == "" {
		surface = "cli"
	}
	return map[string]any{
		"turn_id": a.currentTurnID, "iteration": a.apiCallCount, "session_id": a.sessionID,
		"model": a.model, "provider": a.provider, "surface": surface,
	}
}

// Flushes both scrubber tails to the callbacks and clears the streamed text of the model response
func (a *AIAgent) resetStreamDeliveryTracking() {
	think, scrub := a.streamThinkScrubber, a.streamContextScrubber
	a.nativeReasoningStreamed = false
	a.streamReasoningHooksEnabled = nil
	deliver := func(tail string) {
		if tail != "" && a.deliverToStreamCallbacks(tail) {
			a.recordStreamedAssistantText(tail)
		}
	}
	if think != nil {
		tail := think.Flush()
		if tail != "" && scrub != nil {
			tail = scrub.Feed(tail)
		}
		deliver(tail)
	}
	if scrub != nil {
		deliver(scrub.Flush())
	}
	a.setCurrentStreamedAssistantText("")
}

// Visible assistant text streamed so far this turn
func (a *AIAgent) currentStreamedAssistantText() string {
	return strings.Join(a.streamedAssistantTextParts, "")
}

// Replaces the streamed text with one piece, none for the empty string
func (a *AIAgent) setCurrentStreamedAssistantText(value string) {
	if value == "" {
		a.streamedAssistantTextParts = nil
		return
	}
	a.streamedAssistantTextParts = []string{value}
}

// Accumulates visible streamed text unless this goroutine is a superseded writer
func (a *AIAgent) recordStreamedAssistantText(text string) {
	if text != "" && !a.streamWriterSuperseded() {
		a.streamedAssistantTextParts = append(a.streamedAssistantTextParts, text)
	}
}

// Whitespace runs collapsed to one space and the ends stripped
func normalizeInterimVisibleText(text string) string {
	return strings.Join(pystr.SplitWhitespace(text, -1), " ")
}

// Think stripped and normalized visible content and streamed text
func (a *AIAgent) interimVisibleAndStreamed(content string) (string, string) {
	return normalizeInterimVisibleText(a.stripThinkBlocks(content)), normalizeInterimVisibleText(a.stripThinkBlocks(a.currentStreamedAssistantText()))
}

// True when the visible content starts with everything already streamed
func (a *AIAgent) interimContentWasStreamed(content string) bool {
	visible, streamed := a.interimVisibleAndStreamed(content)
	return visible != "" && streamed != "" && strings.HasPrefix(visible, streamed)
}

// True when the visible content equals the streamed text exactly
func (a *AIAgent) interimContentFullyStreamed(content string) bool {
	visible, streamed := a.interimVisibleAndStreamed(content)
	return visible != "" && visible == streamed
}

// A list value as a slice of any, false when the value is not a list
func listOfAny(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// Joined output text parts of a message item that carry visible text
func outputTextOfParts(parts []any) string {
	var b strings.Builder
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok || part["type"] != "output_text" {
			continue
		}
		if text, ok := part["text"].(string); ok && pystr.Strip(text) != "" {
			b.WriteString(text)
		}
	}
	return pystr.Strip(b.String())
}

// Visible Codex commentary items one string each, none when commentary display is off
func (a *AIAgent) extractCodexInterimVisibleParts(msg map[string]any) []string {
	if !a.showCommentary {
		return nil
	}
	items, _ := listOfAny(msg["codex_message_items"])
	var out []string
	for _, it := range items {
		item, ok := it.(map[string]any)
		if !ok || item["type"] != "message" {
			continue
		}
		phase, ok := item["phase"].(string)
		parts, isList := listOfAny(item["content"])
		if !ok || pystr.Lower(pystr.Strip(phase)) != "commentary" || !isList {
			continue
		}
		if visible := a.visibleCommentary(outputTextOfParts(parts)); visible != "" {
			out = append(out, visible)
		}
	}
	return out
}

// Think stripped and redacted commentary, empty when nothing visible remains
func (a *AIAgent) visibleCommentary(text string) string {
	visible := pystr.Strip(a.stripThinkBlocks(text))
	if visible == "" {
		return visible
	}
	return tools.RedactSensitiveTextWith(a.env, visible, tools.RedactOptions{})
}

// Every visible Codex commentary item joined by blank lines
func (a *AIAgent) extractCodexInterimVisibleText(msg map[string]any) string {
	return pystr.Strip(strings.Join(a.extractCodexInterimVisibleParts(msg), "\n\n"))
}

// Codex commentary first, else the think stripped flattened top level content
func (a *AIAgent) interimAssistantVisibleText(msg map[string]any) string {
	if text := a.extractCodexInterimVisibleText(msg); text != "" {
		return text
	}
	return pystr.Strip(a.stripThinkBlocks(state.FlattenMessageText(msg["content"], "\n")))
}

// True when the normalized text was already handed to the interim callback
func (a *AIAgent) interimTextWasDelivered(text string) bool {
	normalized := normalizeInterimVisibleText(text)
	return normalized != "" && a.deliveredInterimTexts[normalized]
}

// Remembers the normalized text as delivered
func (a *AIAgent) recordDeliveredInterimText(text string) {
	if normalized := normalizeInterimVisibleText(text); normalized != "" {
		if a.deliveredInterimTexts == nil {
			a.deliveredInterimTexts = map[string]bool{}
		}
		a.deliveredInterimTexts[normalized] = true
	}
}

// Hands visible text to the interim callback and marks the record delivered, nothing on a held turn
func (a *AIAgent) deliverInterim(visible string, alreadyStreamed bool, record []string) {
	if a.holdStream || a.interimAssistantCallback == nil {
		return
	}
	fn := reflect.ValueOf(a.interimAssistantCallback)
	if fn.Kind() == reflect.Func && fn.IsNil() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			runAgentLog().Debug("interim_assistant_callback error", "error", r)
		}
	}()
	if fn.Kind() != reflect.Func {
		panic(fmt.Sprintf("TypeError: '%T' object is not callable", a.interimAssistantCallback))
	}
	callDynamic(fn, []any{visible, alreadyStreamed})
	for _, part := range record {
		a.recordDeliveredInterimText(part)
	}
}

// Delivers one completed live Codex commentary message at once
func (a *AIAgent) fireStreamedCodexCommentary(text string) {
	if a.interimAssistantCallback == nil {
		return
	}
	visible := a.visibleCommentary(text)
	if visible == "" || visible == "(empty)" || a.interimTextWasDelivered(visible) {
		return
	}
	a.deliverInterim(visible, false, []string{visible})
}

func (a *AIAgent) FireStreamedCodexCommentary(text string) { a.fireStreamedCodexCommentary(text) }

// Surfaces a mid turn assistant commentary message once, deduplicated against earlier deliveries
func (a *AIAgent) emitInterimAssistantMessage(msg map[string]any) {
	if msg == nil {
		return
	}
	commentary := a.extractCodexInterimVisibleParts(msg)
	seen := map[string]bool{}
	var undelivered []string
	for _, part := range commentary {
		key := normalizeInterimVisibleText(part)
		if key != "" && !seen[key] && !a.interimTextWasDelivered(part) {
			seen[key] = true
			undelivered = append(undelivered, part)
		}
	}
	var visible string
	if len(commentary) > 0 {
		visible = pystr.Strip(strings.Join(undelivered, "\n\n"))
	} else {
		visible = a.interimAssistantVisibleText(msg)
	}
	if visible == "" || visible == "(empty)" || a.interimTextWasDelivered(visible) {
		return
	}
	alreadyStreamed := a.interimContentFullyStreamed(visible)
	a.enqueueStreamHook("on_interim_message", map[string]any{"text": visible, "already_streamed": alreadyStreamed})
	record := undelivered
	if len(record) == 0 {
		record = []string{visible}
	}
	a.deliverInterim(visible, alreadyStreamed, record)
}

// Guards creation of the single writer lock and token store on an agent built without init
var streamWriterStateInit sync.Mutex

// Creates the single writer guard lock and per goroutine token store when an agent skipped init
func (a *AIAgent) ensureStreamWriterState() {
	streamWriterStateInit.Lock()
	defer streamWriterStateInit.Unlock()
	if a.streamWriterLock == nil {
		a.streamWriterLock = &sync.Mutex{}
	}
	if a.streamWriterTLS == nil {
		a.streamWriterTLS = &pythread.Local{}
	}
}

// True when this goroutine claimed the sink and a newer attempt claimed it since
func (a *AIAgent) streamWriterSuperseded() bool {
	a.ensureStreamWriterState()
	v, ok := a.streamWriterTLS.Get()
	if !ok {
		return false
	}
	a.streamWriterLock.Lock()
	defer a.streamWriterLock.Unlock()
	return v.(int) != a.streamWriterToken
}

// Counts a dropped superseded delta and warns on the first drop and every power of two
func (a *AIAgent) noteDroppedStreamWriter(where string) {
	a.ensureStreamWriterState()
	a.streamWriterLock.Lock()
	a.streamWriterDropped++
	n := a.streamWriterDropped
	a.streamWriterLock.Unlock()
	if n == 1 || n&(n-1) == 0 {
		runAgentLog().Warn(fmt.Sprintf("Dropped delta from a superseded stream writer at %s (discarded=%d this turn) — a stale stream tried to write into the turn after a retry superseded it.", where, n))
	}
}

// Scrubs one text delta and fans it out to the callbacks and plugin hooks
func (a *AIAgent) fireStreamDelta(text string) {
	if a.streamWriterSuperseded() {
		a.noteDroppedStreamWriter("_fire_stream_delta")
		return
	}
	prependedBreak := a.streamNeedsBreak && text != "" && pystr.Strip(text) != ""
	if prependedBreak {
		a.streamNeedsBreak = false
		text = "\n\n" + text
	}
	think, scrub := a.streamThinkScrubber, a.streamContextScrubber
	hidden := ""
	if think != nil {
		text = think.Feed(text)
		hidden = think.LastHidden()
	} else {
		text = a.stripThinkBlocks(text)
	}
	if hidden != "" && !a.nativeReasoningStreamed {
		a.fireReasoningDeltaFrom(hidden, true)
	}
	if scrub != nil {
		text = scrub.Feed(text)
	} else {
		text = memory.SanitizeContext(text)
	}
	if !prependedBreak && len(a.streamedAssistantTextParts) == 0 {
		text = strings.TrimLeft(text, "\n")
	}
	if text == "" {
		return
	}
	delivered := a.deliverToStreamCallbacks(text)
	a.enqueueStreamHook("on_stream_delta", map[string]any{"delta": text, "kind": "text"})
	if delivered {
		a.recordStreamedAssistantText(text)
	}
}

// Native provider reasoning delta, ending inline think forwarding for this model response
func (a *AIAgent) fireReasoningDelta(text string) { a.fireReasoningDeltaFrom(text, false) }

// Fires the reasoning callback and the opted in plugin hook, fencing superseded writers
func (a *AIAgent) fireReasoningDeltaFrom(text string, inline bool) {
	if !inline {
		a.nativeReasoningStreamed = true
	}
	if a.streamWriterSuperseded() {
		a.noteDroppedStreamWriter("_fire_reasoning_delta")
		return
	}
	callQuietly(a.reasoningCallback, text)
	enabled := a.streamReasoningHooksEnabled
	if enabled == nil {
		v, ok := a.streamReasoningDeltasEnabled()
		if !ok {
			return
		}
		enabled = &v
		a.streamReasoningHooksEnabled = enabled
	}
	if *enabled {
		a.enqueueStreamHookLabeled("on_stream_delta", "reasoning on_stream_delta", map[string]any{"delta": text, "kind": "reasoning"})
	}
}

// plugins.stream_reasoning_deltas for this stream, false ok when the lookup panicked
func (a *AIAgent) streamReasoningDeltasEnabled() (enabled, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			runAgentLog().Debug("reasoning on_stream_delta plugin hook enqueue failed", "error", r)
			enabled, ok = false, false
		}
	}()
	return plugins.StreamReasoningDeltasEnabled(a.homeContext()), true
}

// Display metadata flag on a held turn's published reply whose commentary items stay for replay only
const CommentaryWithheld = tuigateway.CommentaryWithheld

// Commentary phase message texts of a row
func commentaryItems(msg map[string]any) []string {
	return tuigateway.CommentaryItems(msg)
}

// Types a held turn's model row hidden when it has visible text, the wire copy unchanged
func withholdHeldRow(a *AIAgent, msg map[string]any) map[string]any {
	if a.holdStream && !pyrepr.Truthy(msg["display_kind"]) && a.interimAssistantVisibleText(msg) != "" {
		msg["display_kind"] = "hidden"
	}
	return msg
}

// Shows the held turn's final row again, withholding its commentary and carrying the stitched reply
func publishHeldFinalRow(a *AIAgent, final map[string]any, finalResponse any, joined bool) {
	if !a.holdStream {
		return
	}
	if final["display_kind"] == "hidden" {
		delete(final, "display_kind")
	}
	if len(commentaryItems(final)) > 0 {
		meta := map[string]any{}
		if old, ok := final["display_metadata"].(map[string]any); ok {
			for k, v := range old {
				meta[k] = v
			}
		}
		meta[CommentaryWithheld] = true
		final["display_metadata"] = meta
	}
	if content := final["content"]; joined && pyrepr.Truthy(content) && !reflect.DeepEqual(content, finalResponse) {
		if _, has := final["api_content"]; !has {
			final["api_content"] = content
		}
		final["content"] = finalResponse
	}
}
