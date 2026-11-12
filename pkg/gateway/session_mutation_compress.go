package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// One canonical compress summarize the agent side runs under the session's policy scope
type SessionCompressJob struct {
	Model      string
	Runtime    map[string]any
	Config     map[string]any
	Options    map[string]any
	Resident   any
	Head       []map[string]any
	Tail       []map[string]any
	Messages   []map[string]any
	FocusTopic *string
	SafeMode   bool
	DB         *state.SessionDB
	Target     string
}

// Agent side of a canonical session compress, its context engines and token estimates
type SessionCompressor interface {
	// Compressed transcript of the job, nil when nothing was removed, with the engine that ran
	Summarize(ctx context.Context, job SessionCompressJob) ([]map[string]any, any, error)
	// Whether the engine is the built in compressor
	BuiltinEngine(engine any) bool
	// Ends a one off engine's session, logging a failure
	EndEngine(engine any, sessionID string, messages []map[string]any)
	// Manual compression report every native compress prints
	ManualSummary(before, after []map[string]any, beforeTokens, afterTokens int, engine any) map[string]any
	// Post commit engine boundary and relay and metrics segment hand off
	NotifyCompressionComplete(ctx context.Context, engine any, platform, sessionKey, newSessionID, oldSessionID string)
	// Fires post_compaction for a committed manual compress
	FirePostCompaction(ctx context.Context, sessionID, oldSessionID string, beforeTokens, afterTokens, messagesRemoved any)
	// Rough request estimate of messages with a system prompt and tool schemas
	RequestTokens(ctx context.Context, messages []map[string]any, systemPrompt string, tools []map[string]any) int
	// Rough message only estimate
	MessagesTokens(ctx context.Context, messages []map[string]any) int
	// System prompt and tool schemas an agent sends
	RequestBasis(agent any) (string, []map[string]any)
}

// Agent side of every canonical compress, assigned at startup by the command wiring
var SessionCompress SessionCompressor

// Parsed compress arguments
type compressRequest struct {
	Preview    bool
	Aggressive bool
	Partial    bool
	KeepLast   int
	FocusTopic *string
}

// Compress request of a session.mutate payload, a focus only payload parsed like every native compress
func compressRequestOf(payload *pyjson.Object) (compressRequest, error) {
	var request compressRequest
	focusOnly := true
	for _, key := range payload.Keys {
		if key != "focus" {
			focusOnly = false
		}
	}
	if focusOnly {
		raw, _ := payload.Get("focus")
		text := ""
		if raw != nil {
			text = pyrepr.Str(raw)
		}
		rest, preview, aggressive := provider.ExtractCompressFlags(pystr.Strip(text))
		partial, keepLast, focus := provider.ParsePartialCompressArgs(rest)
		if focus != nil && *focus == "" {
			focus = nil
		}
		request = compressRequest{Preview: preview, Aggressive: aggressive, Partial: partial, KeepLast: keepLast, FocusTopic: focus}
	} else {
		request = compressRequest{Preview: pyTruthy(payload.GetOr("preview", false)), Partial: pyTruthy(payload.GetOr("partial", false)), KeepLast: 2}
		if keep, has := payload.Get("keep_last"); has {
			n, ok := pathIndex(keep)
			if !ok {
				return request, runtimeRefusal("invalid_params")
			}
			request.KeepLast = n
		}
		if focus, has := payload.Get("focus"); has && focus != nil {
			text := pyrepr.Str(focus)
			request.FocusTopic = &text
		}
	}
	if request.Aggressive {
		return request, runtimeRefusal("unsupported_compress_options")
	}
	return request, nil
}

// Agent side of compress, an AttributeError when the wiring never assigned it
func sessionCompressor() (SessionCompressor, error) {
	if SessionCompress == nil {
		return nil, &PyError{Kind: "AttributeError", Msg: "session compress has no agent side"}
	}
	return SessionCompress, nil
}

// Engine and owner a committed compress notifies at its boundary
type compressEngineOwner struct {
	engine     any
	platform   string
	sessionKey string
	owned      bool
}

// Platform label of a source, cli without one
func compressPlatform(source *SessionSource) string {
	if source == nil || source.Platform == "" {
		return "cli"
	}
	return string(source.Platform)
}

// Messages as JSON objects the compress commit reads
func objectMessages(messages []map[string]any) []any {
	out := make([]any, len(messages))
	for i, m := range messages {
		out[i], _ = pyjson.AsObject(m)
	}
	return out
}

// Summarizes the session transcript off the commit, a preview or noop report when nothing is written
func prepareCompress(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, prepared *pyjson.Object) (*pyjson.Object, error) {
	compressor, err := sessionCompressor()
	if err != nil {
		return nil, err
	}
	request, err := compressRequestOf(payload)
	if err != nil {
		return nil, err
	}
	snapshot, _ := prepared.Get("snapshot")
	policy, err := preparedPolicy(prepared)
	if err != nil {
		return nil, err
	}
	cfg, err := policy.config(ctx, a, true)
	if err != nil {
		return nil, err
	}
	options, _ := cfg["compression"].(map[string]any)
	if options == nil {
		options = map[string]any{}
	}
	if pyTruthy(options["checkpoint_required"]) {
		return nil, runtimeRefusal("compression_checkpoint_required")
	}
	resolver, err := runtimeResolver(a)
	if err != nil {
		return nil, err
	}
	model, runtime, err := resolver.ResolveSessionAgentRuntime(ctx, live.Source, live.Route, nil, true)
	if err != nil {
		return nil, err
	}
	if runtime["api_mode"] == "codex_app_server" {
		return nil, runtimeRefusal("runtime_coordination_required")
	}
	rawTarget, err := pyItem(snapshot, "target")
	if err != nil {
		return nil, err
	}
	target := pyrepr.Str(rawTarget)
	history, err := a.DB.GetMessagesAsConversation(ctx, target, state.ConversationOptions{})
	if err != nil {
		return nil, err
	}
	var messages []map[string]any
	for _, m := range history {
		if role := m["role"]; role == "user" || role == "assistant" || role == "tool" {
			messages = append(messages, m)
		}
	}
	head, tail := messages, []map[string]any(nil)
	if request.Partial {
		head, tail = provider.SplitHistoryForPartialCompress(messages, request.KeepLast)
		if len(tail) == 0 {
			head = messages
		}
	}
	if request.Preview {
		report := provider.SummarizeCompressPreview(messages, request.Partial, request.KeepLast, request.FocusTopic, compressor.RequestTokens(ctx, messages, "", nil))
		return pyjson.ObjectOf("status", "preview", "lines", report["lines"], "head_count", report["head_count"],
			"tail_count", report["tail_count"], "message_count", report["total"]), nil
	}
	var resident any
	if cached, ok := a.Runner.(cachedAgentRunner); ok {
		resident = cached.CachedAgentFor(live.Route)
	}
	job := SessionCompressJob{Model: model, Runtime: runtime, Config: cfg, Options: options, Resident: resident, Head: head, Tail: tail,
		Messages: messages, FocusTopic: request.FocusTopic, SafeMode: policy.SafeMode, DB: a.DB, Target: target}
	var compressed []map[string]any
	var engine any
	err = policyScope(ctx, policy, a, func(scoped context.Context) error {
		var err error
		compressed, engine, err = compressor.Summarize(scoped, job)
		return err
	})
	if err != nil {
		return nil, err
	}
	if compressed == nil {
		if resident == nil && !compressor.BuiltinEngine(engine) {
			compressor.EndEngine(engine, target, messages)
		}
		var row *pyjson.Object
		if resident == nil {
			if row, err = a.DB.GetSession(ctx, target); err != nil {
				return nil, err
			}
		}
		tokens := compressRequestTokens(ctx, compressor, resident, messages, row)
		return pyjson.ObjectOf("status", "noop", "summary", compressor.ManualSummary(messages, messages, tokens, tokens, engine)), nil
	}
	summary := compressor.ManualSummary(messages, compressed, compressor.RequestTokens(ctx, messages, "", nil), compressor.RequestTokens(ctx, compressed, "", nil), engine)
	var hook any
	if !policy.SafeMode {
		hook = map[string]any{"before_tokens": compressor.MessagesTokens(ctx, messages), "after_tokens": compressor.MessagesTokens(ctx, compressed),
			"messages_removed": len(messages) - len(compressed)}
	}
	owner := &compressEngineOwner{engine: engine, platform: compressPlatform(live.Source), sessionKey: live.Route,
		owned: resident == nil && !compressor.BuiltinEngine(engine)}
	inPlace := true
	if raw, has := options["in_place"]; has && raw != nil {
		if text, isText := raw.(string); isText {
			lowered := pystr.Lower(pystr.Strip(text))
			inPlace = lowered == "1" || lowered == "true" || lowered == "yes" || lowered == "on"
		} else {
			inPlace = pyTruthy(raw)
		}
	}
	return preparedWith(prepared, "messages", objectMessages(compressed), "summary", summary, "post_compaction", hook,
		"engine_owner", owner, "in_place", inPlace), nil
}

// Engine owner a prepared compress carries, nil for an exact retry
func preparedEngineOwner(prepared *pyjson.Object) *compressEngineOwner {
	if prepared == nil {
		return nil
	}
	raw, _ := prepared.Get("engine_owner")
	owner, _ := raw.(*compressEngineOwner)
	return owner
}

// Notifies the committed boundary and ends a one off engine after the receipt transaction, swallowing engine failures
func finishCompressEngine(ctx context.Context, a *SessionAuthority, prepared *pyjson.Object, result *pyjson.Object) {
	owner := preparedEngineOwner(prepared)
	if owner == nil || (result == nil && !owner.owned) {
		return
	}
	compressor, err := sessionCompressor()
	if err != nil {
		slog.Warn("context engine compression notification failed: " + pyExceptionRepr(err))
		return
	}
	scoped := profileRuntimeScope(ctx, a.ProfileID, false)
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(fmt.Sprintf("context engine compression notification failed: %v", r))
		}
	}()
	if result != nil {
		newSID, _ := result.Get("target_session_id")
		oldSID, _ := result.Get("previous_target_session_id")
		compressor.NotifyCompressionComplete(scoped, owner.engine, owner.platform, owner.sessionKey, pyrepr.Str(newSID), pyrepr.Str(oldSID))
	}
	if owner.owned {
		var target any
		if result != nil {
			target, _ = result.Get("target_session_id")
		} else {
			target, _ = pyItem(prepared, "snapshot", "target")
		}
		raw, _ := prepared.Get("messages")
		list, _ := raw.([]any)
		messages := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if obj, ok := item.(*pyjson.Object); ok {
				messages = append(messages, pyjson.Plain(obj).(map[string]any))
			}
		}
		compressor.EndEngine(owner.engine, pyrepr.Str(target), messages)
	}
}

// Fires post_compaction for a committed compress in the session's profile, swallowing plugin failures
func firePostCompactionHook(ctx context.Context, a *SessionAuthority, prepared *pyjson.Object, result *pyjson.Object) {
	compressor, err := sessionCompressor()
	if err != nil {
		slog.Warn("post_compaction hook delivery failed: " + pyExceptionRepr(err))
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(fmt.Sprintf("post_compaction hook delivery failed: %v", r))
		}
	}()
	target, _ := result.Get("target_session_id")
	previous, _ := result.Get("previous_target_session_id")
	old := pyrepr.Str(previous)
	if pyjson.Equal(previous, target) {
		old = ""
	}
	raw, _ := prepared.Get("post_compaction")
	hook, _ := raw.(map[string]any)
	compressor.FirePostCompaction(profileRuntimeScope(ctx, a.ProfileID, false), pyrepr.Str(target), old,
		hook["before_tokens"], hook["after_tokens"], hook["messages_removed"])
}

// Rough request estimate of a compress noop from the agent's prompt and tools or a cold session's stored pin
func compressRequestTokens(ctx context.Context, compressor SessionCompressor, agent any, messages []map[string]any, row *pyjson.Object) int {
	prompt, schemas := "", []map[string]any(nil)
	if agent != nil {
		prompt, schemas = compressor.RequestBasis(agent)
	}
	if agent == nil && row != nil {
		prompt = ""
		if value, _ := row.Get("system_prompt"); pyTruthy(value) {
			prompt = pyrepr.Str(value)
		}
		var pin any
		if raw, _ := row.Get("tool_names"); pyTruthy(raw) {
			if text, ok := raw.(string); ok {
				if loaded, err := pyjson.Loads(text); err == nil {
					pin = loaded
				}
			}
		}
		schemas = mcp.PinnedToolDefs(ctx, terminalEnv(ctx), pin)
	}
	if len(schemas) == 0 {
		schemas = nil
	}
	return compressor.RequestTokens(ctx, messages, prompt, schemas)
}
