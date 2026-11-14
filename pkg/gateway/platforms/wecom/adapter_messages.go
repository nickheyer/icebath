package wecom

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python repr of a decoded value for a percent r log argument
func reprOf(v any) string { return pyrepr.Repr(v) }

// Turns one message callback into a message event after dedup and the intake policy
func (a *WeComAdapter) onMessage(ctx context.Context, payload *pyjson.Object) error {
	body, ok := payload.Vals["body"].(*pyjson.Object)
	if !ok || body == nil {
		return nil
	}
	reqID := payloadReqID(payload)
	msgID := pyrepr.Str(pyrepr.Or(pyrepr.Or(body.Vals["msgid"], reqID), uuidHex()))
	sender := dictOrEmpty(body, "from")
	senderID := textOf(sender.Vals["userid"])
	if a.dedup.IsDuplicate(msgID) {
		var senderUser any
		if len(sender.Keys) > 0 {
			senderUser = sender.Vals["userid"]
		}
		slog.Info(fmt.Sprintf("[%s] Duplicate message %s ignored (dedup drop) req_id=%s sender=%s chattype=%s", a.Name(), msgID, reqID,
			reprOf(senderUser), reprOf(body.Vals["chattype"])))
		return nil
	}
	if reqID != "" {
		a.mu.Lock()
		gateway.BoundedPut(a.replyReqIDs, msgID, reqID, dedupMaxSize)
		a.mu.Unlock()
	}
	chatID := textOf(pyrepr.Or(body.Vals["chatid"], senderID))
	slog.Info(fmt.Sprintf("[%s] Inbound callback: chattype=%s chatid=%s sender=%s msgtype=%s has_chatid=%s", a.Name(), reprOf(body.Vals["chattype"]),
		reprOf(body.Vals["chatid"]), reprOf(senderID), reprOf(body.Vals["msgtype"]), pyrepr.Str(pyrepr.Truthy(body.Vals["chatid"]))))
	if chatID == "" {
		slog.Info(fmt.Sprintf("[%s] Missing chat id, skipping message; body_keys=%s", a.Name(), listRepr(body.Keys)))
		return nil
	}
	isGroup := lowerOf(body.Vals["chattype"]) == "group"
	admitted, err := a.admitInbound(ctx, isGroup, chatID, senderID)
	if err != nil || !admitted {
		return err
	}
	a.rememberChatReqID(chatID, reqID)
	text, replyText := extractText(body)
	if isGroup && text != "" {
		text = pystr.Strip(reSub(groupMentionRE, text, ""))
	}
	mediaURLs, mediaTypes, err := a.extractMedia(ctx, body)
	if err != nil {
		return err
	}
	messageType := deriveMessageType(body, text, mediaTypes)
	hasReplyContext := replyText != "" && (text != "" || len(mediaURLs) > 0)
	if replyText != "" && !hasReplyContext {
		text = replyText
	}
	if text == "" && len(mediaURLs) == 0 {
		slog.Info(fmt.Sprintf("[%s] Empty WeCom message skipped: is_group=%s chat=%s msgtype=%s", a.Name(), pyrepr.Str(isGroup), chatID, reprOf(body.Vals["msgtype"])))
		return nil
	}
	fields := gateway.SourceFields{ChatType: "dm", MessageID: &msgID}
	if isGroup {
		fields.ChatType = "group"
	}
	if senderID != "" {
		fields.UserID, fields.UserName = &senderID, &senderID
	}
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source = messageType, a.BuildSource(ctx, chatID, fields)
	event.RawMessage, event.MessageID = payload, &msgID
	event.MediaURLs, event.MediaTypes = mediaURLs, mediaTypes
	event.Timestamp = time.Now().UTC()
	if hasReplyContext {
		quoteID := "quote:" + msgID
		event.ReplyToMessageID, event.ReplyToText = &quoteID, &replyText
	}
	key, err := a.TextBatchKey(ctx, event)
	if err != nil {
		return err
	}
	hasPendingBatch := a.PendingTextBatch(key) != nil
	isAttachmentOnly := len(mediaURLs) > 0 && pystr.Strip(text) == ""
	delay, _ := a.TextBatchDelays()
	if (messageType == gateway.MessageTypeText && (delay > 0 || hasPendingBatch)) || (isAttachmentOnly && a.attachmentTextMergeDelaySeconds > 0) {
		return a.EnqueueTextEvent(ctx, event)
	}
	return a.HandleMessage(ctx, event)
}

// Applies the DM or group policy at intake, logging a dropped message
func (a *WeComAdapter) admitInbound(ctx context.Context, isGroup bool, chatID, senderID string) (bool, error) {
	if !isGroup {
		allowed, err := a.IsDMIntakeAllowed(ctx, senderID)
		if err != nil {
			return false, err
		}
		if !allowed {
			slog.Info(fmt.Sprintf("[%s] DM sender %s blocked by policy", a.Name(), senderID))
		}
		return allowed, nil
	}
	a.mu.Lock()
	a.groupChatIDs[chatID] = true
	a.mu.Unlock()
	allowed, err := a.IsGroupAllowed(ctx, chatID, senderID)
	if err != nil {
		return false, err
	}
	if !allowed {
		slog.Info(fmt.Sprintf("[%s] Group message DROPPED by policy: chat=%s sender=%s group_policy=%s (set group_policy to 'open' or add to group_allow_from to receive)",
			a.Name(), chatID, senderID, reprOf(a.GroupPolicy)))
	}
	return allowed, nil
}

// Buffers the event, real text joining a buffered attachment promoting it to text and lending its quote context
func (a *WeComAdapter) EnqueueTextEvent(ctx context.Context, event *gateway.MessageEvent) error {
	return a.RunLoopTurn(ctx, func(ctx context.Context) error {
		key, err := a.TextBatchKey(ctx, event)
		if err != nil {
			return err
		}
		existing := a.PendingTextBatch(key)
		if err := a.BasePlatformAdapter.EnqueueTextEvent(ctx, event); err != nil {
			return err
		}
		if existing != nil && pystr.Strip(event.Text) != "" {
			existing.MessageType = gateway.MessageTypeText
			if event.ReplyToText != nil && *event.ReplyToText != "" && (existing.ReplyToText == nil || *existing.ReplyToText == "") {
				existing.ReplyToText = event.ReplyToText
				existing.ReplyToMessageID = event.ReplyToMessageID
			}
		}
		return nil
	})
}

// Quiet period of a pending batch, an attachment only batch waiting for its text frame
func (a *WeComAdapter) TextBatchDelayFor(pending *gateway.MessageEvent) float64 {
	if pending != nil && len(pending.MediaURLs) > 0 && pystr.Strip(pending.Text) == "" {
		return a.attachmentTextMergeDelaySeconds
	}
	lastLen := 0
	if pending != nil {
		lastLen = pending.LastChunkLen()
	}
	delay, split := a.TextBatchDelays()
	if lastLen >= wecomSplitThreshold {
		return split
	}
	return delay
}

// Message text joined from its text parts and the quoted text or voice transcript it replies to
func extractText(body *pyjson.Object) (string, string) {
	msgtype := lowerOf(body.Vals["msgtype"])
	var textParts []string
	if msgtype == "mixed" {
		items, _ := dictOrEmpty(body, "mixed").Vals["msg_item"].([]any)
		for _, raw := range items {
			if item, ok := raw.(*pyjson.Object); ok && item != nil && lowerOf(item.Vals["msgtype"]) == "text" {
				textParts = append(textParts, contentOf(item, "text"))
			}
		}
	} else {
		textParts = append(textParts, contentOf(body, "text"))
		if msgtype == "voice" {
			textParts = append(textParts, contentOf(body, "voice"))
		}
		if msgtype == "appmsg" {
			textParts = append(textParts, textOf(dictOrEmpty(body, "appmsg").Vals["title"]))
		}
	}
	quote := dictOrEmpty(body, "quote")
	replyText := ""
	if quoteType := lowerOf(quote.Vals["msgtype"]); quoteType == "text" || quoteType == "voice" {
		replyText = contentOf(quote, quoteType)
	}
	var parts []string
	for _, part := range textParts {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return pystr.Strip(strings.Join(parts, "\n")), replyText
}

// Message type from the cached media types, the text and the declared msgtype
func deriveMessageType(body *pyjson.Object, text string, mediaTypes []string) gateway.MessageType {
	hasImage := false
	for _, mtype := range mediaTypes {
		if strings.HasPrefix(mtype, "application/") || strings.HasPrefix(mtype, "text/") {
			return gateway.MessageTypeDocument
		}
		hasImage = hasImage || strings.HasPrefix(mtype, "image/")
	}
	if hasImage {
		if text != "" {
			return gateway.MessageTypeText
		}
		return gateway.MessageTypePhoto
	}
	if lowerOf(body.Vals["msgtype"]) == "voice" {
		return gateway.MessageTypeVoice
	}
	return gateway.MessageTypeText
}

// Chat level group policy narrowed by the per group sender allowlist
func (a *WeComAdapter) IsGroupAllowed(ctx context.Context, chatID, senderID string) (bool, error) {
	allowed, err := a.OwnAccessPolicyMixin.IsGroupAllowed(ctx, chatID, senderID)
	if err != nil || !allowed {
		return false, err
	}
	groupCfg := a.resolveGroupCfg(chatID)
	senderAllow := coerceList(pyrepr.Or(groupCfg["allow_from"], groupCfg["allowFrom"]))
	if len(senderAllow) == 0 {
		return true, nil
	}
	return entriesMatch(senderAllow, senderID), nil
}

// Group config by exact key, then case insensitive key, then the star entry, only dict values counting
func (a *WeComAdapter) resolveGroupCfg(chatID string) map[string]any {
	if cfg, ok := a.groups[chatID].(map[string]any); ok {
		return cfg
	}
	lowered := pystr.Lower(chatID)
	keys := make([]string, 0, len(a.groups))
	for k := range a.groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if cfg, ok := a.groups[k].(map[string]any); ok && pystr.Lower(k) == lowered {
			return cfg
		}
	}
	if cfg, ok := a.groups["*"].(map[string]any); ok {
		return cfg
	}
	return map[string]any{}
}

// Caches the latest inbound req id of a chat, a fresh one reviving its stream channel
func (a *WeComAdapter) rememberChatReqID(chatID, reqID string) {
	chatID, reqID = pystr.Strip(chatID), pystr.Strip(reqID)
	if chatID == "" || reqID == "" {
		return
	}
	a.mu.Lock()
	gateway.BoundedPut(a.lastChatReqIDs, chatID, reqID, dedupMaxSize)
	a.mu.Unlock()
	a.stream.clearExpiredChat(chatID)
}

// Inbound req id recorded for a replied message id, empty for a blank or quote reference
func (a *WeComAdapter) ReplyReqIDForMessage(replyTo string) string {
	normalized := pystr.Strip(replyTo)
	if normalized == "" || strings.HasPrefix(normalized, "quote:") {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	reqID, _ := a.replyReqIDs.Get(normalized)
	return reqID
}

// Last inbound req id of a chat, empty when none is cached
func (a *WeComAdapter) LastChatReqID(chatID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	reqID, _ := a.lastChatReqIDs.Get(chatID)
	return reqID
}

// Explicit reply mapping, else the last inbound req id of the chat
func (a *WeComAdapter) cachedReplyReqID(chatID, replyTo string) string {
	if reqID := a.ReplyReqIDForMessage(replyTo); reqID != "" {
		return reqID
	}
	return a.LastChatReqID(chatID)
}

// Drops the req ids bound to a lost subscription without closing the websocket
func (a *WeComAdapter) forceReconnectOnStaleSubscription(errcode int) {
	if errcode != StreamNotSubscribedErrcode {
		return
	}
	slog.Warn(fmt.Sprintf("[%s] Got errcode %d (subscription lost) — clearing stale state", a.Name(), errcode))
	a.mu.Lock()
	a.lastChatReqIDs.Clear()
	a.replyReqIDs.Clear()
	a.mu.Unlock()
}

// Error text of a response carrying a non zero errcode, empty otherwise
func responseError(response *pyjson.Object) string {
	errcode := getOr(response, "errcode", int64(0))
	if errcode == nil || pyjson.Equal(errcode, int64(0)) {
		return ""
	}
	return fmt.Sprintf("WeCom errcode %s: %s", pyrepr.Str(errcode), pyrepr.Str(pyrepr.Or(response.Vals["errmsg"], "unknown error")))
}

// RuntimeError naming the operation when the response carries an error
func (a *WeComAdapter) RaiseForWeComError(response *pyjson.Object, operation string) error {
	if errText := responseError(response); errText != "" {
		return runtimeError(fmt.Sprintf("%s failed: %s", operation, errText))
	}
	return nil
}

// Markdown message body capped at the message length limit
func markdownBody(content string) []any {
	return []any{"msgtype", "markdown", "markdown", obj("content", pystr.Slice(content, 0, maxMessageLength))}
}

// Sends markdown as a passive reply to an inbound req id, an error for a refused reply
func (a *WeComAdapter) sendReplyMarkdown(ctx context.Context, replyReqID, content string) (*pyjson.Object, error) {
	response, err := a.sendReplyRequest(ctx, replyReqID, obj(markdownBody(content)...))
	if err != nil {
		return nil, err
	}
	if err := a.RaiseForWeComError(response, "send reply markdown"); err != nil {
		return nil, err
	}
	return response, nil
}

// Sends markdown proactively to a chat
func (a *WeComAdapter) sendProactiveMarkdown(ctx context.Context, chatID, content string) (*pyjson.Object, error) {
	return a.sendRequest(ctx, appCmdSend, obj(append([]any{"chatid", chatID}, markdownBody(content)...)...), requestTimeoutSeconds)
}

// Queues a send on the per chat lane of the live send queue
func (a *WeComAdapter) EnqueueChatSend(ctx context.Context, chatID string, send func(ctx context.Context) (any, error), isControl bool) (any, error) {
	a.mu.Lock()
	queue := a.queue
	a.mu.Unlock()
	return queue.EnqueueChatSend(ctx, chatID, send, isControl)
}

// Sends standalone markdown chunk by chunk through the per chat queue, approval prompts on the control lane
func (a *WeComAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if chatID == "" {
		return gateway.SendResult{Success: false, Error: "chat_id is required"}, nil
	}
	isControl := pyrepr.Truthy(metadata["is_approval_prompt"])
	forceProactive := pyrepr.Truthy(metadata["force_proactive_send"])
	delete(metadata, "is_approval_prompt")
	delete(metadata, "force_proactive_send")
	chunks := gateway.TruncateMessage(content, maxMessageLength, nil)
	result, err := gateway.SendChunks(ctx, chunks, func(ctx context.Context, chunk string) (*gateway.SendResult, error) {
		value, err := a.EnqueueChatSend(ctx, chatID, func(sctx context.Context) (any, error) {
			return a.sendInner(sctx, chatID, chunk, replyTo, forceProactive)
		}, isControl)
		if err != nil {
			return nil, err
		}
		sent := value.(gateway.SendResult)
		return &sent, nil
	})
	if err != nil || result == nil {
		return gateway.SendResult{}, err
	}
	return *result, nil
}

// Sends one chunk as a passive reply when a req id is usable and proactively otherwise
func (a *WeComAdapter) sendInner(ctx context.Context, chatID, content, replyTo string, forceProactive bool) (gateway.SendResult, error) {
	a.mu.Lock()
	isGroupChat := a.groupChatIDs[chatID]
	a.mu.Unlock()
	replyReqID := ""
	if !forceProactive || isGroupChat {
		replyReqID = a.cachedReplyReqID(chatID, replyTo)
	}
	var response *pyjson.Object
	var err error
	switch {
	case replyReqID != "":
		if response, err = a.sendReplyMarkdown(ctx, replyReqID, content); err != nil && (isTimeout(err) || isRuntimeError(err)) {
			slog.Warn(fmt.Sprintf("[%s] Passive reply failed (%s), falling back to proactive send", a.Name(), errText(err)))
			response, err = a.sendProactiveMarkdown(ctx, chatID, content)
		}
	case isGroupChat:
		slog.Warn(fmt.Sprintf("[%s] No cached req_id for group chat %s — cannot send (groups require passive reply via req_id)", a.Name(), chatID))
		return gateway.SendResult{Success: false, Error: "No req_id available for group chat (passive reply required)"}, nil
	default:
		response, err = a.sendProactiveMarkdown(ctx, chatID, content)
	}
	switch {
	case err != nil && ctx.Err() != nil:
		return gateway.SendResult{}, ctx.Err()
	case isTimeout(err):
		return gateway.SendResult{Success: false, Error: "Timeout sending message to WeCom"}, nil
	case err != nil:
		slog.Error(fmt.Sprintf("[%s] Send failed: %s", a.Name(), errText(err)))
		return a.sendFailure(errText(err), strings.Contains(errText(err), fmt.Sprint(StreamNotSubscribedErrcode))), nil
	}
	if failure := responseError(response); failure != "" {
		return a.sendFailure(failure, pyjson.Equal(getOr(response, "errcode", int64(0)), int64(StreamNotSubscribedErrcode))), nil
	}
	var messageID any = payloadReqID(response)
	if messageID == "" {
		messageID = uuidHex()[:12]
	}
	return gateway.SendResult{Success: true, MessageID: messageID, RawResponse: response}, nil
}

// Failed send result, a lost subscription scheduling the purge of the stale req ids
func (a *WeComAdapter) sendFailure(failure string, subscriptionLost bool) gateway.SendResult {
	if subscriptionLost {
		go a.forceReconnectOnStaleSubscription(StreamNotSubscribedErrcode)
	}
	return gateway.SendResult{Success: false, Error: failure}
}

// Name and type of a chat, a group for an id starting with group
func (a *WeComAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if chatID != "" && strings.HasPrefix(pystr.Lower(chatID), "group") {
		chatType = "group"
	}
	return map[string]any{"name": chatID, "type": chatType}, nil
}

// Native stream frames are available in DMs and groups alike
func (a *WeComAdapter) SupportsNativeStreaming(chatType string, metadata map[string]any) bool {
	return a.stream.SupportsNativeStreaming(chatType, metadata)
}

// Seeds, updates or finalizes the stream turn of a chat, false when the caller must send normally
func (a *WeComAdapter) SendStreamFrame(ctx context.Context, text string, finalize bool, chatID, replyTo, turnID string) (bool, error) {
	return a.stream.SendStreamFrame(ctx, text, finalize, chatID, replyTo, turnID)
}

// Typing indicator, a no op since the seed stream frame already shows typing
func (a *WeComAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return a.stream.SendTyping(ctx, chatID, metadata)
}
