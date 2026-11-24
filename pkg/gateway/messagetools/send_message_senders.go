package messagetools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/telegram"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

func extSet(exts ...string) map[string]bool {
	out := map[string]bool{}
	for _, e := range exts {
		out[e] = true
	}
	return out
}

var (
	imageExts             = extSet(".jpg", ".jpeg", ".png", ".webp", ".gif")
	videoExts             = extSet(".mp4", ".mov", ".avi", ".mkv", ".webm", ".3gp")
	audioExts             = extSet(".ogg", ".opus", ".mp3", ".m2a", ".wav", ".m4a", ".flac")
	voiceExts             = extSet(".ogg", ".opus")
	telegramSendAudioExts = extSet(".mp3", ".m4a")
	captionableExts       = extSet(".jpg", ".jpeg", ".png", ".webp", ".gif", ".mp4", ".mov", ".avi", ".mkv", ".webm", ".3gp",
		".pdf", ".doc", ".docx", ".txt", ".md", ".csv", ".xlsx", ".zip")
	telegramTransientMarkers = []string{"bad gateway", "502", "too many requests", "429", "service unavailable", "503",
		"gateway timeout", "504"}
	urlSecretQueryRE      = mustPyRE(`(?i)([?&](?:access_token|api[_-]?key|auth[_-]?token|token|signature|sig)=)([^&#\s]+)`)
	genericSecretAssignRE = mustPyRE(`(?i)\b(access_token|api[_-]?key|auth[_-]?token|signature|sig)\s*=\s*([^\s,;]+)`)
	telegramHTMLTagRE     = mustPyRE(`<[a-zA-Z/][^>]*>`)
)

const (
	telegramCaptionLimit = 1024
	defaultCaptionLimit  = 4096
	noDeliverable        = "No deliverable text or media remained after processing MEDIA tags"
)

// Telegram adapter media probes, replaced by tests that patch the adapter helpers
var (
	probeVoiceDuration = telegram.ProbeVoiceDurationSeconds
	probeVideoGeometry = telegram.ProbeVideoGeometry
	videoThumbnailJPEG = telegram.VideoThumbnailJPEG
)

// Sleep between Telegram retries, replaced by tests that patch asyncio.sleep
var telegramRetrySleep = func(ctx context.Context, seconds float64) error {
	t := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Idempotent plugin scan of the active profile, replaced by tests that patch discover_plugins
var discoverPlugins = func(ctx context.Context) { plugins.DiscoverPlugins(ctx, false) }

func init() {
	gateway.SendMessageSendersError = sendError
	gateway.SendMessageSendersLiveAdapter = func(ctx context.Context, platform gateway.Platform) gateway.PlatformAdapter {
		_, adapter := liveAdapter(ctx, platform, "")
		live, _ := adapter.(gateway.PlatformAdapter)
		return live
	}
}

func mustPyRE(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

func pySub(re *regexp2.Regexp, text string, repl func(m regexp2.Match) string) string {
	out, err := re.ReplaceFunc(text, repl, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Lowercased extension of a path
func lowerExt(path string) string {
	_, ext := pathutil.SplitExt(path)
	return strings.ToLower(ext)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Stripped text as the caption of exactly one captionable non voice file it fits, else nil and the text
func mediaCaptionSplit(text string, media []gateway.MediaRef, maxCaptionLen int) (*string, string) {
	stripped := strings.TrimSpace(text)
	if stripped == "" || len(media) != 1 || utf8.RuneCountInString(stripped) > maxCaptionLen || media[0].IsVoice ||
		!captionableExts[lowerExt(media[0].Path)] {
		return nil, text
	}
	return &stripped, ""
}

// Error text with secrets, token query values and secret assignments redacted
func sanitizeErrorText(text any) string {
	redacted := pySub(urlSecretQueryRE, tools.RedactSensitiveText(anyutil.String(text)), func(m regexp2.Match) string {
		return m.GroupByNumber(1).String() + "***"
	})
	return pySub(genericSecretAssignRE, redacted, func(m regexp2.Match) string { return m.GroupByNumber(1).String() + "=***" })
}

// Error payload with redacted message
func sendError(message string) map[string]any {
	return map[string]any{"error": sanitizeErrorText(message)}
}

// Success payload with the fields and the warnings only when there are any
func sendSuccess(platform string, chatID any, warnings []string, fields map[string]any) map[string]any {
	out := map[string]any{"success": true, "platform": platform, "chat_id": chatID}
	for k, v := range fields {
		out[k] = v
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return out
}

// Seconds to wait before retrying a Telegram send, false when the failure is final
func telegramRetryDelay(err error, attempt int) (float64, bool) {
	var te *telegramError
	if errors.As(err, &te) && te.RetryAfter != nil {
		return max(*te.RetryAfter, 0.0), true
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "timed out") || strings.Contains(text, "timeout") {
		return 0, false
	}
	for _, marker := range telegramTransientMarkers {
		if strings.Contains(text, marker) {
			return float64(int64(1) << attempt), true
		}
	}
	return 0, false
}

// Send message with bounded retries on transient failures
func sendTelegramMessageWithRetry(ctx context.Context, bot telegramBotAPI, attempts int, kwargs map[string]any) (*telegramMessage, error) {
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		msg, err := bot.Send(ctx, "send_message", kwargs)
		if err == nil {
			return msg, nil
		}
		lastErr = err
		delay, ok := telegramRetryDelay(err, attempt)
		if !ok || attempt >= attempts-1 {
			return nil, err
		}
		pkgLog().Warn(fmt.Sprintf("Transient Telegram send failure (attempt %d/%d), retrying in %.1fs: %s",
			attempt+1, attempts, delay, sanitizeErrorText(err)))
		if err := telegramRetrySleep(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// Whether a Telegram error reports a missing forum topic
func isTelegramThreadNotFound(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "thread not found")
}

// Bot honouring TELEGRAM_PROXY, a direct connection when attaching the proxy fails
func telegramBot(ctx context.Context, token any) (telegramBotAPI, error) {
	proxy := ""
	bot, err := func() (telegramBotAPI, error) {
		var err error
		if proxy, err = gateway.ResolveProxyURL(ctx, "TELEGRAM_PROXY", []string{"api.telegram.org"}, ""); err != nil {
			return nil, err
		}
		if proxy == "" {
			return newTelegramBot(token, "")
		}
		pkgLog().Info(fmt.Sprintf("send_message: standalone Telegram send routed through proxy %s", gateway.SafeProxyURLForLog(proxy)))
		return newTelegramBot(token, proxy)
	}()
	if err == nil {
		return bot, nil
	}
	pkgLog().Warn(fmt.Sprintf("send_message: failed to attach Telegram proxy (%s), falling back to direct connection",
		cmp.Or(gateway.DescribeProxyError(err, proxy), sanitizeErrorText(err))))
	return newTelegramBot(token, "")
}

// Message thread keyword of a topic id, none for no topic or the forum General topic
func telegramThreadKwargs(threadID *string) (map[string]any, error) {
	out := map[string]any{}
	if threadID == nil || *threadID == "" || *threadID == "1" {
		return out, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(*threadID), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("the Telegram thread id %s is not a whole number", *threadID)
	}
	out["message_thread_id"] = int(n)
	return out, nil
}

// Adapter media method and kind, a document when forced, else image, video or voice by extension
func adapterMediaMethod(ext string, voice, forceDocument bool) (string, string) {
	switch {
	case forceDocument:
		return "send_document", "document"
	case imageExts[ext]:
		return "send_image_file", "image"
	case videoExts[ext]:
		return "send_video", "video"
	case voice:
		return "send_voice", "audio"
	}
	return "send_document", "document"
}

// Bot API media upload by extension, photo unless forced, video, voice note, audio for mp3 and m4a, else document
func telegramSendMedia(ctx context.Context, bot telegramBotAPI, chatID any, f *os.File, ext string, isVoice, forceDocument bool, kwargs map[string]any) (*telegramMessage, error) {
	kind := "document"
	switch {
	case !forceDocument && imageExts[ext]:
		kind = "photo"
	case videoExts[ext]:
		kind = "video"
	case isVoice && voiceExts[ext]:
		kind = "voice"
	case telegramSendAudioExts[ext]:
		kind = "audio"
	}
	call := map[string]any{"chat_id": chatID, kind: f}
	for k, v := range kwargs {
		call[k] = v
	}
	return bot.Send(ctx, "send_"+kind, call)
}

// One text chunk retried without the topic when it is gone and as plain text when parsing fails
// MarkdownV2 escapes stripped for the plain text fallback, the text unchanged when stripping fails
func stripMdv2Safe(text string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = text
		}
	}()
	return telegram.StripMdv2(text)
}

func telegramSendTextChunk(ctx context.Context, bot telegramBotAPI, chatID any, chunk string, parseMode any, hasHTML bool, textKwargs map[string]any) (*telegramMessage, error) {
	send := func(text string, mode any) (*telegramMessage, error) {
		kwargs := map[string]any{"chat_id": chatID, "text": text, "parse_mode": mode}
		for k, v := range textKwargs {
			kwargs[k] = v
		}
		return sendTelegramMessageWithRetry(ctx, bot, 3, kwargs)
	}
	msg, err := send(chunk, parseMode)
	if err == nil {
		return msg, nil
	}
	if isTelegramThreadNotFound(err) && textKwargs["message_thread_id"] != nil {
		tid := textKwargs["message_thread_id"]
		delete(textKwargs, "message_thread_id")
		pkgLog().Warn(fmt.Sprintf("Thread %s not found in _send_telegram, retrying without message_thread_id", anyutil.String(tid)))
		return send(chunk, parseMode)
	}
	errText := strings.ToLower(err.Error())
	if strings.Contains(errText, "parse") || strings.Contains(errText, "markdown") || strings.Contains(errText, "html") {
		pkgLog().Warn(fmt.Sprintf("Parse mode %s failed in _send_telegram, falling back to plain text: %s",
			anyutil.String(parseMode), sanitizeErrorText(err)))
		if hasHTML {
			return send(chunk, nil)
		}
		return send(stripMdv2Safe(chunk), nil)
	}
	return nil, err
}

// One file upload retried without the topic when it is gone and with a plain caption when parsing fails
func telegramSendOneMedia(ctx context.Context, bot telegramBotAPI, chatID any, mediaPath string, isVoice bool, caption *string, parseMode any, hasHTML bool, threadKwargs map[string]any, forceDocument bool) (*telegramMessage, error) {
	ext := lowerExt(mediaPath)
	voiceNote := voiceExts[ext] && isVoice
	mediaKwargs := map[string]any{}
	for k, v := range threadKwargs {
		mediaKwargs[k] = v
	}
	if caption != nil && !voiceNote {
		mediaKwargs["caption"], mediaKwargs["parse_mode"] = *caption, parseMode
	}
	if voiceNote || telegramSendAudioExts[ext] {
		if duration, ok := probeVoiceDuration(ctx, mediaPath); ok {
			mediaKwargs["duration"] = duration
		}
	}
	thumbPath := ""
	if videoExts[ext] {
		if geometry, ok := probeVideoGeometry(ctx, mediaPath); ok {
			mediaKwargs["width"], mediaKwargs["height"] = geometry.Width, geometry.Height
			var duration *int
			if geometry.Duration != nil {
				mediaKwargs["duration"] = *geometry.Duration
				duration = geometry.Duration
			}
			if thumbPath = videoThumbnailJPEG(ctx, mediaPath, duration); thumbPath != "" {
				mediaKwargs["thumbnail"] = thumbPath
			}
		}
	}
	if thumbPath != "" {
		defer os.Remove(thumbPath)
	}
	f, err := os.Open(mediaPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	msg, err := telegramSendMedia(ctx, bot, chatID, f, ext, isVoice, forceDocument, mediaKwargs)
	if err == nil {
		return msg, nil
	}
	errText := strings.ToLower(err.Error())
	switch {
	case isTelegramThreadNotFound(err) && anyutil.IsTrue(mediaKwargs["message_thread_id"]):
		tid := mediaKwargs["message_thread_id"]
		delete(mediaKwargs, "message_thread_id")
		pkgLog().Warn(fmt.Sprintf("Thread %s not found for media send, retrying without message_thread_id", anyutil.String(tid)))
	case anyutil.IsTrue(mediaKwargs["parse_mode"]) && (strings.Contains(errText, "parse") || strings.Contains(errText, "caption")):
		pkgLog().Warn(fmt.Sprintf("Caption parse failed for media send, retrying plain: %s", sanitizeErrorText(err)))
		delete(mediaKwargs, "parse_mode")
		if c, _ := mediaKwargs["caption"].(string); !hasHTML && c != "" {
			mediaKwargs["caption"] = stripMdv2Safe(c)
		}
	default:
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	return telegramSendMedia(ctx, bot, chatID, f, ext, isVoice, forceDocument, mediaKwargs)
}

// Formatted text, parse mode and whether it is HTML, Markdown converted to MarkdownV2 otherwise
func telegramFormat(message string) (string, any, bool) {
	if ok, _ := telegramHTMLTagRE.MatchString(message); ok {
		return message, "HTML", true
	}
	return telegram.FormatMessage(message), "MarkdownV2", false
}

// One shot Telegram Bot API send of text chunks and media, parse failures falling back to plain text
func sendTelegram(ctx context.Context, token any, chatID, message string, mediaFiles []gateway.MediaRef, threadID *string, disableLinkPreviews, forceDocument bool) (map[string]any, error) {
	result, err := func() (map[string]any, error) {
		formatted, sendParseMode, hasHTML := telegramFormat(message)
		bot, err := telegramBot(ctx, token)
		if err != nil {
			return nil, err
		}
		intChatID := telegram.NormalizeTelegramChatID(chatID)
		threadKwargs, err := telegramThreadKwargs(threadID)
		if err != nil {
			return nil, err
		}
		textKwargs := map[string]any{}
		for k, v := range threadKwargs {
			textKwargs[k] = v
		}
		if disableLinkPreviews {
			textKwargs["disable_web_page_preview"] = true
		}
		var lastMsg *telegramMessage
		var warnings []string
		var tgCaption *string
		if cap, _ := mediaCaptionSplit(message, mediaFiles, telegramCaptionLimit); cap != nil && gateway.UTF16Len(formatted) <= telegramCaptionLimit {
			f := formatted
			tgCaption, formatted = &f, ""
		}
		if strings.TrimSpace(formatted) != "" {
			for _, chunk := range gateway.TruncateMessage(formatted, 4096, gateway.UTF16Len) {
				if lastMsg, err = telegramSendTextChunk(ctx, bot, intChatID, chunk, sendParseMode, hasHTML, textKwargs); err != nil {
					return nil, err
				}
			}
		}
		for _, m := range mediaFiles {
			if !pathExists(m.Path) {
				warnings = append(warnings, "Media file not found, skipping: "+m.Path)
				pkgLog().Warn(warnings[len(warnings)-1])
				if tgCaption != nil && lastMsg == nil {
					kwargs := map[string]any{"chat_id": intChatID, "text": *tgCaption, "parse_mode": sendParseMode}
					for k, v := range textKwargs {
						kwargs[k] = v
					}
					if msg, capErr := sendTelegramMessageWithRetry(ctx, bot, 3, kwargs); capErr != nil {
						pkgLog().Warn(fmt.Sprintf("Telegram caption-fallback send failed for missing media: %s", sanitizeErrorText(capErr)))
					} else {
						lastMsg, tgCaption = msg, nil
					}
				}
				continue
			}
			msg, mediaErr := telegramSendOneMedia(ctx, bot, intChatID, m.Path, m.IsVoice, tgCaption, sendParseMode, hasHTML, threadKwargs, forceDocument)
			if mediaErr != nil {
				if errors.Is(mediaErr, context.Canceled) {
					return nil, mediaErr
				}
				warnings = append(warnings, sanitizeErrorText(fmt.Sprintf("Failed to send media %s: %s", m.Path, mediaErr)))
				pkgLog().Error(warnings[len(warnings)-1])
				continue
			}
			lastMsg = msg
		}
		if lastMsg == nil {
			out := map[string]any{"error": noDeliverable}
			if len(warnings) > 0 {
				out["warnings"] = warnings
			}
			return out, nil
		}
		return sendSuccess("telegram", chatID, warnings, map[string]any{"message_id": anyutil.String(lastMsg.MessageID)}), nil
	}()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return sendError(fmt.Sprintf("Telegram send failed: %s", err)), nil
	}
	return result, nil
}

// Default profile adapter registry of a bare runner
type adapterMapRunner interface {
	Adapters() *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter]
}

// Runner and the active profile's adapter of the in process gateway, nil adapter when none or the lookup fails
func liveAdapter(ctx context.Context, platform gateway.Platform, lookupFailedWarning string) (runner, adapter any) {
	runner = gateway.CurrentGatewayRunner()
	if runner == nil {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			if lookupFailedWarning != "" {
				pkgLog().Warn(lookupFailedWarning, "error", fmt.Sprint(r))
			}
			adapter = nil
		}
	}()
	resolve, ok := runner.(gateway.AuthorizationAdapterRunner)
	if !ok {
		registry, ok := runner.(adapterMapRunner)
		if !ok {
			panic("the gateway runner gives no access to its platform adapters")
		}
		live, _ := registry.Adapters().Get(platform)
		if live == nil {
			return runner, nil
		}
		return runner, live
	}
	profile := profiles.GetActiveProfileName(ctx)
	if live := resolve.AuthorizationAdapter(ctx, platform, &profile); live != nil {
		return runner, live
	}
	return runner, nil
}

// Standalone sender of a registered plugin after the plugin scan, else the error payload naming it
func pluginStandaloneSender(ctx context.Context, platformName, label string, discover bool) (func(context.Context, map[string]any) (map[string]any, error), map[string]any) {
	if discover {
		discoverPlugins(ctx)
	}
	entry := gateway.Registry.Get(ctx, platformName)
	if entry == nil || entry.StandaloneSenderFn == nil {
		if label == "" {
			label = platformName
		}
		return nil, map[string]any{"error": label + " plugin not registered or missing standalone_sender_fn"}
	}
	return entry.StandaloneSenderFn, nil
}

// Keyword arguments of one standalone sender call
func standaloneKwargs(pconfig *gateway.PlatformConfig, chatID, message string, kw map[string]any) map[string]any {
	out := map[string]any{"pconfig": pconfig, "chat_id": chatID, "message": message}
	for k, v := range kw {
		out[k] = v
	}
	return out
}

// nil for a nil thread id, else its text
func optString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// nil for a nil media list, else the list
func mediaArg(media []gateway.MediaRef) any {
	if media == nil {
		return nil
	}
	return media
}

// One shot text send through a plugin standalone sender
func registryStandaloneSend(ctx context.Context, platformName string, pconfig *gateway.PlatformConfig, chatID, message string, threadID *string) (map[string]any, error) {
	sender, errPayload := pluginStandaloneSender(ctx, platformName, "", true)
	if errPayload != nil {
		return errPayload, nil
	}
	return sender(ctx, standaloneKwargs(pconfig, chatID, message, map[string]any{"thread_id": optString(threadID)}))
}

// Matrix send through the live gateway adapter, else an ephemeral adapter connected for this send
func sendMatrixViaAdapter(ctx context.Context, pconfig *gateway.PlatformConfig, chatID, message string, mediaFiles []gateway.MediaRef, threadID *string) (map[string]any, error) {
	var metadata map[string]any
	if threadID != nil && *threadID != "" {
		metadata = map[string]any{"thread_id": *threadID}
	}
	_, live := liveAdapter(ctx, gateway.PlatformMatrix,
		"Matrix: live gateway adapter lookup failed; falling back to an ephemeral connect (may re-init E2EE per send)")
	if live != nil {
		return matrixSendCore(ctx, live, chatID, message, mediaFiles, metadata)
	}
	entry := gateway.Registry.Get(ctx, string(gateway.PlatformMatrix))
	if entry == nil || entry.AdapterFactory == nil {
		return map[string]any{"error": "Matrix platform adapter is not registered in this icebath build"}, nil
	}
	built, err := entry.AdapterFactory(ctx, pconfig)
	if err != nil {
		return nil, err
	}
	adapter, ok := built.(gateway.PlatformAdapter)
	if !ok {
		return sendError("Matrix send failed: the adapter factory did not build a platform adapter"), nil
	}
	defer func() {
		defer func() { _ = recover() }()
		_ = adapter.Disconnect(ctx)
	}()
	result, err := func() (res map[string]any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		connected, err := adapter.Connect(ctx, false)
		if err != nil {
			return nil, err
		}
		if !connected {
			return sendError("Matrix connect failed"), nil
		}
		return matrixSendCore(ctx, adapter, chatID, message, mediaFiles, metadata)
	}()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return sendError(fmt.Sprintf("Matrix send failed: %s", err)), nil
	}
	return result, nil
}

// Text then each media file through a Matrix adapter, the last message id on success
func matrixSendCore(ctx context.Context, live any, chatID, message string, mediaFiles []gateway.MediaRef, metadata map[string]any) (map[string]any, error) {
	adapter, ok := live.(gateway.PlatformAdapter)
	if !ok {
		return nil, errors.New("the live Matrix adapter cannot send")
	}
	var last *gateway.SendResult
	if strings.TrimSpace(message) != "" {
		r, err := adapter.Send(ctx, chatID, message, "", metadata)
		if err != nil {
			return nil, err
		}
		if last = &r; !r.Success {
			return sendError("Matrix send failed: " + anyutil.String(r.Error)), nil
		}
	}
	for _, m := range mediaFiles {
		if !pathExists(m.Path) {
			return sendError("Media file not found: " + m.Path), nil
		}
		ext := lowerExt(m.Path)
		method, _ := adapterMediaMethod(ext, (voiceExts[ext] && m.IsVoice) || audioExts[ext], false)
		r, err := callAdapterMedia(ctx, adapter, method, chatID, m.Path, "", "", metadata)
		if err != nil {
			return nil, err
		}
		if last = &r; !r.Success {
			return sendError("Matrix media send failed: " + anyutil.String(r.Error)), nil
		}
	}
	if last == nil {
		return map[string]any{"error": noDeliverable}, nil
	}
	return sendSuccess("matrix", chatID, nil, map[string]any{"message_id": last.MessageID}), nil
}

// Adapter media send by python method name
func callAdapterMedia(ctx context.Context, adapter gateway.PlatformAdapter, method, chatID, path, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	switch method {
	case "send_image_file":
		return adapter.SendImageFile(ctx, chatID, path, caption, replyTo, metadata, nil)
	case "send_video":
		return adapter.SendVideo(ctx, chatID, path, caption, replyTo, metadata, nil)
	case "send_voice":
		return adapter.SendVoice(ctx, chatID, path, caption, replyTo, metadata, nil)
	}
	return adapter.SendDocument(ctx, chatID, path, caption, "", replyTo, metadata, nil)
}

// Telegram REST delivery used by the standalone sender outside the gateway
func SendTelegram(ctx context.Context, token any, chatID, message string, mediaFiles []gateway.MediaRef, threadID *string,
	disableLinkPreviews, forceDocument bool) (map[string]any, error) {
	return sendTelegram(ctx, token, chatID, message, mediaFiles, threadID, disableLinkPreviews, forceDocument)
}
