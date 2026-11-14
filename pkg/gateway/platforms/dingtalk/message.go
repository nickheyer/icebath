package dingtalk

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// AttributeError an attribute read raises on a value of another type
func attributeError(v any, attr string) error {
	return &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)}
}

// Inbound chatbot message with the callback fields the adapter reads beside the parser fields
type IncomingMessage struct {
	ChatbotMessage
	IsInAtList                any
	SessionWebhook            any
	SenderNick                any
	RobotCode                 any
	SessionWebhookExpiredTime any
	MessageID                 any
	SenderID                  any
	ConversationID            any
	CreateAt                  any
	ConversationType          any
	ConversationTitle         any
	SenderStaffID             any
}

// Callback payload value required to be a JSON object, the AttributeError items raises otherwise
func requireDict(v any) (*pyjson.Object, error) {
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, attributeError(v, "items")
	}
	return obj, nil
}

// Value of a required payload key, the KeyError a missing one raises
func requireKey(d *pyjson.Object, key string) (any, error) {
	v, ok := d.Get(key)
	if !ok {
		return nil, &gateway.PyError{Kind: "KeyError", Msg: pyrepr.Repr(key)}
	}
	return v, nil
}

// Maps a callback payload onto a message the way the stream SDK does, unmapped names kept as extensions
func incomingMessageFromDict(data any) (*IncomingMessage, error) {
	d, err := requireDict(data)
	if err != nil {
		return nil, err
	}
	msg := &IncomingMessage{}
	msg.Extensions = map[string]any{}
	for _, name := range d.Keys {
		value := d.Vals[name]
		switch name {
		case "isInAtList":
			msg.IsInAtList = value
		case "sessionWebhook":
			msg.SessionWebhook = value
		case "senderNick":
			msg.SenderNick = value
		case "robotCode":
			msg.RobotCode = value
		case "sessionWebhookExpiredTime":
			n, err := pyint.Of(value)
			if err != nil {
				return nil, err
			}
			if n.IsInt64() {
				msg.SessionWebhookExpiredTime = n.Int64()
			} else {
				msg.SessionWebhookExpiredTime = n
			}
		case "msgId":
			msg.MessageID = value
		case "senderId":
			msg.SenderID = value
		case "conversationId":
			msg.ConversationID = value
		case "createAt":
			msg.CreateAt = value
		case "conversationType":
			msg.ConversationType = value
		case "conversationTitle":
			msg.ConversationTitle = value
		case "senderStaffId":
			msg.SenderStaffID = value
		case "chatbotUserId", "isAdmin", "chatbotCorpId", "senderCorpId":
		case "atUsers":
			users, ok := value.([]any)
			if !ok {
				return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(value))}
			}
			for _, user := range users {
				if _, err := requireDict(user); err != nil {
					return nil, err
				}
			}
		case "msgtype":
			kind, _ := value.(string)
			msg.MessageType = kind
			switch kind {
			case "text":
				raw, err := requireKey(d, "text")
				if err != nil {
					return nil, err
				}
				fields, err := requireDict(raw)
				if err != nil {
					return nil, err
				}
				content := &TextContent{}
				if text, ok := fields.Get("content"); ok {
					content.Content = pyrepr.Str(text)
					if text == nil {
						content.Content = ""
					}
				}
				msg.Text = content
			case "picture":
				raw, err := requireKey(d, "content")
				if err != nil {
					return nil, err
				}
				fields, err := requireDict(raw)
				if err != nil {
					return nil, err
				}
				image := &ImageContent{}
				if code, ok := fields.Get("downloadCode"); ok && code != nil {
					image.DownloadCode = pyrepr.Str(code)
				}
				msg.ImageContent = image
			case "richText":
				raw, err := requireKey(d, "content")
				if err != nil {
					return nil, err
				}
				fields, err := requireDict(raw)
				if err != nil {
					return nil, err
				}
				rich := &RichTextContent{RichTextList: []any{}}
				if list, ok := fields.Get("richText"); ok {
					rich.RichTextList = pyjson.Plain(list)
				}
				msg.RichTextContent = rich
			}
		case "hostingContext":
			fields, ok := value.(*pyjson.Object)
			if !ok {
				return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(value))}
			}
			for _, key := range []string{"userId", "nick"} {
				if _, err := requireKey(fields, key); err != nil {
					return nil, err
				}
			}
		case "conversationMsgContext":
			entries, ok := value.([]any)
			if !ok {
				return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(value))}
			}
			for _, entry := range entries {
				fields, ok := entry.(*pyjson.Object)
				if !ok {
					return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(entry))}
				}
				for _, key := range []string{"readStatus", "sendTime", "senderUserId"} {
					if _, err := requireKey(fields, key); err != nil {
						return nil, err
					}
				}
			}
		default:
			msg.Extensions[name] = pyjson.Plain(value)
		}
	}
	return msg, nil
}

// Value of a message field read with a default for an unset one, the getattr or fallback of the adapter
func orEmpty(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return ""
}
