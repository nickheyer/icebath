package bluebubbles

import (
	"context"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Whether an associatedMessageType is a tapback code, 2000 to 2005 added and 3000 to 3005 removed
func isTapback(v any) bool {
	code, ok := anyutil.Int(v)
	return ok && ((code >= 2000 && code <= 2005) || (code >= 3000 && code <= 3005))
}

// Message record of a webhook payload, the data dict, the first dict in a data list, message or the payload
func extractPayloadRecord(payload map[string]any) map[string]any {
	data := payload["data"]
	if m, ok := jsonutil.AsMap(data); ok {
		return m
	}
	if items, ok := data.([]any); ok {
		for _, item := range items {
			if m, isDict := jsonutil.AsMap(item); isDict {
				if len(m) > 0 {
					return m
				}
				break
			}
		}
	}
	if m, ok := jsonutil.AsMap(payload["message"]); ok {
		return m
	}
	return payload
}

// First candidate that is a string with text, stripped, empty when none is
func valueOf(candidates ...any) string {
	for _, c := range candidates {
		if s, ok := c.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

// Webhook body as JSON, else a form with a JSON payload, data or message field
func parseWebhookBody(raw []byte) (any, error) {
	body := strings.ToValidUTF8(string(raw), "\uFFFD")
	if v, err := jsonutil.Decode(body); err == nil {
		return v, nil
	}
	pairs := urlutil.QueryPairs(body)
	payloadStr := ""
	for _, key := range []string{"payload", "data", "message"} {
		for _, kv := range pairs {
			if kv[0] == key && kv[1] != "" {
				payloadStr = kv[1]
				break
			}
		}
		if payloadStr != "" {
			break
		}
	}
	if payloadStr == "" {
		return map[string]any{}, nil
	}
	return jsonutil.Decode(payloadStr)
}

// Objects of a list payload field, nothing for a falsy value, a kind error for anything but a list of objects
func iterDicts(v any) ([]map[string]any, error) {
	if !anyutil.IsTrue(v) {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, kindError("an array", v)
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m, err := dictOf(item)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Downloads inbound attachments and returns their cached paths, mime types and the message type
func (a *BlueBubblesAdapter) collectAttachments(ctx context.Context, record map[string]any) ([]string, []string, gateway.MessageType, error) {
	mediaURLs, mediaTypes := []string{}, []string{}
	msgType := gateway.MessageTypeText
	atts, err := iterDicts(record["attachments"])
	if err != nil {
		return nil, nil, msgType, err
	}
	for _, att := range atts {
		attGUID := textOf(getOr(att, "guid", ""))
		cached := ""
		if attGUID != "" {
			if cached, err = a.downloadAttachment(ctx, attGUID, att); err != nil {
				return nil, nil, msgType, err
			}
		}
		if cached == "" {
			continue
		}
		mime, err := lowerText(att["mimeType"])
		if err != nil {
			return nil, nil, msgType, err
		}
		mediaURLs = append(mediaURLs, cached)
		mediaTypes = append(mediaTypes, mime)
		uti := textOf(att["uti"])
		isVoice := strings.HasPrefix(mime, "audio/") || strings.HasSuffix(uti, "caf")
		switch {
		case strings.HasPrefix(mime, "image/"):
			msgType = gateway.MessageTypePhoto
		case isVoice:
			msgType = gateway.MessageTypeVoice
		case strings.HasPrefix(mime, "video/"):
			msgType = gateway.MessageTypeVideo
		default:
			msgType = gateway.MessageTypeDocument
		}
	}
	if len(mediaURLs) > 1 {
		for _, m := range mediaTypes {
			if strings.SplitN(m, "/", 2)[0] == "image" {
				msgType = gateway.MessageTypePhoto
				break
			}
		}
	}
	return mediaURLs, mediaTypes, msgType, nil
}

// Auth value a callback carries in the password or guid query parameter or headers, false when absent
func webhookToken(request *gateway.WebRequest) (string, bool) {
	q := request.Query()
	for _, key := range []string{"password", "guid"} {
		if v := q.Get(key); v != "" {
			return v, true
		}
	}
	for _, key := range []string{"x-password", "x-guid", "x-bluebubbles-guid"} {
		if v := request.Header.Get(key); v != "" {
			return v, true
		}
	}
	return "", false
}

// First element of a chats list when it is an object, nothing for a falsy value or text, a kind error for any other value
func firstChat(chats any) (map[string]any, error) {
	if !anyutil.IsTrue(chats) {
		return nil, nil
	}
	switch c := chats.(type) {
	case []any:
		m, _ := jsonutil.AsMap(c[0])
		return m, nil
	case string:
		return nil, nil
	}
	return nil, kindError("an array", chats)
}

// Chat GUID, chat identifier and sender read from the many BlueBubbles payload shapes
func resolveChatAndSender(payload, record map[string]any) (string, string, string, error) {
	chatGUID := valueOf(record["chatGuid"], payload["chatGuid"], record["chat_guid"], payload["chat_guid"], payload["guid"])
	if chatGUID == "" {
		chat, err := firstChat(record["chats"])
		if err != nil {
			return "", "", "", err
		}
		if chat != nil {
			chatGUID = textOf(anyutil.Or(chat["guid"], chat["chatGuid"]))
		}
	}
	chatIdentifier := valueOf(record["chatIdentifier"], record["identifier"], payload["chatIdentifier"], payload["identifier"])
	var handleAddr any
	if handle, ok := jsonutil.AsMap(record["handle"]); ok {
		handleAddr = handle["address"]
	}
	sender := valueOf(handleAddr, record["sender"], record["from"], record["address"])
	if sender == "" {
		sender = chatIdentifier
	}
	if sender == "" {
		sender = chatGUID
	}
	if chatGUID == "" && chatIdentifier == "" && sender != "" {
		chatIdentifier = sender
	}
	return chatGUID, chatIdentifier, sender, nil
}

// Authenticates a server callback, normalises the message and dispatches it with a read receipt
func (a *BlueBubblesAdapter) handleWebhook(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if token, ok := webhookToken(request); !ok || token != a.password {
		return gateway.WebJSONResponse(jsonutil.ObjectOf("error", "unauthorized"), http.StatusUnauthorized, nil)
	}
	raw, err := request.Read()
	var payload any
	if err == nil {
		payload, err = parseWebhookBody(raw)
	}
	if err != nil {
		pkgLog().Error("[bluebubbles] webhook parse error: " + err.Error())
		return gateway.WebJSONResponse(jsonutil.ObjectOf("error", "invalid payload"), http.StatusBadRequest, nil)
	}
	pm, err := dictOf(payload)
	if err != nil {
		return nil, err
	}
	if eventType := valueOf(pm["type"], pm["event"]); eventType != "" && !messageEvents[eventType] {
		return okResponse(), nil
	}
	record := extractPayloadRecord(pm)
	if anyutil.IsTrue(record["isFromMe"]) || anyutil.IsTrue(record["fromMe"]) || anyutil.IsTrue(record["is_from_me"]) {
		return okResponse(), nil
	}
	if isTapback(record["associatedMessageType"]) {
		return okResponse(), nil
	}
	text := valueOf(record["text"], record["message"], record["body"])
	chatGUID, chatIdentifier, sender, err := resolveChatAndSender(pm, record)
	if err != nil {
		return nil, err
	}
	sessionChatID := chatGUID
	if sessionChatID == "" {
		sessionChatID = chatIdentifier
	}
	isGroup := anyutil.IsTrue(record["isGroup"]) || strings.Contains(chatGUID, ";+;")
	if isGroup && a.requireMention {
		if !a.messageMatchesMentionPatterns(text) {
			pkgLog().Debug("[bluebubbles] ignoring group message (require_mention=true, no mention pattern matched)")
			return okResponse(), nil
		}
		text = a.cleanMentionText(text)
	}
	mediaURLs, mediaTypes, msgType, err := a.collectAttachments(ctx, record)
	if err != nil {
		return nil, err
	}
	if text == "" && len(mediaURLs) > 0 {
		text = "(attachment)"
	}
	if sender == "" || (chatGUID == "" && chatIdentifier == "") || text == "" {
		return gateway.WebJSONResponse(jsonutil.ObjectOf("error", "missing message fields"), http.StatusBadRequest, nil)
	}
	chatType := "dm"
	if isGroup {
		chatType = "group"
	}
	chatName := chatIdentifier
	if chatName == "" {
		chatName = sender
	}
	source := a.BuildSource(ctx, sessionChatID, gateway.SourceFields{ChatName: &chatName, ChatType: chatType,
		UserID: &sender, UserName: &sender, ChatIDAlt: optional(chatIdentifier)})
	event := gateway.NewMessageEvent(text)
	event.MessageType = msgType
	event.Source = source
	event.RawMessage = payload
	event.MessageID = optional(valueOf(record["guid"], record["messageGuid"], record["id"]))
	event.ReplyToMessageID = optional(valueOf(record["threadOriginatorGuid"], record["associatedMessageGuid"]))
	event.MediaURLs, event.MediaTypes = mediaURLs, mediaTypes
	a.TrackBackgroundTask(gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error { return a.handleMessage(tctx, event) }))
	if a.sendReadReceipts && sessionChatID != "" {
		gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
			a.MarkRead(tctx, sessionChatID)
			return nil
		})
	}
	return okResponse(), nil
}
