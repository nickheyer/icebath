package telegram

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"os"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	richMessageMaxChars  = 32768
	generalTopicThreadID = "1"
)

// Python truthiness of a config value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Float env var with non finite or unparseable values reading as the default, then clamped to the bounds
func envFloatClamped(name string, def float64, minValue, maxValue *float64) float64 {
	value := def
	if raw, ok := os.LookupEnv(name); ok {
		parsed, err := pyint.ParseFloat(raw)
		if err != nil {
			parsed = def
		}
		value = parsed
	}
	if math.IsInf(value, 0) || math.IsNaN(value) {
		value = def
	}
	if minValue != nil {
		value = math.Max(value, *minValue)
	}
	if maxValue != nil {
		value = math.Min(value, *maxValue)
	}
	return value
}

// Telegram chat type mapped to the gateway chat type, private as dm and supergroup as forum or group
func normalizeChatType(chatType any, isForum bool) string {
	raw := "dm"
	if pyTruthy(chatType) {
		raw = pyrepr.Str(chatType)
	}
	normalized := pystr.Lower(pystr.Strip(raw))
	if normalized == "" {
		normalized = "dm"
	}
	switch normalized {
	case "private":
		return "dm"
	case "supergroup":
		if isForum {
			return "forum"
		}
		return "group"
	}
	return normalized
}

// TELEGRAM_ALLOWED_USERS decision for the user, nil when no allowlist is configured
func envAllowlistDecision(ctx context.Context, userID string) *bool {
	allowedCSV := pystr.Strip(gateway.PlatformGateEnv(ctx, "TELEGRAM_ALLOWED_USERS", ""))
	if allowedCSV == "" {
		return nil
	}
	allowed := map[string]bool{}
	for _, uid := range strings.Split(allowedCSV, ",") {
		if uid = pystr.Strip(uid); uid != "" {
			allowed[uid] = true
		}
	}
	decision := allowed["*"] || allowed[userID]
	return &decision
}

// Full name of a Telegram user or private chat, first and last name joined by a space
func fullName(first, last string) string {
	if last == "" {
		return first
	}
	return first + " " + last
}

// Pointer to the stripped text, nil when it strips to nothing
func nonEmpty(text string) *string {
	if text = pystr.Strip(text); text == "" {
		return nil
	}
	return &text
}

// Whether Telegram auth env vars make an early intake decision safe
func telegramAuthEnvConfigured(ctx context.Context) bool {
	for _, key := range []string{"TELEGRAM_ALLOWED_USERS", "TELEGRAM_GROUP_ALLOWED_USERS", "TELEGRAM_GROUP_ALLOWED_CHATS",
		"TELEGRAM_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS", "GATEWAY_ALLOW_ALL_USERS"} {
		if pystr.Strip(gateway.PlatformGateEnv(ctx, key, "")) != "" {
			return true
		}
	}
	return false
}

// Text of a metadata id as Python str, nil when absent or falsy
func metadataID(metadata map[string]any, keys ...string) *string {
	var value any
	for _, key := range keys {
		if value = metadata[key]; pyTruthy(value) {
			break
		}
	}
	if value == nil {
		return nil
	}
	text := pyrepr.Str(value)
	return &text
}

// Thread id from send metadata, thread_id first and message_thread_id second
func metadataThreadID(metadata map[string]any) *string {
	return metadataID(metadata, "thread_id", "message_thread_id")
}

// Native Bot API DM topic id from send metadata
func metadataDirectMessagesTopicID(metadata map[string]any) *string {
	return metadataID(metadata, "direct_messages_topic_id", "telegram_direct_messages_topic_id")
}

// Reply anchor carried in send metadata, nil when absent or not a positive message id
func metadataReplyToMessageID(metadata map[string]any) *int {
	return messageIDInt(ParseTelegramMessageID(metadata["telegram_reply_to_message_id"]))
}

// Whether the metadata marks a private chat topic lane
func dmTopicFallback(metadata map[string]any) bool {
	return len(metadata) > 0 && pyTruthy(metadata["telegram_dm_topic_reply_fallback"])
}

// Whether a send targets a private DM topic lane that needs a reply anchor
func isPrivateDMTopicSend(threadID *string, metadata map[string]any) bool {
	if metadataDirectMessagesTopicID(metadata) != nil {
		return dmTopicFallback(metadata) && metadataReplyToMessageID(metadata) != nil
	}
	if len(metadata) > 0 && pyTruthy(metadata["telegram_dm_topic_created_for_send"]) {
		return false
	}
	return threadID != nil && *threadID != "" && dmTopicFallback(metadata)
}

const dmTopicMissingAnchorError = "Telegram DM topic delivery requires a reply anchor; refusing to send outside the requested topic"

// Reply anchor of a send, the explicit reply first then the DM topic metadata anchor unless replies are off
func replyToMessageIDForSend(replyTo *string, metadata map[string]any, replyToMode string) *int {
	var raw any
	if replyTo != nil {
		raw = *replyTo
	}
	if anchor := messageIDInt(ParseTelegramMessageID(raw)); anchor != nil {
		return anchor
	}
	if dmTopicFallback(metadata) && replyToMode != "off" {
		return metadataReplyToMessageID(metadata)
	}
	return nil
}

// Forum and DM topic routing of a Bot API send, nil fields omitted from the request
type threadKwargs struct {
	MessageThreadID       *int
	DirectMessagesTopicID *int
}

// Routing kwargs for forum topics, native DM topics and private chat topic lanes
func threadKwargsForSend(threadID *string, metadata map[string]any, replyToMessageID *int, replyToMode string) (threadKwargs, error) {
	fallback := dmTopicFallback(metadata)
	if fallback && replyToMode != "off" {
		if replyToMessageID == nil {
			replyToMessageID = metadataReplyToMessageID(metadata)
		}
		if replyToMessageID == nil {
			threadMessageID, err := messageThreadIDForSend(threadID)
			if err != nil {
				return threadKwargs{}, err
			}
			if threadMessageID != nil {
				return threadKwargs{MessageThreadID: threadMessageID}, nil
			}
			direct, err := directTopicKwargs(metadata)
			if err != nil || direct == nil {
				return threadKwargs{}, err
			}
			return *direct, nil
		}
	} else if !fallback {
		direct, err := directTopicKwargs(metadata)
		if err != nil {
			return threadKwargs{}, err
		}
		if direct != nil {
			return *direct, nil
		}
	}
	threadMessageID, err := messageThreadIDForSend(threadID)
	if err != nil {
		return threadKwargs{}, err
	}
	return threadKwargs{MessageThreadID: threadMessageID}, nil
}

// Native Bot API DM topic routing, nil when the metadata carries no direct_messages_topic_id
func directTopicKwargs(metadata map[string]any) (*threadKwargs, error) {
	directTopicID := metadataDirectMessagesTopicID(metadata)
	if directTopicID == nil {
		return nil, nil
	}
	id, err := pyIntOf(*directTopicID)
	if err != nil {
		return nil, err
	}
	return &threadKwargs{DirectMessagesTopicID: &id}, nil
}

// Integer forum thread id for sendMessage, nil for no thread or the General topic
func messageThreadIDForSend(threadID *string) (*int, error) {
	if threadID == nil || *threadID == "" || *threadID == generalTopicThreadID {
		return nil, nil
	}
	id, err := pyIntOf(*threadID)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// Integer thread id for sendChatAction, General kept so the typing bubble lands there
func messageThreadIDForTyping(threadID *string) (*int, error) {
	if threadID == nil || *threadID == "" {
		return nil, nil
	}
	id, err := pyIntOf(*threadID)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// Python int() of text as a Go int, the ValueError text on failure
func pyIntOf(text string) (int, error) {
	n, err := pyint.Of(text)
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// Whether the Bot API rejected the forum or DM thread as missing
func isThreadNotFoundError(err error) bool {
	return strings.Contains(pystr.Lower(err.Error()), "thread not found")
}

// Whether err is a Bot API BadRequest by class name or type
func isBadRequestError(err error) bool {
	name := pystr.Lower(className(err))
	if name == "badrequest" || strings.HasSuffix(name, "badrequest") {
		return true
	}
	return isPTBError(err, "BadRequest")
}

// Whether a DM topic send should retry with routing stripped after a stale anchor or a rejected topic id
func shouldRetryWithoutDMTopicReplyAnchor(err error, metadata map[string]any, replyToMessageID *int) bool {
	if !dmTopicFallback(metadata) || !isBadRequestError(err) {
		return false
	}
	errLower := pystr.Lower(err.Error())
	if replyToMessageID != nil && strings.Contains(errLower, "message to be replied not found") {
		return true
	}
	if !pyTruthy(metadata["direct_messages_topic_id"]) {
		return false
	}
	for _, marker := range []string{"direct_messages_topic", "message thread not found", "thread not found", "topic_closed",
		"topic_deleted", "topic not found"} {
		if strings.Contains(errLower, marker) {
			return true
		}
	}
	return false
}

// Whether err reports another getUpdates poller holding the bot token
func looksLikePollingConflict(err error) bool {
	text := pystr.Lower(err.Error())
	return pystr.Lower(className(err)) == "conflict" || strings.Contains(text, "terminated by other getupdates request") ||
		strings.Contains(text, "another bot instance is running")
}

// Whether err is a terminal credential failure by type, InvalidToken or Forbidden
func looksLikeAuthError(err error) bool {
	switch pystr.Lower(className(err)) {
	case "invalidtoken", "forbidden":
		return true
	}
	return isPTBError(err, "InvalidToken") || isPTBError(err, "Forbidden")
}

// Whether err is a transient transport failure that warrants a reconnect
func looksLikeNetworkError(err error) bool {
	switch pystr.Lower(className(err)) {
	case "badrequest", "invalidtoken", "forbidden", "retryafter":
		return false
	case "networkerror", "timedout", "connectionerror":
		return true
	}
	for _, class := range []string{"BadRequest", "InvalidToken", "Forbidden", "RetryAfter"} {
		if isPTBError(err, class) {
			return false
		}
	}
	if isPTBError(err, "NetworkError") || isPTBError(err, "TimedOut") {
		return true
	}
	return isOSError(err)
}

// Whether any error in the wrapped graph matches the class name marker or one of the text markers
func exceptionGraphMatches(err error, nameMarker string, textMarkers ...string) bool {
	for _, cur := range iterExceptionGraph(err) {
		text := pystr.Lower(cur.Error())
		if strings.Contains(pystr.Lower(className(cur)), nameMarker) {
			return true
		}
		for _, m := range textMarkers {
			if strings.Contains(text, m) {
				return true
			}
		}
	}
	return false
}

// Whether a timeout wraps a connect timeout, when the request never reached Telegram and a resend is safe
func looksLikeConnectTimeout(err error) bool {
	return exceptionGraphMatches(err, "connecttimeout", "connect timeout", "connect timed out")
}

// Whether a timeout wraps a connection pool timeout, when the request was never sent
func looksLikePoolTimeout(err error) bool {
	for _, cur := range iterExceptionGraph(err) {
		name := pystr.Lower(className(cur))
		text := pystr.Lower(cur.Error())
		if strings.Contains(name, "pooltimeout") || strings.Contains(text, "pool timeout") ||
			(strings.Contains(text, "connection pool") && strings.Contains(text, "occupied")) {
			return true
		}
	}
	return false
}

// Whether the content fits the rich message character cap
func contentFitsRichLimits(content string) bool {
	return len([]rune(content)) <= richMessageMaxChars
}

// Whether a live bot client can carry rich message requests
var (
	richDetailsRE       = mustTelegramRE(`(?is)<details\b[^>]*>.*?</details>`)
	richMathInDetailsRE = mustTelegramRE(`(?is)(\$\$.*?\$\$|\\\[.*?\\\]|\\\(.*?\\\)|\\(?:sum|frac|alpha|beta|gamma|delta|theta|lambda|mu|pi|sigma|int|prod|sqrt|lim|infty|begin\{(?:equation|align|matrix|cases)\}))`)
	richCJKRE           = mustTelegramRE("[぀-ヿ㐀-䶿一-鿿가-힯豈-﫿\U00020000-\U000323af]")
	richTaskListRE      = mustTelegramRE(`(?m)^\s*[-*]\s+\[[ xX]\]\s+`)
	richDetailsTagRE    = mustTelegramRE(`(?m)^<details\b|^</details>|^<summary\b|^</summary>`)
)

// Whether math inside a details block would crash Telegram Desktop
func hasTelegramDesktopDetailsMathCrashShape(content string) bool {
	if content == "" {
		return false
	}
	for _, block := range regexp2FindAll(richDetailsRE, content) {
		if regexp2Search(richMathInDetailsRE, block) {
			return true
		}
	}
	return false
}

// Whether CJK text would garble in Telegram Desktop rich rendering
func hasTelegramDesktopCJKRichGarbleShape(content string) bool {
	return content != "" && regexp2Search(richCJKRE, content)
}

// Whether the content holds constructs MarkdownV2 degrades, tables, task lists, details blocks or block math
func needsRichRendering(content string) bool {
	if content == "" {
		return false
	}
	for _, line := range pystr.Splitlines(content, false) {
		if gateway.IsTableSeparatorLine(line) {
			return true
		}
	}
	if regexp2Search(richTaskListRE, content) || regexp2Search(richDetailsTagRE, content) {
		return true
	}
	return strings.Contains(content, "$$")
}

// Rich message from raw markdown with normalized line breaks, never the MarkdownV2 escaped text
func richMessagePayload(content string, skipEntityDetection bool) models.InputRichMessage {
	return models.InputRichMessage{Markdown: richNormalizeLinebreaks(content), SkipEntityDetection: skipEntityDetection}
}

// Positive message id as an int, nil for none
func messageIDInt(id *big.Int) *int {
	if id == nil || id.Sign() == 0 {
		return nil
	}
	n := int(id.Int64())
	return &n
}

// Whether the pattern matches anywhere in the text, the re.search of hermes
func regexp2Search(re *regexp2.Regexp, text string) bool {
	m, err := re.FindStringMatch(text)
	return err == nil && m != nil
}

// Every non overlapping match of the pattern in order, the re.findall of a group less pattern
func regexp2FindAll(re *regexp2.Regexp, text string) []string {
	var out []string
	m, err := re.FindStringMatch(text)
	for err == nil && m != nil {
		out = append(out, m.String())
		m, err = re.FindNextMatch(m)
	}
	return out
}

// Whether the rich endpoint itself is unavailable, which latches rich delivery off
func isRichCapabilityError(err error) bool {
	switch pystr.Lower(className(err)) {
	case "endpointnotfound", "invalidtoken":
		return true
	}
	switch className(err) {
	case "AttributeError", "TypeError", "NotImplementedError":
		return true
	}
	if coded, ok := err.(interface{ ErrorCode() int }); ok && coded.ErrorCode() == 404 {
		return true
	}
	s := pystr.Lower(err.Error())
	if (strings.Contains(s, "method") || strings.Contains(s, "endpoint")) && (strings.Contains(s, "not found") || strings.Contains(s, "does not exist")) {
		return true
	}
	return strings.Contains(s, "no such method")
}

// Whether a rich failure is permanent or a capability gap, so a legacy resend cannot duplicate
func isRichFallbackError(err error) bool {
	if isBadRequestError(err) || isRichCapabilityError(err) {
		return true
	}
	s := pystr.Lower(err.Error())
	return strings.Contains(s, "unsupported") || strings.Contains(s, "not implemented")
}

// Whether err is a TimedOut or carries a timed out message
func isTimedOut(err error) bool {
	return isPTBError(err, "TimedOut") || strings.Contains(pystr.Lower(err.Error()), "timed out")
}

// Whether a send goes out silently, true in important mode unless the metadata asks to notify or carries an approval prompt
func notificationKwargs(mode string, metadata map[string]any) bool {
	if mode != "important" || pyTruthy(metadata["notify"]) || pyTruthy(metadata["is_approval_prompt"]) {
		return false
	}
	return true
}

// Whether chunk index should reply thread to the anchor under the reply_to_mode
func shouldThreadReply(replyTo *string, chunkIndex int, replyToMode string) bool {
	if replyTo == nil || *replyTo == "" {
		return false
	}
	switch replyToMode {
	case "off":
		return false
	case "all":
		return true
	}
	return chunkIndex == 0
}

// Reply anchor routing of one chunk, whether it is a private DM topic send and whether its anchor was turned off
func chunkReplyRouting(replyTo *string, metadata map[string]any, threadID *string, index int, replyToMode string) (bool, bool, *int) {
	metadataReplyTo := metadataReplyToMessageID(metadata)
	privateDMTopicSend := isPrivateDMTopicSend(threadID, metadata)
	dmTopicReplyToOff := privateDMTopicSend && replyToMode == "off" && dmTopicFallback(metadata)
	var raw any
	if replyTo != nil {
		raw = *replyTo
	}
	var source *string
	if anchor := messageIDInt(ParseTelegramMessageID(raw)); anchor != nil {
		text := fmt.Sprint(*anchor)
		source = &text
	} else if privateDMTopicSend && metadataReplyTo != nil {
		text := fmt.Sprint(*metadataReplyTo)
		source = &text
	}
	shouldThread := false
	if privateDMTopicSend {
		shouldThread = source != nil && replyToMode != "off"
	} else {
		shouldThread = shouldThreadReply(source, index, replyToMode)
	}
	var replyToID *int
	if shouldThread && source != nil && *source != "" {
		id, _ := pyIntOf(*source)
		replyToID = &id
	}
	return privateDMTopicSend, dmTopicReplyToOff, replyToID
}

// Routing of a single rich send mirroring the first chunk of send, false when legacy must own the DM topic failure
func computeSingleSendRouting(replyTo *string, metadata map[string]any, threadID *string, replyToMode string) (*int, threadKwargs, bool, error) {
	privateDMTopicSend, dmTopicReplyToOff, replyToID := chunkReplyRouting(replyTo, metadata, threadID, 0, replyToMode)
	thread, err := threadKwargsForSend(threadID, metadata, replyToID, replyToMode)
	if err != nil {
		return nil, threadKwargs{}, false, err
	}
	if privateDMTopicSend && replyToID == nil && !dmTopicReplyToOff && (thread.DirectMessagesTopicID == nil || *thread.DirectMessagesTopicID == 0) {
		return nil, threadKwargs{}, false, nil
	}
	return replyToID, thread, true, nil
}
