package dingtalk

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func mustText(t *testing.T, m *ChatbotMessage) string {
	t.Helper()
	got, err := ExtractText(m)
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}
	return got
}

func TestExtractTextReadsEverySDKShape(t *testing.T) {
	if got := mustText(t, &ChatbotMessage{Text: &TextContent{Content: "hello from new sdk"}, MessageType: "text"}); got != "hello from new sdk" || strings.Contains(got, "TextContent(") {
		t.Fatalf("text content = %q", got)
	}
	rich := &RichTextContent{RichTextList: []any{map[string]any{"text": "hello "}, map[string]any{"text": "world"}}}
	if got := mustText(t, &ChatbotMessage{RichTextContent: rich}); !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("rich text = %q", got)
	}
	if got := mustText(t, &ChatbotMessage{MessageType: "text"}); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := mustText(t, &ChatbotMessage{Text: map[string]any{"content": "  legacy dict  "}}); got != "legacy dict" {
		t.Fatalf("legacy dict = %q", got)
	}
	legacy := []any{map[string]any{"content": "from content"}, "bare", map[string]any{"text": ""}, map[string]any{"text": "tail"}}
	if got := mustText(t, &ChatbotMessage{RichText: legacy}); got != "from content tail" {
		t.Fatalf("legacy rich list = %q", got)
	}
	if got := mustText(t, &ChatbotMessage{RichTextContent: &RichTextContent{RichTextList: []any{}}, RichText: []any{map[string]any{"text": "unused"}}}); got != "" {
		t.Fatalf("empty rich_text_list falls back to the object = %q", got)
	}
}

func TestExtractTextCardFallbacks(t *testing.T) {
	cases := []struct {
		msgType string
		ext     map[string]any
		want    string
	}{
		{"card", map[string]any{"card": map[string]any{"title": "Q3经营分析报告", "content": map[string]any{"url": "https://dingtalk.com/doc/abc123"}}},
			"[文档] Q3经营分析报告 https://dingtalk.com/doc/abc123"},
		{"card", map[string]any{"card": map[string]any{"title": "周报模板", "content": map[string]any{"docUrl": "https://docs.dingtalk.com/xyz"}}},
			"[文档] 周报模板 https://docs.dingtalk.com/xyz"},
		{"interactiveCard", map[string]any{"content": map[string]any{"title": "项目看板", "biz_custom_action_url": "https://dingtalk.com/doc/kanban"}},
			"[文档卡片] 项目看板 https://dingtalk.com/doc/kanban"},
		{"interactiveCard", map[string]any{"content": map[string]any{"biz_custom_action_url": "https://x"}}, "[文档卡片] https://x"},
		{"interactiveCard", map[string]any{"content": map[string]any{}}, ""},
		{"card", map[string]any{"card": map[string]any{"title": "T", "content": ` {"docUrl": "https://json"} `}}, "[文档] T https://json"},
		{"card", map[string]any{"card": map[string]any{"content": "not json"}}, "not json"},
		{"card", map[string]any{"card": "flat", "text": map[string]any{"content": "  raw text  "}}, "raw text"},
		{"audio", map[string]any{"content": map[string]any{"recognition": " 你好 "}}, "你好"},
		{"file", map[string]any{"content": map[string]any{"fileName": "report.pdf"}}, "[文件] report.pdf"},
		{"file", map[string]any{"content": map[string]any{}}, ""},
	}
	for _, c := range cases {
		if got := mustText(t, &ChatbotMessage{MessageType: c.msgType, Extensions: c.ext}); got != c.want {
			t.Fatalf("%s %v = %q want %q", c.msgType, c.ext, got, c.want)
		}
	}
}

func TestExtractTextPreservesAtMentions(t *testing.T) {
	for _, text := range []string{"@bot hello", "contact alice@example.com", "git@github.com:foo/bar.git", "what does @openai think", "@机器人 转发给 @老王"} {
		if got := mustText(t, &ChatbotMessage{Text: text}); got != text {
			t.Fatalf("mangled: %q -> %q", text, got)
		}
	}
}

func TestExtractTextRaisesOnNonStringPayloads(t *testing.T) {
	cases := []struct {
		m    *ChatbotMessage
		want string
	}{
		{&ChatbotMessage{Text: map[string]any{"content": int64(5)}}, "AttributeError: 'int' object has no attribute 'strip'"},
		{&ChatbotMessage{RichText: []any{map[string]any{"text": int64(7)}}}, "TypeError: sequence item 0: expected str instance, int found"},
		{&ChatbotMessage{MessageType: "card", Extensions: map[string]any{"card": map[string]any{"content": map[string]any{"url": true}}}},
			"TypeError: sequence item 0: expected str instance, bool found"},
	}
	for _, c := range cases {
		if _, err := ExtractText(c.m); err == nil || err.Error() != c.want {
			t.Fatalf("err = %v want %s", err, c.want)
		}
	}
	_, err := ExtractText(cases[0].m)
	if pyrepr.ExceptionName(err) != "AttributeError" {
		t.Fatalf("class = %s", pyrepr.ExceptionName(err))
	}
}

func mustMedia(t *testing.T, m *ChatbotMessage) (gateway.MessageType, []string, []string) {
	t.Helper()
	mt, urls, types, err := ExtractMedia(m)
	if err != nil {
		t.Fatalf("ExtractMedia: %v", err)
	}
	return mt, urls, types
}

func TestExtractMediaKeepsVoiceAndImageClassification(t *testing.T) {
	mt, urls, types := mustMedia(t, &ChatbotMessage{MessageType: "richText", RichText: []any{map[string]any{"type": "voice", "downloadCode": "dl_voice_rt"}}})
	if mt != gateway.MessageTypeVoice || !reflect.DeepEqual(urls, []string{"dl_voice_rt"}) || !reflect.DeepEqual(types, []string{"audio"}) {
		t.Fatalf("voice = %s %v %v", mt, urls, types)
	}
	mt, urls, types = mustMedia(t, &ChatbotMessage{MessageType: "image", Extensions: map[string]any{"content": map[string]any{"downloadCode": "dl_img_noext"}}})
	if mt != gateway.MessageTypePhoto || !reflect.DeepEqual(urls, []string{"dl_img_noext"}) || !reflect.DeepEqual(types, []string{"application/octet-stream"}) {
		t.Fatalf("image = %s %v %v", mt, urls, types)
	}
}

func TestExtractMediaClassifiesEveryPayloadKind(t *testing.T) {
	cases := []struct {
		m     *ChatbotMessage
		mt    gateway.MessageType
		urls  []string
		types []string
	}{
		{&ChatbotMessage{ImageContent: &ImageContent{DownloadCode: "pic"}, MessageType: "picture"}, gateway.MessageTypePhoto, []string{"pic"}, []string{"image"}},
		{&ChatbotMessage{MessageType: "picture"}, gateway.MessageTypePhoto, []string{}, []string{}},
		{&ChatbotMessage{MessageType: "audio"}, gateway.MessageTypeVoice, []string{}, []string{}},
		{&ChatbotMessage{MessageType: "text"}, gateway.MessageTypeText, []string{}, []string{}},
		{&ChatbotMessage{MessageType: "richText", RichTextContent: &RichTextContent{RichTextList: []any{
			map[string]any{"type": "picture", "download_code": "a"}, map[string]any{"type": "video", "downloadCode": "b"}, "skip",
			map[string]any{"type": "picture"}}}}, gateway.MessageTypePhoto, []string{"a", "b"}, []string{"image", "application/octet-stream"}},
		{&ChatbotMessage{RichText: []any{map[string]any{"type": "audio", "downloadCode": "c"}, map[string]any{"type": int64(3), "downloadCode": "d"}}},
			gateway.MessageTypeDocument, []string{"c", "d"}, []string{"application/octet-stream", "application/octet-stream"}},
		{&ChatbotMessage{RichText: []any{map[string]any{"type": "voice", "downloadCode": "v"}}}, gateway.MessageTypeVoice, []string{"v"}, []string{"audio"}},
		{&ChatbotMessage{MessageType: "file", Extensions: map[string]any{"content": map[string]any{"downloadCode": "f", "fileName": "Report.PDF"}}},
			gateway.MessageTypeDocument, []string{"f"}, []string{"application/pdf"}},
		{&ChatbotMessage{MessageType: "file", Extensions: map[string]any{"content": map[string]any{"downloadCode": "g", "fileName": "shot.png"}}},
			gateway.MessageTypePhoto, []string{"g"}, []string{"image/png"}},
		{&ChatbotMessage{MessageType: "file", Extensions: map[string]any{"content": map[string]any{"fileName": "x.pdf"}}}, gateway.MessageTypeText, []string{}, []string{}},
		{&ChatbotMessage{MessageType: "file", Extensions: map[string]any{"content": map[string]any{"downloadCode": "h", "fileName": "noext"}}},
			gateway.MessageTypeDocument, []string{"h"}, []string{"application/octet-stream"}},
	}
	for i, c := range cases {
		mt, urls, types := mustMedia(t, c.m)
		if mt != c.mt || !reflect.DeepEqual(urls, c.urls) || !reflect.DeepEqual(types, c.types) {
			t.Fatalf("case %d = %s %v %v", i, mt, urls, types)
		}
	}
	unhashable := &ChatbotMessage{RichText: []any{map[string]any{"type": map[string]any{}, "downloadCode": "x"}}}
	if _, _, _, err := ExtractMedia(unhashable); err == nil || err.Error() != "TypeError: unhashable type: 'dict'" {
		t.Fatalf("unhashable = %v", err)
	}
	badName := &ChatbotMessage{MessageType: "file", Extensions: map[string]any{"content": map[string]any{"downloadCode": "x", "fileName": int64(9)}}}
	if _, _, _, err := ExtractMedia(badName); err == nil || err.Error() != "TypeError: argument of type 'int' is not iterable" {
		t.Fatalf("int file name = %v", err)
	}
}

func TestCollectDownloadCodesNamesEveryResolvableContainer(t *testing.T) {
	img := &ImageContent{DownloadCode: "pic"}
	item := map[string]any{"downloadCode": "a", "pictureDownloadCode": "b", "download_code": ""}
	content := map[string]any{"downloadCode": "f"}
	m := &ChatbotMessage{ImageContent: img, RichTextContent: &RichTextContent{RichTextList: []any{item, "skip"}}, MessageType: "file",
		Extensions: map[string]any{"content": content}}
	got := CollectDownloadCodes(m)
	if len(got) != 4 || got[0].Container != any(img) || got[0].Key != "download_code" || got[1].Key != "downloadCode" ||
		got[2].Key != "pictureDownloadCode" || got[3].Key != "downloadCode" {
		t.Fatalf("codes = %+v", got)
	}
	got[3].Container.(map[string]any)["downloadCode"] = "https://resolved"
	if content["downloadCode"] != "https://resolved" {
		t.Fatalf("container is not the payload dict")
	}
	if got := CollectDownloadCodes(&ChatbotMessage{RichText: []any{map[string]any{"downloadCode": "legacy"}}, MessageType: "text"}); len(got) != 0 {
		t.Fatalf("legacy rich text is not resolved = %+v", got)
	}
}
