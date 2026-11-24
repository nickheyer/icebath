package dingtalk

import (
	"fmt"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Failure for a payload value of the wrong JSON kind, naming the value read and the kind expected
func kindError(what, want string, v any) error {
	return fmt.Errorf("%s: expected %s, got %s", what, want, anyutil.Kind(v))
}

// Failure for a key a payload object must carry
func missingKey(what, key string) error {
	return fmt.Errorf("%s: missing key %q", what, key)
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

// Callback payload value required to be a JSON object, a kind error naming what was read otherwise
func requireDict(what string, v any) (*jsonutil.Object, error) {
	obj, ok := v.(*jsonutil.Object)
	if !ok {
		return nil, kindError(what, "an object", v)
	}
	return obj, nil
}

// Value of a key the payload object named by what must carry, a missing key error otherwise
func requireKey(what string, d *jsonutil.Object, key string) (any, error) {
	v, ok := d.Get(key)
	if !ok {
		return nil, missingKey(what, key)
	}
	return v, nil
}

// Maps a callback payload onto a message the way the stream SDK does, unmapped names kept as extensions
func incomingMessageFromDict(data any) (*IncomingMessage, error) {
	d, err := requireDict("callback data", data)
	if err != nil {
		return nil, err
	}
	msg := &IncomingMessage{}
	msg.Extensions = map[string]any{}
	for _, name := range d.Keys() {
		value := d.Value(name)
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
			n, err := anyutil.ToInt(value)
			if err != nil {
				return nil, err
			}
			msg.SessionWebhookExpiredTime = n
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
				return nil, kindError("atUsers", "an array", value)
			}
			for _, user := range users {
				if _, err := requireDict("atUsers entry", user); err != nil {
					return nil, err
				}
			}
		case "msgtype":
			kind, _ := value.(string)
			msg.MessageType = kind
			switch kind {
			case "text":
				raw, err := requireKey("callback data", d, "text")
				if err != nil {
					return nil, err
				}
				fields, err := requireDict("text", raw)
				if err != nil {
					return nil, err
				}
				content := &TextContent{}
				if text, ok := fields.Get("content"); ok {
					content.Content = anyutil.String(text)
					if text == nil {
						content.Content = ""
					}
				}
				msg.Text = content
			case "picture":
				raw, err := requireKey("callback data", d, "content")
				if err != nil {
					return nil, err
				}
				fields, err := requireDict("content", raw)
				if err != nil {
					return nil, err
				}
				image := &ImageContent{}
				if code, ok := fields.Get("downloadCode"); ok && code != nil {
					image.DownloadCode = anyutil.String(code)
				}
				msg.ImageContent = image
			case "richText":
				raw, err := requireKey("callback data", d, "content")
				if err != nil {
					return nil, err
				}
				fields, err := requireDict("content", raw)
				if err != nil {
					return nil, err
				}
				rich := &RichTextContent{RichTextList: []any{}}
				if list, ok := fields.Get("richText"); ok {
					rich.RichTextList = jsonutil.Plain(list)
				}
				msg.RichTextContent = rich
			}
		case "hostingContext":
			fields, err := requireDict("hostingContext", value)
			if err != nil {
				return nil, err
			}
			for _, key := range []string{"userId", "nick"} {
				if _, err := requireKey("hostingContext", fields, key); err != nil {
					return nil, err
				}
			}
		case "conversationMsgContext":
			entries, ok := value.([]any)
			if !ok {
				return nil, kindError("conversationMsgContext", "an array", value)
			}
			for _, entry := range entries {
				fields, err := requireDict("conversationMsgContext entry", entry)
				if err != nil {
					return nil, err
				}
				for _, key := range []string{"readStatus", "sendTime", "senderUserId"} {
					if _, err := requireKey("conversationMsgContext entry", fields, key); err != nil {
						return nil, err
					}
				}
			}
		default:
			msg.Extensions[name] = jsonutil.Plain(value)
		}
	}
	return msg, nil
}
