package messagetools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/telegram"
)

type botCall struct {
	method string
	kwargs map[string]any
}

// Bot double recording each call and answering from a per method queue of messages or errors
type fakeTelegramBot struct {
	mu      sync.Mutex
	calls   []botCall
	results map[string][]any
}

func (b *fakeTelegramBot) Send(_ context.Context, method string, kwargs map[string]any) (*telegramMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := map[string]any{}
	for k, v := range kwargs {
		cp[k] = v
	}
	b.calls = append(b.calls, botCall{method, cp})
	queue := b.results[method]
	if len(queue) == 0 {
		return &telegramMessage{MessageID: 1}, nil
	}
	next := queue[0]
	if len(queue) > 1 {
		b.results[method] = queue[1:]
	}
	if err, ok := next.(error); ok {
		return nil, err
	}
	return next.(*telegramMessage), nil
}

func (b *fakeTelegramBot) callsOf(method string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, c := range b.calls {
		if c.method == method {
			out = append(out, c.kwargs)
		}
	}
	return out
}

type botConstruction struct {
	token any
	proxy string
}

// Installs the bot double as the Bot constructor with proxy env cleared and an isolated home
func installTelegramBot(t *testing.T, bot *fakeTelegramBot) *[]botConstruction {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, v := range []string{"TELEGRAM_PROXY", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	gateway.SetGatewayRunnerRef(nil)
	var built []botConstruction
	prev := newTelegramBot
	newTelegramBot = func(token any, proxy string) (telegramBotAPI, error) {
		built = append(built, botConstruction{token, proxy})
		return bot, nil
	}
	t.Cleanup(func() { newTelegramBot = prev })
	return &built
}

func newBot(results map[string][]any) *fakeTelegramBot {
	if results == nil {
		results = map[string][]any{}
	}
	return &fakeTelegramBot{results: results}
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustSendTelegram(t *testing.T, chatID, message string, media []gateway.MediaRef, threadID *string, disablePreviews bool) map[string]any {
	t.Helper()
	res, err := sendTelegram(context.Background(), "tok", chatID, message, media, threadID, disablePreviews, false)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func strp(s string) *string { return &s }

// Installs a no op plugin scan, or the given one, for the test
func patchDiscovery(t *testing.T, fn func(ctx context.Context)) {
	t.Helper()
	prev := discoverPlugins
	if fn == nil {
		fn = func(context.Context) {}
	}
	discoverPlugins = fn
	t.Cleanup(func() { discoverPlugins = prev })
}

// Registers an entry for the test and unregisters it afterwards
func registerEntry(t *testing.T, entry *gateway.PlatformEntry) {
	t.Helper()
	ctx := context.Background()
	if err := gateway.Registry.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = gateway.Registry.Unregister(ctx, entry.Name, "") })
}

func TestSingleImageShortTextBecomesCaption(t *testing.T) {
	caption, body := mediaCaptionSplit("This Caption", []gateway.MediaRef{{Path: "/tmp/F22.png"}}, defaultCaptionLimit)
	if caption == nil || *caption != "This Caption" || body != "" {
		t.Fatalf("caption=%v body=%q", caption, body)
	}
}

func TestMultiFileKeepsSeparateBody(t *testing.T) {
	caption, body := mediaCaptionSplit("two photos", []gateway.MediaRef{{Path: "/tmp/a.png"}, {Path: "/tmp/b.png"}}, defaultCaptionLimit)
	if caption != nil || body != "two photos" {
		t.Fatalf("caption=%v body=%q", caption, body)
	}
}

func TestUnknownExtensionKeepsSeparateBody(t *testing.T) {
	caption, body := mediaCaptionSplit("some audio", []gateway.MediaRef{{Path: "/tmp/song.mp3"}}, defaultCaptionLimit)
	if caption != nil || body != "some audio" {
		t.Fatalf("caption=%v body=%q", caption, body)
	}
}

func TestImageCaptionRidesBubbleNoSeparateText(t *testing.T) {
	bot := newBot(map[string][]any{"send_photo": {&telegramMessage{MessageID: 2}}})
	installTelegramBot(t, bot)
	img := writeTemp(t, "a.png", []byte("x"))
	res := mustSendTelegram(t, "123", "This Caption", []gateway.MediaRef{{Path: img}}, nil, false)
	if res["success"] != true || len(bot.callsOf("send_message")) != 0 {
		t.Fatalf("res=%v calls=%v", res, bot.calls)
	}
	photos := bot.callsOf("send_photo")
	if len(photos) != 1 || photos[0]["caption"] != "This Caption" {
		t.Fatalf("photos=%v", photos)
	}
}

func TestMultiFileKeepsSeparateText(t *testing.T) {
	bot := newBot(nil)
	installTelegramBot(t, bot)
	dir := t.TempDir()
	img, img2 := filepath.Join(dir, "a.png"), filepath.Join(dir, "b.jpg")
	_ = os.WriteFile(img, []byte("x"), 0o644)
	_ = os.WriteFile(img2, []byte("x"), 0o644)
	res := mustSendTelegram(t, "123", "two pics", []gateway.MediaRef{{Path: img}, {Path: img2}}, nil, false)
	if res["success"] != true || len(bot.callsOf("send_message")) == 0 {
		t.Fatalf("res=%v", res)
	}
	photos := bot.callsOf("send_photo")
	if len(photos) != 2 {
		t.Fatalf("photos=%v", photos)
	}
	for _, p := range photos {
		if c, _ := p["caption"].(string); c != "" {
			t.Fatalf("caption leaked %v", p)
		}
	}
}

func TestSendTelegramStandaloneProxyProxyEnvPassedToHttpxRequest(t *testing.T) {
	bot := newBot(map[string][]any{"send_message": {&telegramMessage{MessageID: 42}}})
	built := installTelegramBot(t, bot)
	t.Setenv("TELEGRAM_PROXY", "socks5://127.0.0.1:1080")
	res := mustSendTelegram(t, "123", "hello world", nil, nil, false)
	if res["success"] != true || len(*built) != 1 || (*built)[0].token != "tok" || (*built)[0].proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("res=%v built=%v", res, *built)
	}
	if len(bot.callsOf("send_message")) != 1 {
		t.Fatalf("calls=%v", bot.calls)
	}
}

func TestSendTelegramStandaloneProxyNoProxyEnvUsesPlainBot(t *testing.T) {
	bot := newBot(map[string][]any{"send_message": {&telegramMessage{MessageID: 42}}})
	built := installTelegramBot(t, bot)
	res := mustSendTelegram(t, "123", "hello world", nil, nil, false)
	if res["success"] != true || len(*built) != 1 || (*built)[0].token != "tok" || (*built)[0].proxy != "" {
		t.Fatalf("res=%v built=%v", res, *built)
	}
	if len(bot.callsOf("send_message")) != 1 {
		t.Fatalf("calls=%v", bot.calls)
	}
}

func TestSendTelegramMediaDeliverySendsPhotoWithCaptionForMediaTag(t *testing.T) {
	img := writeTemp(t, "photo.png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...))
	bot := newBot(map[string][]any{"send_photo": {&telegramMessage{MessageID: 2}}})
	installTelegramBot(t, bot)
	res := mustSendTelegram(t, "12345", "Hello there", []gateway.MediaRef{{Path: img}}, nil, false)
	if res["success"] != true || res["message_id"] != "2" || len(bot.callsOf("send_message")) != 0 {
		t.Fatalf("res=%v calls=%v", res, bot.calls)
	}
	if photos := bot.callsOf("send_photo"); len(photos) != 1 || photos[0]["caption"] != "Hello there" {
		t.Fatalf("photos=%v", photos)
	}
}

func TestSendTelegramMediaDeliverySendsVoiceForOggWithVoiceDirective(t *testing.T) {
	voice := writeTemp(t, "voice.ogg", append([]byte("OggS"), make([]byte, 32)...))
	bot := newBot(map[string][]any{"send_voice": {&telegramMessage{MessageID: 7}}})
	installTelegramBot(t, bot)
	res := mustSendTelegram(t, "12345", "", []gateway.MediaRef{{Path: voice, IsVoice: true}}, nil, false)
	if res["success"] != true || len(bot.callsOf("send_voice")) != 1 || len(bot.callsOf("send_audio")) != 0 || len(bot.callsOf("send_message")) != 0 {
		t.Fatalf("res=%v calls=%v", res, bot.calls)
	}
}

func TestSendTelegramMediaDeliveryMissingMediaReturnsErrorWithoutLeakingRawTag(t *testing.T) {
	bot := newBot(nil)
	installTelegramBot(t, bot)
	res := mustSendTelegram(t, "12345", "", []gateway.MediaRef{{Path: "/tmp/does-not-exist.png"}}, nil, false)
	if _, ok := res["error"]; !ok || len(bot.callsOf("send_message")) != 0 {
		t.Fatalf("res=%v calls=%v", res, bot.calls)
	}
}

// Discord entry with the plugin's routing metadata and a recording standalone sender
func discordEntry(t *testing.T, sender func(ctx context.Context, kw map[string]any) (map[string]any, error)) {
	patchDiscovery(t, nil)
	e := gateway.NewPlatformEntry("discord", "Discord", nil, func() bool { return true })
	e.MaxMessageLength = 2000
	e.StandaloneSend = &gateway.StandaloneSendPolicy{Label: "Discord", Media: true, AllSends: true, Captionable: true, EmptyListBetweenChunks: true}
	e.StandaloneSenderFn = sender
	registerEntry(t, e)
}

func TestSendToPlatformChunkingLongMessageIsChunked(t *testing.T) {
	var sent []string
	discordEntry(t, func(_ context.Context, kw map[string]any) (map[string]any, error) {
		sent = append(sent, kw["message"].(string))
		return map[string]any{"success": true, "message_id": "1"}, nil
	})
	res, err := SendToPlatform(context.Background(), gateway.PlatformDiscord, &gateway.PlatformConfig{Enabled: true, Token: "***", Extra: map[string]any{}},
		"ch", strings.Repeat("word ", 1000), SendOptions{})
	if err != nil || res["success"] != true || len(sent) < 3 {
		t.Fatalf("res=%v err=%v sent=%d", res, err, len(sent))
	}
	for _, c := range sent {
		if utf8.RuneCountInString(c) > 2020 {
			t.Fatalf("chunk too long %d", len(c))
		}
	}
}

func TestSendToPlatformChunkingSignalLongMessageIsChunked(t *testing.T) {
	patchDiscovery(t, nil)
	var sent []string
	e := gateway.NewPlatformEntry("signal", "Signal", nil, func() bool { return true })
	e.MaxMessageLength = 8000
	e.StandaloneSend = &gateway.StandaloneSendPolicy{Label: "Signal", Media: true, EmptyListBetweenChunks: true, TextDirect: true}
	e.StandaloneSenderFn = func(_ context.Context, kw map[string]any) (map[string]any, error) {
		sent = append(sent, kw["message"].(string))
		return map[string]any{"success": true, "platform": "signal", "chat_id": kw["chat_id"]}, nil
	}
	registerEntry(t, e)
	long := strings.Repeat("word ", 8000/5+500)
	res, err := SendToPlatform(context.Background(), gateway.PlatformSignal, &gateway.PlatformConfig{Enabled: true,
		Extra: map[string]any{"http_url": "http://localhost:8080", "account": "+15551234567"}}, "+15557654321", long, SendOptions{})
	if err != nil || res["success"] != true || len(sent) < 2 {
		t.Fatalf("res=%v err=%v sent=%d", res, err, len(sent))
	}
	for _, c := range sent {
		if utf8.RuneCountInString(c) > 8000 || strings.Contains(c, "truncated, full output saved to") {
			t.Fatalf("bad chunk len %d", len(c))
		}
	}
}

func TestSendToPlatformChunkingTelegramMarkdownExpansionIsChunkedBeforeSend(t *testing.T) {
	var lengths []int
	bot := &fakeTelegramBot{results: map[string][]any{}}
	installTelegramBot(t, bot)
	lenBot := &lengthBot{inner: bot, lengths: &lengths}
	newTelegramBot = func(any, string) (telegramBotAPI, error) { return lenBot, nil }
	res, err := SendToPlatform(context.Background(), gateway.PlatformTelegram, &gateway.PlatformConfig{Enabled: true, Token: "tok", Extra: map[string]any{}},
		"123", strings.Repeat("!", 4096), SendOptions{})
	if err != nil || res["success"] != true || len(lengths) < 2 {
		t.Fatalf("res=%v err=%v lengths=%v", res, err, lengths)
	}
	for _, n := range lengths {
		if n > 4096 {
			t.Fatalf("send of %d units", n)
		}
	}
}

// Bot double refusing texts over the Telegram limit the way the Bot API does
type lengthBot struct {
	inner   *fakeTelegramBot
	lengths *[]int
}

func (b *lengthBot) Send(ctx context.Context, method string, kwargs map[string]any) (*telegramMessage, error) {
	n := gateway.UTF16Len(kwargs["text"].(string))
	*b.lengths = append(*b.lengths, n)
	if n > 4096 {
		return nil, errors.New("Message is too long")
	}
	return &telegramMessage{MessageID: len(*b.lengths)}, nil
}

// Adapter double recording sends, with optional connect and native image and document methods
type recordingAdapter struct {
	*gateway.BasePlatformAdapter
	mu    sync.Mutex
	calls [][]any
	id    string
}

func (a *recordingAdapter) record(c ...any) {
	a.mu.Lock()
	a.calls = append(a.calls, c)
	a.mu.Unlock()
}
func (a *recordingAdapter) Connect(context.Context, bool) (bool, error) {
	a.record("connect")
	return true, nil
}
func (a *recordingAdapter) Disconnect(context.Context) error { a.record("disconnect"); return nil }
func (a *recordingAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (gateway.SendResult, error) {
	a.record("send", chatID, content, metadata)
	return gateway.SendResult{Success: true, MessageID: a.id}, nil
}
func (a *recordingAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return nil, nil
}

type imageAdapter struct{ recordingAdapter }

func (a *imageAdapter) SendImageFile(_ context.Context, chatID, path, _, _ string, _, _ map[string]any) (gateway.SendResult, error) {
	a.record("send_image_file", chatID, path)
	return gateway.SendResult{Success: true, MessageID: "$img"}, nil
}

type documentAdapter struct{ recordingAdapter }

func (a *documentAdapter) SendDocument(_ context.Context, chatID, path, _, _, _ string, _, _ map[string]any) (gateway.SendResult, error) {
	a.record("send_document", chatID, path)
	return gateway.SendResult{Success: true, MessageID: "$doc"}, nil
}

// Runner double exposing only the default profile adapter map
type mapRunner struct{ adapters map[string]any }

func (r *mapRunner) Adapters() map[string]any { return r.adapters }

func TestMatrixMediaLiveAdapterReuseLiveAdapterSkipsConnectDisconnect(t *testing.T) {
	img := writeTemp(t, "photo.png", []byte("\x89PNG\r\n"))
	live := &imageAdapter{}
	live.id = "$text"
	live.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformMatrix, live)
	gateway.SetGatewayRunnerRef(&mapRunner{adapters: map[string]any{"matrix": live}})
	t.Cleanup(func() { gateway.SetGatewayRunnerRef(nil) })
	res, err := sendMatrixViaAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Token: "tok", Extra: map[string]any{}},
		"!room:example.com", "here is an image", []gateway.MediaRef{{Path: img}}, nil)
	if err != nil || res["success"] != true || res["message_id"] != "$img" {
		t.Fatalf("res=%v err=%v", res, err)
	}
	want := [][]any{{"send", "!room:example.com", "here is an image", map[string]any(nil)}, {"send_image_file", "!room:example.com", img}}
	if !reflect.DeepEqual(live.calls, want) {
		t.Fatalf("calls=%v", live.calls)
	}
}

func TestMatrixMediaLiveAdapterReuseLiveAdapterNotAvailableFallsBackToEphemeral(t *testing.T) {
	doc := writeTemp(t, "doc.pdf", []byte("%PDF-1.4"))
	gateway.SetGatewayRunnerRef(nil)
	ephemeral := &documentAdapter{}
	ephemeral.id = "$txt"
	ephemeral.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformMatrix, ephemeral)
	registerEntry(t, gateway.NewPlatformEntry("matrix", "Matrix", func(context.Context, *gateway.PlatformConfig) (any, error) {
		return ephemeral, nil
	}, func() bool { return true }))
	res, err := sendMatrixViaAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Token: "tok", Extra: map[string]any{}},
		"!room:example.com", "report attached", []gateway.MediaRef{{Path: doc}}, nil)
	if err != nil || res["success"] != true {
		t.Fatalf("res=%v err=%v", res, err)
	}
	want := [][]any{{"connect"}, {"send", "!room:example.com", "report attached", map[string]any(nil)}, {"send_document", "!room:example.com", doc}, {"disconnect"}}
	if !reflect.DeepEqual(ephemeral.calls, want) {
		t.Fatalf("calls=%v", ephemeral.calls)
	}
}

func TestSendTelegramHtmlDetectionHtmlMessageUsesHtmlParseMode(t *testing.T) {
	bot := newBot(nil)
	installTelegramBot(t, bot)
	mustSendTelegram(t, "123", "<b>Hello</b> world", nil, nil, false)
	calls := bot.callsOf("send_message")
	if len(calls) != 1 || calls[0]["parse_mode"] != "HTML" || calls[0]["text"] != "<b>Hello</b> world" {
		t.Fatalf("calls=%v", calls)
	}
}

func TestSendTelegramHtmlDetectionTransientBadGatewayRetriesTextSend(t *testing.T) {
	bot := newBot(map[string][]any{"send_message": {errors.New("502 Bad Gateway"), &telegramMessage{MessageID: 2}}})
	installTelegramBot(t, bot)
	sleeps := 0
	prev := telegramRetrySleep
	telegramRetrySleep = func(context.Context, float64) error { sleeps++; return nil }
	t.Cleanup(func() { telegramRetrySleep = prev })
	res := mustSendTelegram(t, "123", "hello", nil, nil, false)
	if res["success"] != true || len(bot.callsOf("send_message")) != 2 || sleeps != 1 {
		t.Fatalf("res=%v sleeps=%d", res, sleeps)
	}
}

func TestSendTelegramThreadIdMappingGeneralTopicThreadIdOmitted(t *testing.T) {
	bot := newBot(nil)
	installTelegramBot(t, bot)
	mustSendTelegram(t, "-1001234567890", "hello", nil, strp("1"), false)
	calls := bot.callsOf("send_message")
	if _, has := calls[0]["message_thread_id"]; len(calls) != 1 || has {
		t.Fatalf("calls=%v", calls)
	}
}

func TestSendTelegramThreadIdMappingNonGeneralTopicThreadIdPreserved(t *testing.T) {
	bot := newBot(nil)
	installTelegramBot(t, bot)
	mustSendTelegram(t, "-1001234567890", "hello", nil, strp("17585"), false)
	if calls := bot.callsOf("send_message"); calls[len(calls)-1]["message_thread_id"] != 17585 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestSendTelegramThreadIdMappingThreadNotFoundRetriesWithoutMessageThreadId(t *testing.T) {
	bot := newBot(map[string][]any{"send_message": {errors.New("Bad Request: message thread not found"), &telegramMessage{MessageID: 2}}})
	installTelegramBot(t, bot)
	mustSendTelegram(t, "-1001234567890", "hello", nil, strp("17585"), false)
	calls := bot.callsOf("send_message")
	if len(calls) != 2 || calls[0]["message_thread_id"] != 17585 {
		t.Fatalf("calls=%v", calls)
	}
	if _, has := calls[1]["message_thread_id"]; has {
		t.Fatalf("retry kept thread %v", calls[1])
	}
}

func TestSendTelegramThreadIdMappingThreadNotFoundForMediaRetriesWithoutMessageThreadId(t *testing.T) {
	bot := newBot(map[string][]any{"send_document": {errors.New("Bad Request: message thread not found"), &telegramMessage{MessageID: 3}}})
	installTelegramBot(t, bot)
	doc := writeTemp(t, "doc.txt", []byte("test content"))
	mustSendTelegram(t, "-1001234567890", "", []gateway.MediaRef{{Path: doc}}, strp("17585"), false)
	calls := bot.callsOf("send_document")
	if len(calls) != 2 || calls[0]["message_thread_id"] != 17585 {
		t.Fatalf("calls=%v", calls)
	}
	if _, has := calls[1]["message_thread_id"]; has {
		t.Fatalf("retry kept thread %v", calls[1])
	}
}

func TestSendToPlatformDiscordMediaMediaFilesPassedOnLastChunkOnly(t *testing.T) {
	var log []any
	discordEntry(t, func(_ context.Context, kw map[string]any) (map[string]any, error) {
		log = append(log, kw["media_files"])
		return map[string]any{"success": true, "platform": "discord", "chat_id": kw["chat_id"], "message_id": "1"}, nil
	})
	long := strings.Repeat("A", 1900) + " " + strings.Repeat("B", 1900)
	media := []gateway.MediaRef{{Path: "/fake/img.png"}}
	res, err := SendToPlatform(context.Background(), gateway.PlatformDiscord, &gateway.PlatformConfig{Enabled: true, Token: "tok", Extra: map[string]any{}},
		"999", long, SendOptions{MediaFiles: media})
	if err != nil || res["success"] != true || len(log) != 2 {
		t.Fatalf("res=%v err=%v log=%v", res, err, log)
	}
	if first, _ := log[0].([]gateway.MediaRef); len(first) != 0 || !reflect.DeepEqual(log[1], media) {
		t.Fatalf("log=%v", log)
	}
}

// Ntfy adapter double recording the send it receives
type ntfyAdapter struct{ recordingAdapter }

func TestSendViaAdapterStandaloneFallbackLiveNtfyAdapterReceivesExplicitPublishTopic(t *testing.T) {
	a := &ntfyAdapter{}
	a.id = "ntfy-id"
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.Platform("ntfy"), a)
	gateway.SetGatewayRunnerRef(&mapRunner{adapters: map[string]any{"ntfy": a}})
	t.Cleanup(func() { gateway.SetGatewayRunnerRef(nil) })
	res, err := sendViaAdapter(context.Background(), gateway.Platform("ntfy"), &gateway.PlatformConfig{Extra: map[string]any{"publish_topic": "configured-topic"}},
		"alerts-channel", "done", nil, nil, false)
	if err != nil || !reflect.DeepEqual(res, map[string]any{"success": true, "message_id": "ntfy-id"}) {
		t.Fatalf("res=%v err=%v", res, err)
	}
	want := [][]any{{"send", "alerts-channel", "done", map[string]any{"publish_topic": "alerts-channel"}}}
	if !reflect.DeepEqual(a.calls, want) {
		t.Fatalf("calls=%v", a.calls)
	}
}

func fakePlatformEntry(send func(ctx context.Context, kw map[string]any) (map[string]any, error)) *gateway.PlatformEntry {
	e := gateway.NewPlatformEntry("fakeplatform", "Fake", func(context.Context, *gateway.PlatformConfig) (any, error) { return nil, nil },
		func() bool { return true })
	e.StandaloneSenderFn = send
	return e
}

func TestSendViaAdapterStandaloneFallbackStandaloneSenderFnRaisesIsCaughtAndFormatted(t *testing.T) {
	patchDiscovery(t, nil)
	gateway.SetGatewayRunnerRef(nil)
	registerEntry(t, fakePlatformEntry(func(context.Context, map[string]any) (map[string]any, error) {
		return nil, errors.New("boom!")
	}))
	res, err := sendViaAdapter(context.Background(), gateway.Platform("fakeplatform"), &gateway.PlatformConfig{Extra: map[string]any{}}, "chat-1", "hi", nil, nil, false)
	if err != nil || len(res) != 1 || !strings.Contains(res["error"].(string), "boom!") {
		t.Fatalf("res=%v err=%v", res, err)
	}
}

func TestSendViaAdapterStandaloneFallbackFreshProcessDiscoversPluginsBeforeTheFallback(t *testing.T) {
	gateway.SetGatewayRunnerRef(nil)
	var sent [][2]string
	entry := fakePlatformEntry(func(_ context.Context, kw map[string]any) (map[string]any, error) {
		sent = append(sent, [2]string{kw["chat_id"].(string), kw["message"].(string)})
		return map[string]any{"success": true, "platform": "fakeplatform", "chat_id": kw["chat_id"]}, nil
	})
	patchDiscovery(t, func(ctx context.Context) { _ = gateway.Registry.Register(ctx, entry, "") })
	t.Cleanup(func() { _, _ = gateway.Registry.Unregister(context.Background(), "fakeplatform", "") })
	res, err := sendViaAdapter(context.Background(), gateway.Platform("fakeplatform"), &gateway.PlatformConfig{Extra: map[string]any{}}, "chat-1", "hi", nil, nil, false)
	if err != nil || res["success"] != true || !reflect.DeepEqual(sent, [][2]string{{"chat-1", "hi"}}) {
		t.Fatalf("res=%v err=%v sent=%v", res, err, sent)
	}
}

func TestSendTelegramThreadNotFoundRetryDisableWebPagePreviewNotLeakedToMediaSends(t *testing.T) {
	bot := newBot(map[string][]any{"send_document": {&telegramMessage{MessageID: 2}}})
	installTelegramBot(t, bot)
	pdf := writeTemp(t, "f.pdf", []byte("%PDF-1.4 test content"))
	res := mustSendTelegram(t, "-100123", "check preview", []gateway.MediaRef{{Path: pdf}}, nil, true)
	docs := bot.callsOf("send_document")
	if res["success"] != true || len(docs) == 0 || docs[0]["caption"] != "check preview" {
		t.Fatalf("res=%v docs=%v", res, docs)
	}
	if _, has := docs[0]["disable_web_page_preview"]; has {
		t.Fatalf("disable_web_page_preview leaked into send_document kwargs")
	}
}

func TestTelegramHTTPBotMapsBotAPIFailures(t *testing.T) {
	cases := []struct {
		code    int
		body    string
		kind    string
		message string
	}{
		{400, `{"ok":false,"description":"Bad Request: message thread not found"}`, "BadRequest", "Message thread not found"},
		{429, `{"ok":false,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`, "RetryAfter", "Flood control exceeded. Retry in 3 seconds"},
		{502, `{"ok":false}`, "NetworkError", "Bad Gateway"},
		{403, `{"ok":false,"description":"Forbidden: bot was blocked by the user"}`, "Forbidden", "Forbidden: bot was blocked by the user"},
		{500, `{"ok":false,"description":"Internal"}`, "NetworkError", "Internal (500)"},
	}
	for _, c := range cases {
		_, err := parseTelegramResponse(c.code, []byte(c.body))
		var te *telegramError
		if !errors.As(err, &te) || te.Kind != c.kind || te.Message != c.message {
			t.Fatalf("%d: %#v", c.code, err)
		}
	}
	msg, err := parseTelegramResponse(200, []byte(`{"ok":true,"result":{"message_id":77}}`))
	if err != nil || msg.MessageID != int64(77) {
		t.Fatalf("msg=%v err=%v", msg, err)
	}
	if d, ok := telegramRetryDelay(&telegramError{Kind: "RetryAfter", Message: "x", RetryAfter: new(float64)}, 0); !ok || d != 0 {
		t.Fatalf("retry after delay %v %v", d, ok)
	}
}

func sendOneVideo(t *testing.T, bot *fakeTelegramBot, path string) {
	t.Helper()
	if _, err := telegramSendOneMedia(context.Background(), bot, "1", path, false, nil, nil, false, map[string]any{}, false); err != nil {
		t.Fatal(err)
	}
}

func TestVideoSendCarriesGeometryAndThumbnail(t *testing.T) {
	thumb := writeTemp(t, "thumb.jpg", append([]byte("\xff\xd8\xff\xe0"), make([]byte, 16)...))
	pg, pt := probeVideoGeometry, videoThumbnailJPEG
	t.Cleanup(func() { probeVideoGeometry, videoThumbnailJPEG = pg, pt })
	d := 59
	probeVideoGeometry = func(context.Context, string) (telegram.VideoGeometry, bool) {
		return telegram.VideoGeometry{Width: 720, Height: 1280, Duration: &d}, true
	}
	videoThumbnailJPEG = func(context.Context, string, *int) string { return thumb }
	video := writeTemp(t, "clip.mp4", make([]byte, 32))
	bot := newBot(nil)
	sendOneVideo(t, bot, video)
	calls := bot.callsOf("send_video")
	if len(calls) != 1 || calls[0]["width"] != 720 || calls[0]["height"] != 1280 || calls[0]["duration"] != 59 || calls[0]["thumbnail"] != thumb {
		t.Fatalf("calls=%v", calls)
	}
	if pathExists(thumb) {
		t.Fatal("the temporary thumbnail must not be left behind")
	}
}

func TestVideoSendWithoutProbeableFileStillSends(t *testing.T) {
	pg := probeVideoGeometry
	t.Cleanup(func() { probeVideoGeometry = pg })
	probeVideoGeometry = func(context.Context, string) (telegram.VideoGeometry, bool) { return telegram.VideoGeometry{}, false }
	video := writeTemp(t, "clip.mp4", make([]byte, 16))
	bot := newBot(nil)
	sendOneVideo(t, bot, video)
	calls := bot.callsOf("send_video")
	_, hasW := calls[0]["width"]
	_, hasT := calls[0]["thumbnail"]
	if len(calls) != 1 || hasW || hasT {
		t.Fatalf("calls=%v", calls)
	}
}
