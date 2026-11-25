package line

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	mimeutil "github.com/nickheyer/icebath/pkg/utils/mime"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Inbound media kinds and the extension their bytes are cached under
var inboundMediaExt = map[string]string{"image": ".jpg", "audio": ".m4a", "video": ".mp4", "file": ".bin"}

// Lifecycle event types that are only logged
var lifecycleEvents = map[string]bool{"follow": true, "unfollow": true, "join": true, "leave": true}

// Health probe answer for tunnels and proxies
func (a *LineAdapter) handleHealth(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	return gateway.WebJSONResponse(jsonutil.ObjectOf("status", "ok", "platform", "line"), 200, nil)
}

// Verifies the signature of a capped body and dispatches each event, answering 200 ok
func (a *LineAdapter) handleWebhook(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	body, err := request.Read()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("LINE: read failed: %v", err))
		return gateway.WebTextResponse(400, "bad request", "", nil), nil
	}
	if len(body) > webhookBodyMaxBytes {
		return gateway.WebTextResponse(413, "payload too large", "", nil), nil
	}
	if !verifyLineSignature(body, request.Header.Get("X-Line-Signature"), a.channelSecret) {
		return gateway.WebTextResponse(401, "invalid signature", "", nil), nil
	}
	text, err := textutil.DecodeUTF8(body)
	if err != nil {
		return gateway.WebTextResponse(400, "bad json", "", nil), nil
	}
	decoded, err := jsonutil.Decode(text)
	if err != nil {
		return gateway.WebTextResponse(400, "bad json", "", nil), nil
	}
	payload, ok := jsonutil.Plain(decoded).(map[string]any)
	if !ok {
		return nil, &errorutil.Error{Kind: "AttributeError", Msg: fmt.Sprintf("webhook body must be a JSON object, got %s", anyutil.Kind(decoded))}
	}
	events, err := iterEvents(payload["events"])
	if err != nil {
		return nil, err
	}
	dispatchCtx := context.WithoutCancel(ctx)
	for _, event := range events {
		obj, isObj := event.(map[string]any)
		if !isObj {
			pkgLog().Error(fmt.Sprintf("LINE: dispatch_event failed: an event is %s, expected a mapping", errchain.Kind(event)))
			continue
		}
		if err := a.dispatchEvent(dispatchCtx, obj); err != nil {
			pkgLog().Error(fmt.Sprintf("LINE: dispatch_event failed\n%v", err))
		}
	}
	return gateway.WebTextResponse(200, "ok", "", nil), nil
}

// Items the events value iterates over, a falsy value as none and the TypeError for a non iterable
func iterEvents(v any) ([]any, error) {
	if !anyutil.IsTrue(v) {
		return nil, nil
	}
	switch t := v.(type) {
	case []any:
		return t, nil
	case map[string]any:
		out := make([]any, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		return out, nil
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, &errorutil.Error{Kind: "TypeError", Msg: fmt.Sprintf("expected a list, got %s", anyutil.Kind(v))}
}

// Drops redeliveries, self echoes and unauthorized sources then routes message and postback events
func (a *LineAdapter) dispatchEvent(ctx context.Context, event map[string]any) error {
	eventType := event["type"]
	source := objField(event, "source")
	webhookEventID := ""
	if v := event["webhookEventId"]; anyutil.IsTrue(v) {
		webhookEventID = anyutil.String(v)
	}
	if webhookEventID != "" && a.dedup.IsDuplicate(webhookEventID) {
		pkgLog().Debug(fmt.Sprintf("LINE: ignoring duplicate webhook event %s", webhookEventID))
		return nil
	}
	a.mu.Lock()
	botUserID := a.botUserID
	a.mu.Unlock()
	if botUserID != "" && strField(source, "userId") == botUserID {
		return nil
	}
	if !allowedForSource(source, a.allowAll, a.allowedUsers, a.allowedGroups, a.allowedRooms) {
		pkgLog().Info(fmt.Sprintf("LINE: rejecting unauthorized source %s", anyutil.Quote(source)))
		return nil
	}
	typ, _ := eventType.(string)
	switch {
	case typ == "message":
		return a.handleMessageEvent(ctx, event)
	case typ == "postback":
		return a.handlePostbackEvent(ctx, event)
	case lifecycleEvents[typ]:
		pkgLog().Info(fmt.Sprintf("LINE: lifecycle event %s from %s", typ, anyutil.Quote(source)))
	default:
		pkgLog().Debug(fmt.Sprintf("LINE: ignoring event type %s", anyutil.Quote(eventType)))
	}
	return nil
}

// Python str of a dict value with a default for a missing key
func getStr(m map[string]any, key, def string) string {
	v, ok := m[key]
	if !ok {
		return def
	}
	return anyutil.String(v)
}

// Stashes the reply token, normalizes text, media, sticker and location messages and hands them to the gateway
func (a *LineAdapter) handleMessageEvent(ctx context.Context, event map[string]any) error {
	msg := objField(event, "message")
	msgType, messageID := strField(msg, "type"), strField(msg, "id")
	replyTok := strField(event, "replyToken")
	source := objField(event, "source")
	chatID, chatType := resolveChat(source)
	userID := strField(source, "userId")
	if userID == "" {
		userID = chatID
	}
	if chatID != "" && replyTok != "" {
		a.mu.Lock()
		a.replyTokens[chatID] = replyToken{token: replyTok, expiresAt: nowSeconds() + lineReplyTokenTTLSeconds}
		a.mu.Unlock()
	}
	mediaURLs, mediaTypes := []string{}, []string{}
	var text string
	switch {
	case msgType == "text":
		if v := msg["text"]; anyutil.IsTrue(v) {
			text = anyutil.String(v)
		}
	case inboundMediaExt[msgType] != "":
		filename := ""
		if v := msg["fileName"]; anyutil.IsTrue(v) {
			filename = anyutil.String(v)
		} else if v := msg["file_name"]; anyutil.IsTrue(v) {
			filename = anyutil.String(v)
		}
		localPath, mediaType := a.downloadMedia(ctx, messageID, msgType, filename)
		if localPath != "" {
			mediaURLs, mediaTypes = []string{localPath}, []string{mediaType}
		}
		text = "[" + msgType + "]"
	case msgType == "sticker":
		text = "[sticker]"
		if keywords := msg["keywords"]; anyutil.IsTrue(keywords) {
			words, err := joinStrings(keywords)
			if err != nil {
				return err
			}
			text = "[sticker: " + words + "]"
		}
	case msgType == "location":
		text = strings.TrimSpace("[location: " + getStr(msg, "title", "") + " " + getStr(msg, "address", "") + "]")
	default:
		text = "[unsupported message type: " + msgType + "]"
	}
	if client := a.currentClient(); chatType == "dm" && client != nil {
		go client.loading(context.WithoutCancel(ctx), chatID, 60)
	}
	source2 := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatType: chatType, UserID: &userID, UserName: &userID,
		ChatName: &chatID, MessageID: &messageID})
	messageType, ok := lineMessageTypes[msgType]
	if !ok {
		messageType = gateway.MessageTypeText
	}
	ev := gateway.NewMessageEvent(text)
	ev.MessageType = messageType
	ev.Source = source2
	ev.RawMessage = event
	ev.MessageID = &messageID
	ev.MediaURLs, ev.MediaTypes = mediaURLs, mediaTypes
	return a.HandleMessage(ctx, ev)
}

// Comma joined string list, a TypeError for an item that is not text
func joinStrings(v any) (string, error) {
	items, ok := v.([]any)
	if !ok {
		if s, isStr := v.(string); isStr {
			return strings.Join(strings.Split(s, ""), ", "), nil
		}
		return "", &errorutil.Error{Kind: "TypeError", Msg: "can only join an iterable"}
	}
	parts := make([]string, 0, len(items))
	for i, item := range items {
		s, isStr := item.(string)
		if !isStr {
			return "", &errorutil.Error{Kind: "TypeError", Msg: fmt.Sprintf("item %d must be text, got %s", i, anyutil.Kind(item))}
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", "), nil
}

// Delivers the cached answer for a postback tap, settling ready and error entries as delivered
func (a *LineAdapter) handlePostbackEvent(ctx context.Context, event map[string]any) error {
	replyTok := strField(event, "replyToken")
	chatID, _ := resolveChat(objField(event, "source"))
	var data any = ""
	if v := objField(event, "postback")["data"]; anyutil.IsTrue(v) {
		data = v
	}
	raw, isStr := data.(string)
	if !isStr {
		return nil
	}
	decoded, err := jsonutil.Decode(raw)
	if err != nil {
		return nil
	}
	parsed, ok := jsonutil.Plain(decoded).(map[string]any)
	if !ok {
		return &errorutil.Error{Kind: "AttributeError", Msg: fmt.Sprintf("postback data must be a JSON object, got %s", anyutil.Kind(decoded))}
	}
	requestID := ""
	if action, _ := parsed["action"].(string); action == "show_response" {
		requestID, _ = parsed["request_id"].(string)
	}
	client := a.currentClient()
	a.mu.Lock()
	var entry *cacheEntry
	if requestID != "" {
		entry = a.cache.Get(requestID)
	}
	if client == nil || replyTok == "" {
		a.mu.Unlock()
		return nil
	}
	if entry == nil {
		if requestID != "" && a.pendingButtons[chatID] == requestID {
			delete(a.pendingButtons, chatID)
		}
		a.mu.Unlock()
		if err := client.reply(ctx, replyTok, []messagingMessage{textMessage(a.expiredText)}); err != nil {
			pkgLog().Debug(fmt.Sprintf("LINE: postback expired-notice reply failed: %v", err))
		}
		return nil
	}
	state, payload := entry.state, entry.payload
	a.mu.Unlock()
	var messages []messagingMessage
	switch state {
	case StateReady:
		messages = textMessages(payloadText(payload, ""))
	case StateError:
		messages = []messagingMessage{textMessage(payloadText(payload, a.interruptedText))}
	case StateDelivered:
		messages = []messagingMessage{textMessage(a.deliveredText)}
	default:
		messages = []messagingMessage{textMessage(a.pendingText)}
	}
	if err := client.reply(ctx, replyTok, messages); err != nil {
		if state != StateReady {
			if state == StateError {
				pkgLog().Warn(fmt.Sprintf("LINE: postback ERROR reply failed: %v", err))
			}
			return nil
		}
		pkgLog().Warn(fmt.Sprintf("LINE: postback reply failed (%v); falling back to push", err))
		if err2 := client.push(ctx, chatID, messages); err2 != nil {
			pkgLog().Error(fmt.Sprintf("LINE: postback push fallback failed: %v", err2))
			return nil
		}
	}
	if state == StateReady || state == StateError {
		a.mu.Lock()
		a.cache.MarkDelivered(requestID)
		delete(a.pendingButtons, chatID)
		a.mu.Unlock()
	}
	return nil
}

// Python str of a cached payload, the fallback when it is falsy
func payloadText(payload any, fallback string) string {
	if !anyutil.IsTrue(payload) {
		return fallback
	}
	return anyutil.String(payload)
}

// Fetches inbound message content and caches it, returning the local path and its MIME type
func (a *LineAdapter) downloadMedia(ctx context.Context, messageID, msgType, filename string) (string, string) {
	client := a.currentClient()
	if client == nil || messageID == "" {
		return "", ""
	}
	data, err := client.fetchContent(ctx, messageID)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("LINE: failed to fetch %s content for %s: %v", msgType, messageID, err))
		return "", ""
	}
	ext, ok := inboundMediaExt[msgType]
	if !ok {
		ext = ".bin"
	}
	var path, mime string
	switch msgType {
	case "image":
		path, err = gateway.CacheImageFromBytesAsync(ctx, data, ext)
		mime = "image/jpeg"
	case "audio", "video":
		if msgType == "audio" {
			path, err = gateway.CacheAudioFromBytesAsync(ctx, data, ext)
		} else {
			path, err = gateway.CacheVideoFromBytesAsync(ctx, data, ext)
		}
		mime = mimeutil.TypeByExtension(ext)
		if mime == "" {
			mime = msgType + "/mp4"
		}
	default:
		documentName := filename
		if documentName == "" {
			documentName = "line_file" + ext
		}
		mime = mimeutil.TypeByName(documentName)
		if mime == "" {
			mime = "application/octet-stream"
		}
		path, err = gateway.CacheDocumentFromBytesAsync(ctx, data, documentName)
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("LINE: failed to cache %s payload: %v", msgType, err))
		return "", ""
	}
	return path, mime
}
