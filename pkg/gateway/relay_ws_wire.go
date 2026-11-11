package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

func wsLog() *slog.Logger { return slog.Default().With("logger", "gateway.relay.ws_transport") }

// Name and alias to command table of the parent slash, installed by the process wiring from pkg/commands
var RelaySlackSubcommandMap func(ctx context.Context) map[string]string

// Drain grace clamped to the remaining disconnect budget after the three teardown awaits and a margin
func relayDisconnectDrainGrace(budgetS *float64) float64 {
	budget := relayEnvDisconnectBudget()
	if budgetS != nil {
		budget = 0.0
		if *budgetS > 0 {
			budget = *budgetS
		}
	}
	reserved := 3*relayTeardownAwaitTimeoutS + 0.5
	return max(0.0, min(relayDisconnectDrainGraceS, budget-reserved))
}

// Adapter disconnect budget of the runner env override, five seconds by default
func relayEnvDisconnectBudget() float64 {
	budget := 5.0
	if raw := pystr.Strip(os.Getenv("ICEBATH_GATEWAY_ADAPTER_DISCONNECT_TIMEOUT")); raw != "" {
		if f, err := pyint.ParseFloat(raw); err == nil {
			budget = 0.0
			if f > 0 {
				budget = f
			}
		}
	}
	return budget
}

// Connector base URL normalized to the ws or wss dial target ending in /relay
func relayWSDialURL(url string) string {
	raw := pystr.Strip(url)
	if strings.HasPrefix(raw, "https://") {
		raw = "wss://" + raw[len("https://"):]
	} else if strings.HasPrefix(raw, "http://") {
		raw = "ws://" + raw[len("http://"):]
	}
	raw = pystr.RstripChars(raw, "/")
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
		obj, ok := pyjson.AsObject(item)
		if !ok {
			continue
		}
		text := obj.GetOr("text", nil)
		if !pyrepr.Truthy(text) {
			continue
		}
		author := any("")
		if src, ok := pyjson.AsObject(obj.GetOr("source", nil)); ok {
			author = pyrepr.Or(pyrepr.Or(src.GetOr("user_name", nil), src.GetOr("user_id", nil)), "")
		}
		if pyrepr.Truthy(author) {
			lines = append(lines, pyrepr.Str(author)+": "+pyrepr.Str(text))
		} else {
			lines = append(lines, pyrepr.Str(text))
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
	parts := pystr.SplitWhitespace(pystr.Strip(text), 1)
	if len(parts) == 0 || parts[0] != "/icebath" {
		return text, messageType
	}
	payload := ""
	if len(parts) > 1 {
		payload = pystr.Strip(parts[1])
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
	if words := pystr.SplitWhitespace(payload, -1); len(words) > 0 {
		firstWord = words[0]
	}
	normalized := payload
	if mapped, ok := subcommands[firstWord]; ok {
		rest := pystr.Strip(payload[len(firstWord):])
		normalized = pystr.Strip(mapped + " " + rest)
	} else if normalized == "" {
		normalized = "/help"
	}
	if strings.HasPrefix(normalized, "/") {
		return normalized, MessageTypeCommand
	}
	return normalized, MessageTypeText
}

// Per media_urls entry value of media[].key matched by URL, padded with empty strings
func relayPerURLFromWire(raw *pyjson.Object, key string) []string {
	urls, ok := raw.GetOr("media_urls", nil).([]any)
	if !ok || len(urls) == 0 {
		return []string{}
	}
	byURL := map[string]string{}
	media, _ := raw.GetOr("media", nil).([]any)
	for _, m := range media {
		obj, ok := pyjson.AsObject(m)
		if !ok {
			continue
		}
		u, ok := obj.GetOr("url", nil).(string)
		if !ok || u == "" {
			continue
		}
		v, _ := obj.GetOr(key, nil).(string)
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
func relayMediaTypesFromWire(raw *pyjson.Object) []string {
	types := relayPerURLFromWire(raw, "mime")
	missing := 0
	for _, t := range types {
		if t == "" {
			missing++
		}
	}
	if media, ok := raw.GetOr("media", nil).([]any); missing > 0 && ok && len(media) > 0 {
		wsLog().Debug(fmt.Sprintf("relay inbound: %d/%d media_urls had no matching media[] mime", missing, len(types)))
	}
	return types
}

// Per attachment display cleaned original filenames parallel to media_urls
func relayMediaFilenamesFromWire(raw *pyjson.Object) []string {
	names := relayPerURLFromWire(raw, "filename")
	for i, n := range names {
		names[i] = CleanAttachmentFilename(n)
	}
	return names
}

// Wire value as a Python str would print it, empty for null
func relayWireStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Optional wire string, nil for an absent or null value
func relayWireOptStr(v any) *string {
	if v == nil {
		return nil
	}
	s := relayWireStr(v)
	return &s
}

// Getter of a wire mapping, an AttributeError for any other non empty value
func relayWireMapping(v any) (*pyjson.Object, error) {
	if !pyrepr.Truthy(v) {
		return pyjson.NewObject(), nil
	}
	if obj, ok := pyjson.AsObject(v); ok {
		return obj, nil
	}
	return nil, &PyError{"AttributeError", "'" + pyrepr.TypeName(v) + "' object has no attribute 'get'"}
}

// Message event rebuilt from the connector inbound payload, unknown platforms as relay and unknown types as text
func relayEventFromWire(ctx context.Context, raw *pyjson.Object) (*MessageEvent, error) {
	src, err := relayWireMapping(raw.GetOr("source", map[string]any{}))
	if err != nil {
		return nil, err
	}
	platform, perr := ParsePlatform(ctx, src.GetOr("platform", "relay"))
	if perr != nil {
		platform = PlatformRelay
	}
	userName := pyrepr.Or(pyrepr.Or(src.GetOr("user_display_name", nil), src.GetOr("user_name", nil)), src.GetOr("user_handle", nil))
	source := &SessionSource{
		Platform:                  platform,
		ChatID:                    relayWireStr(src.GetOr("chat_id", "")),
		ChatType:                  relayWireStr(src.GetOr("chat_type", "dm")),
		ChatName:                  relayWireOptStr(src.GetOr("chat_name", nil)),
		UserID:                    relayWireOptStr(src.GetOr("user_id", nil)),
		UserName:                  relayWireOptStr(userName),
		ThreadID:                  relayWireOptStr(src.GetOr("thread_id", nil)),
		ChatTopic:                 relayWireOptStr(src.GetOr("chat_topic", nil)),
		UserIDAlt:                 relayWireOptStr(src.GetOr("user_id_alt", nil)),
		ChatIDAlt:                 relayWireOptStr(src.GetOr("chat_id_alt", nil)),
		ScopeID:                   relayWireOptStr(src.GetOr("scope_id", nil)),
		ParentChatID:              relayWireOptStr(src.GetOr("parent_chat_id", nil)),
		MessageID:                 relayWireOptStr(src.GetOr("message_id", nil)),
		Profile:                   relayWireOptStr(src.GetOr("profile", nil)),
		AutoThreadCreated:         pyrepr.Truthy(src.GetOr("auto_thread_created", false)),
		AutoThreadInitialName:     relayWireOptStr(src.GetOr("auto_thread_initial_name", nil)),
		ProspectiveThreadID:       relayWireOptStr(src.GetOr("prospective_thread_id", nil)),
		DeliveredViaUpstreamRelay: true,
	}
	msgType := MessageTypeText
	if s, ok := raw.GetOr("message_type", "text").(string); ok && relayValidMessageType(s) {
		msgType = MessageType(s)
	}
	text := relayWireStr(raw.GetOr("text", ""))
	if platform == PlatformSlack {
		text, msgType = relayNormalizeSlackParentCommand(ctx, text, msgType)
	}
	replyTo, err := relayWireMapping(raw.GetOr("reply_to", nil))
	if err != nil {
		return nil, err
	}
	event := NewMessageEvent(text)
	event.MessageType = msgType
	event.Source = source
	event.MessageID = relayWireOptStr(raw.GetOr("message_id", nil))
	event.ReplyToMessageID = relayWireOptStr(raw.GetOr("reply_to_message_id", nil))
	event.ReplyToText = relayWireOptStr(replyTo.GetOr("text", nil))
	event.ReplyToAuthorName = relayWireOptStr(replyTo.GetOr("author", nil))
	event.ReplyToIsOwnMessage = pyrepr.Truthy(replyTo.GetOr("is_own", false))
	event.MediaURLs = []string{}
	if urls, ok := raw.GetOr("media_urls", nil).([]any); ok {
		for _, u := range urls {
			event.MediaURLs = append(event.MediaURLs, relayWireStr(u))
		}
	}
	event.MediaTypes = relayMediaTypesFromWire(raw)
	event.MediaFilenames = relayMediaFilenamesFromWire(raw)
	event.ChannelContext = relayRenderContext(raw.GetOr("context", nil))
	if pr, ok := pyjson.AsObject(raw.GetOr("prompt_response", nil)); ok {
		event.PromptResponse = map[string]any{}
		for _, k := range pr.Keys {
			event.PromptResponse[k] = pr.Vals[k]
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
func relayPassthroughFromWire(raw *pyjson.Object) PassthroughForward {
	body, err := pybase64.B64Decode(relayWireStr(pyrepr.Or(raw.GetOr("bodyB64", ""), "")))
	if err != nil {
		body = []byte{}
	}
	var headers [][2]string
	if pairs, ok := pyrepr.Or(raw.GetOr("headers", []any{}), []any{}).([]any); ok {
		for _, pair := range pairs {
			if p, ok := pair.([]any); ok && len(p) == 2 {
				headers = append(headers, [2]string{pyrepr.Str(p[0]), pyrepr.Str(p[1])})
			}
		}
	}
	return PassthroughForward{
		Platform: pyrepr.Str(raw.GetOr("platform", "")), BotID: pyrepr.Str(raw.GetOr("botId", "")),
		Method: pyrepr.Str(raw.GetOr("method", "")), Path: pyrepr.Str(raw.GetOr("path", "")), Headers: headers,
		Body: body, Profile: relayWireOptStr(raw.GetOr("profile", nil)),
	}
}
