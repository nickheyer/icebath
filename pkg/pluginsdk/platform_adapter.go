package pluginsdk

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Text send of an adapter
type SendRequest struct {
	ChatID   string
	Content  string
	ReplyTo  string
	Metadata map[string]any
}

// Media send of an adapter, URL for a remote image and Path for a local file
type MediaRequest struct {
	ChatID   string
	URL      string
	Path     string
	Caption  string
	FileName string
	ReplyTo  string
	Metadata map[string]any
	// Further keywords the caller passed, such as the thread of a forum topic
	Extra map[string]any
}

// One image of a gallery send
type ImageRef struct {
	URL string
	Alt string
}

// Approval a dangerous command waits on
type ExecApprovalRequest struct {
	ChatID         string
	Command        string
	SessionKey     string
	Description    string
	Metadata       map[string]any
	AllowPermanent bool
	AllowSession   bool
	SmartDenied    bool
	RequestID      string
}

// Rendered approval prompt with its action buttons
type ExecApprovalPrompt struct {
	ChatID      string
	SessionKey  string
	Text        string
	Actions     []ApprovalAction
	Command     string
	Description string
	SmartDenied bool
	Metadata    map[string]any
	RequestID   string
}

// One button of an approval prompt
type ApprovalAction struct {
	Label  string
	Choice string
	Style  string
}

// Warning a platform shows the user, LogicalPlatform naming the surface it renders on
type WarningRequest struct {
	ChatID  string
	Content string
	// Text a surface that hides warnings shows in place of Content
	Hidden          string
	ReplyTo         string
	Caption         string
	Metadata        map[string]any
	ShownMetadata   map[string]any
	LogicalPlatform string
}

// One streamed reply event
type StreamEvent struct {
	// One of message_chunk, message_stop, commentary, tool_call_chunk, tool_call_finished, long_tool_hint or gateway_notice
	Kind     string
	Text     string
	Final    bool
	ToolName string
	Preview  string
	Args     map[string]any
	Index    int
	Duration float64
	OK       bool
	// Kind of a gateway notice
	Notice string
	Extra  map[string]any
}

// Sink a stream renderer writes the reply through
type StreamSink struct {
	delta, segment, commentary *HostFunc
}

// Appends text to the current message
func (s *StreamSink) Delta(ctx context.Context, text string) error {
	_, err := s.delta.Call(ctx, []any{text}, nil)
	return err
}

// Ends the current message segment
func (s *StreamSink) SegmentBreak(ctx context.Context) error {
	_, err := s.segment.Call(ctx, nil, nil)
	return err
}

// Writes commentary beside the message
func (s *StreamSink) Commentary(ctx context.Context, text string) error {
	_, err := s.commentary.Call(ctx, []any{text}, nil)
	return err
}

// Preview of one tool call a tool progress line shows
type ToolPreview struct {
	Text      string
	Truncated bool
	URL       string
}

// PCM format of streamed speech
type AudioFormat struct {
	SampleRate  int
	Channels    int
	SampleWidth int
}

// Streaming speech playback an adapter began, Audible and Aborted kept in step with the gateway around every call
type TTSStream struct {
	ChatID  string
	Format  AudioFormat
	Audible bool
	Aborted bool
	// Platform state the adapter keeps for the playback
	State any
}

// Adapter of one platform connection, every hook optional except Connect, Disconnect and Send, an unset one keeping the gateway behaviour
type Adapter struct {
	// Display name of the adapter, the platform name when empty
	Name string
	// Opens the connection and starts what listens for messages, false when it could not, a WebAssembly plugin running only while icebath calls it
	Connect    func(ctx context.Context, reconnect bool) (bool, error)
	Disconnect func(ctx context.Context) error
	Send       func(ctx context.Context, req SendRequest) (SendResult, error)
	// Name and type of a chat, a direct message named by its id when unset
	GetChatInfo         func(ctx context.Context, chatID string) (map[string]any, error)
	EditMessage         func(ctx context.Context, chatID, messageID, content string, finalize bool) (SendResult, error)
	DeleteMessage       func(ctx context.Context, chatID, messageID string) (bool, error)
	CreateHandoffThread func(ctx context.Context, parentChatID, name string) (threadID string, ok bool, err error)
	SendTyping          func(ctx context.Context, chatID string, metadata map[string]any) error
	StopTyping          func(ctx context.Context, chatID string) error
	SendImage           func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendAnimation       func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendImageFile       func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendVoice           func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendVideo           func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendDocument        func(ctx context.Context, req MediaRequest) (SendResult, error)
	PlayTTS             func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendMultipleImages  func(ctx context.Context, chatID string, images []ImageRef, metadata map[string]any, humanDelay float64) (SendResult, error)
	// Notice only one user of a chat sees, an empty user id addressing the chat
	SendPrivateNotice func(ctx context.Context, chatID, userID, content, replyTo string, metadata map[string]any) (SendResult, error)
	SendDraft         func(ctx context.Context, chatID string, draftID int, content string, metadata map[string]any) (SendResult, error)
	SendExecApproval  func(ctx context.Context, req ExecApprovalRequest) (SendResult, error)
	SendSlashConfirm  func(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (SendResult, error)
	SendClarify       func(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (SendResult, error)
	FormatMessage     func(ctx context.Context, content string) (string, error)
	// Longest message a chat takes
	MaxMessageLengthForChat func(ctx context.Context, chatID string) (int, error)
	// Length the platform counts a text as, the chat id empty when none is known
	MessageLength              func(ctx context.Context, chatID, text string) (int, error)
	EnforcesOwnAccessPolicy    func(ctx context.Context) bool
	AuthorizationIsUpstream    func(ctx context.Context) bool
	SupportsDraftStreaming     func(ctx context.Context, chatType string, metadata map[string]any, chatID string) bool
	PrefersFreshFinalStreaming func(ctx context.Context, content string, metadata map[string]any) bool
	StreamingOverflowLimit     func(ctx context.Context) (limit int, ok bool)
	SendPathDegraded           func(ctx context.Context) bool
	IsConnected                func(ctx context.Context) bool
	PrepareTTSText             func(ctx context.Context, text string) (string, error)
	OnProcessingStart          func(ctx context.Context, event MessageEvent) error
	OnProcessingComplete       func(ctx context.Context, event MessageEvent, outcome string) error
	// Takes over a message the gateway routes to the adapter, Gateway.HandleMessage continuing into the gateway pipeline
	HandleMessage         func(ctx context.Context, event MessageEvent) error
	ToolsetsForSource     func(ctx context.Context, source SessionSource) (toolsets []string, ok bool)
	CancelBackgroundTasks func(ctx context.Context) error
	RenderMessageEvent    func(ctx context.Context, event StreamEvent, sink *StreamSink) error
	FormatToolEvent       func(ctx context.Context, event StreamEvent, mode string, previewMaxLen int) (line string, ok bool, err error)
	FormatToolPreview     func(ctx context.Context, preview ToolPreview) (string, error)
	TextBatchKey          func(ctx context.Context, event MessageEvent) (string, error)
	EnqueueTextEvent      func(ctx context.Context, event MessageEvent) error
	// Seconds a text batch waits for more parts, pending nil before any part arrived
	TextBatchDelay         func(ctx context.Context, pending *MessageEvent) float64
	PopTextBatch           func(ctx context.Context, key string) (*MessageEvent, error)
	DispatchTextBatch      func(ctx context.Context, event MessageEvent) error
	FlushTextBatch         func(ctx context.Context, key string) error
	ApprovalHeader         func(ctx context.Context) string
	ApprovalReasonLabel    func(ctx context.Context) string
	ApprovalSmartDenyLine  func(ctx context.Context) string
	ApprovalActionLabels   func(ctx context.Context) map[string]string
	ApprovalEscape         func(ctx context.Context, text string) string
	ApprovalCommandBudget  func(ctx context.Context, description string, smartDenied bool) int
	SendExecApprovalPrompt func(ctx context.Context, prompt ExecApprovalPrompt) (SendResult, error)
	// Shows a warning, nil when the platform shows none
	EmitWarning            func(ctx context.Context, req WarningRequest) (*SendResult, error)
	EmitMediaWarning       func(ctx context.Context, req WarningRequest) (SendResult, error)
	WarningText            func(ctx context.Context, req WarningRequest) (text string, ok bool, err error)
	WarningNotificationsOn func(ctx context.Context, req WarningRequest) bool
	SupportsStreamingTTS   func(ctx context.Context, chatID string, format AudioFormat) bool
	// Begins a speech playback, nil when the chat cannot play one
	BeginStreamingTTS  func(ctx context.Context, chatID string, format AudioFormat, metadata map[string]any) (*TTSStream, error)
	WriteStreamingTTS  func(ctx context.Context, stream *TTSStream, chunk []byte) error
	FinishStreamingTTS func(ctx context.Context, stream *TTSStream, interrupted bool) error
	AbortStreamingTTS  func(ctx context.Context, stream *TTSStream, errText string) error
	// Whether a failed send is final and must not be retried
	SendRetryIsFinal func(ctx context.Context, result SendResult) bool
	// Runs after the gateway marked the adapter connected
	Connected func(ctx context.Context, listenerBase string) error
	// Runs after the gateway marked the adapter disconnected
	Disconnected func(ctx context.Context) error
	// Runs after the gateway recorded a fatal connection error
	FatalError func(ctx context.Context, code, message string, retryable bool) error
}

// Stream event kind of each event class the gateway names
var streamEventKinds = map[string]string{"MessageChunk": "message_chunk", "MessageStop": "message_stop", "Commentary": "commentary",
	"ToolCallChunk": "tool_call_chunk", "ToolCallFinished": "tool_call_finished", "LongToolHint": "long_tool_hint", "GatewayNotice": "gateway_notice"}

type buildersKey struct{}

// Class the gateway names for the positional argument of the platform call the context serves, empty when it names none
func builderClass(ctx context.Context, position int) string {
	builders, _ := ctx.Value(buildersKey{}).([]any)
	name := text(argAt(builders, position))
	return name[strings.LastIndexAny(name, ":.")+1:]
}

// Stream event of wire fields and the class the gateway named for them
func streamEventOf(class string, m map[string]any) StreamEvent {
	index, _ := integer(m["index"])
	e := StreamEvent{Kind: streamEventKinds[class], Text: text(m["text"]), Final: argFlag(m, "final"), ToolName: text(m["tool_name"]),
		Preview: text(m["preview"]), Args: asMap(m["args"]), Index: int(index), Duration: number(m["duration"]), OK: argFlag(m, "ok"),
		Extra: asMap(m["extra"])}
	if e.Kind == "gateway_notice" {
		e.Notice = text(m["kind"])
	}
	return e
}

// Audio format of wire fields
func audioFormatOf(m map[string]any) AudioFormat {
	rate, _ := integer(m["sample_rate"])
	channels, _ := integer(m["channels"])
	width, _ := integer(m["sample_width"])
	return AudioFormat{SampleRate: int(rate), Channels: int(channels), SampleWidth: int(width)}
}

// Media request of a chat and a target with the keyword options, every keyword the request does not name kept as extra
func mediaOf(args []any, kwargs map[string]any, url bool) MediaRequest {
	r := MediaRequest{ChatID: scalarText(argAt(args, 0)), Caption: text(kwargs["caption"]), FileName: text(kwargs["file_name"]),
		ReplyTo: text(kwargs["reply_to"]), Metadata: asMap(kwargs["metadata"]), Extra: map[string]any{}}
	if url {
		r.URL = argText(args, 1)
	} else {
		r.Path = argText(args, 1)
	}
	for k, v := range kwargs {
		switch k {
		case "caption", "file_name", "reply_to", "metadata":
		default:
			r.Extra[k] = v
		}
	}
	return r
}

// Wire value of one send
func sendReply(r SendResult, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return r.fields(), nil
}

// Object the gateway drives an adapter through, one method per hook the adapter sets
func adapterObject(p *peer, platform string, a *Adapter, gw *Gateway) (*Object, error) {
	if a.Connect == nil || a.Disconnect == nil || a.Send == nil {
		return nil, fmt.Errorf("the adapter of platform %q needs Connect, Disconnect and Send", platform)
	}
	obj := &Object{Type: "PlatformAdapter", gateway: gw}
	m := map[string]Func{}
	add := func(name string, set bool, fn Func) {
		if set {
			m[name] = fn
		}
	}
	meta := func(kwargs map[string]any) map[string]any { return asMap(kwargs["metadata"]) }
	chat := func(args []any) string { return scalarText(argAt(args, 0)) }
	event := func(args []any, i int) MessageEvent { return eventOf(argMap(args, i)) }
	m["connect"] = func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
		return a.Connect(ctx, argFlag(kw, "is_reconnect"))
	}
	m["disconnect"] = func(ctx context.Context, _ []any, _ map[string]any) (any, error) { return nil, a.Disconnect(ctx) }
	m["send"] = func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.Send(ctx, SendRequest{ChatID: chat(args), Content: argText(args, 1), ReplyTo: text(kw["reply_to"]), Metadata: meta(kw)}))
	}
	m["get_chat_info"] = func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		if a.GetChatInfo == nil {
			return map[string]any{"name": chat(args), "type": "dm"}, nil
		}
		return a.GetChatInfo(ctx, chat(args))
	}
	add("edit_message", a.EditMessage != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.EditMessage(ctx, chat(args), scalarText(argAt(args, 1)), argText(args, 2), argFlag(kw, "finalize")))
	})
	add("delete_message", a.DeleteMessage != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.DeleteMessage(ctx, chat(args), scalarText(argAt(args, 1)))
	})
	add("create_handoff_thread", a.CreateHandoffThread != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		thread, ok, err := a.CreateHandoffThread(ctx, chat(args), argText(args, 1))
		if err != nil || !ok {
			return nil, err
		}
		return thread, nil
	})
	add("send_typing", a.SendTyping != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return nil, a.SendTyping(ctx, chat(args), meta(kw))
	})
	add("stop_typing", a.StopTyping != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.StopTyping(ctx, chat(args))
	})
	media := func(name string, fn func(context.Context, MediaRequest) (SendResult, error), url bool) {
		add(name, fn != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
			return sendReply(fn(ctx, mediaOf(args, kw, url)))
		})
	}
	media("send_image", a.SendImage, true)
	media("send_animation", a.SendAnimation, true)
	media("send_image_file", a.SendImageFile, false)
	media("send_voice", a.SendVoice, false)
	media("send_video", a.SendVideo, false)
	media("send_document", a.SendDocument, false)
	media("play_tts", a.PlayTTS, false)
	add("send_multiple_images", a.SendMultipleImages != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		var images []ImageRef
		for _, raw := range asList(argAt(args, 1)) {
			if pair := asList(raw); len(pair) == 2 {
				images = append(images, ImageRef{URL: text(pair[0]), Alt: text(pair[1])})
			}
		}
		return sendReply(a.SendMultipleImages(ctx, chat(args), images, meta(kw), number(kw["human_delay"])))
	})
	add("send_private_notice", a.SendPrivateNotice != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.SendPrivateNotice(ctx, chat(args), scalarText(argAt(args, 1)), argText(args, 2), text(kw["reply_to"]), meta(kw)))
	})
	add("send_draft", a.SendDraft != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		id, _ := integer(argAt(args, 1))
		return sendReply(a.SendDraft(ctx, chat(args), int(id), argText(args, 2), meta(kw)))
	})
	add("send_exec_approval", a.SendExecApproval != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.SendExecApproval(ctx, ExecApprovalRequest{ChatID: chat(args), Command: argText(args, 1), SessionKey: argText(args, 2),
			Description: text(kw["description"]), Metadata: meta(kw), AllowPermanent: argFlag(kw, "allow_permanent"),
			AllowSession: argFlag(kw, "allow_session"), SmartDenied: argFlag(kw, "smart_denied"), RequestID: text(kw["request_id"])}))
	})
	add("send_slash_confirm", a.SendSlashConfirm != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.SendSlashConfirm(ctx, chat(args), argText(args, 1), argText(args, 2), argText(args, 3), argText(args, 4), meta(kw)))
	})
	add("send_clarify", a.SendClarify != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		var choices []string
		if raw := argAt(args, 2); raw != nil {
			choices = stringsOf(raw)
		}
		return sendReply(a.SendClarify(ctx, chat(args), argText(args, 1), choices, argText(args, 3), argText(args, 4), meta(kw)))
	})
	add("format_message", a.FormatMessage != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.FormatMessage(ctx, argText(args, 0))
	})
	add("max_message_length_for_chat", a.MaxMessageLengthForChat != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		n, err := a.MaxMessageLengthForChat(ctx, chat(args))
		return int64(n), err
	})
	flag := func(name string, fn func(context.Context) bool) {
		add(name, fn != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) { return fn(ctx), nil })
	}
	flag("enforces_own_access_policy", a.EnforcesOwnAccessPolicy)
	flag("authorization_is_upstream", a.AuthorizationIsUpstream)
	flag("send_path_degraded", a.SendPathDegraded)
	flag("is_connected", a.IsConnected)
	add("supports_draft_streaming", a.SupportsDraftStreaming != nil, func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
		return a.SupportsDraftStreaming(ctx, text(kw["chat_type"]), meta(kw), scalarText(kw["chat_id"])), nil
	})
	add("prefers_fresh_final_streaming", a.PrefersFreshFinalStreaming != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return a.PrefersFreshFinalStreaming(ctx, argText(args, 0), meta(kw)), nil
	})
	add("streaming_overflow_limit", a.StreamingOverflowLimit != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		limit, ok := a.StreamingOverflowLimit(ctx)
		if !ok {
			return nil, nil
		}
		return int64(limit), nil
	})
	add("prepare_tts_text", a.PrepareTTSText != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.PrepareTTSText(ctx, argText(args, 0))
	})
	add("on_processing_start", a.OnProcessingStart != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.OnProcessingStart(ctx, event(args, 0))
	})
	add("on_processing_complete", a.OnProcessingComplete != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.OnProcessingComplete(ctx, event(args, 0), argText(args, 1))
	})
	add("handle_message", a.HandleMessage != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.HandleMessage(ctx, event(args, 0))
	})
	add("toolsets_for_source", a.ToolsetsForSource != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		toolsets, ok := a.ToolsetsForSource(ctx, sourceOf(argMap(args, 0)))
		if !ok {
			return nil, nil
		}
		return stringsAny(toolsets), nil
	})
	add("cancel_background_tasks", a.CancelBackgroundTasks != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		return nil, a.CancelBackgroundTasks(ctx)
	})
	add("render_message_event", a.RenderMessageEvent != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		sinkArgs := argMap(args, 1)
		sink := &StreamSink{}
		sink.delta, _ = sinkArgs["on_delta"].(*HostFunc)
		sink.segment, _ = sinkArgs["on_segment_break"].(*HostFunc)
		sink.commentary, _ = sinkArgs["on_commentary"].(*HostFunc)
		if sink.delta == nil || sink.segment == nil || sink.commentary == nil {
			return nil, &Error{Type: "TypeError", Message: "render_message_event received no stream sink"}
		}
		return nil, a.RenderMessageEvent(ctx, streamEventOf(builderClass(ctx, 0), argMap(args, 0)), sink)
	})
	add("format_tool_event", a.FormatToolEvent != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		limit, _ := integer(kw["preview_max_len"])
		line, ok, err := a.FormatToolEvent(ctx, streamEventOf(builderClass(ctx, 0), argMap(args, 0)), text(kw["mode"]), int(limit))
		if err != nil || !ok {
			return nil, err
		}
		return line, nil
	})
	add("format_tool_preview", a.FormatToolPreview != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		preview := argMap(args, 0)
		return a.FormatToolPreview(ctx, ToolPreview{Text: text(preview["text"]), Truncated: argFlag(preview, "truncated"), URL: text(preview["url"])})
	})
	add("_text_batch_key", a.TextBatchKey != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.TextBatchKey(ctx, event(args, 0))
	})
	add("_enqueue_text_event", a.EnqueueTextEvent != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.EnqueueTextEvent(ctx, event(args, 0))
	})
	add("_text_batch_delay_for", a.TextBatchDelay != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		var pending *MessageEvent
		if raw := asMap(argAt(args, 0)); raw != nil {
			e := eventOf(raw)
			pending = &e
		}
		return a.TextBatchDelay(ctx, pending), nil
	})
	add("_pop_text_batch", a.PopTextBatch != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		e, err := a.PopTextBatch(ctx, argText(args, 0))
		if err != nil || e == nil {
			return nil, err
		}
		return e.fields(), nil
	})
	add("_dispatch_text_batch", a.DispatchTextBatch != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.DispatchTextBatch(ctx, event(args, 0))
	})
	add("_flush_text_batch", a.FlushTextBatch != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.FlushTextBatch(ctx, argText(args, 0))
	})
	label := func(name string, fn func(context.Context) string) {
		add(name, fn != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) { return fn(ctx), nil })
	}
	label("_EA_HEADER", a.ApprovalHeader)
	label("_EA_REASON_LABEL", a.ApprovalReasonLabel)
	label("_EA_SMART_DENY_LINE", a.ApprovalSmartDenyLine)
	add("_EA_ACTION_LABELS", a.ApprovalActionLabels != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		return a.ApprovalActionLabels(ctx), nil
	})
	add("_ea_escape", a.ApprovalEscape != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.ApprovalEscape(ctx, argText(args, 0)), nil
	})
	add("_exec_approval_cmd_budget", a.ApprovalCommandBudget != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		smart, _ := argAt(args, 1).(bool)
		return int64(a.ApprovalCommandBudget(ctx, argText(args, 0), smart)), nil
	})
	add("_send_exec_approval_prompt", a.SendExecApprovalPrompt != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		f := argMap(args, 0)
		prompt := ExecApprovalPrompt{ChatID: scalarText(f["chat_id"]), SessionKey: text(f["session_key"]), Text: text(f["text"]), Command: text(f["command"]),
			Description: text(f["description"]), SmartDenied: argFlag(f, "smart_denied"), Metadata: asMap(f["metadata"]), RequestID: text(f["request_id"])}
		for _, raw := range asList(f["actions"]) {
			if action := asList(raw); len(action) == 3 {
				prompt.Actions = append(prompt.Actions, ApprovalAction{Label: text(action[0]), Choice: text(action[1]), Style: text(action[2])})
			}
		}
		return sendReply(a.SendExecApprovalPrompt(ctx, prompt))
	})
	warning := func(kw map[string]any) WarningRequest {
		return WarningRequest{ChatID: scalarText(kw["chat_id"]), ReplyTo: text(kw["reply_to"]), Caption: text(kw["caption"]), Metadata: meta(kw),
			ShownMetadata: asMap(kw["shown_metadata"]), LogicalPlatform: text(kw["logical_platform"])}
	}
	add("emit_warning", a.EmitWarning != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		req := warning(kw)
		req.ChatID, req.Content = chat(args), argText(args, 1)
		r, err := a.EmitWarning(ctx, req)
		if err != nil || r == nil {
			return nil, err
		}
		return r.fields(), nil
	})
	add("emit_media_warning", a.EmitMediaWarning != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		req := warning(kw)
		req.ChatID, req.Content = chat(args), argText(args, 1)
		return sendReply(a.EmitMediaWarning(ctx, req))
	})
	add("warning_text", a.WarningText != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		req := warning(kw)
		req.Content, req.Hidden = argText(args, 0), argText(args, 1)
		line, ok, err := a.WarningText(ctx, req)
		if err != nil || !ok {
			return nil, err
		}
		return line, nil
	})
	add("warning_notifications_enabled", a.WarningNotificationsOn != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		req := warning(kw)
		req.LogicalPlatform = argText(args, 0)
		return a.WarningNotificationsOn(ctx, req), nil
	})
	add("supports_streaming_tts", a.SupportsStreamingTTS != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.SupportsStreamingTTS(ctx, chat(args), audioFormatOf(argMap(args, 1))), nil
	})
	add("begin_streaming_tts", a.BeginStreamingTTS != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		stream, err := a.BeginStreamingTTS(ctx, chat(args), audioFormatOf(argMap(args, 1)), asMap(argAt(args, 2)))
		if err != nil || stream == nil {
			return nil, err
		}
		handle := &Object{Type: "StreamingTTSHandle", Attrs: map[string]any{}, held: stream}
		p.pushStream(handle, stream)
		return handle, nil
	})
	streamCall := func(name string, set bool, run func(ctx context.Context, s *TTSStream, args []any, kw map[string]any) error) {
		add(name, set, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
			handle, _ := argAt(args, 0).(*Object)
			s := p.pullStream(handle)
			if s == nil {
				return nil, &Error{Type: "LookupError", Message: "the streaming speech handle was not begun by this adapter"}
			}
			err := run(ctx, s, args[1:], kw)
			p.pushStream(handle, s)
			return nil, err
		})
	}
	streamCall("write_streaming_tts", a.WriteStreamingTTS != nil, func(ctx context.Context, s *TTSStream, args []any, _ map[string]any) error {
		chunk, _ := argAt(args, 0).([]byte)
		return a.WriteStreamingTTS(ctx, s, chunk)
	})
	streamCall("finish_streaming_tts", a.FinishStreamingTTS != nil, func(ctx context.Context, s *TTSStream, _ []any, kw map[string]any) error {
		return a.FinishStreamingTTS(ctx, s, argFlag(kw, "interrupted"))
	})
	streamCall("abort_streaming_tts", a.AbortStreamingTTS != nil, func(ctx context.Context, s *TTSStream, args []any, _ map[string]any) error {
		return a.AbortStreamingTTS(ctx, s, argText(args, 0))
	})
	add("_send_retry_is_final", a.SendRetryIsFinal != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.SendRetryIsFinal(ctx, sendResultOf(argMap(args, 0))), nil
	})
	add("_mark_connected", a.Connected != nil, func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
		return nil, a.Connected(ctx, text(kw["listener_base"]))
	})
	add("_mark_disconnected", a.Disconnected != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		return nil, a.Disconnected(ctx)
	})
	add("_set_fatal_error", a.FatalError != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return nil, a.FatalError(ctx, argText(args, 0), argText(args, 1), argFlag(kw, "retryable"))
	})
	attrs := map[string]any{}
	if a.Name != "" {
		attrs["name"] = a.Name
	}
	if length := a.MessageLength; length != nil {
		counter := func(chatID string) Func {
			return func(ctx context.Context, args []any, _ map[string]any) (any, error) {
				n, err := length(ctx, chatID, argText(args, 0))
				return int64(n), err
			}
		}
		attrs["message_len_fn"] = p.ownedRef(obj, platform+".message_len_fn", counter(""))
		var mu sync.Mutex
		perChat := map[string]map[string]any{}
		m["message_len_fn_for_chat"] = func(_ context.Context, args []any, _ map[string]any) (any, error) {
			chatID := chat(args)
			mu.Lock()
			defer mu.Unlock()
			if perChat[chatID] == nil {
				perChat[chatID] = p.ownedRef(obj, platform+".message_len_fn_for_chat", counter(chatID))
			}
			return perChat[chatID], nil
		}
	}
	obj.Attrs, obj.Methods = attrs, m
	return obj, nil
}

// Stream of the object the gateway holds for it, with the flags the gateway wrote onto the object
func (p *peer) pullStream(obj *Object) *TTSStream {
	if obj == nil {
		return nil
	}
	s, _ := obj.held.(*TTSStream)
	if s == nil {
		return nil
	}
	attrsMu.Lock()
	s.Audible, _ = obj.Attrs["audible"].(bool)
	s.Aborted, _ = obj.Attrs["aborted"].(bool)
	attrsMu.Unlock()
	return s
}

// Copies the stream fields onto the object the gateway holds for it
func (p *peer) pushStream(obj *Object, s *TTSStream) {
	attrsMu.Lock()
	obj.Attrs["chat_id"], obj.Attrs["audible"], obj.Attrs["aborted"] = s.ChatID, s.Audible, s.Aborted
	obj.Attrs["audio_format"] = map[string]any{"sample_rate": int64(s.Format.SampleRate), "channels": int64(s.Format.Channels),
		"sample_width": int64(s.Format.SampleWidth)}
	attrsMu.Unlock()
}

// Names among those asked that a plugin object answers itself, every other one keeping the behaviour of the gateway
func (p *peer) definedOutside(params map[string]any) (any, error) {
	obj, err := p.requireObject(params["ref"])
	if err != nil {
		return nil, err
	}
	defined := []any{}
	for _, raw := range asList(params["names"]) {
		name := text(raw)
		_, attr := obj.Attr(name)
		if _, method := obj.Methods[name]; method || attr {
			defined = append(defined, name)
		}
	}
	return defined, nil
}

// Hands the gateway callbacks of an adapter to the bridge the adapter factory received
func (p *peer) platformBind(params map[string]any) (any, error) {
	obj, err := p.requireObject(params["ref"])
	if err != nil {
		return nil, err
	}
	if obj.gateway == nil {
		return nil, &Error{Type: "TypeError", Message: fmt.Sprintf("%s object is not an adapter a platform registration built", obj.Type)}
	}
	args := p.decodeList(params["args"])
	handle, _ := argAt(args, 0).(*HostFunc)
	run, _ := argAt(args, 1).(*HostFunc)
	lifecycle, _ := argAt(args, 2).(*HostFunc)
	if handle == nil || run == nil || lifecycle == nil {
		return nil, &Error{Type: "TypeError", Message: "a platform bind takes the message, handler and lifecycle callbacks"}
	}
	obj.gateway.bind(handle, run, lifecycle)
	return nil, nil
}

// Calls or reads one name of an adapter, private names included, a hook of its own that it lacks answering nothing
func (p *peer) platformCall(ctx context.Context, params map[string]any) (any, error) {
	obj, err := p.requireObject(params["ref"])
	if err != nil {
		return nil, err
	}
	name := text(params["name"])
	fn := obj.Methods[name]
	if truthy(params["get"]) {
		if value, ok := obj.Attr(name); ok {
			return value, nil
		}
		if fn == nil {
			return nil, &Error{Type: "AttributeError", Message: name}
		}
		return fn(context.WithValue(ctx, selfKey{}, obj), []any{}, map[string]any{})
	}
	if fn == nil {
		if truthy(params["own"]) {
			return nil, nil
		}
		return nil, &Error{Type: "AttributeError", Message: name}
	}
	ctx = context.WithValue(context.WithValue(ctx, selfKey{}, obj), buildersKey{}, asList(params["build"]))
	return fn(ctx, p.decodeList(params["args"]), p.decodeMap(params["kwargs"]))
}
