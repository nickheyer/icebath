package telegram

import "testing"

func link(offset, length, url any) MessageEntity {
	return MessageEntity{Type: "text_link", Offset: offset, Length: length, URL: url}
}

func TestExpandLinkEntities(t *testing.T) {
	out := expandLinkEntities(EntityMessage{Text: "\u0421\u0441\u044b\u043b\u043a\u0430: \u0442\u0443\u0442\n#tag", Entities: []MessageEntity{link(8, 3, "https://github.com/Cysharp/R3")}})
	if out != "\u0421\u0441\u044b\u043b\u043a\u0430: \u0442\u0443\u0442 (https://github.com/Cysharp/R3)\n#tag" {
		t.Fatal(out)
	}
	if again := expandLinkEntities(EntityMessage{Text: out, Entities: []MessageEntity{link(8, 3, "https://github.com/Cysharp/R3")}}); again != out {
		t.Fatal(again)
	}
	if got := expandLinkEntities(EntityMessage{Caption: "\u0421\u043c\u043e\u0442\u0440\u0438 \u0442\u0443\u0442 \u043f\u0440\u043e\u0435\u043a\u0442", CaptionEntities: []MessageEntity{link(7, 3, "https://example.com/x")}}); got != "\u0421\u043c\u043e\u0442\u0440\u0438 \u0442\u0443\u0442 (https://example.com/x) \u043f\u0440\u043e\u0435\u043a\u0442" {
		t.Fatal(got)
	}
	if got := expandLinkEntities(EntityMessage{Text: "\U0001f525 \u0442\u0443\u0442", Entities: []MessageEntity{link(3, 3, "https://example.com/emoji")}}); got != "\U0001f525 \u0442\u0443\u0442 (https://example.com/emoji)" {
		t.Fatal(got)
	}
	for _, e := range []MessageEntity{link(1, 99, "https://example.com/past-end"), link(0, 1, 123), link("invalid", 1, "https://example.com/bad-offset")} {
		if got := expandLinkEntities(EntityMessage{Text: "abc", Entities: []MessageEntity{e}}); got != "abc" {
			t.Fatal(got)
		}
	}
	if got := expandLinkEntities(EntityMessage{Text: "plain text", Caption: "linked caption", CaptionEntities: []MessageEntity{link(0, 6, "https://example.com/caption")}}); got != "plain text" {
		t.Fatal(got)
	}
	if got := expandLinkEntities(EntityMessage{Text: "\U0001f525 link", Entities: []MessageEntity{link(1, 1, "https://example.com/mid-surrogate")}}); got != "\U0001f525 link" {
		t.Fatal(got)
	}
	url := "https://example.com/self"
	if got := expandLinkEntities(EntityMessage{Text: "see " + url + " now", Entities: []MessageEntity{link(4, len(url), url)}}); got != "see "+url+" now" {
		t.Fatal(got)
	}
	if got := expandLinkEntities(EntityMessage{Text: "go here", Entities: []MessageEntity{{Type: "MessageEntityType.TEXT_LINK", Offset: 3, Length: 4, URL: " https://h "}}}); got != "go here (https://h)" {
		t.Fatal(got)
	}
}
