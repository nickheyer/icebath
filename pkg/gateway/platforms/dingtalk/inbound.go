package dingtalk

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var DingTalkTypeMapping = map[string]string{"picture": "image", "voice": "audio"}

var ExtMap = map[string]string{
	"pdf": "application/pdf", "doc": "application/msword", "xls": "application/vnd.ms-excel",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"png":  "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp",
	"md": "text/markdown", "txt": "text/plain", "csv": "text/csv", "zip": "application/zip", "mp4": "video/mp4",
}

type richMedia struct {
	mime     string
	promoted gateway.MessageType
}

var richMediaByType = map[string]richMedia{
	"image": {"image", gateway.MessageTypePhoto},
	"video": {"video", gateway.MessageTypeVideo},
	"file":  {"application/octet-stream", gateway.MessageTypeDocument},
}

type TextContent struct{ Content string }

type ImageContent struct{ DownloadCode string }

type RichTextContent struct{ RichTextList any }

// Inbound ChatbotMessage fields the parsers read, with SDK attribute shapes kept as any where they vary
type ChatbotMessage struct {
	Text            any
	RichTextContent any
	RichText        any
	MessageType     string
	Extensions      map[string]any
	ImageContent    *ImageContent
}

// Container and key of one download code the adapter resolves to a URL in place
type DownloadCodeRef struct {
	Container any
	Key       string
}

func asDict(v any) (map[string]any, bool) {
	d, ok := v.(map[string]any)
	return d, ok
}

// Top level json.loads object as a payload dict so the URL lookup reads it like any other dict
func loadedDict(v any) any {
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return v
	}
	out := make(map[string]any, obj.Len())
	for _, k := range obj.Keys {
		out[k], _ = obj.Get(k)
	}
	return out
}

func dictGet(d map[string]any, key string, def any) any {
	if v, ok := d[key]; ok {
		return v
	}
	return def
}

func or(vals ...any) any {
	for _, v := range vals[:len(vals)-1] {
		if pyrepr.Truthy(v) {
			return v
		}
	}
	return vals[len(vals)-1]
}

func strip(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(v))
	}
	return pystr.Strip(s), nil
}

func join(items []any) (string, error) {
	parts := make([]string, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			return "", fmt.Errorf("TypeError: sequence item %d: expected str instance, %s found", i, pyrepr.TypeName(it))
		}
		parts[i] = s
	}
	return strings.Join(parts, " "), nil
}

func extensions(m *ChatbotMessage) map[string]any {
	if len(m.Extensions) == 0 {
		return map[string]any{}
	}
	return m.Extensions
}

// Extension content when it is a dict, otherwise nil
func extContent(m *ChatbotMessage) map[string]any {
	d, _ := asDict(dictGet(extensions(m), "content", map[string]any{}))
	return d
}

// Rich text item list from the current rich_text_content shape or the legacy rich_text field
func richList(m *ChatbotMessage) ([]any, bool) {
	richText := or(m.RichTextContent, m.RichText)
	if !pyrepr.Truthy(richText) {
		return nil, false
	}
	list := richText
	if rc, ok := richText.(*RichTextContent); ok && pyrepr.Truthy(rc.RichTextList) {
		list = rc.RichTextList
	}
	out, ok := list.([]any)
	return out, ok
}

func urlOf(raw any) any {
	if d, ok := asDict(raw); ok {
		return or(dictGet(d, "url", ""), dictGet(d, "docUrl", ""))
	}
	return ""
}

// Card title and document URL from the card extension, else the raw extension text
func cardText(m *ChatbotMessage) (string, error) {
	ext := extensions(m)
	content := ""
	if card, ok := asDict(dictGet(ext, "card", map[string]any{})); ok {
		title, rawContent := dictGet(card, "title", ""), dictGet(card, "content", "")
		docURL := urlOf(rawContent)
		if s, isStr := rawContent.(string); isStr && pystr.Strip(s) != "" {
			if parsed, err := pyjson.Loads(pystr.Strip(s)); err == nil {
				docURL = urlOf(loadedDict(parsed))
			} else {
				docURL = rawContent
			}
		}
		var parts []any
		if pyrepr.Truthy(title) {
			parts = append(parts, "[文档] "+pyrepr.Str(title))
		}
		if pyrepr.Truthy(docURL) {
			parts = append(parts, docURL)
		}
		var err error
		if content, err = join(parts); err != nil {
			return "", err
		}
	}
	if content != "" {
		return content, nil
	}
	if extText, ok := asDict(dictGet(ext, "text", map[string]any{})); ok {
		return strip(or(dictGet(extText, "content", ""), ""))
	}
	return "", nil
}

// Interactive card title and action URL from the content extension
func interactiveCardText(m *ChatbotMessage) (string, error) {
	ec := extContent(m)
	if ec == nil {
		ec = map[string]any{}
	}
	docURL, title := dictGet(ec, "biz_custom_action_url", ""), dictGet(ec, "title", "")
	if !pyrepr.Truthy(docURL) && !pyrepr.Truthy(title) {
		return "", nil
	}
	head := "[文档卡片]"
	if pyrepr.Truthy(title) {
		head = "[文档卡片] " + pyrepr.Str(title)
	}
	parts := []any{head}
	if pyrepr.Truthy(docURL) {
		parts = append(parts, docURL)
	}
	return join(parts)
}

func extField(m *ChatbotMessage, field string) any {
	if ec := extContent(m); len(ec) > 0 {
		return dictGet(ec, field, "")
	}
	return ""
}

var emptyTextFallbacks = map[string]func(*ChatbotMessage) (string, error){
	"audio": func(m *ChatbotMessage) (string, error) { return strip(or(extField(m, "recognition"), "")) },
	"file": func(m *ChatbotMessage) (string, error) {
		if name := extField(m, "fileName"); pyrepr.Truthy(name) {
			return "[文件] " + pyrepr.Str(name), nil
		}
		return "", nil
	},
	"card":            cardText,
	"interactiveCard": interactiveCardText,
}

func richItemText(item any) any {
	if d, ok := asDict(item); ok {
		return or(dictGet(d, "text", nil), dictGet(d, "content", nil), "")
	}
	return ""
}

// Plain text of a message from either SDK shape, falling back by message type when no text is present
func ExtractText(m *ChatbotMessage) (string, error) {
	text := or(m.Text, "")
	var raw any
	if tc, ok := text.(*TextContent); ok {
		raw = tc.Content
	} else if d, ok := asDict(text); ok {
		raw = dictGet(d, "content", "")
	} else {
		raw = pyrepr.Str(text)
	}
	content, err := strip(raw)
	if err != nil {
		return "", err
	}
	if content == "" {
		if list, ok := richList(m); ok {
			var parts []any
			for _, it := range list {
				if t := richItemText(it); pyrepr.Truthy(t) {
					parts = append(parts, t)
				}
			}
			joined, err := join(parts)
			if err != nil {
				return "", err
			}
			content = pystr.Strip(joined)
		}
	}
	if content == "" {
		if fallback := emptyTextFallbacks[m.MessageType]; fallback != nil {
			return fallback(m)
		}
	}
	return content, nil
}

func codeString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

func hashableKey(v any) (string, bool, error) {
	switch v.(type) {
	case map[string]any, []any:
		return "", false, fmt.Errorf("TypeError: unhashable type: '%s'", pyrepr.TypeName(v))
	}
	s, ok := v.(string)
	return s, ok, nil
}

// Extension used for the MIME lookup of a file name, raising like Python on a truthy non string name
func fileNameExt(fname any) (string, error) {
	if !pyrepr.Truthy(fname) {
		return "", nil
	}
	if s, ok := fname.(string); ok {
		if !strings.Contains(s, ".") {
			return "", nil
		}
		return pystr.Lower(s[strings.LastIndex(s, ".")+1:]), nil
	}
	contains := false
	switch c := fname.(type) {
	case []any:
		for _, e := range c {
			contains = contains || pyrepr.Equal(e, ".")
		}
	default:
		d, ok := asDict(fname)
		if !ok {
			return "", fmt.Errorf("TypeError: argument of type '%s' is not iterable", pyrepr.TypeName(fname))
		}
		_, contains = d["."]
	}
	if contains {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'rsplit'", pyrepr.TypeName(fname))
	}
	return "", nil
}

// Message type, download codes or URLs and MIME types of the media a message carries
func ExtractMedia(m *ChatbotMessage) (gateway.MessageType, []string, []string, error) {
	msgType := gateway.MessageTypeText
	mediaURLs, mediaTypes := []string{}, []string{}
	if m.ImageContent != nil && m.ImageContent.DownloadCode != "" {
		mediaURLs = append(mediaURLs, m.ImageContent.DownloadCode)
		mediaTypes = append(mediaTypes, "image")
		msgType = gateway.MessageTypePhoto
	}
	list, _ := richList(m)
	for _, raw := range list {
		item, ok := asDict(raw)
		if !ok {
			continue
		}
		dlCode := or(item["downloadCode"], item["download_code"], "")
		if !pyrepr.Truthy(dlCode) {
			continue
		}
		itemType, isStr, err := hashableKey(dictGet(item, "type", ""))
		if err != nil {
			return "", nil, nil, err
		}
		mapped := "file"
		if v, ok := DingTalkTypeMapping[itemType]; ok && isStr {
			mapped = v
		}
		var media richMedia
		if mapped == "audio" {
			media = richMedia{"audio", gateway.MessageTypeAudio}
			if isStr && itemType == "voice" {
				media.promoted = gateway.MessageTypeVoice
			}
		} else {
			media = richMediaByType[mapped]
		}
		mediaURLs = append(mediaURLs, codeString(dlCode))
		mediaTypes = append(mediaTypes, media.mime)
		if msgType == gateway.MessageTypeText {
			msgType = media.promoted
		}
	}
	switch m.MessageType {
	case "picture":
		if len(mediaURLs) == 0 {
			msgType = gateway.MessageTypePhoto
		}
	case "richText":
		if msgType == gateway.MessageTypeText {
			for _, t := range mediaTypes {
				if strings.Contains(t, "image") {
					msgType = gateway.MessageTypePhoto
					break
				}
			}
		}
	case "audio":
		if msgType == gateway.MessageTypeText {
			msgType = gateway.MessageTypeVoice
		}
	case "file", "image":
		ec := extContent(m)
		if ec == nil {
			ec = map[string]any{}
		}
		if dlCode := or(ec["downloadCode"], ""); pyrepr.Truthy(dlCode) {
			ext, err := fileNameExt(dictGet(ec, "fileName", ""))
			if err != nil {
				return "", nil, nil, err
			}
			mime, ok := ExtMap[ext]
			if !ok {
				mime = "application/octet-stream"
			}
			mediaURLs = append(mediaURLs, codeString(dlCode))
			mediaTypes = append(mediaTypes, mime)
			if msgType == gateway.MessageTypeText {
				msgType = gateway.MessageTypeDocument
				if m.MessageType == "image" || strings.HasPrefix(mime, "image/") {
					msgType = gateway.MessageTypePhoto
				}
			}
		}
	}
	return msgType, mediaURLs, mediaTypes, nil
}

// Container and key pairs whose download code the adapter resolves to a URL
func CollectDownloadCodes(m *ChatbotMessage) []DownloadCodeRef {
	var codes []DownloadCodeRef
	if m.ImageContent != nil && m.ImageContent.DownloadCode != "" {
		codes = append(codes, DownloadCodeRef{m.ImageContent, "download_code"})
	}
	if rc, ok := m.RichTextContent.(*RichTextContent); ok && rc != nil {
		list, _ := or(rc.RichTextList, []any{}).([]any)
		for _, raw := range list {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"downloadCode", "pictureDownloadCode", "download_code"} {
				if pyrepr.Truthy(item[key]) {
					codes = append(codes, DownloadCodeRef{item, key})
				}
			}
		}
	}
	if m.MessageType == "file" || m.MessageType == "image" {
		if ec := extContent(m); len(ec) > 0 && pyrepr.Truthy(ec["downloadCode"]) {
			codes = append(codes, DownloadCodeRef{ec, "downloadCode"})
		}
	}
	return codes
}
