package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const relayDiscordLabelsMax = 2048

// Connector prompt codec id alphabet, Python's dollar also matching before one trailing newline
var relayPromptIDRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,32}\n?$`)

func relayDiscordLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.relay.adapter_discord_interaction")
}

// Chat name and topic the Discord text lane last carried for a chat
type relayChatLabels struct {
	name, topic *string
}

func (l relayChatLabels) equal(o relayChatLabels) bool {
	same := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return same(l.name, o.name) && same(l.topic, o.topic)
}

// Ordered map key of a scope and chat pair
func relayLabelKey(scope, chatID string) string { return scope + "\x00" + chatID }

// Copy of an optional label text
func relayLabelCopy(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

// Recorded label value as optional text, nil staying nil
func relayLabelText(v any) *string {
	if v == nil {
		return nil
	}
	s := pyrepr.Str(v)
	return &s
}

// Session store surface that records and reads a chat's labels
type relayChatLabelStore interface {
	RecordChatLabels(ctx context.Context, source *SessionSource) (bool, error)
	ChatLabels(ctx context.Context, platform Platform, scopeID *string, chatID string) (any, any, bool, error)
}

// Installed label store, nil when none or a nil session store is installed
func (a *RelayAdapter) labelStore() relayChatLabelStore {
	raw := a.SessionStore()
	if s, ok := raw.(*SessionStore); ok && s == nil {
		return nil
	}
	store, _ := raw.(relayChatLabelStore)
	return store
}

// Captures the scope, then clears the profile stamp before admission on a single profile runner
func (a *RelayAdapter) captureIngressScope(event *MessageEvent) {
	a.captureScope(event)
	holder, ok := handlerOwner(a).(gatewayConfigRunner)
	if !ok || holder == nil || event.Source == nil {
		return
	}
	if config := holder.GatewayConfig(); config != nil && !(config.MultiplexProfiles != nil && *config.MultiplexProfiles) {
		event.Source.Profile = nil
	}
}

// Keeps the text lane's Discord chat labels for the interaction lane and records changed ones in the store
func (a *RelayAdapter) rememberDiscordLabels(ctx context.Context, source *SessionSource) error {
	if source == nil || source.Platform != PlatformDiscord || source.ChatID == "" {
		return nil
	}
	scope := ""
	if source.ScopeID != nil {
		scope = *source.ScopeID
	}
	key := relayLabelKey(scope, source.ChatID)
	labels := relayChatLabels{relayLabelCopy(source.ChatName), relayLabelCopy(source.ChatTopic)}
	a.mu.Lock()
	a.discordChatLabels.set(key, labels)
	a.discordChatLabels.evict(relayDiscordLabelsMax)
	recorded, has := a.discordLabelsRecorded.get(key)
	a.mu.Unlock()
	store := a.labelStore()
	if store == nil || (has && recorded.equal(labels)) {
		return nil
	}
	written, err := store.RecordChatLabels(ctx, source)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		relayDiscordLog().Debug("relay: Discord chat labels not recorded", "exc_info", err)
		written = false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !written {
		a.discordLabelsRecorded.pop(key)
		return nil
	}
	a.discordLabelsRecorded.set(key, labels)
	a.discordLabelsRecorded.evict(relayDiscordLabelsMax)
	return nil
}

// Text lane's last labels for a chat, asking the store after a restart or eviction
func (a *RelayAdapter) discordChatLabelsFor(ctx context.Context, scope, chatID string) (relayChatLabels, error) {
	key := relayLabelKey(scope, chatID)
	a.mu.Lock()
	cached, ok := a.discordChatLabels.get(key)
	a.mu.Unlock()
	if ok {
		return cached, nil
	}
	var recorded *relayChatLabels
	if store := a.labelStore(); store != nil {
		scopeID := scope
		name, topic, found, err := store.ChatLabels(ctx, PlatformDiscord, &scopeID, chatID)
		if err != nil {
			if ctx.Err() != nil {
				return relayChatLabels{}, ctx.Err()
			}
			relayDiscordLog().Debug("relay: recorded Discord chat labels unreadable", "exc_info", err)
			return relayChatLabels{}, nil
		}
		if found {
			recorded = &relayChatLabels{relayLabelText(name), relayLabelText(topic)}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	value := relayChatLabels{}
	if recorded != nil {
		value = *recorded
		a.discordLabelsRecorded.set(key, value)
		a.discordLabelsRecorded.evict(relayDiscordLabelsMax)
	}
	a.discordChatLabels.set(key, value)
	a.discordChatLabels.evict(relayDiscordLabelsMax)
	value, _ = a.discordChatLabels.get(key)
	return value, nil
}

// Handles a connector forwarded passthrough, a Discord interaction taking the chat path and others logged and dropped
func (a *RelayAdapter) onPassthrough(ctx context.Context, forward *PassthroughForward, bufferID any) error {
	err := a.passthroughDiscord(ctx, forward)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		relayDiscordLog().Warn("relay passthrough_forward handling failed", "exc_info", err)
	}
	return nil
}

// Dispatches a forwarded Discord interaction as a message event, logging any other forward as dropped
func (a *RelayAdapter) passthroughDiscord(ctx context.Context, forward *PassthroughForward) error {
	if forward.Platform == "discord" {
		event, err := a.discordInteractionToEvent(forward)
		if err != nil {
			return err
		}
		if event != nil {
			a.captureIngressScope(event)
			consumed, err := a.consumePromptResponse(ctx, event)
			if err != nil || consumed {
				return err
			}
			src := event.Source
			scope := ""
			if src.ScopeID != nil {
				scope = *src.ScopeID
			}
			labels, err := a.discordChatLabelsFor(ctx, scope, src.ChatID)
			if err != nil {
				return err
			}
			src.ChatName, src.ChatTopic = labels.name, labels.topic
			return a.HandleMessage(RelayPassthroughEvent(ctx, a.transport, forward, event), event)
		}
	}
	relayDiscordLog().Info(fmt.Sprintf("relay passthrough_forward dropped (no handler): platform=%s method=%s path=%s",
		forward.Platform, forward.Method, forward.Path))
	return nil
}

// Mapping get of a decoded JSON value, the AttributeError of Python for a non mapping
func relayDictGet(v any, key string) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))}
	}
	return m[key], nil
}

// Mapping get returning nil for a non mapping
func relayMapGet(v any, key string) any {
	if m, ok := v.(map[string]any); ok {
		return m[key]
	}
	return nil
}

// Python membership of a value among the integer interaction or channel type codes
func relayJSONIn(v any, codes ...int) bool {
	for _, code := range codes {
		if pyrepr.Equal(v, code) {
			return true
		}
	}
	return false
}

// Optional text of a truthy value, nil for a falsy one
func relayTruthyStr(v any) *string {
	if !pyrepr.Truthy(v) {
		return nil
	}
	s := pyrepr.Str(v)
	return &s
}

// Message event of a forwarded Discord interaction body, nil for an unusable body
func (a *RelayAdapter) discordInteractionToEvent(forward *PassthroughForward) (*MessageEvent, error) {
	if !utf8.Valid(forward.Body) {
		return nil, nil
	}
	parsed, err := pyjson.Loads(string(forward.Body))
	if err != nil {
		return nil, nil
	}
	payload, ok := pyjson.Plain(parsed).(map[string]any)
	if !ok {
		return nil, nil
	}
	itype := payload["type"]
	var data any = map[string]any{}
	if pyrepr.Truthy(payload["data"]) {
		data = payload["data"]
	}
	messageType := MessageTypeText
	text := ""
	switch {
	case relayJSONIn(itype, 2):
		name, err := relayDictGet(data, "name")
		if err != nil {
			return nil, err
		}
		text = strings.TrimRight("/"+pyrepr.Str(pyrepr.Or(name, "")), "/")
		if text != "" {
			options, _ := relayDictGet(data, "options")
			parts := append([]string{text}, relayRenderInteractionOptions(options)...)
			text = pystr.Strip(strings.Join(parts, " "))
			messageType = MessageTypeCommand
		}
	case relayJSONIn(itype, 3):
		customID, err := relayDictGet(data, "custom_id")
		if err != nil {
			return nil, err
		}
		text = pyrepr.Str(pyrepr.Or(customID, ""))
	case relayJSONIn(itype, 5):
		text, err = relayRenderModalSubmit(data)
		if err != nil {
			return nil, err
		}
	}
	var member any = map[string]any{}
	if pyrepr.Truthy(payload["member"]) {
		member = payload["member"]
	}
	user := relayMapGet(member, "user")
	if !pyrepr.Truthy(user) {
		user = payload["user"]
	}
	if _, isMap := user.(map[string]any); !isMap {
		user = map[string]any{}
	}
	guildID := payload["guild_id"]
	var userName *string
	for _, v := range []any{relayMapGet(member, "nick"), relayMapGet(user, "global_name"), relayMapGet(user, "username")} {
		if pyrepr.Truthy(v) {
			userName = relayTruthyStr(v)
			break
		}
	}
	chatID := pyrepr.Str(pyrepr.Or(payload["channel_id"], ""))
	channel, isMap := payload["channel"].(map[string]any)
	if !isMap {
		channel = map[string]any{}
	}
	isThread := pyrepr.Truthy(guildID) && relayJSONIn(channel["type"], 10, 11, 12)
	chatType := "dm"
	if isThread {
		chatType = "thread"
	} else if pyrepr.Truthy(guildID) {
		chatType = "group"
	}
	source := &SessionSource{Platform: PlatformDiscord, ChatID: chatID, ChatType: chatType,
		UserID: relayTruthyStr(relayMapGet(user, "id")), UserName: userName, ScopeID: relayTruthyStr(guildID),
		MessageID: relayTruthyStr(payload["id"]), DeliveredViaUpstreamRelay: true, Profile: relayLabelCopy(forward.Profile)}
	if isThread {
		threadID := chatID
		source.ThreadID = &threadID
		source.ParentChatID = relayTruthyStr(channel["parent_id"])
	}
	event := NewMessageEvent(text)
	event.MessageType = messageType
	event.Source = source
	if relayJSONIn(itype, 3) {
		if promptID, optionID, ok := relayDecodePromptToken(text); ok {
			var promptMessageID any
			if msg, isMsg := payload["message"].(map[string]any); isMsg && pyrepr.Truthy(msg["id"]) {
				promptMessageID = pyrepr.Str(msg["id"])
			}
			event.PromptResponse = map[string]any{"prompt_id": promptID, "option_id": optionID, "prompt_message_id": promptMessageID}
			event.Text = "/" + optionID
			event.MessageType = MessageTypeCommand
		}
	}
	return event, nil
}

// Prompt and option ids of an hp1 callback token, false for any other token
func relayDecodePromptToken(token string) (string, string, bool) {
	parts := strings.Split(token, ":")
	if len(parts) != 3 || parts[0] != "hp1" {
		return "", "", false
	}
	if !relayPromptIDRe.MatchString(parts[1]) || !relayPromptIDRe.MatchString(parts[2]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// Modal custom id then one input id and value line per submitted non empty input
func relayRenderModalSubmit(data any) (string, error) {
	customID, err := relayDictGet(data, "custom_id")
	if err != nil {
		return "", err
	}
	lines := []string{pyrepr.Str(pyrepr.Or(customID, ""))}
	components, _ := relayDictGet(data, "components")
	initial, err := relayListOf(pyrepr.Or(components, []any{}))
	if err != nil {
		return "", err
	}
	stack := relayReversed(initial)
	for len(stack) > 0 {
		comp := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		m, ok := comp.(map[string]any)
		if !ok {
			continue
		}
		nested := m["components"]
		if !pyrepr.Truthy(nested) {
			nested = []any{}
			if pyrepr.Truthy(m["component"]) {
				nested = []any{m["component"]}
			}
		}
		if list, isList := nested.([]any); isList {
			stack = append(stack, relayReversed(list)...)
		}
		value, has := m["value"]
		if !has {
			value = m["values"]
		}
		if list, isList := value.([]any); isList {
			texts := make([]string, len(list))
			for i, v := range list {
				texts[i] = pyrepr.Str(v)
			}
			value = strings.Join(texts, ", ")
		}
		if value != nil && pystr.Strip(pyrepr.Str(value)) != "" {
			lines = append(lines, fmt.Sprintf("%s: %s", pyrepr.Str(pyrepr.Or(m["custom_id"], "")), pyrepr.Str(value)))
		}
	}
	return pystr.Strip(strings.Join(lines, "\n")), nil
}

// List of a decoded JSON iterable, the TypeError of Python for a non iterable
func relayListOf(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		out := make([]any, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not reversible", pyrepr.TypeName(v))}
}

func relayReversed(list []any) []any {
	out := make([]any, len(list))
	for i, v := range list {
		out[len(list)-1-i] = v
	}
	return out
}

// Text parts of interaction options, subcommands contributing their name then their nested options
func relayRenderInteractionOptions(options any) []string {
	list, ok := options.([]any)
	if !ok {
		return []string{}
	}
	parts := []string{}
	for _, opt := range list {
		m, ok := opt.(map[string]any)
		if !ok {
			continue
		}
		if relayJSONIn(m["type"], 1, 2) {
			if sub := pystr.Strip(pyrepr.Str(pyrepr.Or(m["name"], ""))); sub != "" {
				parts = append(parts, sub)
			}
			parts = append(parts, relayRenderInteractionOptions(m["options"])...)
			continue
		}
		if value := m["value"]; value != nil {
			if s := pystr.Strip(pyrepr.Str(value)); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return parts
}
