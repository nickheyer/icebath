package hosted

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Module whose BasePlatformAdapter methods a hosted adapter inherits rather than defines
const baseModule = "gateway.platforms.base"

// Builder of an inbound event the child turns back into a MessageEvent
const eventBuilder = "gateway.platforms.event:MessageEvent"

// Python methods the wrapper forwards when the plugin's class defines them outside the upstream base
var forwarded = []string{"connect", "disconnect", "send", "get_chat_info", "edit_message", "delete_message", "create_handoff_thread",
	"send_typing", "stop_typing", "send_image", "send_animation", "send_image_file", "send_voice", "send_video", "send_document",
	"send_multiple_images", "send_private_notice", "send_draft", "send_exec_approval", "send_slash_confirm", "send_clarify",
	"format_message", "max_message_length_for_chat", "enforces_own_access_policy", "authorization_is_upstream",
	"supports_draft_streaming", "prefers_fresh_final_streaming", "streaming_overflow_limit", "send_path_degraded",
	"prepare_tts_text", "play_tts", "on_processing_start", "on_processing_complete", "handle_message", "toolsets_for_source",
	"cancel_background_tasks", "name"}

// Platform adapter whose object lives in the plugin host, Go base behaviour wherever the plugin class keeps the upstream one
type Adapter struct {
	*gateway.BasePlatformAdapter
	obj     *plugins.HostedObject
	defined map[string]bool
}

// Adapter the hosted factory builds for a platform config, nil when the factory returns None
func buildAdapter(ctx context.Context, platform string, f *plugins.HostedCallable, cfg *gateway.PlatformConfig) (any, error) {
	value, _, err := f.CallWith(ctx, []any{cfg.ToDict()}, nil, plugins.HostedCallOptions{Build: []string{platformConfigBuilder}, Describe: adapterBase})
	if err != nil || value == nil {
		return nil, err
	}
	obj, ok := value.(*plugins.HostedObject)
	if !ok {
		return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("adapter_factory returned a %s, not a platform adapter", pyrepr.TypeName(value))}
	}
	return NewAdapter(ctx, gateway.Platform(platform), cfg, obj)
}

// Wraps a hosted adapter object and routes its inbound events to the Go base and the runner
func NewAdapter(ctx context.Context, platform gateway.Platform, cfg *gateway.PlatformConfig, obj *plugins.HostedObject) (*Adapter, error) {
	defined, err := obj.DefinedOutside(ctx, baseModule, append(append([]string{}, forwarded...), surface...))
	if err != nil {
		return nil, err
	}
	a := &Adapter{obj: obj, defined: defined}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, platform, a)
	err = obj.BindPlatform(ctx, !defined["handle_message"], func(ctx context.Context, event map[string]any) (any, error) {
		ev, err := eventFromDict(ctx, event)
		if err != nil {
			return nil, err
		}
		return nil, a.BasePlatformAdapter.HandleMessage(ctx, ev)
	}, func(ctx context.Context, event map[string]any) (any, error) {
		handler := a.MessageHandler()
		if handler == nil {
			return nil, nil
		}
		ev, err := eventFromDict(ctx, event)
		if err != nil {
			return nil, err
		}
		return handler(ctx, ev)
	}, a.baseLifecycle)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Hosted object the adapter forwards to
func (a *Adapter) Object() *plugins.HostedObject { return a.obj }

// None for an empty optional string argument
func optional(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func optionalPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Keyword arguments with the extra keywords a Go caller passed merged over them
func withExtra(kw, extra map[string]any) map[string]any {
	for k, v := range extra {
		kw[k] = v
	}
	return kw
}

// SendResult of a hosted send, its dataclass fields read by name
func sendResult(v any, err error) (gateway.SendResult, error) {
	if err != nil {
		return gateway.SendResult{}, err
	}
	m := plainMap(v)
	if m == nil {
		return gateway.SendResult{}, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("expected a SendResult, got %s", pyrepr.TypeName(v))}
	}
	out := gateway.SendResult{Success: config.PyTruthy(m["success"]), MessageID: m["message_id"], Error: m["error"],
		RawResponse: m["raw_response"], Retryable: config.PyTruthy(m["retryable"])}
	if f, ok := number(m["retry_after"]); ok {
		out.RetryAfter = &f
	}
	if ids, ok := m["continuation_message_ids"].([]any); ok {
		out.ContinuationMessageIDs = ids
	}
	if kind, ok := m["error_kind"].(string); ok {
		out.ErrorKind = &kind
	}
	return out, nil
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// Forwards a send style method and reads its SendResult
func (a *Adapter) send(ctx context.Context, method string, args []any, kw map[string]any) (gateway.SendResult, error) {
	return sendResult(a.obj.Call(ctx, method, args, kw))
}

// Answer of a forwarded method with no context, a raise surfacing as a panic like the Go overrides
func (a *Adapter) must(method string, args []any, kw map[string]any) any {
	v, err := a.obj.Call(context.Background(), method, args, kw)
	if err != nil {
		panic(err)
	}
	return v
}

func (a *Adapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	v, err := a.obj.Call(ctx, "connect", nil, map[string]any{"is_reconnect": isReconnect})
	if err != nil {
		return false, err
	}
	return config.PyTruthy(v), nil
}

func (a *Adapter) Disconnect(ctx context.Context) error {
	_, err := a.obj.Call(ctx, "disconnect", nil, nil)
	return err
}

func (a *Adapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.send(ctx, "send", []any{chatID, content}, map[string]any{"reply_to": optional(replyTo), "metadata": metadata})
}

func (a *Adapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	v, err := a.obj.Call(ctx, "get_chat_info", []any{chatID}, nil)
	return plainMap(v), err
}

func (a *Adapter) Name() string {
	if !a.defined["name"] {
		return a.BasePlatformAdapter.Name()
	}
	v, err := a.obj.Get(context.Background(), "name")
	if err != nil {
		panic(err)
	}
	return config.PyStr(v)
}

func (a *Adapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	if !a.defined["edit_message"] {
		return a.BasePlatformAdapter.EditMessage(ctx, chatID, messageID, content, finalize)
	}
	return a.send(ctx, "edit_message", []any{chatID, messageID, content}, map[string]any{"finalize": finalize})
}

func (a *Adapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	if !a.defined["delete_message"] {
		return a.BasePlatformAdapter.DeleteMessage(ctx, chatID, messageID)
	}
	v, err := a.obj.Call(ctx, "delete_message", []any{chatID, messageID}, nil)
	return config.PyTruthy(v), err
}

func (a *Adapter) CreateHandoffThread(ctx context.Context, parentChatID, name string) (string, bool, error) {
	if !a.defined["create_handoff_thread"] {
		return a.BasePlatformAdapter.CreateHandoffThread(ctx, parentChatID, name)
	}
	v, err := a.obj.Call(ctx, "create_handoff_thread", []any{parentChatID, name}, nil)
	if err != nil || v == nil {
		return "", false, err
	}
	return config.PyStr(v), true, nil
}

func (a *Adapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	if !a.defined["send_typing"] {
		return a.BasePlatformAdapter.SendTyping(ctx, chatID, metadata)
	}
	_, err := a.obj.Call(ctx, "send_typing", []any{chatID}, map[string]any{"metadata": metadata})
	return err
}

func (a *Adapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	if !a.defined["stop_typing"] {
		return a.BasePlatformAdapter.StopTyping(ctx, chatID, metadata)
	}
	_, err := a.obj.Call(ctx, "stop_typing", []any{chatID}, nil)
	return err
}

func (a *Adapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_image"] {
		return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, metadata)
	}
	return a.send(ctx, "send_image", []any{chatID, imageURL}, map[string]any{"caption": optional(caption), "reply_to": optional(replyTo), "metadata": metadata})
}

func (a *Adapter) SendAnimation(ctx context.Context, chatID, animationURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_animation"] {
		return a.BasePlatformAdapter.SendAnimation(ctx, chatID, animationURL, caption, replyTo, metadata)
	}
	return a.send(ctx, "send_animation", []any{chatID, animationURL}, map[string]any{"caption": optional(caption), "reply_to": optional(replyTo), "metadata": metadata})
}

func (a *Adapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_image_file"] {
		return a.BasePlatformAdapter.SendImageFile(ctx, chatID, imagePath, caption, replyTo, metadata, extra)
	}
	return a.send(ctx, "send_image_file", []any{chatID, imagePath},
		withExtra(map[string]any{"caption": optional(caption), "reply_to": optional(replyTo), "metadata": metadata}, extra))
}

func (a *Adapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_voice"] {
		return a.BasePlatformAdapter.SendVoice(ctx, chatID, audioPath, caption, replyTo, metadata, extra)
	}
	return a.send(ctx, "send_voice", []any{chatID, audioPath},
		withExtra(map[string]any{"caption": optional(caption), "reply_to": optional(replyTo), "metadata": metadata}, extra))
}

func (a *Adapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_video"] {
		return a.BasePlatformAdapter.SendVideo(ctx, chatID, videoPath, caption, replyTo, metadata, extra)
	}
	return a.send(ctx, "send_video", []any{chatID, videoPath},
		withExtra(map[string]any{"caption": optional(caption), "reply_to": optional(replyTo), "metadata": metadata}, extra))
}

func (a *Adapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_document"] {
		return a.BasePlatformAdapter.SendDocument(ctx, chatID, filePath, caption, fileName, replyTo, metadata, extra)
	}
	return a.send(ctx, "send_document", []any{chatID, filePath}, withExtra(map[string]any{"caption": optional(caption),
		"file_name": optional(fileName), "reply_to": optional(replyTo), "metadata": metadata}, extra))
}

func (a *Adapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any, humanDelay float64) (gateway.SendResult, error) {
	if !a.defined["send_multiple_images"] {
		return a.BasePlatformAdapter.SendMultipleImages(ctx, chatID, images, metadata, humanDelay)
	}
	pairs := make([]any, len(images))
	for i, img := range images {
		pairs[i] = []any{img.URL, img.Alt}
	}
	return a.send(ctx, "send_multiple_images", []any{chatID, pairs}, map[string]any{"metadata": metadata, "human_delay": humanDelay})
}

func (a *Adapter) SendPrivateNotice(ctx context.Context, chatID string, userID *string, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_private_notice"] {
		return a.BasePlatformAdapter.SendPrivateNotice(ctx, chatID, userID, content, replyTo, metadata)
	}
	return a.send(ctx, "send_private_notice", []any{chatID, optionalPtr(userID), content}, map[string]any{"reply_to": optional(replyTo), "metadata": metadata})
}

func (a *Adapter) SendDraft(ctx context.Context, chatID string, draftID int, content string, metadata map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_draft"] {
		return a.BasePlatformAdapter.SendDraft(ctx, chatID, draftID, content, metadata)
	}
	return a.send(ctx, "send_draft", []any{chatID, int64(draftID), content}, map[string]any{"metadata": metadata})
}

func (a *Adapter) SendExecApproval(ctx context.Context, chatID, command, sessionKey string, description *string, metadata map[string]any, allowPermanent, allowSession, smartDenied bool, requestID *string) (gateway.SendResult, error) {
	if !a.defined["send_exec_approval"] {
		return a.BasePlatformAdapter.SendExecApproval(ctx, chatID, command, sessionKey, description, metadata, allowPermanent, allowSession, smartDenied, requestID)
	}
	return a.send(ctx, "send_exec_approval", []any{chatID, command, sessionKey}, map[string]any{"description": optionalPtr(description),
		"metadata": metadata, "allow_permanent": allowPermanent, "allow_session": allowSession, "smart_denied": smartDenied, "request_id": optionalPtr(requestID)})
}

func (a *Adapter) SendSlashConfirm(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_slash_confirm"] {
		return a.BasePlatformAdapter.SendSlashConfirm(ctx, chatID, title, message, sessionKey, confirmID, metadata)
	}
	return a.send(ctx, "send_slash_confirm", []any{chatID, title, message, sessionKey, confirmID}, map[string]any{"metadata": metadata})
}

func (a *Adapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	if !a.defined["send_clarify"] {
		return a.BasePlatformAdapter.SendClarify(ctx, chatID, question, choices, clarifyID, sessionKey, metadata)
	}
	var list any
	if choices != nil {
		items := make([]any, len(choices))
		for i, c := range choices {
			items[i] = c
		}
		list = items
	}
	return a.send(ctx, "send_clarify", []any{chatID, question, list, clarifyID, sessionKey}, map[string]any{"metadata": metadata})
}

func (a *Adapter) FormatMessage(content string) string {
	if !a.defined["format_message"] {
		return a.BasePlatformAdapter.FormatMessage(content)
	}
	return config.PyStr(a.must("format_message", []any{content}, nil))
}

func (a *Adapter) MaxMessageLengthForChat(chatID string) (int, error) {
	if !a.defined["max_message_length_for_chat"] {
		return a.BasePlatformAdapter.MaxMessageLengthForChat(chatID)
	}
	v, err := a.obj.Call(context.Background(), "max_message_length_for_chat", []any{chatID}, nil)
	if err != nil {
		return 0, err
	}
	n, _ := number(v)
	return int(n), nil
}

func (a *Adapter) EnforcesOwnAccessPolicy() bool {
	if !a.defined["enforces_own_access_policy"] {
		return a.BasePlatformAdapter.EnforcesOwnAccessPolicy()
	}
	return config.PyTruthy(a.must("enforces_own_access_policy", nil, nil))
}

func (a *Adapter) AuthorizationIsUpstream() bool {
	if !a.defined["authorization_is_upstream"] {
		return a.BasePlatformAdapter.AuthorizationIsUpstream()
	}
	return config.PyTruthy(a.must("authorization_is_upstream", nil, nil))
}

func (a *Adapter) SupportsDraftStreaming(chatType string, metadata map[string]any, chatID string) bool {
	if !a.defined["supports_draft_streaming"] {
		return a.BasePlatformAdapter.SupportsDraftStreaming(chatType, metadata, chatID)
	}
	return config.PyTruthy(a.must("supports_draft_streaming", nil, map[string]any{"chat_type": optional(chatType), "metadata": metadata, "chat_id": optional(chatID)}))
}

func (a *Adapter) PrefersFreshFinalStreaming(content string, metadata map[string]any, chatID string) bool {
	if !a.defined["prefers_fresh_final_streaming"] {
		return a.BasePlatformAdapter.PrefersFreshFinalStreaming(content, metadata, chatID)
	}
	return config.PyTruthy(a.must("prefers_fresh_final_streaming", []any{content}, map[string]any{"metadata": metadata}))
}

func (a *Adapter) StreamingOverflowLimit() (int, bool) {
	if !a.defined["streaming_overflow_limit"] {
		return a.BasePlatformAdapter.StreamingOverflowLimit()
	}
	n, ok := number(a.must("streaming_overflow_limit", nil, nil))
	return int(n), ok
}

func (a *Adapter) SendPathDegraded() bool {
	if !a.defined["send_path_degraded"] {
		return a.BasePlatformAdapter.SendPathDegraded()
	}
	return config.PyTruthy(a.must("send_path_degraded", nil, nil))
}

func (a *Adapter) PrepareTTSText(ctx context.Context, text string) string {
	if !a.defined["prepare_tts_text"] {
		return a.BasePlatformAdapter.PrepareTTSText(ctx, text)
	}
	v, err := a.obj.Call(ctx, "prepare_tts_text", []any{text}, nil)
	if err != nil {
		panic(err)
	}
	return config.PyStr(v)
}

func (a *Adapter) PlayTTS(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if !a.defined["play_tts"] {
		return a.BasePlatformAdapter.PlayTTS(ctx, chatID, audioPath, caption, replyTo, metadata, extra)
	}
	return a.send(ctx, "play_tts", []any{chatID, audioPath},
		withExtra(map[string]any{"caption": optional(caption), "reply_to": optional(replyTo), "metadata": metadata}, extra))
}

func (a *Adapter) OnProcessingStart(ctx context.Context, event *gateway.MessageEvent) error {
	if !a.defined["on_processing_start"] {
		return a.BasePlatformAdapter.OnProcessingStart(ctx, event)
	}
	_, err := a.obj.CallBuilt(ctx, "on_processing_start", []any{eventToDict(event)}, nil, []string{eventBuilder})
	return err
}

func (a *Adapter) OnProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	if !a.defined["on_processing_complete"] {
		return a.BasePlatformAdapter.OnProcessingComplete(ctx, event, outcome)
	}
	_, err := a.obj.CallBuilt(ctx, "on_processing_complete", []any{eventToDict(event), string(outcome)}, nil,
		[]string{eventBuilder, "gateway.platforms.base:ProcessingOutcome"})
	return err
}

func (a *Adapter) HandleMessage(ctx context.Context, event *gateway.MessageEvent) error {
	if !a.defined["handle_message"] {
		return a.BasePlatformAdapter.HandleMessage(ctx, event)
	}
	_, err := a.obj.CallBuilt(ctx, "handle_message", []any{eventToDict(event)}, nil, []string{eventBuilder})
	return err
}

func (a *Adapter) ToolsetsForSource(source *gateway.SessionSource) ([]string, bool) {
	if !a.defined["toolsets_for_source"] {
		return a.BasePlatformAdapter.ToolsetsForSource(source)
	}
	v, err := a.obj.CallBuilt(context.Background(), "toolsets_for_source", []any{sourceToDict(source)}, nil, []string{"gateway.session:SessionSource.from_dict"})
	if err != nil {
		panic(err)
	}
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = config.PyStr(item)
	}
	return out, true
}

func (a *Adapter) CancelBackgroundTasks(ctx context.Context) {
	if a.defined["cancel_background_tasks"] {
		if _, err := a.obj.Call(ctx, "cancel_background_tasks", nil, nil); err != nil {
			panic(err)
		}
		return
	}
	a.BasePlatformAdapter.CancelBackgroundTasks(ctx)
}

// Python field dict of an event as the child rebuilds it, timestamp in ISO form
func eventToDict(e *gateway.MessageEvent) map[string]any {
	strs := func(items []string) []any {
		out := make([]any, len(items))
		for i, s := range items {
			out[i] = s
		}
		return out
	}
	inlined := make([]any, len(e.MediaTextInlined))
	for i, b := range e.MediaTextInlined {
		if b != nil {
			inlined[i] = *b
		}
	}
	var replyExpected any
	if e.ReplyExpected != nil {
		replyExpected = *e.ReplyExpected
	}
	var updateID any
	if e.PlatformUpdateID != nil {
		updateID = *e.PlatformUpdateID
	}
	var promptResponse any
	if e.PromptResponse != nil {
		promptResponse = e.PromptResponse
	}
	metadata := e.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	return map[string]any{"text": e.Text, "message_type": string(e.MessageType), "user_id": optionalPtr(e.UserID), "user_name": optionalPtr(e.UserName),
		"source": sourceToDict(e.Source), "raw_message": rawMessage(e.RawMessage), "message_id": optionalPtr(e.MessageID),
		"ledger_message_id": optionalPtr(e.LedgerMessageID), "reply_anchor_override": optionalPtr(e.ReplyAnchorOverride),
		"platform_update_id": updateID, "media_urls": strs(e.MediaURLs), "media_types": strs(e.MediaTypes), "media_text_inlined": inlined,
		"reply_to_message_id": optionalPtr(e.ReplyToMessageID), "reply_to_text": optionalPtr(e.ReplyToText),
		"reply_to_author_id": optionalPtr(e.ReplyToAuthorID), "reply_to_author_name": optionalPtr(e.ReplyToAuthorName),
		"reply_to_is_own_message": e.ReplyToIsOwnMessage, "prompt_response": promptResponse, "auto_skill": e.AutoSkill,
		"channel_prompt": optionalPtr(e.ChannelPrompt), "channel_context": optionalPtr(e.ChannelContext), "internal": e.Internal,
		"metadata": metadata, "timestamp": e.Timestamp.Format("2006-01-02T15:04:05.999999"), "allow_gateway_control": e.AllowGatewayControl,
		"reply_expected": replyExpected, "inherit_channel_pin": e.InheritChannelPin, "edit_revision": optionalPtr(e.EditRevision),
		"media_filenames": strs(e.MediaFilenames)}
}

// Raw platform message the child receives, the decoded mapping a hosted adapter produced or nothing for a Go object
func rawMessage(v any) any {
	switch v.(type) {
	case nil, string, bool, int64, float64, map[string]any, []any, plugins.Record:
		return v
	}
	return nil
}

// Wire dict of a session source, None for a missing one
func sourceToDict(s *gateway.SessionSource) any {
	if s == nil {
		return nil
	}
	obj := s.ToDict()
	out := make(map[string]any, obj.Len())
	for _, k := range obj.Keys {
		v, _ := obj.Get(k)
		out[k] = v
	}
	return out
}

// Go event of the field dict a hosted adapter sent inbound
func eventFromDict(ctx context.Context, d map[string]any) (*gateway.MessageEvent, error) {
	e := &gateway.MessageEvent{Text: config.PyStr(orEmpty(d["text"])), MessageType: gateway.MessageTypeText, AllowGatewayControl: true,
		Metadata: map[string]any{}, MediaURLs: []string{}, MediaTypes: []string{}, MediaFilenames: []string{}, Timestamp: time.Now()}
	if mt, ok := d["message_type"].(string); ok {
		e.MessageType = gateway.MessageType(mt)
	}
	if src := plainMap(d["source"]); src != nil {
		source, err := gateway.SessionSourceFromDict(ctx, src)
		if err != nil {
			return nil, err
		}
		e.Source = source
	}
	strPtr := func(key string) *string {
		if v, ok := d[key]; ok && v != nil {
			s := config.PyStr(v)
			return &s
		}
		return nil
	}
	strList := func(key string) []string {
		items, _ := d[key].([]any)
		out := make([]string, len(items))
		for i, item := range items {
			out[i] = config.PyStr(item)
		}
		return out
	}
	e.UserID, e.UserName, e.MessageID = strPtr("user_id"), strPtr("user_name"), strPtr("message_id")
	e.LedgerMessageID, e.ReplyAnchorOverride = strPtr("ledger_message_id"), strPtr("reply_anchor_override")
	e.ReplyToMessageID, e.ReplyToText = strPtr("reply_to_message_id"), strPtr("reply_to_text")
	e.ReplyToAuthorID, e.ReplyToAuthorName = strPtr("reply_to_author_id"), strPtr("reply_to_author_name")
	e.ChannelPrompt, e.ChannelContext, e.EditRevision = strPtr("channel_prompt"), strPtr("channel_context"), strPtr("edit_revision")
	if _, ok := d["media_urls"]; ok {
		e.MediaURLs = strList("media_urls")
	}
	if _, ok := d["media_types"]; ok {
		e.MediaTypes = strList("media_types")
	}
	if _, ok := d["media_filenames"]; ok {
		e.MediaFilenames = strList("media_filenames")
	}
	if items, ok := d["media_text_inlined"].([]any); ok {
		e.MediaTextInlined = make([]*bool, len(items))
		for i, item := range items {
			if b, ok := item.(bool); ok {
				e.MediaTextInlined[i] = &b
			}
		}
	}
	if n, ok := d["platform_update_id"].(int64); ok {
		e.PlatformUpdateID = &n
	}
	if b, ok := d["reply_expected"].(bool); ok {
		e.ReplyExpected = &b
	}
	e.RawMessage, e.AutoSkill = d["raw_message"], d["auto_skill"]
	e.PromptResponse = plainMap(d["prompt_response"])
	if m := plainMap(d["metadata"]); m != nil {
		e.Metadata = m
	}
	e.ReplyToIsOwnMessage, e.Internal = config.PyTruthy(d["reply_to_is_own_message"]), config.PyTruthy(d["internal"])
	e.InheritChannelPin = config.PyTruthy(d["inherit_channel_pin"])
	if v, ok := d["allow_gateway_control"]; ok {
		e.AllowGatewayControl = config.PyTruthy(v)
	}
	if ts := plainMap(d["timestamp"]); ts != nil {
		parsed, err := parseISO(config.PyStr(ts["__datetime__"]))
		if err != nil {
			return nil, err
		}
		e.Timestamp = parsed
	}
	return e, nil
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// Time of a Python isoformat string, naive values read as local time like datetime.now
func parseISO(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &gateway.PyError{Kind: "ValueError", Msg: fmt.Sprintf("Invalid isoformat string: %s", config.PyRepr(s))}
}
