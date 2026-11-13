package dingtalk

import (
	"bytes"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// File extension to MIME type of DingTalk file and image messages
var extMIME = map[string]string{
	"pdf": "application/pdf", "doc": "application/msword", "xls": "application/vnd.ms-excel",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"ppt":  "application/vnd.ms-powerpoint",
	"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"png":  "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp", "bmp": "image/bmp",
	"md": "text/markdown", "txt": "text/plain", "csv": "text/csv", "zip": "application/zip", "rar": "application/vnd.rar",
	"mp4": "video/mp4", "mov": "video/quicktime", "mp3": "audio/mpeg", "wav": "audio/wav", "amr": "audio/amr",
}

// One inbound chatbot message decoded from the stream callback data
type chatbotMessage struct {
	raw               map[string]any
	MessageID         string
	ConversationID    string
	ConversationType  string
	ConversationTitle string
	SenderID          string
	SenderNick        string
	SenderStaffID     string
	RobotCode         string
	MsgType           string
	SessionWebhook    string
	WebhookExpiresAt  int64
	CreateAt          int64
	IsInAtList        bool
}

// One downloadable attachment of a message with the kind it reaches the agent as
type attachment struct {
	code     string
	kind     string
	mime     string
	fileName string
}

// Message decoded from callback data, an error when the data is not a JSON object
func parseChatbotMessage(data string) (*chatbotMessage, error) {
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("callback data is not a JSON object")
	}
	m := &chatbotMessage{raw: raw}
	m.MessageID = str(raw["msgId"])
	m.ConversationID = str(raw["conversationId"])
	m.ConversationType = str(raw["conversationType"])
	m.ConversationTitle = str(raw["conversationTitle"])
	m.SenderID = str(raw["senderId"])
	m.SenderNick = str(raw["senderNick"])
	m.SenderStaffID = str(raw["senderStaffId"])
	m.RobotCode = str(raw["robotCode"])
	m.MsgType = str(raw["msgtype"])
	m.SessionWebhook = firstNonEmpty(str(raw["sessionWebhook"]), str(raw["session_webhook"]))
	m.WebhookExpiresAt = num(raw["sessionWebhookExpiredTime"])
	m.CreateAt = num(raw["createAt"])
	m.IsInAtList = truthy(raw["isInAtList"])
	return m, nil
}

// Whether the message arrived in a group conversation
func (m *chatbotMessage) isGroup() bool { return m.ConversationType == "2" }

// Chat the message belongs to, the conversation or the sender when no conversation is given
func (m *chatbotMessage) chatID() string { return firstNonEmpty(m.ConversationID, m.SenderID) }

// Creation time of the message, now when the platform gave none
func (m *chatbotMessage) timestamp() time.Time {
	if m.CreateAt > 0 {
		return time.UnixMilli(m.CreateAt)
	}
	return time.Now()
}

// Content object of the message, empty when absent or not an object
func (m *chatbotMessage) content() map[string]any { return obj(m.raw["content"]) }

// Rich text items of a rich text message
func (m *chatbotMessage) richItems() []map[string]any {
	if m.MsgType != "richText" {
		return nil
	}
	list, _ := m.content()["richText"].([]any)
	var items []map[string]any
	for _, it := range list {
		if item, ok := it.(map[string]any); ok {
			items = append(items, item)
		}
	}
	return items
}

// Plain text of the message, rich text joined, then a per type fallback, mentions left in place
func (m *chatbotMessage) text() string {
	content := ""
	if m.MsgType == "text" {
		content = strings.TrimSpace(str(obj(m.raw["text"])["content"]))
	}
	if content == "" {
		var parts []string
		for _, item := range m.richItems() {
			if t := firstNonEmpty(str(item["text"]), str(item["content"])); t != "" {
				parts = append(parts, t)
			}
		}
		content = strings.TrimSpace(strings.Join(parts, " "))
	}
	if content != "" {
		return content
	}
	switch m.MsgType {
	case "audio":
		return strings.TrimSpace(str(m.content()["recognition"]))
	case "file":
		if name := str(m.content()["fileName"]); name != "" {
			return "[文件] " + name
		}
	case "card":
		return m.cardText()
	case "interactiveCard":
		return m.interactiveCardText()
	}
	return ""
}

// Document card title and URL, else the raw text the card carried
func (m *chatbotMessage) cardText() string {
	card := obj(m.raw["card"])
	title := str(card["title"])
	docURL := urlOf(card["content"])
	if s, ok := card["content"].(string); ok && strings.TrimSpace(s) != "" {
		var parsed any
		if json.Unmarshal([]byte(strings.TrimSpace(s)), &parsed) == nil {
			docURL = urlOf(parsed)
		} else {
			docURL = s
		}
	}
	var parts []string
	if title != "" {
		parts = append(parts, "[文档] "+title)
	}
	if docURL != "" {
		parts = append(parts, docURL)
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	return strings.TrimSpace(str(obj(m.raw["text"])["content"]))
}

// Interactive document card title and action URL
func (m *chatbotMessage) interactiveCardText() string {
	c := m.content()
	docURL, title := str(c["biz_custom_action_url"]), str(c["title"])
	if docURL == "" && title == "" {
		return ""
	}
	head := "[文档卡片]"
	if title != "" {
		head += " " + title
	}
	if docURL != "" {
		return head + " " + docURL
	}
	return head
}

// Message type and the attachments to download, voice recognition text standing in for the audio
func (m *chatbotMessage) attachments() (gateway.MessageType, []attachment) {
	msgType := gateway.MessageTypeText
	var out []attachment
	add := func(a attachment, promoted gateway.MessageType) {
		if a.code == "" {
			return
		}
		out = append(out, a)
		if msgType == gateway.MessageTypeText {
			msgType = promoted
		}
	}
	c := m.content()
	code := firstNonEmpty(str(c["downloadCode"]), str(c["download_code"]), str(c["pictureDownloadCode"]))
	switch m.MsgType {
	case "picture":
		add(attachment{code: code, kind: "image", mime: "image/jpeg"}, gateway.MessageTypePhoto)
		msgType = gateway.MessageTypePhoto
	case "richText":
		for _, item := range m.richItems() {
			itemCode := firstNonEmpty(str(item["downloadCode"]), str(item["download_code"]), str(item["pictureDownloadCode"]))
			name := str(item["fileName"])
			switch str(item["type"]) {
			case "picture", "image":
				add(attachment{code: itemCode, kind: "image", mime: "image/jpeg", fileName: name}, gateway.MessageTypePhoto)
			case "video":
				add(attachment{code: itemCode, kind: "video", mime: "video/mp4", fileName: name}, gateway.MessageTypeVideo)
			case "voice":
				add(attachment{code: itemCode, kind: "audio", fileName: name}, gateway.MessageTypeVoice)
			case "audio":
				add(attachment{code: itemCode, kind: "audio", fileName: name}, gateway.MessageTypeAudio)
			default:
				add(attachment{code: itemCode, kind: "document", mime: mimeForName(name), fileName: name}, gateway.MessageTypeDocument)
			}
		}
	case "audio":
		if strings.TrimSpace(str(c["recognition"])) == "" {
			add(attachment{code: code, kind: "audio"}, gateway.MessageTypeVoice)
		}
		msgType = gateway.MessageTypeVoice
	case "video":
		add(attachment{code: code, kind: "video", mime: "video/mp4"}, gateway.MessageTypeVideo)
	case "file", "image":
		name := str(c["fileName"])
		mime := mimeForName(name)
		if m.MsgType == "image" || strings.HasPrefix(mime, "image/") {
			add(attachment{code: str(c["downloadCode"]), kind: "image", mime: mime, fileName: name}, gateway.MessageTypePhoto)
		} else {
			add(attachment{code: str(c["downloadCode"]), kind: "document", mime: mime, fileName: name}, gateway.MessageTypeDocument)
		}
	}
	return msgType, out
}

// MIME type of a file name by its extension, octet stream when unknown
func mimeForName(name string) string {
	if mime, ok := extMIME[strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))]; ok {
		return mime
	}
	return "application/octet-stream"
}

// Document URL of a card content object
func urlOf(v any) string {
	o := obj(v)
	return firstNonEmpty(str(o["url"]), str(o["docUrl"]))
}

// Object value, empty when the value is not an object
func obj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Text of a decoded JSON scalar, empty for null and containers
func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

// Integer of a decoded JSON number or numeric string, zero otherwise
func num(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		if f, err := x.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(x)
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// Truthiness of a decoded JSON flag
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1"
	case json.Number:
		return x.String() != "0"
	}
	return false
}

// First non empty string
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Extension for downloaded bytes from magic numbers, the MIME type, then the file name
func sniffExt(data []byte, mime, fileName string) string {
	switch {
	case bytes.HasPrefix(data, []byte("#!AMR-WB\n")), bytes.HasPrefix(data, []byte("#!AMR\n")):
		return ".amr"
	case bytes.HasPrefix(data, []byte("\x89PNG")):
		return ".png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return ".jpg"
	case bytes.HasPrefix(data, []byte("GIF8")):
		return ".gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return ".wav"
	case bytes.HasPrefix(data, []byte("OggS")):
		return ".ogg"
	case bytes.HasPrefix(data, []byte("ID3")), len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return ".mp3"
	case len(data) >= 8 && string(data[4:8]) == "ftyp":
		if len(data) >= 12 && (string(data[8:12]) == "M4A " || string(data[8:12]) == "m4a ") {
			return ".m4a"
		}
		return ".mp4"
	}
	if ext := path.Ext(fileName); ext != "" {
		return strings.ToLower(ext)
	}
	return mimeExt[mime]
}

// Canonical extension of the MIME types DingTalk attachments arrive as
var mimeExt = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp", "image/bmp": ".bmp",
	"video/mp4": ".mp4", "video/quicktime": ".mov", "audio/mpeg": ".mp3", "audio/wav": ".wav", "audio/amr": ".amr",
	"audio/ogg": ".ogg", "application/pdf": ".pdf", "application/zip": ".zip", "text/plain": ".txt",
}
