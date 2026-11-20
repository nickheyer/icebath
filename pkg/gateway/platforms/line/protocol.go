package line

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/google/uuid"
	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"
	"github.com/line/line-bot-sdk-go/v8/linebot/webhook"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	linePerBubbleChars        = 5000
	lineSafeBubbleChars       = 4500
	lineMaxMessagesPerCall    = 5
	lineReplyTokenTTLSeconds  = 50
	webhookBodyMaxBytes       = 1_048_576
	defaultWebhookPort        = 8646
	defaultWebhookPath        = "/line/webhook"
	defaultMediaPathPrefix    = "/line/media"
	defaultSlowResponseThresh = 45.0
	mediaTokenTTLSeconds      = 1800
	lineImageMaxBytes         = 10 * 1024 * 1024
	lineAVMaxBytes            = 200 * 1024 * 1024

	defaultPendingReplyTextKey = "platform.line.pending.still_thinking"
	defaultButtonLabelKey      = "platform.line.pending.button_label"
	defaultDeliveredTextKey    = "platform.line.pending.delivered"
	defaultInterruptedTextKey  = "platform.line.pending.interrupted"
	defaultExpiredTextKey      = "platform.line.pending.expired"
)

// Bind hosts LINE cannot fetch media from, a public URL being required for them
var wildcardHosts = map[string]bool{"0.0.0.0": true, "::": true, "": true}

// LINE message type to normalized message type, recorded audio clips taking the voice path
var lineMessageTypes = map[string]gateway.MessageType{
	"text": gateway.MessageTypeText, "image": gateway.MessageTypePhoto, "video": gateway.MessageTypeVideo,
	"audio": gateway.MessageTypeVoice, "file": gateway.MessageTypeDocument, "location": gateway.MessageTypeLocation,
	"sticker": gateway.MessageTypeSticker,
}

// Transparent 1x1 PNG served as the video preview when none is supplied
var fallbackPNGPreview = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x00, 0x05, 0x00, 0x01, 0x00, 0x37, 0x7a, 0x7f, 0xf2, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
	0x44, 0xae, 0x42, 0x60, 0x82,
}

// Markdown LINE cannot render, applied in order with code blocks first so their content survives
var mdStripRules = []struct {
	re   *regexp2.Regexp
	repl func(m regexp2.Match) string
}{
	{pyre.MustCompile("(?s)```[a-zA-Z0-9_+-]*\\n?(.*?)```"), func(m regexp2.Match) string {
		return strings.TrimRight(m.GroupByNumber(1).String(), "\n")
	}},
	{pyre.MustCompile("`([^`]+)`"), group1},
	{pyre.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+)\)`), func(m regexp2.Match) string {
		return m.GroupByNumber(1).String() + " (" + m.GroupByNumber(2).String() + ")"
	}},
	{pyre.MustCompile(`\*\*(.+?)\*\*`), group1},
	{pyre.MustCompile(`(?<!\*)\*(?!\s)(.+?)(?<!\s)\*(?!\*)`), group1},
	{pyre.MustCompile(`(?m)^#{1,6}\s+`), func(regexp2.Match) string { return "" }},
	{pyre.MustCompile(`(?m)^[\s]*[-*+]\s+`), func(regexp2.Match) string { return "• " }},
}

// First capture group of a match
func group1(m regexp2.Match) string { return m.GroupByNumber(1).String() }

// Markdown stripped for LINE with label (url) links kept tappable and code block content kept
func stripMarkdownPreservingURLs(text string) string {
	if text == "" {
		return text
	}
	for _, rule := range mdStripRules {
		out, err := rule.re.ReplaceFunc(text, rule.repl, -1, -1)
		if err != nil {
			panic(err)
		}
		text = out
	}
	return text
}

// Rune index of the last sep lying wholly before end, -1 when absent
func rfindRunes(runes []rune, sep string, end int) int {
	if end > len(runes) {
		end = len(runes)
	}
	s := string(runes[:end])
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(s[:i])
}

// Text split into at most five bubbles at paragraph, line or word breaks with overflow ellipsised
func splitForLine(text string, maxChars int) []string {
	runes := []rune(text)
	if text == "" || len(runes) <= maxChars {
		if text == "" {
			return []string{}
		}
		return []string{text}
	}
	chunks := []string{}
	remaining := runes
	for len(remaining) > 0 && len(chunks) < lineMaxMessagesPerCall && len(remaining) > maxChars {
		cuts := []int{rfindRunes(remaining, "\n\n", maxChars), rfindRunes(remaining, "\n", maxChars), rfindRunes(remaining, " ", maxChars)}
		cut := cuts[len(cuts)-1]
		for _, c := range cuts {
			if c >= int(float64(maxChars)*0.5) {
				cut = c
				break
			}
		}
		if cut <= 0 {
			cut = maxChars
		}
		chunks = append(chunks, pystr.Rstrip(string(remaining[:cut])))
		remaining = []rune(pystr.Lstrip(string(remaining[cut:])))
	}
	if len(remaining) > 0 && len(chunks) < lineMaxMessagesPerCall {
		chunks = append(chunks, string(remaining))
	} else if len(remaining) > 0 {
		last := []rune(chunks[len(chunks)-1])
		if len(last) > maxChars-1 {
			last = last[:maxChars-1]
		}
		chunks[len(chunks)-1] = pystr.Rstrip(string(last)) + "…"
	}
	return chunks
}

// X-Line-Signature check, the header being the canonical base64 of HMAC-SHA256 over the raw body
func verifyLineSignature(body []byte, signature, channelSecret string) bool {
	if signature == "" || channelSecret == "" || body == nil {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != signature {
		return false
	}
	return webhook.ValidateSignature(channelSecret, signature, body)
}

// Slow LLM postback cache state
type State string

const (
	StatePending   State = "pending"
	StateReady     State = "ready"
	StateDelivered State = "delivered"
	StateError     State = "error"
)

// One postback cache entry with its state and cached payload
type cacheEntry struct {
	state   State
	payload any
}

// In memory slow LLM postback cache moving pending to ready or error and then delivered
type RequestCache struct {
	entries map[string]*cacheEntry
}

// Empty cache
func NewRequestCache() *RequestCache { return &RequestCache{entries: map[string]*cacheEntry{}} }

// Fresh pending request id for a chat
func (c *RequestCache) RegisterPending(chatID string) string {
	rid := uuid.NewString()
	c.entries[rid] = &cacheEntry{state: StatePending}
	return rid
}

// Entry of a request id, nil when unknown
func (c *RequestCache) Get(requestID string) *cacheEntry { return c.entries[requestID] }

// Moves an entry in an allowed state to the new state, delivered keeping its payload
func (c *RequestCache) transition(requestID string, allowed []State, state State, payload any) {
	entry := c.entries[requestID]
	if entry == nil {
		return
	}
	for _, s := range allowed {
		if entry.state == s {
			entry.state = state
			if state != StateDelivered {
				entry.payload = payload
			}
			return
		}
	}
}

// Caches the answer of a pending request
func (c *RequestCache) SetReady(requestID string, payload any) {
	c.transition(requestID, []State{StatePending}, StateReady, payload)
}

// Caches the error text of a pending request
func (c *RequestCache) SetError(requestID, message string) {
	c.transition(requestID, []State{StatePending}, StateError, message)
}

// Settles a ready or errored request as delivered
func (c *RequestCache) MarkDelivered(requestID string) {
	c.transition(requestID, []State{StateReady, StateError}, StateDelivered, nil)
}

// LINE source type to its id key and normalized chat type
var sourceKinds = map[string][2]string{"group": {"groupId", "group"}, "room": {"roomId", "room"}, "user": {"userId", "dm"}}

// String field of a decoded JSON object, empty when absent or not a string
func strField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// Object field of a decoded JSON object, empty when absent or not an object
func objField(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	if o == nil {
		return map[string]any{}
	}
	return o
}

// Chat id and chat type of a LINE event source block
func resolveChat(source map[string]any) (string, string) {
	kind, ok := sourceKinds[strField(source, "type")]
	if !ok {
		return "", "dm"
	}
	return strField(source, kind[0]), kind[1]
}

// Three list gate over users, groups and rooms
func allowedForSource(source map[string]any, allowAll bool, userIDs, groupIDs, roomIDs map[string]bool) bool {
	if allowAll {
		return true
	}
	sid, chatType := resolveChat(source)
	lists := map[string]map[string]bool{"dm": userIDs, "group": groupIDs, "room": roomIDs}
	return sid != "" && lists[chatType][sid]
}

// LINE text message capped to the per bubble maximum
func textMessage(text string) messaging_api.MessageInterface {
	if pystr.Len(text) > linePerBubbleChars {
		text = pystr.Slice(text, 0, linePerBubbleChars-1) + "…"
	}
	return &messaging_api.TextMessage{Text: text}
}

// Content markdown stripped, chunked and capped into at most five LINE text messages
func textMessages(content string) []messaging_api.MessageInterface {
	chunks := splitForLine(stripMarkdownPreservingURLs(content), lineSafeBubbleChars)
	out := []messaging_api.MessageInterface{}
	for _, c := range chunks {
		out = append(out, textMessage(c))
	}
	if len(out) > lineMaxMessagesPerCall {
		out = out[:lineMaxMessagesPerCall]
	}
	return out
}

// Postback payload of the show response action, the json.dumps form with its default separators
func postbackData(requestID string) string {
	raw, _ := json.Marshal(requestID)
	return `{"action": "show_response", "request_id": ` + string(raw) + `}`
}

// Template Buttons bubble whose tap fetches the slow answer, text capped at 160 and alt text at 400
func buildPostbackButtonMessage(text, buttonLabel, requestID, defaultLabel string) messaging_api.MessageInterface {
	truncated := text
	if pystr.Len(text) > 160 {
		truncated = pystr.Slice(text, 0, 157) + "..."
	}
	alt := text
	if pystr.Len(text) > 400 {
		alt = pystr.Slice(text, 0, 397) + "..."
	}
	label := pystr.Slice(buttonLabel, 0, 20)
	if label == "" {
		label = pystr.Slice(defaultLabel, 0, 20)
	}
	display := pystr.Slice(buttonLabel, 0, 300)
	if display == "" {
		display = pystr.Slice(defaultLabel, 0, 300)
	}
	action := &messaging_api.PostbackAction{Label: label, Data: postbackData(requestID), DisplayText: display}
	return &messaging_api.TemplateMessage{AltText: alt,
		Template: &messaging_api.ButtonsTemplate{Text: truncated, Actions: []messaging_api.ActionInterface{action}}}
}

// Busy ack and heartbeat markers that bypass a pending postback cache so they land as visible bubbles
var systemBypassPrefixes = []string{"⚡", "⏳", "⏩", "💾"}

// Whether content opens with a busy ack marker
func isSystemBypass(content string) bool {
	if content == "" {
		return false
	}
	for _, p := range systemBypassPrefixes {
		if strings.HasPrefix(content, p) {
			return true
		}
	}
	return false
}

// Whether a send is a mid turn progress or status send that must not become the cached answer
func isInterimSend(content string, metadata map[string]any) bool {
	if v, ok := metadata["_interim_send"]; ok && pyrepr.Truthy(v) {
		return true
	}
	return isSystemBypass(content)
}

// Comma separated value as a set of its stripped non empty members
func csvSet(value string) map[string]bool {
	out := map[string]bool{}
	for _, x := range strings.Split(value, ",") {
		if s := pystr.Strip(x); s != "" {
			out[s] = true
		}
	}
	return out
}

// URL safe random token of 32 bytes, the secrets.token_urlsafe form
func tokenURLSafe() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
