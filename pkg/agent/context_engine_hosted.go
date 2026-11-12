package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Context engine whose object lives in the plugin host, every method a call and the base state mirrored both ways
type hostedContextEngine struct {
	ContextEngineBase
	obj    *plugins.HostedObject
	ctx    context.Context
	name   string
	synced ContextEngineBase
}

var _ ContextEngine = (*hostedContextEngine)(nil)
var _ ContextEngineInfo = (*hostedContextEngine)(nil)
var _ compressKeywordInspector = (*hostedContextEngine)(nil)
var _ engineCarryOver = (*hostedContextEngine)(nil)
var _ realPromptTokensEngine = (*hostedContextEngine)(nil)
var _ compactionClearLatch = (*hostedContextEngine)(nil)

// Wraps a hosted engine object, reading its name and base attributes once
func newHostedContextEngine(ctx context.Context, obj *plugins.HostedObject) (*hostedContextEngine, error) {
	name, err := obj.Get(ctx, "name")
	if err != nil {
		return nil, err
	}
	e := &hostedContextEngine{ContextEngineBase: NewContextEngineBase(), obj: obj, ctx: ctx, name: pyrepr.Str(name)}
	e.pull()
	return e, nil
}

func hostedInt(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func hostedFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func hostedMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case plugins.Record:
		return map[string]any(m)
	}
	return nil
}

func hostedMapList(v any) []map[string]any {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m := hostedMap(item); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func intFields(b *ContextEngineBase) map[string]*int {
	return map[string]*int{"last_prompt_tokens": &b.LastPromptTokens, "last_completion_tokens": &b.LastCompletionTokens,
		"last_total_tokens": &b.LastTotalTokens, "threshold_tokens": &b.ThresholdTokens, "context_length": &b.ContextLength,
		"compression_count": &b.CompressionCount, "protect_first_n": &b.ProtectFirstN, "protect_last_n": &b.ProtectLastN}
}

// Reads the hosted base attributes into the mirrored base, an absent one keeping the class default
func (e *hostedContextEngine) pull() {
	b := &e.ContextEngineBase
	for name, field := range intFields(b) {
		if v, err := e.obj.Get(e.ctx, name); err == nil {
			if n, ok := hostedInt(v); ok {
				*field = n
			}
		}
	}
	if v, err := e.obj.Get(e.ctx, "threshold_percent"); err == nil {
		if f, ok := hostedFloat(v); ok {
			b.ThresholdPercent = f
		}
	}
	if v, err := e.obj.Get(e.ctx, "emit_automatic_compaction_status"); err == nil {
		b.EmitAutomaticCompactionStatus = pyrepr.Truthy(v)
	}
	if v, err := e.obj.Get(e.ctx, "model_thresholds"); err == nil {
		if m := hostedMap(v); m != nil {
			b.ModelThresholds = map[string]float64{}
			for k, raw := range m {
				if f, ok := hostedFloat(raw); ok {
					b.ModelThresholds[k] = f
				}
			}
		}
	}
	e.synced = *b
	e.synced.ModelThresholds = b.ModelThresholds
}

// Writes the base fields the host side changed since the last read back onto the hosted object
func (e *hostedContextEngine) push() {
	b, old := &e.ContextEngineBase, &e.synced
	oldInts := intFields(old)
	for name, field := range intFields(b) {
		if *field != *oldInts[name] {
			if err := e.obj.Set(e.ctx, name, *field); err != nil {
				panic(err)
			}
		}
	}
	if b.ThresholdPercent != old.ThresholdPercent {
		if err := e.obj.Set(e.ctx, "threshold_percent", b.ThresholdPercent); err != nil {
			panic(err)
		}
	}
	if b.EmitAutomaticCompactionStatus != old.EmitAutomaticCompactionStatus {
		if err := e.obj.Set(e.ctx, "emit_automatic_compaction_status", b.EmitAutomaticCompactionStatus); err != nil {
			panic(err)
		}
	}
}

// Calls a hosted method between a push and a pull, raising its error like the Python call
func (e *hostedContextEngine) call(method string, args []any, kwargs map[string]any) any {
	out, err := e.callErr(method, args, kwargs)
	if err != nil {
		panic(err)
	}
	return out
}

func (e *hostedContextEngine) callErr(method string, args []any, kwargs map[string]any) (any, error) {
	e.push()
	out, err := e.obj.Call(e.ctx, method, args, kwargs)
	e.pull()
	return out, err
}

func optionalInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func anyMaps(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func (e *hostedContextEngine) Name() string { return e.name }

func (e *hostedContextEngine) Base() *ContextEngineBase {
	e.push()
	e.pull()
	return &e.ContextEngineBase
}

func (e *hostedContextEngine) UpdateFromResponse(usage map[string]any) {
	e.call("update_from_response", []any{usage}, nil)
}

func (e *hostedContextEngine) ShouldCompress(promptTokens *int) bool {
	return pyrepr.Truthy(e.call("should_compress", []any{optionalInt(promptTokens)}, nil))
}

func (e *hostedContextEngine) ShouldCompressInfo(promptTokens *int) (bool, *string) {
	out, _ := e.call("should_compress_info", []any{optionalInt(promptTokens)}, nil).([]any)
	if len(out) != 2 {
		panic(fmt.Errorf("TypeError: should_compress_info must return a (bool, reason) pair"))
	}
	if out[1] == nil {
		return pyrepr.Truthy(out[0]), nil
	}
	reason := pyrepr.Str(out[1])
	return pyrepr.Truthy(out[0]), &reason
}

func (e *hostedContextEngine) AcceptsCompressKeyword(keyword string) bool {
	return e.obj.AcceptsKeyword("compress", keyword)
}

// Compresses through the hosted engine, passing only the optional keywords its signature accepts
func (e *hostedContextEngine) Compress(messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string) ([]map[string]any, error) {
	kwargs := map[string]any{}
	offer := map[string]any{"current_tokens": optionalInt(currentTokens), "force": force, "memory_context": memoryContext}
	if focusTopic != nil {
		offer["focus_topic"] = *focusTopic
	} else {
		offer["focus_topic"] = nil
	}
	for k, v := range offer {
		if e.obj.AcceptsKeyword("compress", k) {
			kwargs[k] = v
		}
	}
	out, err := e.callErr("compress", []any{anyMaps(messages)}, kwargs)
	if err != nil {
		return nil, err
	}
	return hostedMapList(out), nil
}

func (e *hostedContextEngine) PruneToolResultsOnly(messages []map[string]any, currentTokens *int) ([]map[string]any, int) {
	out, _ := e.call("prune_tool_results_only", []any{anyMaps(messages)}, map[string]any{"current_tokens": optionalInt(currentTokens)}).([]any)
	if len(out) != 2 {
		panic(fmt.Errorf("TypeError: prune_tool_results_only must return a (messages, n_pruned) pair"))
	}
	n, _ := hostedInt(out[1])
	return hostedMapList(out[0]), n
}

func (e *hostedContextEngine) SelectContext(requestMessages, conversationMessages []map[string]any, incomingMessage map[string]any, budgetTokens int) []map[string]any {
	var conv any
	if conversationMessages != nil {
		conv = anyMaps(conversationMessages)
	}
	var incoming any
	if incomingMessage != nil {
		incoming = incomingMessage
	}
	out := e.call("select_context", []any{anyMaps(requestMessages)},
		map[string]any{"conversation_messages": conv, "incoming_message": incoming, "budget_tokens": budgetTokens})
	if out == nil {
		return nil
	}
	return hostedMapList(out)
}

func (e *hostedContextEngine) OnTurnComplete(messages []map[string]any, usage map[string]any, kwargs map[string]any) {
	var u any
	if usage != nil {
		u = usage
	}
	e.call("on_turn_complete", []any{anyMaps(messages), u}, kwargs)
}

func (e *hostedContextEngine) ShouldCompressPreflight(messages []map[string]any) bool {
	return pyrepr.Truthy(e.call("should_compress_preflight", []any{anyMaps(messages)}, nil))
}

func (e *hostedContextEngine) ShouldDeferPreflightToRealUsage(roughTokens int) bool {
	return pyrepr.Truthy(e.call("should_defer_preflight_to_real_usage", []any{roughTokens}, nil))
}

func (e *hostedContextEngine) GetAutomaticCompactionStatusMessage(phase, defaultMessage string, context map[string]any) *string {
	kwargs := map[string]any{"phase": phase, "default_message": defaultMessage}
	for k, v := range context {
		kwargs[k] = v
	}
	out := e.call("get_automatic_compaction_status_message", nil, kwargs)
	if out == nil {
		return nil
	}
	text := pyrepr.Str(out)
	return &text
}

func (e *hostedContextEngine) HasContentToCompress(messages []map[string]any) bool {
	return pyrepr.Truthy(e.call("has_content_to_compress", []any{anyMaps(messages)}, nil))
}

func (e *hostedContextEngine) OnSessionStart(sessionID string, kwargs map[string]any) {
	e.call("on_session_start", []any{sessionID}, kwargs)
}

func (e *hostedContextEngine) OnSessionEnd(sessionID string, messages []map[string]any) {
	e.call("on_session_end", []any{sessionID, anyMaps(messages)}, nil)
}

func (e *hostedContextEngine) OnSessionReset() { e.call("on_session_reset", nil, nil) }

// Forwards the carry over hook to the hosted engine, skipped when its object has no such attribute
func (e *hostedContextEngine) CarryOverNewSessionContext(oldSessionID, newSessionID string) {
	if e.hasAttr("carry_over_new_session_context") {
		e.call("carry_over_new_session_context", []any{oldSessionID, newSessionID}, nil)
	}
}

// Hosted last_real_prompt_tokens as int of the value or zero, false when the object has no such attribute
func (e *hostedContextEngine) LastRealPromptTokens() (int, bool) {
	v, err := e.obj.Get(e.ctx, "last_real_prompt_tokens")
	var builtin *plugins.HostBuiltinError
	if errors.As(err, &builtin) && builtin.Type == "AttributeError" {
		return 0, false
	}
	if err != nil {
		panic(err)
	}
	if !pyrepr.Truthy(v) {
		return 0, true
	}
	n, err := pyint.Of(v)
	if err != nil {
		panic(err)
	}
	return int(n.Int64()), true
}

// Truth of the hosted compaction boundary latch, false when the object has no such attribute
func (e *hostedContextEngine) VerifyCompactionClearedThreshold() bool {
	v, err := e.obj.Get(e.ctx, "_verify_compaction_cleared_threshold")
	var builtin *plugins.HostBuiltinError
	if errors.As(err, &builtin) && builtin.Type == "AttributeError" {
		return false
	}
	if err != nil {
		panic(err)
	}
	return pyrepr.Truthy(v)
}

// Python hasattr on the hosted object, AttributeError meaning absent and any other error raised
func (e *hostedContextEngine) hasAttr(name string) bool {
	if e.obj.HasMethod(name) {
		return true
	}
	_, err := e.obj.Get(e.ctx, name)
	var builtin *plugins.HostBuiltinError
	if errors.As(err, &builtin) && builtin.Type == "AttributeError" {
		return false
	}
	if err != nil {
		panic(err)
	}
	return true
}

func (e *hostedContextEngine) GetToolSchemas() []map[string]any {
	return hostedMapList(e.call("get_tool_schemas", nil, nil))
}

func (e *hostedContextEngine) HandleToolCall(name string, args map[string]any, kwargs map[string]any) string {
	return pyrepr.Str(e.call("handle_tool_call", []any{name, args}, kwargs))
}

func (e *hostedContextEngine) GetStatus() map[string]any {
	return hostedMap(e.call("get_status", nil, nil))
}

// Clones through the hosted clone_for_agent, the copy adapted as its own engine
func (e *hostedContextEngine) CloneForAgent() ContextEngine {
	out := e.call("clone_for_agent", nil, nil)
	obj, ok := out.(*plugins.HostedObject)
	if !ok {
		panic(fmt.Errorf("TypeError: clone_for_agent returned %T, not a context engine", out))
	}
	clone, err := newHostedContextEngine(e.ctx, obj)
	if err != nil {
		panic(err)
	}
	return clone
}

func (e *hostedContextEngine) UpdateModel(model string, contextLength int, baseURL, apiKey, provider, apiMode string) {
	e.call("update_model", []any{model, contextLength},
		map[string]any{"base_url": baseURL, "api_key": apiKey, "provider": provider, "api_mode": apiMode})
}

// Engine attribute the getattr readers probe, false when the engine does not carry it
func engineAttr(cc ContextEngine, name string) (any, bool) {
	switch e := cc.(type) {
	case *ContextCompressor:
		switch name {
		case "model":
			return e.model, true
		case "base_url":
			return e.baseURL, true
		case "api_key":
			return e.apiKey, true
		case "provider":
			return e.provider, true
		case "api_mode":
			return e.apiMode, true
		case "threshold_tokens_cap":
			return optionalInt(e.thresholdTokensCap), true
		}
	case *hostedContextEngine:
		if v, err := e.obj.Get(e.ctx, name); err == nil {
			return v, true
		}
	}
	return nil, false
}

// Effective threshold cap the engine reports for the window, nil when it has none
func engineEffectiveThresholdCap(cc ContextEngine, contextLength int) *int {
	switch e := cc.(type) {
	case *ContextCompressor:
		return e.effectiveThresholdCap(contextLength)
	case *hostedContextEngine:
		if !e.obj.HasMethod("_effective_threshold_cap") {
			return nil
		}
		if n, ok := hostedInt(e.call("_effective_threshold_cap", []any{contextLength}, nil)); ok {
			return &n
		}
	}
	return nil
}
