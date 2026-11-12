package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Image cache writer of inbound photos and stickers, the cache_image_from_bytes_async hermes imports
var cacheImageFromBytesAsync = gateway.CacheImageFromBytesAsync

// Boolean gate of the scoped env, then the extra key and its fallbacks, then the default
func (a *TelegramAdapter) extraBool(ctx context.Context, key, envName, def string, fallbackKeys ...string) bool {
	configured, err := gateway.ExtraOrSecret(ctx, a.Config.Extra, key, envName, nil, true)
	if err != nil {
		configured = nil
	}
	for _, alt := range fallbackKeys {
		if configured == nil {
			configured = a.Config.Extra[alt]
		}
	}
	if configured == nil {
		configured = def
	}
	if b, ok := configured.(bool); ok {
		return b
	}
	switch pystr.Lower(pystr.Strip(pyrepr.Str(configured))) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// Comma or list allowlist of the scoped env, then the extra key, else empty
func (a *TelegramAdapter) extraStrSet(ctx context.Context, key, envName string) map[string]bool {
	raw, err := gateway.ExtraOrSecret(ctx, a.Config.Extra, key, envName, "", false)
	if err != nil {
		raw = ""
	}
	raw = gateway.DecodeJSONListLiteral(raw)
	out := map[string]bool{}
	if list, ok := raw.([]any); ok {
		for _, part := range list {
			if s := pystr.Strip(pyrepr.Str(part)); s != "" {
				out[s] = true
			}
		}
		return out
	}
	for _, part := range strings.Split(pyrepr.Str(raw), ",") {
		if s := pystr.Strip(part); s != "" {
			out[s] = true
		}
	}
	return out
}

func (a *TelegramAdapter) requireMention(ctx context.Context) bool {
	return a.extraBool(ctx, "require_mention", "TELEGRAM_REQUIRE_MENTION", "false")
}

func (a *TelegramAdapter) observeUnmentionedGroupMessages(ctx context.Context) bool {
	return a.extraBool(ctx, "observe_unmentioned_group_messages", "TELEGRAM_OBSERVE_UNMENTIONED_GROUP_MESSAGES", "false",
		"ingest_unmentioned_group_messages")
}

func (a *TelegramAdapter) guestMode(ctx context.Context) bool {
	return a.extraBool(ctx, "guest_mode", "TELEGRAM_GUEST_MODE", "false")
}

func (a *TelegramAdapter) exclusiveBotMentions(ctx context.Context) bool {
	return a.extraBool(ctx, "exclusive_bot_mentions", "TELEGRAM_EXCLUSIVE_BOT_MENTIONS", "true")
}

func (a *TelegramAdapter) botsRequireMention(ctx context.Context) bool {
	return a.extraBool(ctx, "bots_require_mention", "TELEGRAM_BOTS_REQUIRE_MENTION", "false")
}

func (a *TelegramAdapter) freeResponseChats(ctx context.Context) map[string]bool {
	return a.extraStrSet(ctx, "free_response_chats", "TELEGRAM_FREE_RESPONSE_CHATS")
}

func (a *TelegramAdapter) freeResponseTopics(ctx context.Context) map[string]bool {
	return a.extraStrSet(ctx, "free_response_topics", "TELEGRAM_FREE_RESPONSE_TOPICS")
}

// Whether the message's chat and topic pair is a free response topic
func (a *TelegramAdapter) isFreeResponseTopic(ctx context.Context, m *models.Message) bool {
	topics := a.freeResponseTopics(ctx)
	if len(topics) == 0 {
		return false
	}
	chatID := chatIDStr(m)
	if chatID == "" {
		return false
	}
	return topics[chatID+":"+topicIDOrGeneral(effectiveMessageThreadID(m))]
}

func (a *TelegramAdapter) allowedChats(ctx context.Context) map[string]bool {
	return a.extraStrSet(ctx, "allowed_chats", "TELEGRAM_ALLOWED_CHATS")
}

func (a *TelegramAdapter) groupAllowedChats(ctx context.Context) map[string]bool {
	return a.extraStrSet(ctx, "group_allowed_chats", "TELEGRAM_GROUP_ALLOWED_CHATS")
}

// Chats where observed context may use a shared source, the group allowlist narrowed by allowed_chats
func (a *TelegramAdapter) observeAllowedChats(ctx context.Context) map[string]bool {
	group := a.groupAllowedChats(ctx)
	if len(group) == 0 {
		return map[string]bool{}
	}
	response := a.allowedChats(ctx)
	if len(response) == 0 {
		return group
	}
	out := map[string]bool{}
	for k := range group {
		if response[k] {
			out[k] = true
		}
	}
	return out
}

func (a *TelegramAdapter) allowedTopics(ctx context.Context) map[string]bool {
	return a.extraStrSet(ctx, "allowed_topics", "TELEGRAM_ALLOWED_TOPICS")
}

// Thread ids to skip from the scoped env or the extra list
func (a *TelegramAdapter) ignoredThreads(ctx context.Context) map[int64]bool {
	raw, err := gateway.ExtraOrSecret(ctx, a.Config.Extra, "ignored_threads", "TELEGRAM_IGNORED_THREADS", "", false)
	if err != nil {
		raw = ""
	}
	raw = gateway.DecodeJSONListLiteral(raw)
	var values []any
	if list, ok := raw.([]any); ok {
		values = list
	} else {
		for _, part := range strings.Split(pyrepr.Str(raw), ",") {
			values = append(values, part)
		}
	}
	out := map[int64]bool{}
	for _, value := range values {
		text := pystr.Strip(pyrepr.Str(value))
		if text == "" {
			continue
		}
		n, convErr := strconv.ParseInt(text, 10, 64)
		if convErr != nil {
			slog.Warn(fmt.Sprintf("[%s] Ignoring invalid Telegram thread id: %s", a.Name(), pyrepr.Repr(value)))
			continue
		}
		out[n] = true
	}
	return out
}

// Wake word patterns of the scoped env, else the profile extra
func (a *TelegramAdapter) compileMentionPatterns(ctx context.Context) []*regexp2.Regexp {
	envRaw := pystr.Strip(gateway.PlatformGateEnv(ctx, "TELEGRAM_MENTION_PATTERNS", ""))
	var patterns any
	if envRaw != "" {
		if parsed, err := pyjson.Loads(envRaw); err == nil {
			patterns = parsed
		} else {
			var parts []any
			for _, line := range pystr.Splitlines(envRaw, false) {
				if s := pystr.Strip(line); s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) == 0 {
				for _, part := range strings.Split(envRaw, ",") {
					if s := pystr.Strip(part); s != "" {
						parts = append(parts, s)
					}
				}
			}
			patterns = parts
		}
	} else {
		patterns = a.Config.Extra["mention_patterns"]
	}
	if patterns == nil {
		return nil
	}
	label := "telegram"
	return gateway.CompileMentionPatterns(patterns, gateway.MentionPatternOptions{LogPrefix: a.Name(), PlatformLabel: &label, DisplayLabel: "Telegram"})
}

// Bare lowercase chat type name
func chatTypeStr(chat *models.Chat) string {
	if chat == nil {
		return ""
	}
	parts := strings.Split(string(chat.Type), ".")
	return pystr.Lower(parts[len(parts)-1])
}

// Chat id text of a message
func chatIDStr(m *models.Message) string { return strconv.FormatInt(m.Chat.ID, 10) }

// Thread id text, the General topic when absent
func topicIDOrGeneral(threadID *string) string {
	if threadID != nil {
		return *threadID
	}
	return generalTopicThreadID
}

// Whether the message is in a group or supergroup
func isGroupChat(m *models.Message) bool {
	t := chatTypeStr(&m.Chat)
	return t == "group" || t == "supergroup"
}

// Routable thread id, General for forum messages without one and nil for reply UI anchors
func effectiveMessageThreadID(m *models.Message) *string {
	chatType := chatTypeStr(&m.Chat)
	isGroup := chatType == "group" || chatType == "supergroup"
	isForum := isGroup && m.Chat.IsForum
	if m.MessageThreadID != 0 {
		if isForum || (isGroup && m.IsTopicMessage) || (chatType == "private" && m.IsTopicMessage) {
			s := strconv.Itoa(m.MessageThreadID)
			return &s
		}
		return nil
	}
	if isForum {
		s := generalTopicThreadID
		return &s
	}
	return nil
}

var foreignBotHandleRE = regexp2.MustCompile(`^(?:[a-z0-9_]{2,29}bot)$`, regexp2.IgnoreCase)

// Live bot username, the observed handle before the getMe cache
func (a *TelegramAdapter) currentBotUsername() string {
	a.mu.Lock()
	observed := a.botUsernameObserved
	a.mu.Unlock()
	if observed != "" {
		return observed
	}
	return pystr.Lower(strings.TrimLeft(a.cachedBotUsername(), "@"))
}

// Records the bot's current username, logging real renames
func (a *TelegramAdapter) noteBotUsername(username string) {
	handle := pystr.Lower(strings.TrimLeft(username, "@"))
	if handle == "" {
		return
	}
	a.mu.Lock()
	previous := a.botUsernameObserved
	if previous == handle {
		a.mu.Unlock()
		return
	}
	a.botUsernameObserved = handle
	now := monotonicNow()
	a.botIdentityCheckedAt = &now
	a.mu.Unlock()
	if previous != "" {
		slog.Info(fmt.Sprintf("[%s] Telegram bot username changed: @%s -> @%s (mention routing now follows the new handle)", a.Name(), previous, handle))
	}
}

// Learns the bot handle from a message Telegram says the bot authored
func (a *TelegramAdapter) observeBotIdentityFromMessage(m *models.Message) {
	id, ok := a.botID()
	if !ok {
		return
	}
	candidates := []*models.User{m.From}
	if m.ReplyToMessage != nil {
		candidates = append(candidates, m.ReplyToMessage.From)
	}
	for _, c := range candidates {
		if c != nil && c.ID == id {
			a.noteBotUsername(c.Username)
		}
	}
}

// Whether the identity was re-read within the TTL, never checked counting as stale
func (a *TelegramAdapter) botIdentityIsFresh() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botIdentityCheckedAt != nil && monotonicNow()-*a.botIdentityCheckedAt < botIdentityTTLSeconds
}

// Re-reads the bot identity when the cache may be stale, a failed probe keeping the last handle
func (a *TelegramAdapter) refreshBotIdentity(ctx context.Context, force bool) {
	b := a.currentBot()
	if b == nil {
		return
	}
	if !force && a.botIdentityIsFresh() {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, seconds(botIdentityProbeTimeout))
	me, err := b.GetMe(probeCtx)
	cancel()
	if err != nil {
		known := a.currentBotUsername()
		if known == "" {
			known = "unknown"
		}
		slog.Debug(fmt.Sprintf("[%s] Telegram identity refresh failed (keeping @%s): %s", a.Name(), known, pyrepr.ExceptionMessage(ptbError(err))))
		return
	}
	a.mu.Lock()
	if a.app != nil {
		a.app.mu.Lock()
		a.app.botUser = me
		a.app.mu.Unlock()
	}
	now := monotonicNow()
	a.botIdentityCheckedAt = &now
	a.mu.Unlock()
	a.noteBotUsername(me.Username)
}

// Whether the message replies to one of the bot's own messages
func (a *TelegramAdapter) isReplyToBot(m *models.Message) bool {
	id, ok := a.botID()
	if !ok || m.ReplyToMessage == nil || m.ReplyToMessage.From == nil {
		return false
	}
	return m.ReplyToMessage.From.ID == id
}

// Text and caption paired with their own entities
func entitySourcesOf(m *models.Message) [2]struct {
	text     string
	entities []models.MessageEntity
} {
	return [2]struct {
		text     string
		entities []models.MessageEntity
	}{{m.Text, m.Entities}, {m.Caption, m.CaptionEntities}}
}

// Entity span text by UTF-16 offsets, ok false when the offsets are unusable
func entitySpan(sourceText string, e models.MessageEntity) (string, bool) {
	if e.Offset < 0 || e.Length <= 0 {
		return "", false
	}
	return telegramEntityText(sourceText, e.Offset, e.Length), true
}

// Entity span using UTF-16 code unit offsets, empty when the span splits a surrogate pair
func telegramEntityText(sourceText string, offset, length int) string {
	if offset < 0 || length <= 0 {
		return ""
	}
	units := utf16.Encode([]rune(sourceText))
	from, to := min(offset, len(units)), min(offset+length, len(units))
	if from >= to {
		return ""
	}
	span := units[from:to]
	if utf16.IsSurrogate(rune(span[0])) && span[0] >= 0xDC00 || utf16.IsSurrogate(rune(span[len(span)-1])) && span[len(span)-1] < 0xDC00 {
		return ""
	}
	return string(utf16.Decode(span))
}

var rawMentionRE = regexp2.MustCompile(`(?i)(?<![A-Za-z0-9_\x60/])@([A-Za-z0-9_]{2,31})\b`, regexp2.None)

// Explicit bot usernames mentioned, foreign handles only when bot shaped and our own handle always
func extractBotMentionUsernames(m *models.Message, selfUsername string) map[string]bool {
	out := map[string]bool{}
	own := pystr.Lower(strings.TrimLeft(selfUsername, "@"))
	isBotHandle := func(handle string) bool {
		if handle == "" {
			return false
		}
		if own != "" && handle == own {
			return true
		}
		ok, _ := foreignBotHandleRE.MatchString(handle)
		return ok
	}
	for _, src := range entitySourcesOf(m) {
		for _, e := range src.entities {
			entityType := pystr.Lower(string(e.Type))
			if entityType != "mention" && entityType != "bot_command" {
				continue
			}
			text, ok := entitySpan(src.text, e)
			if !ok {
				continue
			}
			text = pystr.Strip(text)
			if entityType == "mention" {
				if handle := pystr.Lower(strings.TrimLeft(text, "@")); isBotHandle(handle) {
					out[handle] = true
				}
				continue
			}
			at := strings.Index(text, "@")
			if at < 0 {
				continue
			}
			if target := pystr.Lower(pystr.Strip(text[at+1:])); isBotHandle(target) {
				out[target] = true
			}
		}
	}
	for _, src := range entitySourcesOf(m) {
		if src.text == "" || len(src.entities) > 0 {
			continue
		}
		for match, _ := rawMentionRE.FindStringMatch(src.text); match != nil; match, _ = rawMentionRE.FindNextMatch(match) {
			if handle := pystr.Lower(match.GroupByNumber(1).String()); isBotHandle(handle) {
				out[handle] = true
			}
		}
	}
	return out
}

// Whether the message addresses the bot by mention, text mention or command suffix
func (a *TelegramAdapter) messageMentionsBot(m *models.Message) bool {
	id, ok := a.botID()
	if !ok {
		return false
	}
	username := a.currentBotUsername()
	expected := ""
	if username != "" {
		expected = "@" + username
	}
	for _, src := range entitySourcesOf(m) {
		for _, e := range src.entities {
			switch pystr.Lower(string(e.Type)) {
			case "mention":
				if expected == "" {
					continue
				}
				if span, ok := entitySpan(src.text, e); ok && pystr.Lower(pystr.Strip(span)) == expected {
					return true
				}
			case "text_mention":
				if e.User != nil && e.User.ID == id {
					return true
				}
			case "bot_command":
				if expected == "" {
					continue
				}
				command, ok := entitySpan(src.text, e)
				if !ok {
					continue
				}
				if at := strings.Index(command, "@"); at >= 0 && pystr.Lower(pystr.Strip(command[at:])) == expected {
					return true
				}
			}
		}
	}
	if username != "" {
		return extractBotMentionUsernames(m, username)[username]
	}
	return false
}

// Fires a TTL guarded identity refresh when routing is about to discard a message naming other bots
func (a *TelegramAdapter) scheduleBotIdentityRecheck(ctx context.Context) {
	a.mu.Lock()
	existing := a.identityRefreshTask
	a.mu.Unlock()
	if (existing != nil && !existing.Done()) || a.botIdentityIsFresh() {
		return
	}
	task := gateway.SpawnAdapterTask(context.WithoutCancel(ctx), func(c context.Context) error {
		a.refreshBotIdentity(c, false)
		return nil
	})
	a.mu.Lock()
	a.identityRefreshTask = task
	a.mu.Unlock()
	a.TrackBackgroundTask(task)
}

// Whether explicit bot handles target other bots and not this one
func (a *TelegramAdapter) explicitBotMentionsExcludeSelf(ctx context.Context, m *models.Message) bool {
	if a.currentBot() == nil {
		return false
	}
	username := a.currentBotUsername()
	if username == "" {
		return false
	}
	mentioned := extractBotMentionUsernames(m, username)
	excludes := len(mentioned) > 0 && !mentioned[username]
	if excludes {
		a.scheduleBotIdentityRecheck(ctx)
	}
	return excludes
}

// Whether the text or caption matches a wake word pattern
func (a *TelegramAdapter) messageMatchesMentionPatterns(m *models.Message) bool {
	for _, candidate := range []string{m.Text, m.Caption} {
		if candidate == "" {
			continue
		}
		for _, p := range a.mentionPatterns {
			if regexp2Search(p, candidate) {
				return true
			}
		}
	}
	return false
}

// Guest mode bypass, an explicit bot mention
func (a *TelegramAdapter) isGuestMention(ctx context.Context, m *models.Message) bool {
	return a.guestMode(ctx) && a.messageMentionsBot(m)
}

// Trigger text with the bot mention removed, the original when nothing is left
func (a *TelegramAdapter) cleanBotTriggerText(text *string) *string {
	username := a.currentBotUsername()
	if text == nil || *text == "" || username == "" {
		return text
	}
	re := regexp2.MustCompile(`(?i)@`+regexp2.Escape(username)+`\b[,:\-]*\s*`, regexp2.None)
	cleaned, err := re.Replace(*text, "", -1, -1)
	if err != nil {
		return text
	}
	if cleaned = pystr.Strip(cleaned); cleaned != "" {
		return &cleaned
	}
	return text
}

// Allowed topic and ignored thread gates, false when blocked and nil when undecided
func (a *TelegramAdapter) topicGatesPass(ctx context.Context, threadID *string, warnNonNumeric bool) *bool {
	blocked := false
	if allowed := a.allowedTopics(ctx); len(allowed) > 0 && !allowed[topicIDOrGeneral(threadID)] {
		return &blocked
	}
	if threadID != nil {
		n, err := strconv.ParseInt(pystr.Strip(*threadID), 10, 64)
		if err != nil {
			if !warnNonNumeric {
				return &blocked
			}
			slog.Warn(fmt.Sprintf("[%s] Ignoring non-numeric Telegram message_thread_id: %s", a.Name(), pyrepr.Repr(*threadID)))
		} else if a.ignoredThreads(ctx)[n] {
			return &blocked
		}
	}
	return nil
}

// Raw thread id of a message as text, nil when absent
func rawThreadID(m *models.Message) *string {
	if m.MessageThreadID == 0 {
		return nil
	}
	s := strconv.Itoa(m.MessageThreadID)
	return &s
}

// Whether bots_require_mention vetoes another bot's message that does not mention this bot
func (a *TelegramAdapter) botSenderSuppressed(ctx context.Context, m *models.Message) bool {
	return a.botsRequireMention(ctx) && a.senderIsOtherBot(m) && !a.messageMentionsBot(m)
}

// Whether an unaddressed group message is stored as observed context instead of dispatched
func (a *TelegramAdapter) shouldObserveUnmentionedGroupMessage(ctx context.Context, m *models.Message) bool {
	if a.isOwnMessage(m) || !a.observeUnmentionedGroupMessages(ctx) || !isGroupChat(m) {
		return false
	}
	if pass := a.topicGatesPass(ctx, rawThreadID(m), false); pass != nil && !*pass {
		return false
	}
	chatID := chatIDStr(m)
	if a.exclusiveBotMentions(ctx) && a.explicitBotMentionsExcludeSelf(ctx, m) {
		return false
	}
	allowed := a.observeAllowedChats(ctx)
	if len(allowed) == 0 || !allowed[chatID] {
		return false
	}
	if a.freeResponseChats(ctx)[chatID] || a.isFreeResponseTopic(ctx, m) {
		return false
	}
	if a.botSenderSuppressed(ctx, m) {
		return true
	}
	if !a.requireMention(ctx) || a.isReplyToBot(m) || a.messageMentionsBot(m) {
		return false
	}
	return !a.messageMatchesMentionPatterns(m)
}

// Chat and topic scoped source of observed group context
func groupObserveSharedSource(source *gateway.SessionSource) *gateway.SessionSource {
	return gateway.ReplaceSource(source, func(s *gateway.SessionSource) { s.UserID, s.UserName, s.UserIDAlt = nil, nil, nil })
}

// Observed history line attributing the sender
func groupObserveAttributedText(event *gateway.MessageEvent) string {
	userID := "unknown"
	if event.Source.UserID != nil && *event.Source.UserID != "" {
		userID = *event.Source.UserID
	}
	name := userID
	if event.Source.UserName != nil && *event.Source.UserName != "" {
		name = *event.Source.UserName
	}
	return fmt.Sprintf("[%s|%s]\n%s", name, userID, event.Text)
}

// Channel prompt explaining observed group context to the agent
func (a *TelegramAdapter) groupObserveChannelPrompt() string {
	username := a.currentBotUsername()
	if username == "" {
		username = "unknown"
	}
	botID := "unknown"
	if id, ok := a.botID(); ok && id != 0 {
		botID = strconv.FormatInt(id, 10)
	}
	return "You are handling a Telegram group chat message.\n" +
		fmt.Sprintf("- Your identity: user_id=%s, @-mention name in this group=@%s\n", botID, username) +
		"- observed Telegram group context may be provided in a separate context-only block " +
		"before the current message; it is not necessarily addressed to you.\n" +
		"- Treat only the current new message as a request explicitly directed at you, " +
		"and use observed context only when the current message asks for it."
}

// Aligns triggered group turns with the observed history attribution
func (a *TelegramAdapter) applyGroupObserveAttribution(ctx context.Context, event *gateway.MessageEvent) *gateway.MessageEvent {
	if !a.observeUnmentionedGroupMessages(ctx) {
		return event
	}
	raw, _ := event.RawMessage.(*models.Message)
	if raw == nil || !isGroupChat(raw) {
		return event
	}
	if allowed := a.observeAllowedChats(ctx); len(allowed) == 0 || !allowed[chatIDStr(raw)] {
		return event
	}
	prompt := a.groupObserveChannelPrompt()
	channelPrompt := prompt
	if event.ChannelPrompt != nil && *event.ChannelPrompt != "" {
		channelPrompt = *event.ChannelPrompt + "\n\n" + prompt
	}
	if event.MessageType == gateway.MessageTypeCommand {
		copied := gateway.ReplaceMessageEventText(event, event.Text)
		copied.ChannelPrompt = &channelPrompt
		return copied
	}
	copied := gateway.ReplaceMessageEventText(event, groupObserveAttributedText(event))
	copied.Source = groupObserveSharedSource(event.Source)
	copied.ChannelPrompt = &channelPrompt
	return copied
}

// Message type of a media message, the first present attachment winning
func mediaMessageType(m *models.Message) gateway.MessageType {
	switch {
	case m.Sticker != nil:
		return gateway.MessageTypeSticker
	case len(m.Photo) > 0:
		return gateway.MessageTypePhoto
	case m.Video != nil, m.VideoNote != nil:
		return gateway.MessageTypeVideo
	case m.Audio != nil:
		return gateway.MessageTypeAudio
	case m.Voice != nil:
		return gateway.MessageTypeVoice
	}
	return gateway.MessageTypeDocument
}

var cachedKindToMessageType = map[string]gateway.MessageType{"image": gateway.MessageTypePhoto, "video": gateway.MessageTypeVideo,
	"audio": gateway.MessageTypeAudio}

// File source of an attachment, its file id and size with the filename, MIME and default kind
type mediaSource struct {
	fileID   string
	fileSize int64
	filename string
	mime     string
	kind     string
}

// Attachment source of a message, nil without one
func observedMediaSource(m *models.Message) *mediaSource {
	switch {
	case len(m.Photo) > 0:
		p := m.Photo[len(m.Photo)-1]
		return &mediaSource{fileID: p.FileID, fileSize: int64(p.FileSize), kind: "image"}
	case m.Video != nil:
		return &mediaSource{fileID: m.Video.FileID, fileSize: m.Video.FileSize, mime: "video/mp4", kind: "video"}
	case m.VideoNote != nil:
		return &mediaSource{fileID: m.VideoNote.FileID, fileSize: int64(m.VideoNote.FileSize), mime: "video/mp4", kind: "video"}
	case m.Voice != nil:
		return &mediaSource{fileID: m.Voice.FileID, fileSize: m.Voice.FileSize, filename: "voice.ogg", mime: "audio/ogg", kind: "audio"}
	case m.Audio != nil:
		return &mediaSource{fileID: m.Audio.FileID, fileSize: m.Audio.FileSize, filename: m.Audio.FileName, kind: "audio"}
	case m.Document != nil:
		return &mediaSource{fileID: m.Document.FileID, fileSize: m.Document.FileSize, filename: m.Document.FileName, mime: pystr.Lower(m.Document.MimeType)}
	}
	return nil
}

// Downloads an attachment by file id through the live application
func (a *TelegramAdapter) downloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if app == nil {
		return nil, "", &runtimeError{msg: "Not connected"}
	}
	return app.downloadFile(ctx, fileID)
}

// Downloads an attachment into the media cache, the status none, oversized, failed, unreadable or ok
func (a *TelegramAdapter) downloadObservedMedia(ctx context.Context, m *models.Message, what string) (string, *gateway.CachedMedia, int64) {
	src := observedMediaSource(m)
	if src == nil {
		return "none", nil, 0
	}
	if !(0 < src.fileSize && src.fileSize <= a.maxDocBytes) {
		return "oversized", nil, src.fileSize
	}
	data, filePath, err := a.downloadFile(ctx, src.fileID)
	var cached *gateway.CachedMedia
	if err == nil {
		filename := src.filename
		if filename == "" {
			filename = filepath.Base(filePath)
		}
		cached, err = gateway.CacheMediaBytes(ctx, data, filename, src.mime, src.kind)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[Telegram] Failed to cache %s: %s", what, redactTelegramErrorText(err)))
		return "failed", nil, 0
	}
	if cached == nil {
		return "unreadable", nil, 0
	}
	return "ok", cached, 0
}

// Caches an unmentioned group attachment and annotates the observed text
func (a *TelegramAdapter) cacheObservedMedia(ctx context.Context, m *models.Message, event *gateway.MessageEvent) {
	status, cached, size := a.downloadObservedMedia(ctx, m, "observed group media")
	switch status {
	case "oversized":
		event.Text = appendObservedNote(event.Text, fmt.Sprintf("[Observed Telegram attachment too large or unverifiable. Maximum: %d MB.]",
			a.maxDocBytes/(1024*1024)))
		slog.Info(fmt.Sprintf("[Telegram] Observed group attachment skipped (size=%s)", pyrepr.Str(sizeValue(size))))
	case "unreadable":
		event.Text = appendObservedNote(event.Text, "[Observed Telegram attachment could not be read, not cached.]")
	case "ok":
		event.MediaURLs, event.MediaTypes = []string{}, []string{}
		a.attachCached(event, cached, cached.ContextNote(ctx), "[Telegram] Cached observed group %s at %s")
	}
}

// File size as the hermes log shows it, None for an unknown size
func sizeValue(size int64) any {
	if size == 0 {
		return nil
	}
	return size
}

// Appends a cached attachment, the first one setting the message type from its kind
func (a *TelegramAdapter) attachCached(event *gateway.MessageEvent, cached *gateway.CachedMedia, note, logFmt string) {
	event.MediaURLs = append(event.MediaURLs, cached.Path)
	event.MediaTypes = append(event.MediaTypes, cached.MediaType)
	if mtype, ok := cachedKindToMessageType[cached.Kind]; ok && len(event.MediaURLs) == 1 {
		event.MessageType = mtype
	}
	event.Text = appendObservedNote(event.Text, note)
	slog.Info(fmt.Sprintf(logFmt, cached.Kind, cached.Path))
}

// Caches media of the message this turn replies to
func (a *TelegramAdapter) cacheRepliedMedia(ctx context.Context, m *models.Message, event *gateway.MessageEvent) {
	if m.ReplyToMessage == nil {
		return
	}
	status, cached, _ := a.downloadObservedMedia(ctx, m.ReplyToMessage, "replied-to media")
	if status == "ok" {
		a.attachCached(event, cached, fmt.Sprintf("[Replied-to %s '%s' saved at: %s]", cached.Kind, cached.DisplayName, cached.Path),
			"[Telegram] Cached replied-to %s at %s")
	}
}

// Note appended after a blank line, the note alone when there is no text
func appendObservedNote(existing, note string) string {
	if note == "" {
		return existing
	}
	if existing != "" {
		return existing + "\n\n" + note
	}
	return note
}

// Catalog key of each media kind the download failure reply names
var mediaKindKeys = map[string]string{"photo": "platform.telegram.media.kind_photo", "voice message": "platform.telegram.media.kind_voice",
	"audio file": "platform.telegram.media.kind_audio", "video file": "platform.telegram.media.kind_video",
	"video note": "platform.telegram.media.kind_video", "attachment": "platform.telegram.media.kind_attachment"}

// Python exception class name of an error
func exceptionClassName(err error) string { return className(err) }

// Surfaces a failed media download to the user with a retry reply and to the agent with a note
func (a *TelegramAdapter) surfaceMediaCacheFailure(ctx context.Context, m *models.Message, event *gateway.MessageEvent, kind string, exc error,
	displayName string) {
	scoped, release := a.MediaDeliveryScope(ctx, event.Source)
	defer release()
	named := ""
	if displayName != "" {
		named = tr(scoped, "platform.telegram.media.named_suffix", "name", displayName)
	}
	kindText := kind
	if key, ok := mediaKindKeys[kind]; ok {
		kindText = tr(scoped, key)
	}
	visible := tr(scoped, "platform.telegram.media.download_failed", "kind", kindText, "name", named, "error", exceptionClassName(exc))
	notice := a.WarningText(scoped, visible, nil, a.Platform, chatIDStr(m), nil)
	if notice != nil && *notice != "" {
		a.AcceptUpdate(scoped)
		if err := a.replyText(scoped, m, *notice); err != nil {
			slog.Warn(fmt.Sprintf("[Telegram] Failed to notify user about %s cache failure: %s", kind, pyrepr.ExceptionMessage(err)))
		}
		event.Text = appendObservedNote(event.Text, fmt.Sprintf("[The user attempted to send a %s%s but it could not be downloaded (%s); they have been asked to retry.]",
			kind, named, exceptionClassName(exc)))
		return
	}
	event.Text = appendObservedNote(event.Text, fmt.Sprintf("[The user attempted to send a %s%s but it could not be downloaded.]", kind, named))
}

// Replies to a message in its chat and topic, the PTB Message.reply_text
func (a *TelegramAdapter) replyText(ctx context.Context, m *models.Message, text string) error {
	b := a.currentBot()
	if b == nil {
		return &runtimeError{msg: "Not connected"}
	}
	params := &bot.SendMessageParams{ChatID: m.Chat.ID, Text: text, ReplyParameters: &models.ReplyParameters{MessageID: m.ID}}
	if m.IsTopicMessage && m.MessageThreadID != 0 {
		params.MessageThreadID = m.MessageThreadID
	}
	_, err := b.SendMessage(ctx, params)
	return ptbError(err)
}

// Session store surface the observed group context writes through
type observeStore interface {
	GetOrCreateSession(ctx context.Context, source *gateway.SessionSource, forceNew, touchActivity bool) (*gateway.SessionEntry, error)
	AppendToTranscript(ctx context.Context, sessionID string, message map[string]any, skipDB bool) error
}

// Appends skipped group chatter to the shared target session without dispatching
func (a *TelegramAdapter) observeUnmentionedGroupMessage(ctx context.Context, m *models.Message, msgType gateway.MessageType, updateID *int64,
	event *gateway.MessageEvent) {
	store, ok := a.SessionStore().(observeStore)
	if !ok || store == nil {
		return
	}
	if event == nil {
		event = a.buildMessageEvent(ctx, m, msgType, updateID)
	}
	entry, err := store.GetOrCreateSession(ctx, groupObserveSharedSource(event.Source), false, true)
	if err == nil {
		line := map[string]any{"role": "user", "content": groupObserveAttributedText(event),
			"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.999999-07:00"), "observed": true}
		if event.MessageID != nil && *event.MessageID != "" {
			line["message_id"] = *event.MessageID
		}
		a.AcceptUpdate(ctx)
		err = store.AppendToTranscript(ctx, entry.SessionID, line, false)
	}
	if err != nil {
		a.failUpdatePreparation(ctx)
		slog.Warn(fmt.Sprintf("[%s] Failed to observe Telegram group message: %s", a.Name(), pyrepr.ExceptionMessage(err)))
		return
	}
	userID := "unknown"
	if event.Source.UserID != nil && *event.Source.UserID != "" {
		userID = *event.Source.UserID
	}
	slog.Info(fmt.Sprintf("[%s] Telegram group message observed (no bot trigger): chat=%d from=%s", a.Name(), m.Chat.ID, userID))
}

// Whether the bot itself sent the message
func (a *TelegramAdapter) isOwnMessage(m *models.Message) bool {
	id, ok := a.botID()
	if !ok || m.From == nil {
		return false
	}
	return m.From.ID == id
}

// Whether the sender is a bot other than this one
func (a *TelegramAdapter) senderIsOtherBot(m *models.Message) bool {
	if m.From == nil || !m.From.IsBot {
		return false
	}
	id, ok := a.botID()
	return !ok || m.From.ID != id
}

// Group trigger rules, DMs unrestricted and groups through allowed chats then the addressing gates
func (a *TelegramAdapter) shouldProcessMessage(ctx context.Context, m *models.Message) bool {
	a.observeBotIdentityFromMessage(m)
	if a.isOwnMessage(m) {
		return false
	}
	if !isGroupChat(m) {
		return true
	}
	if pass := a.topicGatesPass(ctx, effectiveMessageThreadID(m), true); pass != nil && !*pass {
		return false
	}
	chatID := chatIDStr(m)
	if a.exclusiveBotMentions(ctx) && a.explicitBotMentionsExcludeSelf(ctx, m) {
		return false
	}
	guest := a.isGuestMention(ctx, m)
	if allowed := a.allowedChats(ctx); len(allowed) > 0 && !allowed[chatID] {
		return guest
	}
	if guest || a.freeResponseChats(ctx)[chatID] || a.isFreeResponseTopic(ctx, m) {
		return true
	}
	if a.botSenderSuppressed(ctx, m) {
		return false
	}
	if !a.requireMention(ctx) || a.isReplyToBot(m) {
		return true
	}
	if !a.guestMode(ctx) && a.messageMentionsBot(m) {
		return true
	}
	return a.messageMatchesMentionPatterns(m)
}

// Lazily registers the command menu for a forum supergroup whose topics ignore the group scope
func (a *TelegramAdapter) ensureForumCommands(ctx context.Context, m *models.Message) {
	a.forumLock.Lock()
	defer a.forumLock.Unlock()
	if m == nil || !m.Chat.IsForum {
		return
	}
	chatID := m.Chat.ID
	a.mu.Lock()
	done := a.forumCommandRegistered[chatID]
	a.mu.Unlock()
	if done {
		return
	}
	err := func() error {
		b := a.currentBot()
		if b == nil {
			return &runtimeError{msg: "'NoneType' object has no attribute 'set_my_commands'"}
		}
		menu, _, err := menuCommands().TelegramMenuCommands(ctx, menuCommands().TelegramMenuMaxCommands(ctx))
		if err != nil {
			return err
		}
		commands := botCommands(menu)
		if _, err := b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: commands, Scope: &models.BotCommandScopeChat{ChatID: chatID}}); err != nil {
			return ptbError(err)
		}
		a.mu.Lock()
		a.forumCommandRegistered[chatID] = true
		a.mu.Unlock()
		slog.Info(fmt.Sprintf("[%s] Lazy-registered %d commands for forum chat %d", a.Name(), len(commands), chatID))
		return nil
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Forum command lazy-registration failed: %s", a.Name(), redactTelegramErrorText(err)))
	}
}

// Logs a message refused by the authorization gate
func logBlockedUser(m *models.Message, level slog.Level, what string) {
	var userID any
	if m.From != nil {
		userID = m.From.ID
	}
	slog.Log(context.Background(), level, fmt.Sprintf("[Telegram] Blocked %s %s in chat %d", what, pyrepr.Str(userID), m.Chat.ID))
}

// Group trigger gate that observes unaddressed chatter when configured, true to proceed
func (a *TelegramAdapter) gateOrObserve(ctx context.Context, m *models.Message, update *Update, msgType gateway.MessageType) bool {
	if a.shouldProcessMessage(ctx, m) {
		return true
	}
	if a.shouldObserveUnmentionedGroupMessage(ctx, m) {
		id := update.UpdateID
		a.observeUnmentionedGroupMessage(ctx, m, msgType, &id, nil)
	}
	return false
}

// Event of an addressed text or command with the trigger cleaned, replied media cached and attribution applied
func (a *TelegramAdapter) buildTriggeredEvent(ctx context.Context, m *models.Message, update *Update, msgType gateway.MessageType) *gateway.MessageEvent {
	id := update.UpdateID
	event := a.buildMessageEvent(ctx, m, msgType, &id)
	text := event.Text
	if cleaned := GroupTriggerText(a, a.contextMessageOf(m), &text); cleaned != nil {
		event.Text = *cleaned
	} else {
		event.Text = ""
	}
	a.cacheRepliedMedia(ctx, m, event)
	return a.applyGroupObserveAttribution(ctx, event)
}

// Handles incoming text, buffering client split chunks into one event
func (a *TelegramAdapter) handleTextMessage(ctx context.Context, update *Update) error {
	m := effectiveMessage(update.Raw)
	if m == nil || m.Text == "" {
		return nil
	}
	if !a.core.isUserAuthorizedFromMessage(ctx, m) {
		logBlockedUser(m, slog.LevelWarn, "unauthorized user")
		return nil
	}
	if !a.gateOrObserve(ctx, m, update, gateway.MessageTypeText) {
		return nil
	}
	a.ensureForumCommands(ctx, update.Raw.Message)
	return a.EnqueueTextEvent(ctx, a.buildTriggeredEvent(ctx, m, update, gateway.MessageTypeText))
}

// Handles incoming commands, near limit pastes going through text batching
func (a *TelegramAdapter) handleCommand(ctx context.Context, update *Update) error {
	m := effectiveMessage(update.Raw)
	if m == nil || m.Text == "" {
		return nil
	}
	if !a.shouldProcessMessage(ctx, m) {
		return nil
	}
	if !a.core.isUserAuthorizedFromMessage(ctx, m) {
		logBlockedUser(m, slog.LevelWarn, "unauthorized user")
		return nil
	}
	a.ensureForumCommands(ctx, m)
	event := a.buildTriggeredEvent(ctx, m, update, gateway.MessageTypeCommand)
	if utf8.RuneCountInString(event.Text) >= telegramSplitThreshold {
		return a.EnqueueTextEvent(ctx, event)
	}
	return a.HandleMessage(ctx, event)
}

// Handles a location or venue pin with a map link and the nearby search prompt
func (a *TelegramAdapter) handleLocationMessage(ctx context.Context, update *Update) error {
	m := effectiveMessage(update.Raw)
	if m == nil {
		return nil
	}
	if !a.core.isUserAuthorizedFromMessage(ctx, m) {
		logBlockedUser(m, slog.LevelWarn, "unauthorized user")
		return nil
	}
	if !a.gateOrObserve(ctx, m, update, gateway.MessageTypeLocation) {
		return nil
	}
	location := m.Location
	if m.Venue != nil {
		location = &m.Venue.Location
	}
	if location == nil {
		return nil
	}
	lat, lon := pyrepr.Str(location.Latitude), pyrepr.Str(location.Longitude)
	parts := []string{"[The user shared a location pin.]"}
	if m.Venue != nil {
		if m.Venue.Title != "" {
			parts = append(parts, "Venue: "+m.Venue.Title)
		}
		if m.Venue.Address != "" {
			parts = append(parts, "Address: "+m.Venue.Address)
		}
	}
	parts = append(parts, "latitude: "+lat, "longitude: "+lon, fmt.Sprintf("Map: https://www.google.com/maps/search/?api=1&query=%s,%s", lat, lon),
		"Ask what they'd like to find nearby (restaurants, cafes, etc.) and any preferences.")
	id := update.UpdateID
	event := a.buildMessageEvent(ctx, m, gateway.MessageTypeLocation, &id)
	event.Text = strings.Join(parts, "\n")
	return a.HandleMessage(ctx, a.applyGroupObserveAttribution(ctx, event))
}

// Session scoped batch key with topic recovery applied first
func (a *TelegramAdapter) TextBatchKey(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	a.ApplyTopicRecovery(event)
	return a.BasePlatformAdapter.TextBatchKey(ctx, event)
}

// Buffers a text chunk, holding it while delayed delivery must be dropped
func (a *TelegramAdapter) EnqueueTextEvent(ctx context.Context, event *gateway.MessageEvent) error {
	if a.ShouldDropDelayedDelivery() {
		a.HoldInboundEvent(ctx, event, "text-enqueue", true)
		return nil
	}
	if err := a.BasePlatformAdapter.EnqueueTextEvent(ctx, event); err != nil {
		return err
	}
	a.AcceptUpdate(ctx)
	return nil
}

// Delayed flush slot store of one batching family
type flushSlots struct {
	pending func() map[string]*gateway.MessageEvent
	owner   func(key string) *gateway.AdapterTask
	release func(key string, task *gateway.AdapterTask)
}

// Shared delayed flush body, a cancel after the pop re-holding the event so it is never lost
func (a *TelegramAdapter) flushBuffered(ctx context.Context, slots flushSlots, pop func(key string) *gateway.MessageEvent, key string, delay float64,
	where string, logFn func(event *gateway.MessageEvent)) error {
	current := gateway.CurrentAdapterTask(ctx)
	committedKey := [2]string{where, key}
	var event *gateway.MessageEvent
	defer func() {
		a.mu.Lock()
		if a.committedFlushTasks[committedKey] == current {
			delete(a.committedFlushTasks, committedKey)
		}
		a.mu.Unlock()
		slots.release(key, current)
	}()
	if !sleepCtx(ctx, delay) {
		return nil
	}
	if owner := slots.owner(key); owner != nil && owner != current {
		return nil
	}
	event = pop(key)
	if event == nil {
		return nil
	}
	slots.release(key, current)
	a.mu.Lock()
	prior := a.committedFlushTasks[committedKey]
	a.committedFlushTasks[committedKey] = current
	a.mu.Unlock()
	if prior != nil && !prior.Done() {
		if err := prior.Wait(ctx); err != nil && ctx.Err() != nil {
			a.HoldInboundEvent(context.WithoutCancel(ctx), event, where+"-flush-cancelled", true)
			return nil
		}
	}
	if ctx.Err() != nil {
		a.HoldInboundEvent(context.WithoutCancel(ctx), event, where+"-flush-cancelled", true)
		return nil
	}
	if a.ShouldDropDelayedDelivery() {
		a.HoldInboundEvent(ctx, event, where+"-flush", true)
		return nil
	}
	if logFn != nil {
		logFn(event)
	}
	return a.HandleMessage(ctx, event)
}

// Adaptive text batch delay, a near split last chunk waiting longest and short totals settling fast
func (a *TelegramAdapter) TextBatchDelayFor(pending *gateway.MessageEvent) float64 {
	lastLen, totalLen := 0, 0
	if pending != nil {
		lastLen, totalLen = pending.LastChunkLen(), utf8.RuneCountInString(pending.Text)
	}
	if lastLen >= telegramSplitThreshold {
		return a.textBatchSplitDelay
	}
	if totalLen <= textBatchFastLen {
		return min(a.textBatchDelaySeconds, textBatchFastDelayS)
	}
	if totalLen <= textBatchShortLen {
		return min(a.textBatchDelaySeconds, textBatchShortDelayS)
	}
	return a.textBatchDelaySeconds
}

// Telegram text flush that holds the event on a cancel after the pop instead of shielding dispatch
func (a *TelegramAdapter) FlushTextBatch(ctx context.Context, key string) error {
	slots := flushSlots{owner: a.TextBatchTask, release: a.ReleaseTextBatchTask}
	return a.flushBuffered(ctx, slots, a.PopTextBatch, key, a.TextBatchDelayFor(a.PendingTextBatch(key)), "text", func(ev *gateway.MessageEvent) {
		slog.Info(fmt.Sprintf("[Telegram] Flushing text batch %s (%d chars)", key, utf8.RuneCountInString(ev.Text)))
	})
}

// Batching key of a photo burst or album within the event's session
func (a *TelegramAdapter) photoBatchKey(ctx context.Context, event *gateway.MessageEvent, m *models.Message) (string, error) {
	sessionKey, err := a.EventSessionKey(ctx, event)
	if err != nil {
		return "", err
	}
	if m.MediaGroupID != "" {
		return sessionKey + ":album:" + m.MediaGroupID, nil
	}
	return sessionKey + ":photo-burst", nil
}

// Flush slots of a map family guarded by the adapter lock
func (a *TelegramAdapter) mapSlots(tasks func() map[string]*gateway.AdapterTask) flushSlots {
	return flushSlots{owner: func(key string) *gateway.AdapterTask {
		a.mu.Lock()
		defer a.mu.Unlock()
		return tasks()[key]
	}, release: func(key string, task *gateway.AdapterTask) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if task != nil && tasks()[key] == task {
			delete(tasks(), key)
		}
	}}
}

// Pop of a map family guarded by the adapter lock
func (a *TelegramAdapter) mapPop(events func() map[string]*gateway.MessageEvent) func(key string) *gateway.MessageEvent {
	return func(key string) *gateway.MessageEvent {
		a.mu.Lock()
		defer a.mu.Unlock()
		e := events()[key]
		delete(events(), key)
		return e
	}
}

// Dispatches a buffered photo burst or album as one event
func (a *TelegramAdapter) flushPhotoBatch(ctx context.Context, key string) error {
	return a.flushBuffered(ctx, a.mapSlots(func() map[string]*gateway.AdapterTask { return a.pendingPhotoBatchTasks }),
		a.mapPop(func() map[string]*gateway.MessageEvent { return a.pendingPhotoBatches }), key, a.mediaBatchDelaySeconds, "photo",
		func(ev *gateway.MessageEvent) {
			slog.Info(fmt.Sprintf("[Telegram] Flushing photo batch %s with %d image(s)", key, len(ev.MediaURLs)))
		})
}

// Merges an event into the pending one of its key or seeds it, caller holding the adapter lock
func mergeIntoPending(pending map[string]*gateway.MessageEvent, key string, event *gateway.MessageEvent) {
	existing, ok := pending[key]
	if !ok {
		pending[key] = event
		return
	}
	existing.AbsorbReplyExpected(event)
	existing.MediaURLs = append(existing.MediaURLs, event.MediaURLs...)
	existing.MediaTypes = append(existing.MediaTypes, event.MediaTypes...)
	if event.Text != "" {
		existing.Text = gateway.MergeCaption(existing.Text, event.Text)
	}
}

// Merges a photo into its pending batch and re-arms the flush
func (a *TelegramAdapter) enqueuePhotoEvent(ctx context.Context, key string, event *gateway.MessageEvent) {
	if a.ShouldDropDelayedDelivery() {
		a.HoldInboundEvent(ctx, event, "photo-enqueue", true)
		return
	}
	a.mu.Lock()
	mergeIntoPending(a.pendingPhotoBatches, key, event)
	a.mu.Unlock()
	a.AcceptUpdate(ctx)
	a.mu.Lock()
	if prior := a.pendingPhotoBatchTasks[key]; prior != nil && !prior.Done() {
		prior.Cancel()
	}
	a.pendingPhotoBatchTasks[key] = gateway.SpawnAdapterTask(context.WithoutCancel(ctx), func(c context.Context) error { return a.flushPhotoBatch(c, key) })
	a.mu.Unlock()
}

// Routes album items through the media group debounce and singles through the photo burst batcher
func (a *TelegramAdapter) routePhotoEvent(ctx context.Context, m *models.Message, event *gateway.MessageEvent) error {
	if a.DropUnresolved(ctx, event) {
		return nil
	}
	if m.MediaGroupID != "" {
		a.queueMediaGroupEvent(ctx, m.MediaGroupID, event)
		return nil
	}
	key, err := a.photoBatchKey(ctx, event, m)
	if err != nil {
		return err
	}
	a.enqueuePhotoEvent(ctx, key, event)
	return nil
}

// First candidate extension the path ends with, else the default
func extFromPath(filePath string, candidates []string, def string) string {
	if filePath != "" {
		lowered := pystr.Lower(filePath)
		for _, c := range candidates {
			if strings.HasSuffix(lowered, c) {
				return c
			}
		}
	}
	return def
}

// Downloads a voice, audio or video attachment into the cache, true when the oversized event was already dispatched
func (a *TelegramAdapter) cacheInboundAV(ctx context.Context, m *models.Message, event *gateway.MessageEvent, src *mediaSource, label, kind, ext,
	mime string) (bool, error) {
	err := func() error {
		allowed, note := telegramMediaSizeAllowed(sizeValue(src.fileSize), int(a.maxDocBytes), label)
		if !allowed {
			if note != nil {
				event.Text = appendObservedNote(event.Text, *note)
			}
			slog.Info(fmt.Sprintf("[Telegram] Skipped oversized user %s (size=%s)", kind, pyrepr.Str(sizeValue(src.fileSize))))
			return errOversizedDispatched
		}
		data, filePath, err := a.downloadFile(ctx, src.fileID)
		if err != nil {
			return err
		}
		var cachedPath string
		if kind == "video" {
			ext = extFromPath(filePath, gateway.SupportedVideoTypeOrder, ext)
			cachedPath, err = gateway.CacheVideoFromBytesAsync(ctx, data, ext)
			mime = "video/mp4"
			if v, ok := mediacache.SupportedVideoTypes[ext]; ok {
				mime = v
			}
		} else {
			cachedPath, err = gateway.CacheAudioFromBytesAsync(ctx, data, ext)
		}
		if err != nil {
			return err
		}
		event.MediaURLs, event.MediaTypes = []string{cachedPath}, []string{mime}
		slog.Info(fmt.Sprintf("[Telegram] Cached user %s at %s", kind, cachedPath))
		return nil
	}()
	if err == errOversizedDispatched {
		return true, a.HandleMessage(ctx, event)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[Telegram] Failed to cache %s: %s", kind, redactTelegramErrorText(err)))
		a.surfaceMediaCacheFailure(ctx, m, event, label, err, "")
	}
	return false, nil
}

var errOversizedDispatched = fmt.Errorf("oversized attachment dispatched")

// Replaces the event text with a user facing note and dispatches it
func (a *TelegramAdapter) dispatchWithText(ctx context.Context, event *gateway.MessageEvent, text string) (bool, error) {
	event.Text = text
	return true, a.HandleMessage(ctx, event)
}

// Sets the cached media path, MIME and message type of an event
func setCachedMedia(event *gateway.MessageEvent, path, mime string, mtype gateway.MessageType, logFmt string) {
	event.MediaURLs, event.MediaTypes, event.MessageType = []string{path}, []string{mime}, mtype
	slog.Info(fmt.Sprintf(logFmt, path))
}

var telegramImageExtensions = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true}

var telegramImageMimeToExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/jpg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}

var telegramImageExtToMime = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif"}

var unsafeDisplayNameRE = regexp.MustCompile(`[^\p{L}\p{N}_.\- ]`)

// First extension of the ordered table whose MIME matches
func extForMime(order []string, table map[string]string, mime string) string {
	for _, ext := range order {
		if table[ext] == mime {
			return ext
		}
	}
	return ""
}

// Caches a document, images taking the photo path, videos the video path and the rest generic media with text injection
func (a *TelegramAdapter) cacheInboundDocument(ctx context.Context, m *models.Message, event *gateway.MessageEvent) (bool, error) {
	doc := m.Document
	handled, err := func() (bool, error) {
		original := doc.FileName
		ext := ""
		if original != "" {
			ext = pystr.Lower(filepath.Ext(original))
		}
		docMime := pystr.Lower(doc.MimeType)
		if ext == "" && docMime != "" {
			ext = telegramImageMimeToExt[docMime]
			if ext == "" {
				ext = gateway.DocumentMimeToExt()[docMime]
			}
		}
		display := joinFirst(original, docMime, ext, "unknown")
		if doc.FileSize == 0 || doc.FileSize > a.maxDocBytes {
			slog.Info(fmt.Sprintf("[Telegram] Document too large: %s bytes", pyrepr.Str(sizeValue(doc.FileSize))))
			return a.dispatchWithText(ctx, event, fmt.Sprintf("The document is too large or its size could not be verified. Maximum: %d MB.",
				a.maxDocBytes/(1024*1024)))
		}
		if telegramImageExtensions[ext] || strings.HasPrefix(docMime, "image/") {
			data, _, err := a.downloadFile(ctx, doc.FileID)
			if err != nil {
				return false, err
			}
			imageExt := ext
			if !telegramImageExtensions[ext] {
				imageExt = telegramImageMimeToExt[docMime]
				if imageExt == "" {
					imageExt = ".jpg"
				}
			}
			cachedPath, err := cacheImageFromBytesAsync(ctx, data, imageExt)
			if err != nil {
				if isValueError(err) {
					slog.Warn(fmt.Sprintf("[Telegram] Failed to cache image document: %s", redactTelegramErrorText(err)))
					return a.dispatchWithText(ctx, event, fmt.Sprintf("Image document '%s' could not be read as an image.", display))
				}
				return false, err
			}
			mime := docMime
			if !strings.HasPrefix(docMime, "image/") {
				mime = telegramImageExtToMime[imageExt]
				if mime == "" {
					mime = "image/jpeg"
				}
			}
			setCachedMedia(event, cachedPath, mime, gateway.MessageTypePhoto, "[Telegram] Cached user image-document at %s")
			return true, a.routePhotoEvent(ctx, m, event)
		}
		if ext == "" && doc.MimeType != "" {
			ext = gateway.VideoMimeToExt()[doc.MimeType]
		}
		if ext == "" && doc.MimeType != "" {
			ext = extForMime(gateway.SupportedImageDocumentTypeOrder, gateway.SupportedImageDocumentTypes, doc.MimeType)
		}
		if mime, ok := mediacache.SupportedVideoTypes[ext]; ok {
			data, _, err := a.downloadFile(ctx, doc.FileID)
			if err != nil {
				return false, err
			}
			cachedPath, err := gateway.CacheVideoFromBytesAsync(ctx, data, ext)
			if err != nil {
				return false, err
			}
			setCachedMedia(event, cachedPath, mime, gateway.MessageTypeVideo, "[Telegram] Cached user video document at %s")
			return true, a.HandleMessage(ctx, event)
		}
		raw, _, err := a.downloadFile(ctx, doc.FileID)
		if err != nil {
			return false, err
		}
		filename := original
		if filename == "" {
			filename = "document" + firstNonEmpty(ext, ".bin")
		}
		cached, err := gateway.CacheMediaBytes(ctx, raw, filename, docMime, "")
		if err != nil {
			return false, err
		}
		if cached == nil {
			return a.dispatchWithText(ctx, event, fmt.Sprintf("Document '%s' could not be cached.", display))
		}
		inlined := false
		event.MediaURLs, event.MediaTypes, event.MediaTextInlined = []string{cached.Path}, []string{cached.MediaType}, []*bool{&inlined}
		if cached.Kind == "audio" {
			event.MessageType = gateway.MessageTypeAudio
		}
		slog.Info(fmt.Sprintf("[Telegram] Cached user %s at %s (%s)", cached.Kind, cached.Path, cached.MediaType))
		const maxTextInjectBytes = 100 * 1024
		isText := gateway.TextInjectExtensions[ext] || strings.HasPrefix(docMime, "text/")
		if isText && len(raw) <= maxTextInjectBytes && utf8.Valid(raw) {
			displayName := unsafeDisplayNameRE.ReplaceAllString(firstNonEmpty(original, "document"+firstNonEmpty(ext, ".txt")), "_")
			injection := fmt.Sprintf("[Content of %s]:\n%s", displayName, string(raw))
			if event.Text != "" {
				event.Text = injection + "\n\n" + event.Text
			} else {
				event.Text = injection
			}
			yes := true
			event.MediaTextInlined = []*bool{&yes}
		}
		return false, nil
	}()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[Telegram] Failed to cache document: %s", redactTelegramErrorText(err)))
		a.surfaceMediaCacheFailure(ctx, m, event, "attachment", err, doc.FileName)
		return false, nil
	}
	return handled, nil
}

// Whether the error is a Python ValueError
func isValueError(err error) bool { return className(err) == "ValueError" }

// First non empty string
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// First non empty of the display candidates
func joinFirst(values ...string) string { return firstNonEmpty(values...) }

// Handles incoming media, caching photos, voice, audio, video and documents before dispatch
func (a *TelegramAdapter) handleMediaMessage(ctx context.Context, update *Update) error {
	m := update.Raw.Message
	if m == nil {
		return nil
	}
	if !a.core.isUserAuthorizedFromMessage(ctx, m) {
		logBlockedUser(m, slog.LevelInfo, "media from unauthorized user")
		return nil
	}
	id := update.UpdateID
	if !a.shouldProcessMessage(ctx, m) {
		if a.shouldObserveUnmentionedGroupMessage(ctx, m) {
			observed := a.buildMessageEvent(ctx, m, mediaMessageType(m), &id)
			if m.Caption != "" {
				expanded := expandLinkEntities(entityMessageOf(m))
				if cleaned := a.cleanBotTriggerText(&expanded); cleaned != nil {
					observed.Text = *cleaned
				}
			}
			a.cacheObservedMedia(ctx, m, observed)
			a.observeUnmentionedGroupMessage(ctx, m, observed.MessageType, &id, observed)
		}
		return nil
	}
	event := a.buildMessageEvent(ctx, m, mediaMessageType(m), &id)
	if m.Caption != "" {
		expanded := expandLinkEntities(entityMessageOf(m))
		if trigger := GroupTriggerText(a, a.contextMessageOf(m), &expanded); trigger != nil {
			event.Text = *trigger
		} else {
			event.Text = ""
		}
	}
	if m.Sticker != nil {
		a.handleSticker(ctx, m, event)
		return a.HandleMessage(ctx, a.applyGroupObserveAttribution(ctx, event))
	}
	event = a.applyGroupObserveAttribution(ctx, event)
	if len(m.Photo) > 0 {
		err := func() error {
			data, filePath, err := a.downloadFile(ctx, m.Photo[len(m.Photo)-1].FileID)
			if err != nil {
				return err
			}
			ext := extFromPath(filePath, []string{".png", ".webp", ".gif", ".jpeg", ".jpg"}, ".jpg")
			cachedPath, err := cacheImageFromBytesAsync(ctx, data, ext)
			if err != nil {
				return err
			}
			setCachedMedia(event, cachedPath, "image/"+strings.TrimLeft(ext, "."), event.MessageType, "[Telegram] Cached user photo at %s")
			return nil
		}()
		if err == nil {
			return a.routePhotoEvent(ctx, m, event)
		}
		slog.Warn(fmt.Sprintf("[Telegram] Failed to cache photo: %s", redactTelegramErrorText(err)))
		a.surfaceMediaCacheFailure(ctx, m, event, "photo", err, "")
	}
	switch {
	case m.Voice != nil:
		src := &mediaSource{fileID: m.Voice.FileID, fileSize: m.Voice.FileSize}
		if done, err := a.cacheInboundAV(ctx, m, event, src, "voice message", "voice", ".ogg", "audio/ogg"); done || err != nil {
			return err
		}
	case m.Audio != nil:
		src := &mediaSource{fileID: m.Audio.FileID, fileSize: m.Audio.FileSize}
		if done, err := a.cacheInboundAV(ctx, m, event, src, "audio file", "audio", ".mp3", "audio/mp3"); done || err != nil {
			return err
		}
	case m.Video != nil || m.VideoNote != nil:
		label, src := "video file", &mediaSource{}
		if m.Video != nil {
			src.fileID, src.fileSize = m.Video.FileID, m.Video.FileSize
		} else {
			label, src.fileID, src.fileSize = "video note", m.VideoNote.FileID, int64(m.VideoNote.FileSize)
		}
		if done, err := a.cacheInboundAV(ctx, m, event, src, label, "video", ".mp4", "video/mp4"); done || err != nil {
			return err
		}
	case m.Document != nil:
		if done, err := a.cacheInboundDocument(ctx, m, event); done || err != nil {
			return err
		}
	}
	if m.MediaGroupID != "" {
		a.queueMediaGroupEvent(ctx, m.MediaGroupID, event)
		return nil
	}
	return a.HandleMessage(ctx, event)
}

// Debounces album items sharing a media group into one event
func (a *TelegramAdapter) queueMediaGroupEvent(ctx context.Context, mediaGroupID string, event *gateway.MessageEvent) {
	if a.ShouldDropDelayedDelivery() {
		a.HoldInboundEvent(ctx, event, "media-group-enqueue", true)
		return
	}
	a.mu.Lock()
	mergeIntoPending(a.mediaGroupEvents, mediaGroupID, event)
	a.mu.Unlock()
	a.AcceptUpdate(ctx)
	a.mu.Lock()
	if prior := a.mediaGroupTasks[mediaGroupID]; prior != nil {
		prior.Cancel()
	}
	a.mediaGroupTasks[mediaGroupID] = gateway.SpawnAdapterTask(context.WithoutCancel(ctx), func(c context.Context) error {
		return a.flushMediaGroupEvent(c, mediaGroupID)
	})
	a.mu.Unlock()
}

// Dispatches a debounced album as one event
func (a *TelegramAdapter) flushMediaGroupEvent(ctx context.Context, mediaGroupID string) error {
	return a.flushBuffered(ctx, a.mapSlots(func() map[string]*gateway.AdapterTask { return a.mediaGroupTasks }),
		a.mapPop(func() map[string]*gateway.MessageEvent { return a.mediaGroupEvents }), mediaGroupID, mediaGroupWaitSeconds, "media-group", nil)
}

// Vision analysis of the sticker tool, the tools vision analyzer in production
var visionAnalyzeTool = tools.VisionAnalyzeTool

// Describes a sticker through vision cached by its unique id, animated and video stickers getting the emoji injection
func (a *TelegramAdapter) handleSticker(ctx context.Context, m *models.Message, event *gateway.MessageEvent) {
	sticker := m.Sticker
	emoji, setName := sticker.Emoji, sticker.SetName
	if sticker.IsAnimated || sticker.IsVideo {
		event.Text = gateway.BuildAnimatedStickerInjection(emoji)
		return
	}
	if cached := gateway.GetCachedStickerDescription(ctx, sticker.FileUniqueID); len(cached) > 0 {
		cachedEmoji, cachedSet := emoji, setName
		if v, ok := cached["emoji"]; ok {
			cachedEmoji = pyrepr.Str(v)
		}
		if v, ok := cached["set_name"]; ok {
			cachedSet = pyrepr.Str(v)
		}
		event.Text = gateway.BuildStickerInjection(pyrepr.Str(cached["description"]), cachedEmoji, cachedSet)
		slog.Info(fmt.Sprintf("[Telegram] Sticker cache hit: %s", sticker.FileUniqueID))
		return
	}
	fallback := "a sticker"
	if emoji != "" {
		fallback = "a sticker with emoji " + emoji
	}
	err := func() error {
		data, _, err := a.downloadFile(ctx, sticker.FileID)
		if err != nil {
			return err
		}
		cachedPath, err := cacheImageFromBytesAsync(ctx, data, ".webp")
		if err != nil {
			return err
		}
		slog.Info(fmt.Sprintf("[Telegram] Analyzing sticker at %s", cachedPath))
		a.AcceptUpdate(ctx)
		raw := visionAnalyzeTool(ctx, urlSafetyEnv(ctx), cachedPath, gateway.StickerVisionPrompt)
		var result map[string]any
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return err
		}
		if pyTruthy(result["success"]) {
			description := "a sticker"
			if v, ok := result["analysis"]; ok {
				description = pyrepr.Str(v)
			}
			if err := gateway.CacheStickerDescription(ctx, sticker.FileUniqueID, description, emoji, setName); err != nil {
				return err
			}
			event.Text = gateway.BuildStickerInjection(description, emoji, setName)
			return nil
		}
		event.Text = gateway.BuildStickerInjection(fallback, emoji, setName)
		return nil
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("[Telegram] Sticker analysis error: %s", redactTelegramErrorText(err)))
		event.Text = gateway.BuildStickerInjection(fallback, emoji, setName)
	}
}

// Topic name and skill of a DM topic or a bound forum topic
func (a *TelegramAdapter) resolveTopicBinding(ctx context.Context, m *models.Message, chatType string, threadID *string) (*string, any) {
	var chatTopic *string
	var topicSkill any
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	if chatType == "dm" && threadID != nil {
		if info := a.getDMTopicInfo(ctx, chatID, *threadID); info != nil {
			if name, ok := info["name"]; ok && name != nil {
				s := pyrepr.Str(name)
				chatTopic = &s
			}
			topicSkill = info["skill"]
		}
		if m.ForumTopicCreated != nil && m.ForumTopicCreated.Name != "" {
			created := m.ForumTopicCreated.Name
			a.cacheDMTopicFromMessage(chatID, *threadID, created)
			if chatTopic == nil || *chatTopic == "" {
				chatTopic = &created
			}
		}
		return chatTopic, topicSkill
	}
	if chatType != "group" || threadID == nil {
		return nil, nil
	}
	var entries []map[string]any
	switch cfg := a.Config.Extra["group_topics"].(type) {
	case map[string]any:
		for cid, topics := range cfg {
			entries = append(entries, map[string]any{"chat_id": cid, "topics": topics})
		}
	case []any:
		for _, e := range cfg {
			if entry, ok := e.(map[string]any); ok {
				entries = append(entries, entry)
			}
		}
	}
	for _, entry := range entries {
		var entryChat any = ""
		if v, has := entry["chat_id"]; has {
			entryChat = v
		}
		if pyrepr.Str(entryChat) != chatID {
			continue
		}
		topics, _ := entry["topics"].([]any)
		for _, raw := range topics {
			topic, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if tid, has := topic["thread_id"]; has && tid != nil && pyrepr.Str(tid) == *threadID {
				if name := topic["name"]; name != nil {
					s := pyrepr.Str(name)
					chatTopic = &s
				}
				topicSkill = topic["skill"]
				break
			}
		}
		break
	}
	return chatTopic, topicSkill
}

// Replied message id and text, the native partial quote first, then text, caption, rich echo and the sent index
func (a *TelegramAdapter) replyContext(ctx context.Context, m *models.Message) (*string, *string) {
	if m.ReplyToMessage == nil {
		return nil, nil
	}
	replyID := strconv.Itoa(m.ReplyToMessage.ID)
	if m.Quote != nil && m.Quote.Text != "" {
		quote := m.Quote.Text
		return &replyID, &quote
	}
	text := firstNonEmpty(m.ReplyToMessage.Text, m.ReplyToMessage.Caption)
	if text != "" {
		return &replyID, &text
	}
	if m.ReplyToMessage.RichMessage != nil {
		var rich any
		if raw, err := json.Marshal(m.ReplyToMessage.RichMessage); err == nil && json.Unmarshal(raw, &rich) == nil {
			if extracted := extractRichReplyText(rich); extracted != nil && *extracted != "" {
				return &replyID, extracted
			}
		}
	}
	if stored, ok := gateway.RichSentLookup(ctx, strconv.FormatInt(m.Chat.ID, 10), replyID); ok && stored != "" {
		return &replyID, &stored
	}
	return &replyID, nil
}

// Message event of a Telegram message with its routed source, reply context, prompts and edit identity
func (a *TelegramAdapter) buildMessageEvent(ctx context.Context, m *models.Message, msgType gateway.MessageType, updateID *int64) *gateway.MessageEvent {
	chat := &m.Chat
	user := m.From
	telegramType := chatTypeStr(chat)
	chatType := "dm"
	switch telegramType {
	case "group", "supergroup":
		chatType = "group"
	case "channel":
		chatType = "channel"
	}
	threadID := effectiveMessageThreadID(m)
	threadID = a.topics.AdoptCreatedTopicThread(a, a.topicMessageOf(ctx, m), chatType, threadID)
	chatTopic, topicSkill := a.resolveTopicBinding(ctx, m, chatType, threadID)
	chatFullName := fullName(chat.FirstName, chat.LastName)
	if chat.FirstName == "" {
		chatFullName = ""
	}
	var userName *string
	switch {
	case user != nil:
		name := fullName(user.FirstName, user.LastName)
		userName = &name
	case chatType == "dm":
		userName = nonEmpty(chatFullName)
	case chatType == "channel":
		userName = nonEmpty(chat.Title)
	}
	chatName := nonEmpty(firstNonEmpty(chat.Title, chatFullName))
	chatID := strconv.FormatInt(chat.ID, 10)
	var userID *string
	if user != nil {
		id := strconv.FormatInt(user.ID, 10)
		userID = &id
	} else if chatType == "dm" || chatType == "channel" {
		userID = &chatID
	}
	messageID := strconv.Itoa(m.ID)
	isBot := user != nil && user.IsBot
	source := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatName: chatName, ChatType: chatType, UserID: userID, UserName: userName,
		ThreadID: threadID, ChatTopic: chatTopic, MessageID: &messageID, IsBot: isBot})
	replyID, replyText := a.replyContext(ctx, m)
	var channelPrompt *string
	promptChannel, promptParent := chatID, ""
	if threadID != nil {
		promptChannel, promptParent = *threadID, chatID
	}
	if prompt, ok := gateway.ResolveChannelPrompt(a.Config.Extra, promptChannel, promptParent); ok {
		channelPrompt = &prompt
	}
	cm := a.contextMessageOf(m)
	event := gateway.NewMessageEvent(expandLinkEntities(entityMessageOf(m)))
	event.MessageType, event.Source, event.RawMessage = msgType, source, m
	event.MessageID, event.PlatformUpdateID = &messageID, updateID
	event.ReplyToMessageID, event.ReplyToText, event.AutoSkill = replyID, replyText, topicSkill
	event.ChannelPrompt = GroupIdentityPrompt(a, cm, channelPrompt)
	event.ReplyExpected = GroupReplyExpected(a, cm, chatType)
	event.EditRevision = editRevision(m, updateID)
	event.Timestamp = time.Unix(int64(m.Date), 0).UTC()
	return event
}

// Identity of one edit of a message, nil for a new send
func editRevision(m *models.Message, updateID *int64) *string {
	if m.EditDate == 0 {
		return nil
	}
	if updateID != nil {
		s := fmt.Sprintf("u%d", *updateID)
		return &s
	}
	sum := sha256.Sum256([]byte(firstNonEmpty(m.Text, m.Caption)))
	s := fmt.Sprintf("%d.%s", m.EditDate, hex.EncodeToString(sum[:])[:16])
	return &s
}

// Entity text and caption of a message for link expansion
func entityMessageOf(m *models.Message) EntityMessage {
	return EntityMessage{Text: m.Text, Entities: entitiesOf(m.Entities), Caption: m.Caption, CaptionEntities: entitiesOf(m.CaptionEntities)}
}

// Helper module entities of SDK entities
func entitiesOf(entities []models.MessageEntity) []MessageEntity {
	out := make([]MessageEntity, 0, len(entities))
	for _, e := range entities {
		me := MessageEntity{Type: string(e.Type), Offset: e.Offset, Length: e.Length}
		if e.URL != "" {
			me.URL = e.URL
		}
		if e.User != nil {
			me.User = &TopicUser{ID: e.User.ID, IsBot: e.User.IsBot}
		}
		out = append(out, me)
	}
	return out
}

// Group addressing view of a message carrying the SDK message for the host callbacks
func (a *TelegramAdapter) contextMessageOf(m *models.Message) *ContextMessage {
	if m == nil {
		return nil
	}
	cm := &ContextMessage{raw: m, IsTopicMessage: m.IsTopicMessage, MessageID: int64(m.ID), ReplyToMessage: a.contextMessageOf(m.ReplyToMessage)}
	if m.Text != "" {
		text := m.Text
		cm.Text = &text
	}
	if m.Caption != "" {
		caption := m.Caption
		cm.Caption = &caption
	}
	if m.MessageThreadID != 0 {
		cm.MessageThreadID = int64(m.MessageThreadID)
	}
	if m.From != nil {
		cm.FromUser = &TopicUser{ID: m.From.ID, IsBot: m.From.IsBot}
	}
	if m.SenderChat != nil {
		cm.SenderChat = m.SenderChat
	}
	if m.Voice != nil {
		cm.Voice = m.Voice
	}
	if m.VideoNote != nil {
		cm.VideoNote = m.VideoNote
	}
	if m.Audio != nil {
		cm.Audio = m.Audio
	}
	return cm
}

// Topic routing view of a message carrying the SDK message and the auth context
func (a *TelegramAdapter) topicMessageOf(ctx context.Context, m *models.Message) *TopicMessage {
	tm := &TopicMessage{raw: m, authCtx: ctx, MessageID: int64(m.ID), Chat: &m.Chat}
	if m.MessageThreadID != 0 {
		tm.MessageThreadID = int64(m.MessageThreadID)
	}
	if m.From != nil {
		tm.FromUser = &TopicUser{ID: m.From.ID, IsBot: m.From.IsBot}
	}
	if m.Date != 0 {
		date := time.Unix(int64(m.Date), 0).UTC()
		tm.Date = &date
	}
	if m.ForumTopicCreated != nil {
		tm.ForumTopicCreated = m.ForumTopicCreated
	}
	return tm
}

// Records an authorized private chat topic creation as a routing signal
func (a *TelegramAdapter) handleForumTopicCreated(ctx context.Context, update *Update) error {
	m := effectiveMessage(update.Raw)
	if m == nil || m.ForumTopicCreated == nil {
		return nil
	}
	a.topics.HandleForumTopicCreated(a, a.topicMessageOf(ctx, m))
	return nil
}

// Text and caption entity sources of a context message
func (a *TelegramAdapter) EntitySources(cm *ContextMessage) []EntitySource {
	if cm == nil || cm.raw == nil {
		return []EntitySource{{Text: firstText(cm.Text)}, {Text: firstText(cm.Caption)}}
	}
	return []EntitySource{{Text: cm.raw.Text, Entities: entitiesOf(cm.raw.Entities)}, {Text: cm.raw.Caption, Entities: entitiesOf(cm.raw.CaptionEntities)}}
}

// Bare lowercase entity type
func (a *TelegramAdapter) EntityType(entity MessageEntity) string {
	parts := strings.Split(pyrepr.Str(entity.Type), ".")
	return pystr.Lower(parts[len(parts)-1])
}

// Entity span of a helper module entity, empty when its offsets are unusable
func (a *TelegramAdapter) EntitySpan(sourceText string, entity MessageEntity) string {
	offset, ok1 := entityInt(entity.Offset)
	length, ok2 := entityInt(entity.Length)
	if !ok1 {
		offset = -1
	}
	if !ok2 {
		length = 0
	}
	if offset < 0 || length <= 0 {
		return ""
	}
	return telegramEntityText(sourceText, offset, length)
}

// Bot user id when connected
func (a *TelegramAdapter) Bot() (any, bool) {
	id, ok := a.botID()
	if !ok {
		return nil, false
	}
	return id, true
}

// Live bot username for the context helpers
func (a *TelegramAdapter) CurrentBotUsername() string { return a.currentBotUsername() }

// Whether the context message replies to the bot
func (a *TelegramAdapter) IsReplyToBot(cm *ContextMessage) bool {
	return cm != nil && cm.raw != nil && a.isReplyToBot(cm.raw)
}

// Whether the context message mentions the bot
func (a *TelegramAdapter) MessageMentionsBot(cm *ContextMessage) bool {
	return cm != nil && cm.raw != nil && a.messageMentionsBot(cm.raw)
}

// Whether the context message matches a wake word
func (a *TelegramAdapter) MessageMatchesMentionPatterns(cm *ContextMessage) bool {
	return cm != nil && cm.raw != nil && a.messageMatchesMentionPatterns(cm.raw)
}

// Whether the context message is in a group chat
func (a *TelegramAdapter) IsGroupChat(cm *ContextMessage) bool {
	return cm != nil && cm.raw != nil && isGroupChat(cm.raw)
}

// Trigger text with the bot mention removed
func (a *TelegramAdapter) CleanBotTriggerText(text *string) *string {
	return a.cleanBotTriggerText(text)
}

// Bare lowercase chat type name of a topic routing chat
func (a *TelegramAdapter) ChatTypeStr(chat any) string {
	c, _ := chat.(*models.Chat)
	return chatTypeStr(c)
}

// Chat id text of a topic routing message
func (a *TelegramAdapter) ChatIDStr(tm *TopicMessage) string {
	if c, ok := tm.Chat.(*models.Chat); ok && c != nil {
		return strconv.FormatInt(c.ID, 10)
	}
	return ""
}

// Whether the sender of a topic routing message is authorized
func (a *TelegramAdapter) IsUserAuthorizedFromMessage(tm *TopicMessage) bool {
	if tm.raw == nil {
		return false
	}
	ctx := tm.authCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return a.core.isUserAuthorizedFromMessage(ctx, tm.raw)
}
