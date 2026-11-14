package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

var onePixelPNG, _ = hex.DecodeString("89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4" +
	"890000000d49444154789c6360000002000154a24f5f0000000049454e44ae426082")

func TestCacheMediaBytesRouting(t *testing.T) {
	ctx := home.WithOverride(context.Background(), t.TempDir())
	doc, err := CacheMediaBytes(ctx, []byte("%PDF-1.4 body"), "report.pdf", "application/pdf", "")
	if err != nil || doc == nil || doc.Kind != "document" || doc.MediaType != "application/pdf" || !strings.Contains(doc.DisplayName, "report.pdf") {
		t.Fatalf("pdf routing = %+v, %v", doc, err)
	}
	if _, err := os.Stat(doc.Path); err != nil || !strings.Contains(doc.ContextNote(ctx), "report.pdf") {
		t.Fatalf("pdf path %q note %q", doc.Path, doc.ContextNote(ctx))
	}
	img, err := CacheMediaBytes(ctx, onePixelPNG, "photo.png", "image/png", "")
	if err != nil || img == nil || img.Kind != "image" || img.MediaType != "image/png" {
		t.Fatalf("png routing = %+v, %v", img, err)
	}
	if _, err := os.Stat(img.Path); err != nil {
		t.Fatal(err)
	}
	exe, err := CacheMediaBytes(ctx, []byte("MZ"), "program.exe", "application/x-msdownload", "")
	if err != nil || exe == nil || exe.Kind != "document" || exe.MediaType != "application/x-msdownload" {
		t.Fatalf("unknown routing = %+v, %v", exe, err)
	}
	if _, err := os.Stat(exe.Path); err != nil {
		t.Fatal(err)
	}
	if bad, err := CacheMediaBytes(ctx, []byte("not an image"), "", "image/png", ""); bad != nil || err != nil {
		t.Fatalf("an image failing validation is dropped, got %+v, %v", bad, err)
	}
	native, err := CacheMediaBytes(ctx, onePixelPNG, "", "", "image")
	if err != nil || native.Kind != "image" || native.MediaType != "image/jpeg" || native.DisplayName != "file" || !strings.HasSuffix(native.Path, ".jpg") {
		t.Fatalf("default kind photo = %+v, %v", native, err)
	}
	named, err := CacheMediaBytes(ctx, []byte("x"), "résumé (1)/x.txt", "", "")
	if err != nil || named.DisplayName != "résumé _1__x.txt" || named.MediaType != "text/plain" {
		t.Fatalf("display name = %+v, %v", named, err)
	}
	if got := resolveMediaExt("", "TEXT/PLAIN"); got != ".txt" {
		t.Fatalf("mime fallback = %q", got)
	}
	if got := resolveMediaExt("", "image/jpeg"); got != ".jpg" {
		t.Fatalf("first image extension wins, got %q", got)
	}
}

func TestCacheMediaBytesReturnsHostPathAndTranslatesInNote(t *testing.T) {
	a := filepath.Join(t.TempDir(), ".icebath")
	b := filepath.Join(a, "profiles", "b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", a)
	for dir, backend := range map[string]string{a: "local", b: "docker"} {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("terminal:\n  backend: "+backend+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := home.WithOverride(context.Background(), b)
	ctx = tools.SetTerminalScope(ctx, tools.PrepareProfileTerminalScope(ctx, b, nil))
	cached, err := CacheMediaBytes(ctx, []byte("%PDF-1.4 x"), "report.pdf", "application/pdf", "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(cached.Path) != filepath.Join(b, "cache", "documents") {
		t.Fatalf("host path %q", cached.Path)
	}
	if note := cached.ContextNote(ctx); !strings.HasPrefix(note, "[document 'report.pdf' saved at: /root/.icebath/cache/documents/doc_") {
		t.Fatalf("note %q", note)
	}
}

func TestAsyncCacheWrappersRunOffTheCaller(t *testing.T) {
	cases := []struct {
		name   string
		target *func(context.Context, []byte, string) (string, error)
		call   func(context.Context, []byte, string) (string, error)
		arg    string
	}{
		{"image", &cacheImageFromBytes, CacheImageFromBytesAsync, ".png"},
		{"audio", &cacheAudioFromBytes, CacheAudioFromBytesAsync, ".ogg"},
		{"video", &cacheVideoFromBytes, CacheVideoFromBytesAsync, ".mp4"},
		{"document", &cacheDocumentFromBytes, CacheDocumentFromBytesAsync, "report.pdf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := *c.target
			defer func() { *c.target = orig }()
			started, release := make(chan struct{}), make(chan struct{})
			var gotData []byte
			var gotArg string
			tickerRan := false
			*c.target = func(_ context.Context, data []byte, arg string) (string, error) {
				gotData, gotArg = data, arg
				close(started)
				select {
				case <-release:
					tickerRan = true
				case <-time.After(time.Second):
				}
				return "cached", nil
			}
			go func() {
				<-started
				close(release)
			}()
			got, err := c.call(context.Background(), []byte("data"), c.arg)
			if err != nil || got != "cached" || string(gotData) != "data" || gotArg != c.arg || !tickerRan {
				t.Fatalf("got %q %v args %q %q ticker %v", got, err, gotData, gotArg, tickerRan)
			}
			block := make(chan struct{})
			defer close(block)
			*c.target = func(context.Context, []byte, string) (string, error) {
				<-block
				return "late", nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := c.call(ctx, []byte("data"), c.arg); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a blocked cache write never blocks the caller past its context, got %v", err)
			}
		})
	}
}

func TestAsyncCacheWrapperPropagatesValidationErrors(t *testing.T) {
	orig := cacheImageFromBytes
	defer func() { cacheImageFromBytes = orig }()
	cacheImageFromBytes = func(context.Context, []byte, string) (string, error) {
		return "", &mediacache.ValueError{Msg: "invalid image"}
	}
	_, err := CacheImageFromBytesAsync(context.Background(), []byte("not-an-image"), ".png")
	var valueErr *mediacache.ValueError
	if !errors.As(err, &valueErr) || valueErr.Msg != "invalid image" {
		t.Fatalf("err = %v", err)
	}
}

func TestCacheMediaBytesAsyncForwardsArguments(t *testing.T) {
	orig := cacheMediaBytesFn
	defer func() { cacheMediaBytesFn = orig }()
	var call []string
	want := &CachedMedia{Path: "cached-media"}
	cacheMediaBytesFn = func(_ context.Context, data []byte, filename, mimeType, defaultKind string) (*CachedMedia, error) {
		call = []string{string(data), filename, mimeType, defaultKind}
		return want, nil
	}
	got, err := CacheMediaBytesAsync(context.Background(), []byte("payload"), "report.pdf", "application/pdf", "document")
	if err != nil || got != want || !reflect.DeepEqual(call, []string{"payload", "report.pdf", "application/pdf", "document"}) {
		t.Fatalf("got %+v %v call %v", got, err, call)
	}
}

func TestAsyncCacheWrapperUsesActiveProfileHome(t *testing.T) {
	profileHome := filepath.Join(t.TempDir(), "profile")
	t.Setenv("ICEBATH_HOME", profileHome)
	data := []byte("\x89PNG\r\n\x1a\nminimal")
	cached, err := CacheImageFromBytesAsync(context.Background(), data, ".png")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(cached) != filepath.Join(profileHome, "cache", "images") {
		t.Fatalf("cached under %q", cached)
	}
	if got, _ := os.ReadFile(cached); string(got) != string(data) {
		t.Fatalf("bytes %q", got)
	}
}

func boolPtr(v bool) *bool { return &v }

func TestMergePendingMessageEventMergesTextAndPhotoFollowups(t *testing.T) {
	source := &SessionSource{Platform: PlatformTelegram, ChatID: "12345", ChatType: "dm"}
	pending := map[string]*MessageEvent{}
	text := NewMessageEvent("first follow-up")
	text.Source = source
	photo := NewMessageEvent("see screenshot")
	photo.MessageType, photo.Source = MessageTypePhoto, source
	photo.MediaURLs, photo.MediaTypes = []string{"/tmp/test.png"}, []string{"image/png"}
	MergePendingMessageEvent(pending, "k", text, true)
	MergePendingMessageEvent(pending, "k", photo, true)
	merged := pending["k"]
	if merged.MessageType != MessageTypePhoto || merged.Text != "first follow-up\n\nsee screenshot" ||
		!reflect.DeepEqual(merged.MediaURLs, []string{"/tmp/test.png"}) || !reflect.DeepEqual(merged.MediaTypes, []string{"image/png"}) {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestPendingMediaMergePreservesPerAttachmentInlineContract(t *testing.T) {
	existing := NewMessageEvent("first")
	existing.MessageType = MessageTypePhoto
	existing.MediaURLs, existing.MediaTypes = []string{"/cache/image.png"}, []string{"image/png"}
	existing.SetPendingSTT("stale", []string{"t"})
	incoming := NewMessageEvent("second")
	incoming.MessageType = MessageTypeDocument
	incoming.MediaURLs, incoming.MediaTypes = []string{"/cache/notes.txt"}, []string{"text/plain"}
	incoming.MediaTextInlined = []*bool{boolPtr(false)}
	pending := map[string]*MessageEvent{"session": existing}
	MergePendingMessageEvent(pending, "session", incoming, false)
	if !reflect.DeepEqual(existing.MediaURLs, []string{"/cache/image.png", "/cache/notes.txt"}) {
		t.Fatalf("media %v", existing.MediaURLs)
	}
	if len(existing.MediaTextInlined) != 2 || existing.MediaTextInlined[0] != nil || *existing.MediaTextInlined[1] {
		t.Fatalf("inline flags %v", existing.MediaTextInlined)
	}
	if _, _, ok := existing.PendingSTT(); ok {
		t.Fatal("a merge drops the derived transcription cache")
	}
	if existing.Text != "first\n\nsecond" || existing.MessageType != MessageTypePhoto {
		t.Fatalf("text %q type %v", existing.Text, existing.MessageType)
	}
	same := NewMessageEvent("first")
	same.MessageType = MessageTypePhoto
	same.MediaURLs = []string{"/cache/b.png"}
	MergePendingMessageEvent(pending, "session", same, false)
	if existing.Text != "first\n\nsecond" || len(existing.MediaURLs) != 3 {
		t.Fatalf("an identical caption is not repeated, got %q", existing.Text)
	}
	plain := NewMessageEvent("replace")
	other := map[string]*MessageEvent{"k": NewMessageEvent("old")}
	MergePendingMessageEvent(other, "k", plain, false)
	if other["k"] != plain {
		t.Fatal("text without merge_text replaces the queued event")
	}
}

func TestOneTurnAnsweringSeveralMessagesIsAddressedIfAnyWas(t *testing.T) {
	for _, c := range []struct{ opener, absorbed, merged *bool }{
		{boolPtr(false), boolPtr(true), boolPtr(true)},
		{boolPtr(false), nil, nil},
		{boolPtr(true), boolPtr(false), boolPtr(true)},
		{boolPtr(false), boolPtr(false), boolPtr(false)},
	} {
		first := NewMessageEvent("hello")
		first.ReplyExpected = c.opener
		second := NewMessageEvent("hello")
		second.ReplyExpected = c.absorbed
		pending := map[string]*MessageEvent{"k": first}
		MergePendingMessageEvent(pending, "k", second, true)
		got := pending["k"].ReplyExpected
		if (got == nil) != (c.merged == nil) || got != nil && *got != *c.merged {
			t.Fatalf("opener %v absorbed %v merged %v", c.opener, c.absorbed, got)
		}
		if pending["k"].Text != "hello\nhello" {
			t.Fatalf("text follow ups append, got %q", pending["k"].Text)
		}
	}
}

func TestSlackResolveChannelSkills(t *testing.T) {
	bind := func(entries ...map[string]any) map[string]any {
		out := make([]any, len(entries))
		for i, e := range entries {
			out[i] = e
		}
		return map[string]any{"channel_skill_bindings": out}
	}
	if got, ok := ResolveChannelSkills(bind(map[string]any{"id": "D0ATH9TQ0G6", "skills": []any{"german-flashcards"}}), "D0ATH9TQ0G6", ""); !ok || !reflect.DeepEqual(got, []string{"german-flashcards"}) {
		t.Fatalf("dm channel match = %v %v", got, ok)
	}
	if _, ok := ResolveChannelSkills(bind(map[string]any{"id": "D0AAA", "skills": []any{"skill-a"}}), "D0BBB", ""); ok {
		t.Fatal("no match returns none")
	}
	if got, ok := ResolveChannelSkills(bind(map[string]any{"id": "D0ATH9TQ0G6", "skill": "german-flashcards"}), "D0ATH9TQ0G6", ""); !ok || !reflect.DeepEqual(got, []string{"german-flashcards"}) {
		t.Fatalf("single skill string = %v %v", got, ok)
	}
	if _, ok := ResolveChannelSkills(bind(map[string]any{"id": "D0ABC", "skills": []any{}}), "D0ABC", ""); ok {
		t.Fatal("empty skills list returns none")
	}
	threads := bind(map[string]any{"id": "P1", "skills": []any{" a ", "b", "a", 3, ""}}, map[string]any{"id": "T1", "skills": []any{"t"}})
	if got, _ := ResolveChannelSkills(threads, "T1", "P1"); !reflect.DeepEqual(got, []string{"t"}) {
		t.Fatalf("the exact channel wins over a parent listed first, got %v", got)
	}
	if got, _ := ResolveChannelSkills(threads, "T9", "P1"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("a thread inherits the deduplicated parent skills, got %v", got)
	}
}

func TestResolveChannelPrompt(t *testing.T) {
	extra := map[string]any{"channel_prompts": map[string]any{"C1": "  be brief  ", "P1": "parent", "BLANK": "   ", "N": 42}}
	for _, c := range []struct {
		channel, parent, want string
		ok                    bool
	}{
		{"C1", "P1", "be brief", true}, {"T1", "P1", "parent", true}, {"BLANK", "P1", "parent", true},
		{"N", "", "42", true}, {"X", "", "", false}, {"", "", "", false},
	} {
		if got, ok := ResolveChannelPrompt(extra, c.channel, c.parent); got != c.want || ok != c.ok {
			t.Fatalf("%s/%s = %q %v", c.channel, c.parent, got, ok)
		}
	}
	if _, ok := ResolveChannelPrompt(map[string]any{"channel_prompts": []any{"x"}}, "C1", ""); ok {
		t.Fatal("a non mapping channel_prompts resolves nothing")
	}
}

func TestCoercePlaintextGatewayCommand(t *testing.T) {
	for text, want := range map[string]string{
		"restart gateway": "/restart", "Please restart the gateway!": "/restart", "restart icebath gateway": "/restart",
		"restart icebath.": "/restart", "restart gateway": "/restart", "restart gateway now": "restart gateway now",
		"/status": "/status", "can you restart gateway": "can you restart gateway",
	} {
		ev := NewMessageEvent(text)
		ev.Source = &SessionSource{Platform: PlatformTelegram, ChatID: "1", ChatType: "dm"}
		CoercePlaintextGatewayCommand(ev)
		if ev.Text != want {
			t.Fatalf("%q became %q", text, ev.Text)
		}
	}
	group := NewMessageEvent("restart gateway")
	group.Source = &SessionSource{Platform: PlatformTelegram, ChatID: "1", ChatType: "group"}
	CoercePlaintextGatewayCommand(group)
	photo := NewMessageEvent("restart gateway")
	photo.MessageType = MessageTypePhoto
	photo.Source = &SessionSource{ChatType: "dm"}
	CoercePlaintextGatewayCommand(photo)
	CoercePlaintextGatewayCommand(nil)
	if group.Text != "restart gateway" || photo.Text != "restart gateway" {
		t.Fatal("only text in a DM is rewritten")
	}
}

func TestSendErrorTextClassifiers(t *testing.T) {
	if !isRetryableError("httpx.ConnectError: refused") || !isRetryableError("Network is unreachable") || isRetryableError("ReadTimeout") || isRetryableError("") {
		t.Fatal("retryable connection failures")
	}
	if !isTimeoutError("Request timed out") || !isTimeoutError("WriteTimeout") || isTimeoutError("ConnectError") {
		t.Fatal("timeouts")
	}
	if isRateLimitedError("") != (ClassifySendError(nil, "") == "rate_limited") || !isRateLimitedError("429 Too Many Requests") {
		t.Fatal("rate limits follow the shared classifier")
	}
	prompt := &ExecApprovalPrompt{Actions: []ExecApprovalAction{{"Allow once", "once", "primary"}, {"Deny", "deny", "danger"}}}
	if !reflect.DeepEqual(prompt.Choices(), []string{"once", "deny"}) {
		t.Fatalf("choices %v", prompt.Choices())
	}
	ttl := 5
	reply := NewEphemeralReply("done", &ttl)
	if reply.Text() != "done" || reply.String() != "done" || *reply.TTLSeconds != 5 {
		t.Fatal("ephemeral reply text")
	}
}

func TestTruncateMessage(t *testing.T) {
	if got := TruncateMessage("Hello world", 100, nil); !reflect.DeepEqual(got, []string{"Hello world"}) {
		t.Fatalf("short message %v", got)
	}
	exact := strings.Repeat("x", 100)
	if got := TruncateMessage(exact, 100, nil); !reflect.DeepEqual(got, []string{exact}) {
		t.Fatalf("exact length %v", got)
	}
	indicator := regexp.MustCompile(`\s*\(\d+/\d+\)$`)
	for _, maxLength := range []int{0, 1, 2} {
		done := make(chan []string, 1)
		go func() { done <- TruncateMessage("abcdefghij", maxLength, nil) }()
		select {
		case chunks := <-done:
			var b strings.Builder
			for _, c := range chunks {
				b.WriteString(indicator.ReplaceAllString(c, ""))
			}
			for _, ch := range "abcdefghij" {
				if !strings.ContainsRune(b.String(), ch) {
					t.Fatalf("char %q lost at max_length %d", ch, maxLength)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("truncate hung for max_length %d", maxLength)
		}
	}
	msg := "Start\n```javascript\n" + strings.Repeat("console.log('x');\n", 80) + "```\nEnd"
	chunks := TruncateMessage(msg, 300, nil)
	if len(chunks) < 2 {
		t.Fatalf("expected a split, got %d", len(chunks))
	}
	reopened := false
	for i, c := range chunks {
		if i > 0 && strings.Contains(c, "```javascript") {
			reopened = true
		}
		if utf8.RuneCountInString(c) > 300 || !strings.HasSuffix(c, fmt.Sprintf("(%d/%d)", i+1, len(chunks))) {
			t.Fatalf("chunk %d is %d long: %q", i, utf8.RuneCountInString(c), c)
		}
	}
	if !reopened {
		t.Fatal("no continuation chunk reopened with the language tag")
	}
	inline := TruncateMessage(strings.Repeat("a ", 20)+"`code span here` and more words after the span to force a split", 60, nil)
	if want := strings.TrimSpace(strings.Repeat("a ", 20)) + " (1/"; !strings.HasPrefix(inline[0], want) {
		t.Fatalf("the split moves before an unpaired inline code span, got %q", inline[0])
	}
}

func TestTruncateMessageUTF16(t *testing.T) {
	short := "Hello \U0001F600 world"
	if got := TruncateMessage(short, 4096, UTF16Len); !reflect.DeepEqual(got, []string{short}) {
		t.Fatalf("short emoji message %v", got)
	}
	msg := strings.Repeat("\U0001F600", 2049)
	if utf8.RuneCountInString(msg) != 2049 || UTF16Len(msg) != 4098 {
		t.Fatal("fixture lengths")
	}
	if got := TruncateMessage(msg, 4096, nil); len(got) != 1 {
		t.Fatalf("without a length function no split is expected, got %d", len(got))
	}
	chunks := TruncateMessage(msg, 4096, UTF16Len)
	if len(chunks) < 2 {
		t.Fatal("utf16 length splits the message")
	}
	for i, c := range chunks {
		if UTF16Len(c) > 4096 {
			t.Fatalf("chunk %d is %d UTF-16 units", i, UTF16Len(c))
		}
	}
}

func TestExtractImages(t *testing.T) {
	images, cleaned := ExtractImages("Just regular text.")
	if len(images) != 0 || cleaned != "Just regular text." {
		t.Fatalf("no images %v %q", images, cleaned)
	}
	images, cleaned = ExtractImages("Here is a photo: ![cat](https://example.com/cat.png)")
	if len(images) != 1 || images[0] != (ImageRef{"https://example.com/cat.png", "cat"}) || strings.Contains(cleaned, "![cat]") {
		t.Fatalf("markdown image %v %q", images, cleaned)
	}
	if images, _ = ExtractImages("![gen](https://fal.media/files/abc123/output.png)"); len(images) != 1 || images[0] != (ImageRef{"https://fal.media/files/abc123/output.png", "gen"}) {
		t.Fatalf("fal media %v", images)
	}
	if images, _ = ExtractImages("![](https://replicate.delivery/pbxt/abc/output)"); len(images) != 1 || images[0] != (ImageRef{"https://replicate.delivery/pbxt/abc/output", ""}) {
		t.Fatalf("replicate %v", images)
	}
	images, cleaned = ExtractImages(`Check this: <img src="https://example.com/photo.png">`)
	if len(images) != 1 || images[0] != (ImageRef{"https://example.com/photo.png", ""}) || strings.Contains(cleaned, "<img") {
		t.Fatalf("html image %v %q", images, cleaned)
	}
	images, cleaned = ExtractImages("Here is the image: ![photo](https://fal.media/cat.png)\nAnd a doc: ![report](https://example.com/report.pdf)")
	if len(images) != 1 || images[0].URL != "https://fal.media/cat.png" || !strings.Contains(cleaned, "![report](https://example.com/report.pdf)") {
		t.Fatalf("mixed %v %q", images, cleaned)
	}
	if !isAnimationURL("https://x.test/a.GIF?sig=1") || isAnimationURL("https://x.test/a.png?f=.gif") {
		t.Fatal("animation urls")
	}
	if open, lang := FenceStateAfter("text\n  ```go run\ncode", false, ""); !open || lang != "go" {
		t.Fatalf("fence open %v %q", open, lang)
	}
	if open, lang := FenceStateAfter("code\n```", true, "go"); open || lang != "" {
		t.Fatalf("fence closed %v %q", open, lang)
	}
}
