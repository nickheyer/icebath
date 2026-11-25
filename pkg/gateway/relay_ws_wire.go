package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	relayHandshakeTimeoutS      = 30.0
	relayOutboundTimeoutS       = 30.0
	relayTeardownAwaitTimeoutS  = 1.0
	relayDisconnectDrainGraceS  = 5.0
	relayUnauthorizedCloseCode  = 4401
	relayExpiredCloseReason     = "expired"
	relayRedialHoldMaxS         = 60.0
	relayAmbiguousReaderExitOps = "send_media"
)

// Name and alias to command table of the parent slash, installed by the process wiring from pkg/commands
var RelaySlackSubcommandMap func(ctx context.Context) map[string]string

// Drain grace clamped to the remaining disconnect budget after the three teardown awaits and a margin
func relayDisconnectDrainGrace(budgetS *float64) float64 {
	budget := relayDisconnectBudget()
	if budgetS != nil {
		budget = 0.0
		if *budgetS > 0 {
			budget = *budgetS
		}
	}
	reserved := 3*relayTeardownAwaitTimeoutS + 0.5
	return max(0.0, min(relayDisconnectDrainGraceS, budget-reserved))
}

// Adapter disconnect budget the runner gives each chat app, never negative
func relayDisconnectBudget() float64 {
	return max(0.0, settings.GatewayAdapterDisconnectTimeout.Get(context.Background()).Seconds())
}

// Connector base URL normalized to the ws or wss dial target ending in /relay
func relayWSDialURL(url string) string {
	raw := strings.TrimSpace(url)
	if strings.HasPrefix(raw, "https://") {
		raw = "wss://" + raw[len("https://"):]
	} else if strings.HasPrefix(raw, "http://") {
		raw = "ws://" + raw[len("http://"):]
	}
	raw = strings.TrimRight(raw, "/")
	if !strings.HasSuffix(raw, "/relay") {
		raw += "/relay"
	}
	return raw
}

// Read only channel history block of the connector context list, nil when nothing usable
func relayRenderContext(context any) *string {
	items, ok := context.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	var lines []string
	for _, item := range items {
		obj, ok := jsonutil.AsObject(item)
		if !ok {
			continue
		}
		text := obj.ValueOr("text", nil)
		if !anyutil.IsTrue(text) {
			continue
		}
		author := any("")
		if src, ok := jsonutil.AsObject(obj.ValueOr("source", nil)); ok {
			author = anyutil.Or(anyutil.Or(src.ValueOr("user_name", nil), src.ValueOr("user_id", nil)), "")
		}
		if anyutil.IsTrue(author) {
			lines = append(lines, anyutil.String(author)+": "+anyutil.String(text))
		} else {
			lines = append(lines, anyutil.String(text))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	out := "[Recent channel messages]\n" + strings.Join(lines, "\n")
	return &out
}

// Native Slack parent slash routing of relay text, unchanged unless it starts with /icebath
func relayNormalizeSlackParentCommand(ctx context.Context, text string, messageType MessageType) (string, MessageType) {
	parts := textutil.FieldsN(strings.TrimSpace(text), 1)
	if len(parts) == 0 || parts[0] != "/icebath" {
		return text, messageType
	}
	payload := ""
	if len(parts) > 1 {
		payload = strings.TrimSpace(parts[1])
	}
	if RelaySlackSubcommandMap == nil {
		panic("gateway.RelaySlackSubcommandMap unassigned: the process wiring installs commands.SlackSubcommandMap over commands.Default and the raw config")
	}
	subcommands := map[string]string{}
	for k, v := range RelaySlackSubcommandMap(ctx) {
		subcommands[k] = v
	}
	subcommands["compact"] = "/compress"
	firstWord := ""
	if words := strings.Fields(payload); len(words) > 0 {
		firstWord = words[0]
	}
	normalized := payload
	if mapped, ok := subcommands[firstWord]; ok {
		rest := strings.TrimSpace(payload[len(firstWord):])
		normalized = strings.TrimSpace(mapped + " " + rest)
	} else if normalized == "" {
		normalized = "/help"
	}
	if strings.HasPrefix(normalized, "/") {
		return normalized, MessageTypeCommand
	}
	return normalized, MessageTypeText
}

// Per media_urls entry value of media[].key matched by URL, padded with empty strings
func relayPerURLFromWire(raw *jsonutil.Object, key string) []string {
	urls, ok := raw.ValueOr("media_urls", nil).([]any)
	if !ok || len(urls) == 0 {
		return []string{}
	}
	byURL := map[string]string{}
	media, _ := raw.ValueOr("media", nil).([]any)
	for _, m := range media {
		obj, ok := jsonutil.AsObject(m)
		if !ok {
			continue
		}
		u, ok := obj.ValueOr("url", nil).(string)
		if !ok || u == "" {
			continue
		}
		v, _ := obj.ValueOr(key, nil).(string)
		byURL[u] = v
	}
	out := make([]string, len(urls))
	for i, u := range urls {
		if s, ok := u.(string); ok {
			out[i] = byURL[s]
		}
	}
	return out
}

// Per attachment MIME types parallel to media_urls
func relayMediaTypesFromWire(raw *jsonutil.Object) []string {
	types := relayPerURLFromWire(raw, "mime")
	missing := 0
	for _, t := range types {
		if t == "" {
			missing++
		}
	}
	if media, ok := raw.ValueOr("media", nil).([]any); missing > 0 && ok && len(media) > 0 {
		pkgLog().Debug(fmt.Sprintf("relay inbound: %d/%d media_urls had no matching media[] mime", missing, len(types)))
	}
	return types
}

// Per attachment display cleaned original filenames parallel to media_urls
func relayMediaFilenamesFromWire(raw *jsonutil.Object) []string {
	names := relayPerURLFromWire(raw, "filename")
	for i, n := range names {
		names[i] = CleanAttachmentFilename(n)
	}
	return names
}

// Optional wire string, nil for an absent or null value
func relayWireOptStr(v any) *string {
	if v == nil {
		return nil
	}
	s := anyutil.String(v)
	return &s
}

// Getter of a wire mapping, an AttributeError for any other non empty value
func relayWireMapping(v any) (*jsonutil.Object, error) {
	if !anyutil.IsTrue(v) {
		return jsonutil.NewObject(), nil
	}
	if obj, ok := jsonutil.AsObject(v); ok {
		return obj, nil
	}
	return nil, &errorutil.Error{"AttributeError", "a relay wire value is " + errchain.Kind(v) + ", expected a mapping"}
}

// Message event rebuilt from the connector inbound payload, unknown platforms as relay and unknown types as text
func relayEventFromWire(ctx context.Context, raw *jsonutil.Object) (*MessageEvent, error) {
	src, err := relayWireMapping(raw.ValueOr("source", map[string]any{}))
	if err != nil {
		return nil, err
	}
	platform, perr := ParsePlatform(ctx, src.ValueOr("platform", "relay"))
	if perr != nil {
		platform = PlatformRelay
	}
	userName := anyutil.Or(anyutil.Or(src.ValueOr("user_display_name", nil), src.ValueOr("user_name", nil)), src.ValueOr("user_handle", nil))
	source := &SessionSource{
		Platform:                  platform,
		ChatID:                    anyutil.String(src.Value("chat_id")),
		ChatType:                  anyutil.String(anyutil.Or(src.Value("chat_type"), "dm")),
		ChatName:                  relayWireOptStr(src.ValueOr("chat_name", nil)),
		UserID:                    relayWireOptStr(src.ValueOr("user_id", nil)),
		UserName:                  relayWireOptStr(userName),
		ThreadID:                  relayWireOptStr(src.ValueOr("thread_id", nil)),
		ChatTopic:                 relayWireOptStr(src.ValueOr("chat_topic", nil)),
		UserIDAlt:                 relayWireOptStr(src.ValueOr("user_id_alt", nil)),
		ChatIDAlt:                 relayWireOptStr(src.ValueOr("chat_id_alt", nil)),
		ScopeID:                   relayWireOptStr(src.ValueOr("scope_id", nil)),
		ParentChatID:              relayWireOptStr(src.ValueOr("parent_chat_id", nil)),
		MessageID:                 relayWireOptStr(src.ValueOr("message_id", nil)),
		Profile:                   relayWireOptStr(src.ValueOr("profile", nil)),
		AutoThreadCreated:         anyutil.IsTrue(src.ValueOr("auto_thread_created", false)),
		AutoThreadInitialName:     relayWireOptStr(src.ValueOr("auto_thread_initial_name", nil)),
		ProspectiveThreadID:       relayWireOptStr(src.ValueOr("prospective_thread_id", nil)),
		DeliveredViaUpstreamRelay: true,
	}
	msgType := MessageTypeText
	if s, ok := raw.ValueOr("message_type", "text").(string); ok && relayValidMessageType(s) {
		msgType = MessageType(s)
	}
	text := anyutil.String(raw.Value("text"))
	if platform == PlatformSlack {
		text, msgType = relayNormalizeSlackParentCommand(ctx, text, msgType)
	}
	replyTo, err := relayWireMapping(raw.ValueOr("reply_to", nil))
	if err != nil {
		return nil, err
	}
	event := NewMessageEvent(text)
	event.MessageType = msgType
	event.Source = source
	event.MessageID = relayWireOptStr(raw.ValueOr("message_id", nil))
	event.ReplyToMessageID = relayWireOptStr(raw.ValueOr("reply_to_message_id", nil))
	event.ReplyToText = relayWireOptStr(replyTo.ValueOr("text", nil))
	event.ReplyToAuthorName = relayWireOptStr(replyTo.ValueOr("author", nil))
	event.ReplyToIsOwnMessage = anyutil.IsTrue(replyTo.ValueOr("is_own", false))
	event.MediaURLs = []string{}
	if urls, ok := raw.ValueOr("media_urls", nil).([]any); ok {
		for _, u := range urls {
			event.MediaURLs = append(event.MediaURLs, anyutil.String(u))
		}
	}
	event.MediaTypes = relayMediaTypesFromWire(raw)
	event.MediaFilenames = relayMediaFilenamesFromWire(raw)
	event.ChannelContext = relayRenderContext(raw.ValueOr("context", nil))
	if pr, ok := jsonutil.AsObject(raw.ValueOr("prompt_response", nil)); ok {
		event.PromptResponse = map[string]any{}
		for _, k := range pr.Keys() {
			event.PromptResponse[k] = pr.Value(k)
		}
	}
	return event, nil
}

// Whether the text names a MessageType member
func relayValidMessageType(s string) bool {
	switch MessageType(s) {
	case MessageTypeText, MessageTypeLocation, MessageTypePhoto, MessageTypeVideo, MessageTypeAudio,
		MessageTypeVoice, MessageTypeDocument, MessageTypeSticker, MessageTypeCommand:
		return true
	}
	return false
}

// PassthroughForward is a connector forwarded passthrough request with its decoded body and ordered headers
type PassthroughForward struct {
	Platform string
	BotID    string
	Method   string
	Path     string
	Headers  [][2]string
	Body     []byte
	Profile  *string
}

// Passthrough request rebuilt from the wire frame, an undecodable body becoming empty
func relayPassthroughFromWire(raw *jsonutil.Object) PassthroughForward {
	body, err := base64.StdEncoding.DecodeString(anyutil.String(raw.Value("bodyB64")))
	if err != nil {
		body = []byte{}
	}
	var headers [][2]string
	if pairs, ok := anyutil.Or(raw.ValueOr("headers", []any{}), []any{}).([]any); ok {
		for _, pair := range pairs {
			if p, ok := pair.([]any); ok && len(p) == 2 {
				headers = append(headers, [2]string{anyutil.String(p[0]), anyutil.String(p[1])})
			}
		}
	}
	return PassthroughForward{
		Platform: anyutil.String(raw.ValueOr("platform", "")), BotID: anyutil.String(raw.ValueOr("botId", "")),
		Method: anyutil.String(raw.ValueOr("method", "")), Path: anyutil.String(raw.ValueOr("path", "")), Headers: headers,
		Body: body, Profile: relayWireOptStr(raw.ValueOr("profile", nil)),
	}
}
