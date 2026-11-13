package pluginsdk

import (
	"context"
	"sync"
)

// Result wire value of one send call
func sendReply(r SendResult, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return r.Map(), nil
}

// Adapter object the host calls, one method per hook the adapter sets
func adapterObject(p *peer, a *Adapter) *Object {
	m := map[string]Func{}
	add := func(name string, ok bool, fn Func) {
		if ok {
			m[name] = fn
		}
	}
	meta := func(kwargs map[string]any) map[string]any {
		out, _ := kwargs["metadata"].(map[string]any)
		return out
	}
	event := func(args []any, i int) MessageEvent { return eventOf(argMap(args, i)) }
	add("connect", a.Connect != nil, func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
		return a.Connect(ctx, argFlag(kw, "is_reconnect"))
	})
	add("disconnect", a.Disconnect != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		return nil, a.Disconnect(ctx)
	})
	add("send", a.Send != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.Send(ctx, SendRequest{ChatID: argText(args, 0), Content: argText(args, 1), ReplyTo: text(kw["reply_to"]), Metadata: meta(kw)}))
	})
	add("get_chat_info", a.GetChatInfo != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.GetChatInfo(ctx, argText(args, 0))
	})
	add("edit_message", a.EditMessage != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.EditMessage(ctx, argText(args, 0), argText(args, 1), argText(args, 2), argFlag(kw, "finalize")))
	})
	add("delete_message", a.DeleteMessage != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.DeleteMessage(ctx, argText(args, 0), argText(args, 1))
	})
	add("create_handoff_thread", a.CreateHandoffThread != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		thread, ok, err := a.CreateHandoffThread(ctx, argText(args, 0), argText(args, 1))
		if err != nil || !ok {
			return nil, err
		}
		return thread, nil
	})
	add("send_typing", a.SendTyping != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return nil, a.SendTyping(ctx, argText(args, 0), meta(kw))
	})
	add("stop_typing", a.StopTyping != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return nil, a.StopTyping(ctx, argText(args, 0), meta(kw))
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
			pair := asList(raw)
			if len(pair) == 2 {
				images = append(images, ImageRef{URL: text(pair[0]), Alt: text(pair[1])})
			}
		}
		return sendReply(a.SendMultipleImages(ctx, argText(args, 0), images, meta(kw), number(kw["human_delay"])))
	})
	add("send_private_notice", a.SendPrivateNotice != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.SendPrivateNotice(ctx, argText(args, 0), argText(args, 1), argText(args, 2), text(kw["reply_to"]), meta(kw)))
	})
	add("send_draft", a.SendDraft != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		id, _ := integer(argAt(args, 1))
		return sendReply(a.SendDraft(ctx, argText(args, 0), int(id), argText(args, 2), meta(kw)))
	})
	add("send_exec_approval", a.SendExecApproval != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.SendExecApproval(ctx, ExecApprovalRequest{ChatID: argText(args, 0), Command: argText(args, 1), SessionKey: argText(args, 2),
			Description: text(kw["description"]), Metadata: meta(kw), AllowPermanent: argFlag(kw, "allow_permanent"), AllowSession: argFlag(kw, "allow_session"),
			SmartDenied: argFlag(kw, "smart_denied"), RequestID: text(kw["request_id"])}))
	})
	add("send_slash_confirm", a.SendSlashConfirm != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return sendReply(a.SendSlashConfirm(ctx, argText(args, 0), argText(args, 1), argText(args, 2), argText(args, 3), argText(args, 4), meta(kw)))
	})
	add("send_clarify", a.SendClarify != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		var choices []string
		if raw := argAt(args, 2); raw != nil {
			choices = stringsOf(raw)
		}
		return sendReply(a.SendClarify(ctx, argText(args, 0), argText(args, 1), choices, argText(args, 3), argText(args, 4), meta(kw)))
	})
	add("format_message", a.FormatMessage != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.FormatMessage(ctx, argText(args, 0))
	})
	add("max_message_length_for_chat", a.MaxMessageLengthForChat != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		n, err := a.MaxMessageLengthForChat(ctx, argText(args, 0))
		return int64(n), err
	})
	add("message_length", a.MessageLength != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		n, err := a.MessageLength(ctx, argText(args, 0), argText(args, 1))
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
		return a.SupportsDraftStreaming(ctx, text(kw["chat_type"]), meta(kw), text(kw["chat_id"])), nil
	})
	add("prefers_fresh_final_streaming", a.PrefersFreshFinalStreaming != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		return a.PrefersFreshFinalStreaming(ctx, argText(args, 0), meta(kw), text(kw["chat_id"])), nil
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
		sink.delta, _ = sinkArgs["delta"].(*HostFunc)
		sink.segment, _ = sinkArgs["segment_break"].(*HostFunc)
		sink.commentary, _ = sinkArgs["commentary"].(*HostFunc)
		return nil, a.RenderMessageEvent(ctx, streamEventOf(argMap(args, 0)), sink)
	})
	add("format_tool_event", a.FormatToolEvent != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		max, _ := integer(kw["preview_max_len"])
		line, ok, err := a.FormatToolEvent(ctx, streamEventOf(argMap(args, 0)), text(kw["mode"]), int(max))
		if err != nil || !ok {
			return nil, err
		}
		return line, nil
	})
	add("format_tool_preview", a.FormatToolPreview != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		pv := argMap(args, 0)
		return a.FormatToolPreview(ctx, ToolPreview{Text: text(pv["text"]), Truncated: argFlag(pv, "truncated"), URL: text(pv["url"])})
	})
	add("text_batch_key", a.TextBatchKey != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.TextBatchKey(ctx, event(args, 0))
	})
	add("enqueue_text_event", a.EnqueueTextEvent != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.EnqueueTextEvent(ctx, event(args, 0))
	})
	add("text_batch_delay", a.TextBatchDelay != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		var pending *MessageEvent
		if raw, ok := argAt(args, 0).(map[string]any); ok {
			e := eventOf(raw)
			pending = &e
		}
		return a.TextBatchDelay(ctx, pending), nil
	})
	add("pop_text_batch", a.PopTextBatch != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		e, err := a.PopTextBatch(ctx, argText(args, 0))
		if err != nil || e == nil {
			return nil, err
		}
		return e.Map(), nil
	})
	add("dispatch_text_batch", a.DispatchTextBatch != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.DispatchTextBatch(ctx, event(args, 0))
	})
	add("flush_text_batch", a.FlushTextBatch != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return nil, a.FlushTextBatch(ctx, argText(args, 0))
	})
	label := func(name string, fn func(context.Context) string) {
		add(name, fn != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) { return fn(ctx), nil })
	}
	label("approval_header", a.ApprovalHeader)
	label("approval_reason_label", a.ApprovalReasonLabel)
	label("approval_smart_deny_line", a.ApprovalSmartDenyLine)
	add("approval_action_labels", a.ApprovalActionLabels != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		out := map[string]any{}
		for k, v := range a.ApprovalActionLabels(ctx) {
			out[k] = v
		}
		return out, nil
	})
	add("approval_escape", a.ApprovalEscape != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.ApprovalEscape(ctx, argText(args, 0)), nil
	})
	add("approval_command_budget", a.ApprovalCommandBudget != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		smart, _ := argAt(args, 1).(bool)
		return int64(a.ApprovalCommandBudget(ctx, argText(args, 0), smart)), nil
	})
	add("send_exec_approval_prompt", a.SendExecApprovalPrompt != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		f := argMap(args, 0)
		prompt := ExecApprovalPrompt{ChatID: text(f["chat_id"]), SessionKey: text(f["session_key"]), Text: text(f["text"]), Command: text(f["command"]),
			Description: text(f["description"]), SmartDenied: argFlag(f, "smart_denied"), RequestID: text(f["request_id"])}
		prompt.Metadata, _ = f["metadata"].(map[string]any)
		for _, raw := range asList(f["actions"]) {
			act := asList(raw)
			if len(act) == 3 {
				prompt.Actions = append(prompt.Actions, ApprovalAction{Label: text(act[0]), Choice: text(act[1]), Style: text(act[2])})
			}
		}
		return sendReply(a.SendExecApprovalPrompt(ctx, prompt))
	})
	add("emit_warning", a.EmitWarning != nil, func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
		r, err := a.EmitWarning(ctx, warningOf(kw))
		if err != nil || r == nil {
			return nil, err
		}
		return r.Map(), nil
	})
	add("emit_media_warning", a.EmitMediaWarning != nil, func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
		return sendReply(a.EmitMediaWarning(ctx, warningOf(kw)))
	})
	add("warning_text", a.WarningText != nil, func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
		line, ok, err := a.WarningText(ctx, warningOf(kw))
		if err != nil || !ok {
			return nil, err
		}
		return line, nil
	})
	add("warning_notifications_enabled", a.WarningNotificationsOn != nil, func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
		return a.WarningNotificationsOn(ctx, warningOf(kw)), nil
	})
	add("supports_streaming_tts", a.SupportsStreamingTTS != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.SupportsStreamingTTS(ctx, argText(args, 0), audioFormatOf(argMap(args, 1))), nil
	})
	streams := &ttsStreams{byObject: map[*Object]*TTSStream{}}
	add("begin_streaming_tts", a.BeginStreamingTTS != nil, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
		stream, err := a.BeginStreamingTTS(ctx, argText(args, 0), audioFormatOf(argMap(args, 1)), meta(kw))
		if err != nil || stream == nil {
			return nil, err
		}
		return streams.object(stream), nil
	})
	streamCall := func(name string, ok bool, run func(ctx context.Context, s *TTSStream, args []any, kw map[string]any) error) {
		add(name, ok, func(ctx context.Context, args []any, kw map[string]any) (any, error) {
			obj, _ := argAt(args, 0).(*Object)
			s := streams.pull(obj)
			if s == nil {
				return nil, &Error{Type: "LookupError", Message: "streaming speech handle was not begun by this adapter"}
			}
			err := run(ctx, s, args[1:], kw)
			streams.push(obj, s)
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
	add("send_retry_is_final", a.SendRetryIsFinal != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return a.SendRetryIsFinal(ctx, sendResultOf(argMap(args, 0))), nil
	})
	attrs := map[string]any{}
	if a.Name != "" {
		attrs["name"] = a.Name
	}
	return &Object{Type: "PlatformAdapter", Attrs: attrs, Methods: m}
}

// Streaming speech handles an adapter began, each kept beside the object the host holds for it
type ttsStreams struct {
	mu       sync.Mutex
	byObject map[*Object]*TTSStream
}

// Object the host holds for a stream, its fields readable and its flags writable by the host
func (t *ttsStreams) object(s *TTSStream) *Object {
	obj := &Object{Type: "StreamingTTSHandle", Attrs: map[string]any{}}
	t.mu.Lock()
	t.byObject[obj] = s
	t.mu.Unlock()
	t.push(obj, s)
	return obj
}

// Stream of a held object with the flags the host wrote onto it
func (t *ttsStreams) pull(obj *Object) *TTSStream {
	if obj == nil {
		return nil
	}
	t.mu.Lock()
	s := t.byObject[obj]
	t.mu.Unlock()
	if s == nil {
		return nil
	}
	if v, ok := obj.Attr("audible"); ok {
		s.Audible, _ = v.(bool)
	}
	if v, ok := obj.Attr("aborted"); ok {
		s.Aborted, _ = v.(bool)
	}
	return s
}

// Copies the stream fields onto its held object for the host to read
func (t *ttsStreams) push(obj *Object, s *TTSStream) {
	obj.SetAttr("chat_id", s.ChatID)
	obj.SetAttr("audible", s.Audible)
	obj.SetAttr("aborted", s.Aborted)
	obj.SetAttr("audio_format", map[string]any{"sample_rate": int64(s.Format.SampleRate), "channels": int64(s.Format.Channels),
		"sample_width": int64(s.Format.SampleWidth)})
}

// Argument at a position, nil when absent
func argAt(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}
	return nil
}
