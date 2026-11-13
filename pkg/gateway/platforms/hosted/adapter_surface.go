package hosted

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"weak"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Python names reached through platform_call, forwarded when the plugin class defines them outside the upstream base
var surface = []string{"is_connected", "message_len_fn", "message_len_fn_for_chat", "render_message_event", "format_tool_event",
	"format_tool_preview", "_wire_plugin_handlers", "rewire_plugin_handlers", "_text_batch_key", "_enqueue_text_event",
	"_text_batch_delay_for", "_pop_text_batch", "_dispatch_text_batch", "_flush_text_batch", "_EA_HEADER", "_EA_REASON_LABEL",
	"_EA_SMART_DENY_LINE", "_EA_ACTION_LABELS", "_ea_escape", "_exec_approval_cmd_budget", "_send_exec_approval_prompt",
	"emit_warning", "emit_media_warning", "warning_text", "warning_notifications_enabled", "supports_streaming_tts",
	"begin_streaming_tts", "write_streaming_tts", "finish_streaming_tts", "abort_streaming_tts", "_send_retry_is_final"}

// Upstream classes the surface arguments are rebuilt into, called with their fields as keywords
const (
	toolPreviewBuilder  = "**agent.display:ToolPreview"
	approvalBuilder     = "**gateway.platforms.base:ExecApprovalPrompt"
	sendResultBuilder   = "**gateway.platforms.base:SendResult"
	audioFormatBuilder  = "**gateway.platforms.base_streaming_tts:AudioFormat"
	streamSinkBuilder   = "**types:SimpleNamespace"
	platformEnumBuilder = "gateway.config:Platform"
	ttsHandleClass      = "gateway.platforms.base_streaming_tts:StreamingTTSHandle"
)

// Python handle object of each streaming TTS handle a hosted adapter began, dropped once the Go handle is collected
var ttsHandles sync.Map

// Remembers the Python handle of a Go handle for as long as the Go handle lives
func holdHandle(handle *gateway.StreamingTTSHandle, obj *plugins.HostedObject) {
	key := weak.Make(handle)
	ttsHandles.Store(key, obj)
	runtime.AddCleanup(handle, func(k weak.Pointer[gateway.StreamingTTSHandle]) { ttsHandles.Delete(k) }, key)
}

// Answer of a surface call
func (a *Adapter) surfaceCall(ctx context.Context, name string, args []any, kw map[string]any, build ...string) (any, error) {
	return a.obj.PlatformCall(ctx, name, args, kw, plugins.PlatformCallOptions{Build: build})
}

// Answer of a surface call with no error return, a raise surfacing as a panic like the Go overrides
func (a *Adapter) mustSurface(name string, args []any, kw map[string]any, build ...string) any {
	v, err := a.surfaceCall(context.Background(), name, args, kw, build...)
	if err != nil {
		panic(err)
	}
	return v
}

// Attribute or property of the adapter surface, a raise surfacing as a panic
func (a *Adapter) surfaceAttr(name string) any {
	v, err := a.obj.PlatformCall(context.Background(), name, nil, nil, plugins.PlatformCallOptions{Get: true})
	if err != nil {
		panic(err)
	}
	return v
}

// Go base state change for a lifecycle hook the plugin called on itself, arguments checked as Python binds them
func (a *Adapter) baseLifecycle(ctx context.Context, name string, args []any, kw map[string]any) error {
	arg := func(i int, key string) (any, bool) {
		if i < len(args) {
			return args[i], true
		}
		v, ok := kw[key]
		return v, ok
	}
	switch name {
	case "_mark_connected":
		listener := ""
		if v, ok := kw["listener_base"]; ok && v != nil {
			listener = config.PyStr(v)
		}
		a.BasePlatformAdapter.MarkConnected(ctx, listener)
	case "_mark_disconnected":
		a.BasePlatformAdapter.MarkDisconnected(ctx)
	case "_set_fatal_error":
		code, okCode := arg(0, "code")
		message, okMessage := arg(1, "message")
		retryable, okRetryable := kw["retryable"]
		if !okCode || !okMessage {
			return &gateway.PyError{Kind: "TypeError", Msg: "_set_fatal_error() missing required positional arguments: 'code' and 'message'"}
		}
		if !okRetryable {
			return &gateway.PyError{Kind: "TypeError", Msg: "_set_fatal_error() missing 1 required keyword-only argument: 'retryable'"}
		}
		a.BasePlatformAdapter.SetFatalError(ctx, config.PyStr(code), config.PyStr(message), config.PyTruthy(retryable))
	}
	return nil
}

// Runs the class implementation of a lifecycle hook past the instance bridge, a raise surfacing as a panic
func (a *Adapter) ownLifecycle(ctx context.Context, name string, args []any, kw map[string]any) {
	if _, err := a.obj.PlatformCall(ctx, name, args, kw, plugins.PlatformCallOptions{Own: true}); err != nil {
		panic(err)
	}
}

func (a *Adapter) MarkConnected(ctx context.Context, listenerBase string) {
	a.BasePlatformAdapter.MarkConnected(ctx, listenerBase)
	var kw map[string]any
	if listenerBase != "" {
		kw = map[string]any{"listener_base": listenerBase}
	}
	a.ownLifecycle(ctx, "_mark_connected", nil, kw)
}

func (a *Adapter) MarkDisconnected(ctx context.Context) {
	a.BasePlatformAdapter.MarkDisconnected(ctx)
	a.ownLifecycle(ctx, "_mark_disconnected", nil, nil)
}

func (a *Adapter) SetFatalError(ctx context.Context, code, message string, retryable bool) {
	a.BasePlatformAdapter.SetFatalError(ctx, code, message, retryable)
	a.ownLifecycle(ctx, "_set_fatal_error", []any{code, message}, map[string]any{"retryable": retryable})
}

func (a *Adapter) IsConnected() bool {
	if !a.defined["is_connected"] {
		return a.BasePlatformAdapter.IsConnected()
	}
	return config.PyTruthy(a.surfaceAttr("is_connected"))
}

// Length function of a hosted callable answer, counting through the plugin's function
func lengthFn(v any) func(string) int {
	f, ok := v.(*plugins.HostedCallable)
	if !ok {
		panic(&gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not callable", pyrepr.TypeName(v))})
	}
	return func(s string) int {
		n, _ := number(mustCall(f, s))
		return int(n)
	}
}

func (a *Adapter) MessageLenFn() func(string) int {
	if !a.defined["message_len_fn"] {
		return a.BasePlatformAdapter.MessageLenFn()
	}
	return lengthFn(a.surfaceAttr("message_len_fn"))
}

func (a *Adapter) MessageLenFnForChat(chatID string) (func(string) int, error) {
	if !a.defined["message_len_fn_for_chat"] {
		return a.BasePlatformAdapter.MessageLenFnForChat(chatID)
	}
	v, err := a.surfaceCall(context.Background(), "message_len_fn_for_chat", []any{chatID}, nil)
	if err != nil {
		return nil, err
	}
	return lengthFn(v), nil
}

// Builder and field dict of a stream event as the upstream gateway.stream_events dataclass
func streamEventArg(event gateway.StreamEvent) (string, map[string]any) {
	const module = "**gateway.stream_events:"
	switch e := event.(type) {
	case *gateway.MessageChunk:
		return streamEventArg(*e)
	case *gateway.MessageStop:
		return streamEventArg(*e)
	case *gateway.Commentary:
		return streamEventArg(*e)
	case *gateway.ToolCallChunk:
		return streamEventArg(*e)
	case *gateway.ToolCallFinished:
		return streamEventArg(*e)
	case *gateway.LongToolHint:
		return streamEventArg(*e)
	case *gateway.GatewayNotice:
		return streamEventArg(*e)
	case gateway.MessageChunk:
		return module + "MessageChunk", map[string]any{"text": e.Text}
	case gateway.MessageStop:
		return module + "MessageStop", map[string]any{"final": e.Final}
	case gateway.Commentary:
		return module + "Commentary", map[string]any{"text": e.Text}
	case gateway.ToolCallChunk:
		var args any
		if e.Args != nil {
			args = plugins.OrderedDict(e.ArgKeys, e.Args)
		}
		return module + "ToolCallChunk", map[string]any{"tool_name": e.ToolName, "preview": optionalPtr(e.Preview), "args": args, "index": int64(e.Index)}
	case gateway.ToolCallFinished:
		return module + "ToolCallFinished", map[string]any{"tool_name": e.ToolName, "duration": e.Duration, "ok": e.OK, "index": int64(e.Index)}
	case gateway.LongToolHint:
		return module + "LongToolHint", map[string]any{"tool_name": e.ToolName, "duration": e.Duration}
	case gateway.GatewayNotice:
		extra := e.Extra
		if extra == nil {
			extra = map[string]any{}
		}
		return module + "GatewayNotice", map[string]any{"kind": e.Kind, "text": e.Text, "extra": extra}
	}
	panic(&gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("unknown stream event %T", event)})
}

// Field dict of a stream sink, its three primitives as Go callables the plugin calls synchronously
func sinkArg(sink gateway.StreamSink) map[string]any {
	text := func(args []any) string {
		if len(args) == 0 {
			return ""
		}
		return config.PyStr(args[0])
	}
	return map[string]any{
		"on_delta": plugins.HostCallable(func(_ context.Context, args []any, _ map[string]any) (any, error) {
			sink.OnDelta(text(args))
			return nil, nil
		}),
		"on_segment_break": plugins.HostCallable(func(context.Context, []any, map[string]any) (any, error) {
			sink.OnSegmentBreak()
			return nil, nil
		}),
		"on_commentary": plugins.HostCallable(func(_ context.Context, args []any, _ map[string]any) (any, error) {
			sink.OnCommentary(text(args))
			return nil, nil
		}),
	}
}

func (a *Adapter) RenderMessageEvent(event gateway.StreamEvent, sink gateway.StreamSink) {
	if !a.defined["render_message_event"] {
		a.BasePlatformAdapter.RenderMessageEvent(event, sink)
		return
	}
	builder, fields := streamEventArg(event)
	a.mustSurface("render_message_event", []any{fields, sinkArg(sink)}, nil, builder, streamSinkBuilder)
}

func (a *Adapter) FormatToolEvent(ctx context.Context, event gateway.StreamEvent, mode string, previewMaxLen int) (string, bool) {
	if !a.defined["format_tool_event"] {
		return a.BasePlatformAdapter.FormatToolEvent(ctx, event, mode, previewMaxLen)
	}
	builder, fields := streamEventArg(event)
	v, err := a.surfaceCall(ctx, "format_tool_event", []any{fields}, map[string]any{"mode": mode, "preview_max_len": int64(previewMaxLen)}, builder)
	if err != nil {
		panic(err)
	}
	if v == nil {
		return "", false
	}
	return config.PyStr(v), true
}

func (a *Adapter) FormatToolPreview(preview tools.ToolPreview) string {
	if !a.defined["format_tool_preview"] {
		return a.BasePlatformAdapter.FormatToolPreview(preview)
	}
	fields := map[string]any{"text": preview.Text, "truncated": preview.Truncated, "url": optional(preview.URL)}
	return config.PyStr(a.mustSurface("format_tool_preview", []any{fields}, nil, toolPreviewBuilder))
}

func (a *Adapter) WirePluginHandlers(ctx context.Context, native any) {
	if !a.defined["_wire_plugin_handlers"] {
		a.BasePlatformAdapter.WirePluginHandlers(ctx, native)
		return
	}
	var arg any
	if o, ok := native.(*plugins.HostedObject); ok {
		arg = o
	}
	if _, err := a.surfaceCall(ctx, "_wire_plugin_handlers", []any{arg}, nil); err != nil {
		panic(err)
	}
}

func (a *Adapter) RewirePluginHandlers(ctx context.Context) {
	if !a.defined["rewire_plugin_handlers"] {
		a.BasePlatformAdapter.RewirePluginHandlers(ctx)
		return
	}
	if _, err := a.surfaceCall(ctx, "rewire_plugin_handlers", nil, nil); err != nil {
		panic(err)
	}
}

func (a *Adapter) TextBatchKey(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	if !a.defined["_text_batch_key"] {
		return a.BasePlatformAdapter.TextBatchKey(ctx, event)
	}
	v, err := a.surfaceCall(ctx, "_text_batch_key", []any{eventToDict(event)}, nil, eventBuilder)
	if err != nil {
		return "", err
	}
	return config.PyStr(v), nil
}

func (a *Adapter) EnqueueTextEvent(ctx context.Context, event *gateway.MessageEvent) error {
	if !a.defined["_enqueue_text_event"] {
		return a.BasePlatformAdapter.EnqueueTextEvent(ctx, event)
	}
	_, err := a.surfaceCall(ctx, "_enqueue_text_event", []any{eventToDict(event)}, nil, eventBuilder)
	return err
}

func (a *Adapter) TextBatchDelayFor(pending *gateway.MessageEvent) float64 {
	if !a.defined["_text_batch_delay_for"] {
		return a.BasePlatformAdapter.TextBatchDelayFor(pending)
	}
	var arg any
	if pending != nil {
		arg = eventToDict(pending)
	}
	n, _ := number(a.mustSurface("_text_batch_delay_for", []any{arg}, nil, eventBuilder))
	return n
}

func (a *Adapter) PopTextBatch(key string) *gateway.MessageEvent {
	if !a.defined["_pop_text_batch"] {
		return a.BasePlatformAdapter.PopTextBatch(key)
	}
	fields := plainMap(a.mustSurface("_pop_text_batch", []any{key}, nil))
	if fields == nil {
		return nil
	}
	ev, err := eventFromDict(context.Background(), fields)
	if err != nil {
		panic(err)
	}
	return ev
}

func (a *Adapter) DispatchTextBatch(ctx context.Context, event *gateway.MessageEvent) error {
	if !a.defined["_dispatch_text_batch"] {
		return a.BasePlatformAdapter.DispatchTextBatch(ctx, event)
	}
	_, err := a.surfaceCall(ctx, "_dispatch_text_batch", []any{eventToDict(event)}, nil, eventBuilder)
	return err
}

func (a *Adapter) FlushTextBatch(ctx context.Context, key string) error {
	if !a.defined["_flush_text_batch"] {
		return a.BasePlatformAdapter.FlushTextBatch(ctx, key)
	}
	_, err := a.surfaceCall(ctx, "_flush_text_batch", []any{key}, nil)
	return err
}

func (a *Adapter) EAHeader(ctx context.Context) string {
	if !a.defined["_EA_HEADER"] {
		return a.BasePlatformAdapter.EAHeader(ctx)
	}
	return config.PyStr(a.surfaceAttr("_EA_HEADER"))
}

func (a *Adapter) EAReasonLabel(ctx context.Context) string {
	if !a.defined["_EA_REASON_LABEL"] {
		return a.BasePlatformAdapter.EAReasonLabel(ctx)
	}
	return config.PyStr(a.surfaceAttr("_EA_REASON_LABEL"))
}

func (a *Adapter) EASmartDenyLine(ctx context.Context) string {
	if !a.defined["_EA_SMART_DENY_LINE"] {
		return a.BasePlatformAdapter.EASmartDenyLine(ctx)
	}
	return config.PyStr(a.surfaceAttr("_EA_SMART_DENY_LINE"))
}

func (a *Adapter) EAActionLabelsFor(ctx context.Context) map[string]string {
	if !a.defined["_EA_ACTION_LABELS"] {
		return a.BasePlatformAdapter.EAActionLabelsFor(ctx)
	}
	labels := plainMap(a.surfaceAttr("_EA_ACTION_LABELS"))
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = config.PyStr(v)
	}
	return out
}

func (a *Adapter) EAEscape(text string) string {
	if !a.defined["_ea_escape"] {
		return a.BasePlatformAdapter.EAEscape(text)
	}
	return config.PyStr(a.mustSurface("_ea_escape", []any{text}, nil))
}

func (a *Adapter) ExecApprovalCmdBudget(ctx context.Context, description string, smartDenied bool) int {
	if !a.defined["_exec_approval_cmd_budget"] {
		return a.BasePlatformAdapter.ExecApprovalCmdBudget(ctx, description, smartDenied)
	}
	v, err := a.surfaceCall(ctx, "_exec_approval_cmd_budget", []any{description, smartDenied}, nil)
	if err != nil {
		panic(err)
	}
	n, _ := number(v)
	return int(n)
}

func (a *Adapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	if !a.defined["_send_exec_approval_prompt"] {
		return a.BasePlatformAdapter.SendExecApprovalPrompt(ctx, prompt)
	}
	actions := make([]any, len(prompt.Actions))
	for i, act := range prompt.Actions {
		actions[i] = []any{act.Label, act.Choice, act.Style}
	}
	var metadata any
	if prompt.Metadata != nil {
		metadata = prompt.Metadata
	}
	fields := map[string]any{"chat_id": prompt.ChatID, "session_key": prompt.SessionKey, "text": prompt.Text, "actions": actions,
		"command": prompt.Command, "description": prompt.Description, "smart_denied": prompt.SmartDenied, "metadata": metadata,
		"request_id": optionalPtr(prompt.RequestID)}
	return sendResult(a.surfaceCall(ctx, "_send_exec_approval_prompt", []any{fields}, nil, approvalBuilder))
}

// Python value of a logical platform keyword, None for the empty platform
func logicalPlatformArg(p gateway.Platform) any {
	if p == "" {
		return nil
	}
	return string(p)
}

func (a *Adapter) EmitWarning(ctx context.Context, chatID, content, replyTo string, metadata map[string]any, logicalPlatform gateway.Platform) (*gateway.SendResult, error) {
	if !a.defined["emit_warning"] {
		return a.BasePlatformAdapter.EmitWarning(ctx, chatID, content, replyTo, metadata, logicalPlatform)
	}
	v, err := a.platformKeywordCall(ctx, "emit_warning", []any{chatID, content},
		map[string]any{"reply_to": optional(replyTo), "metadata": metadata}, logicalPlatform)
	if err != nil || v == nil {
		return nil, err
	}
	result, err := sendResult(v, nil)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Surface call whose logical_platform keyword crosses rebuilt as the upstream Platform enum
func (a *Adapter) platformKeywordCall(ctx context.Context, name string, args []any, kw map[string]any, logicalPlatform gateway.Platform) (any, error) {
	kw["logical_platform"] = logicalPlatformArg(logicalPlatform)
	return a.obj.PlatformCall(ctx, name, args, kw, plugins.PlatformCallOptions{BuildKw: map[string]string{"logical_platform": platformEnumBuilder}})
}

func (a *Adapter) EmitMediaWarning(ctx context.Context, chatID, notice, caption, replyTo string, metadata, shownMetadata map[string]any) (gateway.SendResult, error) {
	if !a.defined["emit_media_warning"] {
		return a.BasePlatformAdapter.EmitMediaWarning(ctx, chatID, notice, caption, replyTo, metadata, shownMetadata)
	}
	return sendResult(a.surfaceCall(ctx, "emit_media_warning", []any{chatID, notice}, map[string]any{"caption": optional(caption),
		"reply_to": optional(replyTo), "metadata": metadata, "shown_metadata": shownMetadata}))
}

func (a *Adapter) WarningText(ctx context.Context, visible string, hidden *string, logicalPlatform gateway.Platform, chatID string, metadata map[string]any) *string {
	if !a.defined["warning_text"] {
		return a.BasePlatformAdapter.WarningText(ctx, visible, hidden, logicalPlatform, chatID, metadata)
	}
	v, err := a.platformKeywordCall(ctx, "warning_text", []any{visible, optionalPtr(hidden)},
		map[string]any{"chat_id": optional(chatID), "metadata": metadata}, logicalPlatform)
	if err != nil {
		panic(err)
	}
	if v == nil {
		return nil
	}
	s := config.PyStr(v)
	return &s
}

func (a *Adapter) WarningNotificationsEnabled(ctx context.Context, logicalPlatform gateway.Platform, chatID string, metadata map[string]any) bool {
	if !a.defined["warning_notifications_enabled"] {
		return a.BasePlatformAdapter.WarningNotificationsEnabled(ctx, logicalPlatform, chatID, metadata)
	}
	v, err := a.surfaceCall(ctx, "warning_notifications_enabled", []any{logicalPlatformArg(logicalPlatform)},
		map[string]any{"chat_id": optional(chatID), "metadata": metadata}, platformEnumBuilder)
	if err != nil {
		panic(err)
	}
	return config.PyTruthy(v)
}

// Field dict of an audio format as the upstream AudioFormat dataclass
func audioFormatArg(f gateway.AudioFormat) map[string]any {
	return map[string]any{"sample_rate": int64(f.SampleRate), "channels": int64(f.Channels), "sample_width": int64(f.SampleWidth)}
}

func (a *Adapter) SupportsStreamingTTS(chatID string, format gateway.AudioFormat) bool {
	if !a.defined["supports_streaming_tts"] {
		return a.BasePlatformAdapter.SupportsStreamingTTS(chatID, format)
	}
	return config.PyTruthy(a.mustSurface("supports_streaming_tts", []any{chatID, audioFormatArg(format)}, nil, "", audioFormatBuilder))
}

func (a *Adapter) BeginStreamingTTS(ctx context.Context, chatID string, format gateway.AudioFormat, metadata map[string]any) (*gateway.StreamingTTSHandle, error) {
	if !a.defined["begin_streaming_tts"] {
		return a.BasePlatformAdapter.BeginStreamingTTS(ctx, chatID, format, metadata)
	}
	v, err := a.obj.PlatformCall(ctx, "begin_streaming_tts", []any{chatID, audioFormatArg(format), metadata}, nil,
		plugins.PlatformCallOptions{Build: []string{"", audioFormatBuilder}, Hold: ttsHandleClass})
	if err != nil || v == nil {
		return nil, err
	}
	obj, ok := v.(*plugins.HostedObject)
	if !ok {
		return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("begin_streaming_tts returned a %s, not a StreamingTTSHandle", pyrepr.TypeName(v))}
	}
	handle := &gateway.StreamingTTSHandle{ChatID: chatID, AudioFormat: format}
	if err := pullHandle(ctx, obj, handle); err != nil {
		return nil, err
	}
	holdHandle(handle, obj)
	return handle, nil
}

// Python handle a Go handle stands for, its Go side bookkeeping written onto it before a call
func pushHandle(ctx context.Context, handle *gateway.StreamingTTSHandle) (*plugins.HostedObject, error) {
	held, ok := ttsHandles.Load(weak.Make(handle))
	if !ok {
		return nil, &gateway.PyError{Kind: "ValueError", Msg: "streaming TTS handle was not begun by this adapter"}
	}
	obj := held.(*plugins.HostedObject)
	if err := obj.Set(ctx, "audible", handle.Audible); err != nil {
		return nil, err
	}
	return obj, obj.Set(ctx, "aborted", handle.Aborted)
}

// Copies the Python handle's chat, format and bookkeeping fields onto the Go handle after a call
func pullHandle(ctx context.Context, obj *plugins.HostedObject, handle *gateway.StreamingTTSHandle) error {
	for name, dst := range map[string]*bool{"audible": &handle.Audible, "aborted": &handle.Aborted} {
		v, err := obj.Get(ctx, name)
		if err != nil {
			return err
		}
		*dst = config.PyTruthy(v)
	}
	if v, err := obj.Get(ctx, "chat_id"); err == nil && v != nil {
		handle.ChatID = config.PyStr(v)
	}
	if v, err := obj.Get(ctx, "audio_format"); err == nil {
		if f := plainMap(v); f != nil {
			for name, dst := range map[string]*int{"sample_rate": &handle.AudioFormat.SampleRate, "channels": &handle.AudioFormat.Channels,
				"sample_width": &handle.AudioFormat.SampleWidth} {
				if n, ok := number(f[name]); ok {
					*dst = int(n)
				}
			}
		}
	}
	return nil
}

// Runs a streaming TTS method on the Python handle, keeping both handles' bookkeeping equal
func (a *Adapter) handleCall(ctx context.Context, name string, handle *gateway.StreamingTTSHandle, args []any, kw map[string]any) error {
	obj, err := pushHandle(ctx, handle)
	if err != nil {
		return err
	}
	_, callErr := a.surfaceCall(ctx, name, append([]any{obj}, args...), kw)
	if err := pullHandle(ctx, obj, handle); err != nil && callErr == nil {
		callErr = err
	}
	return callErr
}

func (a *Adapter) WriteStreamingTTS(ctx context.Context, handle *gateway.StreamingTTSHandle, chunk []byte) error {
	if !a.defined["write_streaming_tts"] {
		return a.BasePlatformAdapter.WriteStreamingTTS(ctx, handle, chunk)
	}
	return a.handleCall(ctx, "write_streaming_tts", handle, []any{chunk}, nil)
}

func (a *Adapter) FinishStreamingTTS(ctx context.Context, handle *gateway.StreamingTTSHandle, interrupted bool) error {
	if !a.defined["finish_streaming_tts"] {
		return a.BasePlatformAdapter.FinishStreamingTTS(ctx, handle, interrupted)
	}
	return a.handleCall(ctx, "finish_streaming_tts", handle, nil, map[string]any{"interrupted": interrupted})
}

func (a *Adapter) AbortStreamingTTS(ctx context.Context, handle *gateway.StreamingTTSHandle, errText *string) error {
	if !a.defined["abort_streaming_tts"] {
		return a.BasePlatformAdapter.AbortStreamingTTS(ctx, handle, errText)
	}
	return a.handleCall(ctx, "abort_streaming_tts", handle, []any{optionalPtr(errText)}, nil)
}

func (a *Adapter) SendRetryIsFinal(result gateway.SendResult) bool {
	if !a.defined["_send_retry_is_final"] {
		return a.BasePlatformAdapter.SendRetryIsFinal(result)
	}
	var retryAfter any
	if result.RetryAfter != nil {
		retryAfter = *result.RetryAfter
	}
	ids := result.ContinuationMessageIDs
	if ids == nil {
		ids = []any{}
	}
	fields := map[string]any{"success": result.Success, "message_id": result.MessageID, "error": result.Error, "raw_response": rawMessage(result.RawResponse),
		"retryable": result.Retryable, "retry_after": retryAfter, "continuation_message_ids": ids, "error_kind": optionalPtr(result.ErrorKind)}
	return config.PyTruthy(a.mustSurface("_send_retry_is_final", []any{fields}, nil, sendResultBuilder))
}
