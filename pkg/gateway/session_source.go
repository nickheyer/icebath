package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// SessionSource is where a message originated, routing responses and recording cron delivery origin
type SessionSource struct {
	Platform                  Platform
	ChatID                    string
	ChatName                  *string
	ChatType                  string
	UserID                    *string
	UserName                  *string
	ThreadID                  *string
	ChatTopic                 *string
	UserIDAlt                 *string
	ChatIDAlt                 *string
	IsBot                     bool
	ScopeID                   *string
	GuildID                   *string
	ParentChatID              *string
	MessageID                 *string
	RoleAuthorized            bool
	Profile                   *string
	ProfileRouteRejected      bool
	AutoThreadCreated         bool
	AutoThreadInitialName     *string
	ProspectiveThreadID       *string
	DeliveredViaUpstreamRelay bool
	DetachedRoute             *string

	identity                 *RoutingIdentity
	transportAdapterRef      PlatformAdapter
	authorizationProfileHome string
	rejectedRouteProfile     *string
}

// Binds the adapter that received the message, the transport ref the base adapter stamps
func (s *SessionSource) SetTransportAdapterRef(adapter PlatformAdapter) {
	s.transportAdapterRef = adapter
}

// Adapter that received the message, nil when no transport ref was stamped
func (s *SessionSource) TransportAdapterRef() PlatformAdapter {
	if s == nil {
		return nil
	}
	return s.transportAdapterRef
}

var sessionChatTypePrefix = map[string]string{"group": "group: ", "channel": "channel: "}

// Mirrors scope id and the deprecated guild id onto each other with scope id winning and defaults the chat type
func (s *SessionSource) Normalize() *SessionSource {
	if s.ChatType == "" {
		s.ChatType = "dm"
	}
	if s.ScopeID == nil && s.GuildID != nil {
		s.ScopeID = s.GuildID
	} else if s.ScopeID != nil {
		s.GuildID = s.ScopeID
	}
	return s
}

func sessionDescribe(chatType, userLabel, chatLabel string) string {
	if chatType == "dm" {
		return "DM with " + userLabel
	}
	return sessionChatTypePrefix[chatType] + chatLabel
}

func firstSet(vals ...*string) (string, bool) {
	for _, v := range vals {
		if v != nil && *v != "" {
			return *v, true
		}
	}
	return "", false
}

// Human readable description of the source with its thread when it has one
func (s *SessionSource) Description() string {
	if s.Platform == PlatformLocal {
		return "CLI terminal"
	}
	user, ok := firstSet(s.UserName, s.UserID)
	if !ok {
		user = "user"
	}
	chat, ok := firstSet(s.ChatName)
	if !ok {
		chat = s.ChatID
	}
	desc := sessionDescribe(s.ChatType, user, chat)
	if s.ThreadID != nil && *s.ThreadID != "" {
		return desc + ", thread: " + *s.ThreadID
	}
	return desc
}

func strOrNil(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// Byte stable wire layout with the always present fields then the truthy optionals around the scope pair
func (s *SessionSource) ToDict() *pyjson.Object {
	d := pyjson.NewObject()
	d.Set("platform", string(s.Platform))
	d.Set("chat_id", s.ChatID)
	d.Set("chat_name", strOrNil(s.ChatName))
	d.Set("chat_type", s.ChatType)
	for _, f := range []struct {
		k string
		v *string
	}{{"user_id", s.UserID}, {"user_name", s.UserName}, {"thread_id", s.ThreadID}, {"chat_topic", s.ChatTopic}} {
		d.Set(f.k, strOrNil(f.v))
	}
	optional := func(fields ...struct {
		k string
		v *string
	}) {
		for _, f := range fields {
			if f.v != nil && *f.v != "" {
				d.Set(f.k, *f.v)
			}
		}
	}
	type kv = struct {
		k string
		v *string
	}
	optional(kv{"user_id_alt", s.UserIDAlt}, kv{"chat_id_alt", s.ChatIDAlt})
	scope := s.ScopeID
	if scope == nil {
		scope = s.GuildID
	}
	if scope != nil && *scope != "" {
		d.Set("scope_id", *scope)
		d.Set("guild_id", *scope)
	}
	optional(kv{"parent_chat_id", s.ParentChatID}, kv{"message_id", s.MessageID}, kv{"profile", s.Profile})
	if s.AutoThreadCreated {
		d.Set("auto_thread_created", true)
	}
	optional(kv{"auto_thread_initial_name", s.AutoThreadInitialName}, kv{"prospective_thread_id", s.ProspectiveThreadID})
	return d
}

func dictStrField(data map[string]any, key string) *string {
	v, ok := data[key]
	if !ok || v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		s = pyrepr.Str(v)
	}
	return &s
}

// Source rebuilt from its wire dict, an unknown platform failing as Platform does
func SessionSourceFromDict(ctx context.Context, data map[string]any) (*SessionSource, error) {
	raw, ok := data["platform"]
	if !ok {
		return nil, &PyError{Kind: "KeyError", Msg: "'platform'"}
	}
	platform, err := ParsePlatform(ctx, raw)
	if err != nil {
		return nil, err
	}
	chatRaw, ok := data["chat_id"]
	if !ok {
		return nil, &PyError{Kind: "KeyError", Msg: "'chat_id'"}
	}
	chatType := "dm"
	if v, ok := data["chat_type"]; ok {
		chatType, _ = v.(string)
	}
	scope := dictStrField(data, "scope_id")
	if _, has := data["scope_id"]; !has {
		scope = dictStrField(data, "guild_id")
	}
	src := &SessionSource{
		Platform: platform, ChatID: pyrepr.Str(chatRaw), ChatType: chatType, ScopeID: scope,
		AutoThreadCreated: pyrepr.Truthy(data["auto_thread_created"]),
		ChatName:          dictStrField(data, "chat_name"), UserID: dictStrField(data, "user_id"),
		UserName: dictStrField(data, "user_name"), ThreadID: dictStrField(data, "thread_id"),
		ChatTopic: dictStrField(data, "chat_topic"), UserIDAlt: dictStrField(data, "user_id_alt"),
		ChatIDAlt: dictStrField(data, "chat_id_alt"), ParentChatID: dictStrField(data, "parent_chat_id"),
		MessageID: dictStrField(data, "message_id"), Profile: dictStrField(data, "profile"),
		AutoThreadInitialName: dictStrField(data, "auto_thread_initial_name"),
		ProspectiveThreadID:   dictStrField(data, "prospective_thread_id"),
	}
	if src.ScopeID != nil {
		src.GuildID = src.ScopeID
	}
	return src, nil
}

// SourcePlatform answers the plugin message route with the source platform
func (s *SessionSource) SourcePlatform() Platform { return s.Platform }

// SourceChatID answers the plugin message route with the source chat
func (s *SessionSource) SourceChatID() string { return s.ChatID }
