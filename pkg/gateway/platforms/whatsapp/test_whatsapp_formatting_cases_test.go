package whatsapp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

// Running adapter with open DM and group policy and the allow all opt in, the formatting cases harness
func openAdapter(t *testing.T) *WhatsAppAdapter {
	t.Helper()
	a := newBareAdapter(t, "/tmp/test-bridge", t.TempDir(), 3000)
	t.Setenv("WHATSAPP_ALLOW_ALL_USERS", "true")
	a.DMPolicy, a.GroupPolicy = "open", "open"
	a.MarkConnected(context.Background(), "")
	a.httpSession = &http.Client{}
	return a
}

// Bridge stand-in recording every send payload and answering with the message id
func recordingBridge(t *testing.T, a *WhatsAppAdapter, messageID string) *[]map[string]any {
	t.Helper()
	var mu sync.Mutex
	var posts []map[string]any
	fakeBridgeServer(t, a, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		body["_path"] = r.URL.Path
		posts = append(posts, body)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"success": true, "messageId": messageID})
	})
	return &posts
}

// Bridge payload object from JSON text
func payload(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	obj, err := pyjson.LoadsDict(text)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

// Media cache directory of the active profile
func cacheDir(t *testing.T, get func(context.Context, string) (string, error)) string {
	t.Helper()
	ctx := context.Background()
	dir, err := get(ctx, home.Dir(ctx))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFormatMessageStrikethrough(t *testing.T) {
	if got := openAdapter(t).FormatMessage("~~deleted~~"); got != "~deleted~" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageHeadersConvertedToBold(t *testing.T) {
	a := openAdapter(t)
	for in, want := range map[string]string{"# Title": "*Title*", "## Subtitle": "*Subtitle*", "### Deep": "*Deep*"} {
		if got := a.FormatMessage(in); got != want {
			t.Fatalf("%q = %q", in, got)
		}
	}
}

func TestFormatMessageBoldHeaderDoesNotDoubleWrap(t *testing.T) {
	a := openAdapter(t)
	for in, want := range map[string]string{"# **Title**": "*Title*", "## __Strong__": "*Strong*"} {
		if got := a.FormatMessage(in); got != want {
			t.Fatalf("%q = %q", in, got)
		}
	}
}

func TestFormatMessageAlreadyWhatsappItalic(t *testing.T) {
	a := openAdapter(t)
	for in, want := range map[string]string{"*italic*": "_italic_", "_italic_": "_italic_"} {
		if got := a.FormatMessage(in); got != want {
			t.Fatalf("%q = %q", in, got)
		}
	}
}

func TestMessageLimitsChunkLimitReservesDefaultSelfChatPrefix(t *testing.T) {
	a := openAdapter(t)
	os.Unsetenv("WHATSAPP_REPLY_PREFIX")
	t.Setenv("WHATSAPP_MODE", "self-chat")
	limit, err := a.OutgoingChunkLimit(context.Background())
	if err != nil || limit != whatsappMaxMessageLength-utf8.RuneCountInString(whatsappDefaultReplyPrefix) {
		t.Fatalf("limit = %d %v", limit, err)
	}
}

func TestSendChunkingShortMessageSingleSend(t *testing.T) {
	a := openAdapter(t)
	posts := recordingBridge(t, a, "msg1")
	result, err := a.Send(context.Background(), "chat1", "short message", "", nil)
	if err != nil || !result.Success || len(*posts) != 1 {
		t.Fatalf("send = %+v %v posts=%d", result, err, len(*posts))
	}
}

func TestSendChunkingLongMessageChunked(t *testing.T) {
	noSleep(t)
	a := openAdapter(t)
	posts := recordingBridge(t, a, "msg1")
	result, err := a.Send(context.Background(), "chat1", strings.Repeat("a ", 3000), "", nil)
	if err != nil || !result.Success || len(*posts) <= 1 {
		t.Fatalf("send = %+v %v posts=%d", result, err, len(*posts))
	}
}

func TestSendChunkingChunksLeaveRoomForBridgePrefix(t *testing.T) {
	noSleep(t)
	a := openAdapter(t)
	os.Unsetenv("WHATSAPP_REPLY_PREFIX")
	t.Setenv("WHATSAPP_MODE", "self-chat")
	posts := recordingBridge(t, a, "msg1")
	_, _ = a.Send(context.Background(), "chat1", strings.Repeat("a ", 3000), "", nil)
	for _, p := range *posts {
		if n := utf8.RuneCountInString(whatsappDefaultReplyPrefix + p["message"].(string)); n > whatsappMaxMessageLength {
			t.Fatalf("chunk of %d runes exceeds the limit", n)
		}
	}
}

func TestBridgeEventMetadataQuotedReplyMetadataIsPreservedInRawMessage(t *testing.T) {
	a := openAdapter(t)
	event := a.buildMessageEvent(context.Background(), payload(t, `{"messageId":"incoming-msg","chatId":"15551234567@s.whatsapp.net",
		"senderId":"15551234567@s.whatsapp.net","senderName":"Tester","chatName":"Tester","isGroup":false,"body":"approved",
		"hasMedia":false,"mediaUrls":[],"quotedMessageId":"outbound-msg","quotedParticipant":"99999999999@s.whatsapp.net",
		"quotedRemoteJid":"15551234567@s.whatsapp.net","hasQuotedMessage":true}`))
	if event == nil {
		t.Fatal("event dropped")
	}
	raw := event.RawMessage.(*pyjson.Object)
	if raw.GetOr("quotedMessageId", nil) != "outbound-msg" || raw.GetOr("quotedParticipant", nil) != "99999999999@s.whatsapp.net" ||
		raw.GetOr("quotedRemoteJid", nil) != "15551234567@s.whatsapp.net" || raw.GetOr("hasQuotedMessage", nil) != true {
		t.Fatalf("raw = %v", raw.Vals)
	}
}

func TestBridgeEventMetadataReplyToUncaptionedImageAttachesQuotedMedia(t *testing.T) {
	a := openAdapter(t)
	quoted := filepath.Join(cacheDir(t, mediacache.ImageCacheDir), "img_original.jpg")
	if err := os.WriteFile(quoted, []byte("fake-jpeg-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"messageId": "reply-msg", "chatId": "15551234567@s.whatsapp.net", "senderId": "15551234567@s.whatsapp.net",
		"senderName": "Ananya", "chatName": "Family", "isGroup": true, "body": "did you save this wedding invite?", "hasMedia": false,
		"mediaUrls": []any{}, "mediaType": "", "quotedMessageId": "original-image-msg", "quotedParticipant": "99999999999@s.whatsapp.net",
		"quotedRemoteJid": "15551234567@s.whatsapp.net", "hasQuotedMessage": true, "quotedText": "", "quotedMediaUrls": []any{quoted},
		"quotedMediaType": "image"}
	raw, _ := json.Marshal(data)
	event := a.buildMessageEvent(context.Background(), payload(t, string(raw)))
	if event == nil {
		t.Fatal("event dropped")
	}
	idx := slices.Index(event.MediaURLs, quoted)
	if idx < 0 || event.MediaTypes[idx] != "image/jpeg" {
		t.Fatalf("media = %v %v", event.MediaURLs, event.MediaTypes)
	}
}

func TestBridgeEventMetadataDocumentMarksMediaTextInlined(t *testing.T) {
	for _, tc := range []struct {
		id      string
		content string
		inlined bool
	}{{"small", "small text", true}, {"large", strings.Repeat("x", 200*1024), false}} {
		t.Run(tc.id, func(t *testing.T) {
			a := openAdapter(t)
			doc := filepath.Join(cacheDir(t, mediacache.DocumentCacheDir), "doc_abc_notes.txt")
			if err := os.WriteFile(doc, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"messageId": "doc-msg", "chatId": "15551234567@s.whatsapp.net",
				"senderId": "15551234567@s.whatsapp.net", "senderName": "Ananya", "isGroup": false, "body": "", "hasMedia": true,
				"mediaUrls": []any{doc}, "mediaType": "document"})
			event := a.buildMessageEvent(context.Background(), payload(t, string(raw)))
			if event == nil {
				t.Fatal("event dropped")
			}
			if strings.Contains(event.Text, "[Content of") != tc.inlined {
				t.Fatalf("text = %.80q", event.Text)
			}
			if len(event.MediaTextInlined) != 1 || *event.MediaTextInlined[0] != tc.inlined {
				t.Fatalf("inlined = %v", event.MediaTextInlined)
			}
		})
	}
}

func TestBridgeEventMetadataQuotedMediaPathOutsideCacheDirIsRejected(t *testing.T) {
	a := openAdapter(t)
	event := a.buildMessageEvent(context.Background(), payload(t, `{"messageId":"reply-msg-2","chatId":"15551234567@s.whatsapp.net",
		"senderId":"15551234567@s.whatsapp.net","senderName":"Ananya","chatName":"Family","isGroup":true,"body":"did you save this?",
		"hasMedia":false,"mediaUrls":[],"mediaType":"","quotedMessageId":"original-image-msg","quotedParticipant":"99999999999@s.whatsapp.net",
		"quotedRemoteJid":"15551234567@s.whatsapp.net","hasQuotedMessage":true,"quotedText":"","quotedMediaUrls":["/etc/passwd"],
		"quotedMediaType":"image"}`))
	if event == nil {
		t.Fatal("event dropped")
	}
	if slices.Contains(event.MediaURLs, "/etc/passwd") {
		t.Fatal("a path outside the caches was attached")
	}
}

func TestBridgeEventMetadataReplyToBotSentImageResolvesFromOutboundIndex(t *testing.T) {
	a := openAdapter(t)
	image := filepath.Join(t.TempDir(), "chart.png")
	if err := os.WriteFile(image, []byte("\x89PNG fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordingBridge(t, a, "BOT_IMG")
	sent, err := a.SendImageFile(context.Background(), "15551234567", image, "", "", nil, nil)
	if err != nil || !sent.Success || sent.MessageID != "BOT_IMG" {
		t.Fatalf("sent = %+v %v", sent, err)
	}
	event := a.buildMessageEvent(context.Background(), payload(t, `{"messageId":"reply-1","chatId":"15551234567@s.whatsapp.net",
		"senderId":"15551234567@s.whatsapp.net","senderName":"Alice","isGroup":false,"body":"what is this?","hasMedia":false,
		"mediaUrls":[],"mediaType":"","quotedMessageId":"BOT_IMG","quotedParticipant":"15550000000@s.whatsapp.net",
		"hasQuotedMessage":true,"quotedText":"","quotedMediaUrls":[],"quotedMediaType":"","botIds":["15550000000@s.whatsapp.net"]}`))
	if event == nil {
		t.Fatal("event dropped")
	}
	if !event.ReplyToIsOwnMessage {
		t.Fatal("quote of our own image not marked own")
	}
	if !slices.Equal(event.MediaURLs, []string{image}) || !slices.Equal(event.MediaTypes, []string{"image/png"}) {
		t.Fatalf("media = %v %v", event.MediaURLs, event.MediaTypes)
	}
}
