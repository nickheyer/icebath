package telegram

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Base platform state the auth, pairing, config and topic binding helpers read
type adapterCore struct {
	base *gateway.BasePlatformAdapter
}

// Background service surface naming how a profile answers a private chat from someone it does not allow
type runnerDMBehavior interface {
	GetUnauthorizedDMBehavior(ctx context.Context, platform gateway.Platform, profile *string) string
}

// Whether an inline button caller may run gated actions, the background service's access check deciding and no verdict refusing
func (g *adapterCore) isCallbackUserAuthorized(userID string, chatID, chatType, threadID *string) bool {
	normalizedUserID := strings.TrimSpace(userID)
	if normalizedUserID == "" {
		return false
	}
	var rawType any
	if chatType != nil {
		rawType = *chatType
	}
	normalizedChatType := normalizeChatType(rawType, threadID != nil)
	effectiveChatID := normalizedUserID
	if chatID != nil && *chatID != "" {
		effectiveChatID = *chatID
	}
	verdict := g.base.IsSenderAuthorized(normalizedUserID, &normalizedChatType, &effectiveChatID, gateway.AuthorizationOpts{ThreadID: threadID})
	return verdict != nil && *verdict
}

// Session source the gateway auth path expects, the sender chat standing in for a channel post author
func (g *adapterCore) sourceFromMessageForAuth(message *models.Message) *gateway.SessionSource {
	var userID, userName *string
	isBot := false
	if user := message.From; user != nil {
		userID = nonEmpty(fmt.Sprint(user.ID))
		isBot = user.IsBot
		name := user.Username
		if name == "" {
			name = fullName(user.FirstName, user.LastName)
		}
		userName = nonEmpty(name)
	}
	if userID == nil && message.SenderChat != nil {
		userID = nonEmpty(fmt.Sprint(message.SenderChat.ID))
		if userName == nil {
			userName = nonEmpty(message.SenderChat.Title)
		}
	}
	chatID := ""
	if message.Chat.ID != 0 {
		chatID = fmt.Sprint(message.Chat.ID)
	} else if userID != nil {
		chatID = *userID
	}
	hasThread := message.MessageThreadID != 0
	isTopicMessage := message.IsTopicMessage
	isForumGroup := message.Chat.IsForum
	chatType := normalizeChatType(string(message.Chat.Type), hasThread && (isTopicMessage || isForumGroup))
	var threadID *string
	if hasThread && ((chatType == "forum" && (isTopicMessage || isForumGroup)) || (chatType == "dm" && isTopicMessage)) {
		tid := fmt.Sprint(message.MessageThreadID)
		threadID = &tid
	}
	return &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: chatID, ChatType: chatType, UserID: userID,
		UserName: userName, ThreadID: threadID, IsBot: isBot}
}

// Session source of a message_reaction actor, an error when actor, chat or message identity is absent
func (g *adapterCore) sourceFromReactionForAuth(ctx context.Context, update *models.Update) (*gateway.SessionSource, error) {
	mr := update.MessageReaction
	if mr == nil {
		return nil, fmt.Errorf("gateway_platform_event source extraction requires a message_reaction update")
	}
	var userID, userName *string
	if user := mr.User; user != nil {
		userID = nonEmpty(fmt.Sprint(user.ID))
		name := user.Username
		if name == "" {
			name = fullName(user.FirstName, user.LastName)
		}
		userName = nonEmpty(name)
	} else if actor := mr.ActorChat; actor != nil {
		userID = nonEmpty(fmt.Sprint(actor.ID))
		name := actor.Username
		if name == "" && actor.FirstName != "" {
			name = fullName(actor.FirstName, actor.LastName)
		}
		if name == "" {
			name = actor.Title
		}
		userName = nonEmpty(name)
	}
	var chatID *string
	if mr.Chat.ID != 0 {
		chatID = nonEmpty(fmt.Sprint(mr.Chat.ID))
	}
	if userID == nil || chatID == nil || mr.MessageID == 0 {
		return nil, fmt.Errorf("gateway_platform_event reaction requires actor, chat, and message identities")
	}
	chatType := normalizeChatType(string(mr.Chat.Type), mr.Chat.IsForum)
	messageID := fmt.Sprint(mr.MessageID)
	return g.base.BuildSource(ctx, *chatID, gateway.SourceFields{ChatType: chatType, UserID: userID, UserName: userName,
		MessageID: &messageID}), nil
}

// Whether a private chat from someone not allowed must still reach the background service, which answers with a pairing code or a decline
func (g *adapterCore) shouldPassUnauthorizedDMForPairing(ctx context.Context, source *gateway.SessionSource) bool {
	if source.ChatType != "dm" {
		return false
	}
	behavior, ok := g.base.GatewayRunner.(runnerDMBehavior)
	if !ok || behavior == nil {
		return false
	}
	profile := source.Profile
	if profile == nil || *profile == "" {
		profile = nil
		if owner := g.base.OwnerProfile(); owner != "" {
			profile = &owner
		}
	}
	resolved, err := callDMBehavior(ctx, behavior, profile)
	if err != nil {
		pkgLog().Warn("[Telegram] Could not tell how the profile answers unknown senders; dropping the private chat", "error", err)
		return false
	}
	return resolved != "ignore"
}

// Unauthorized DM behavior from the background service, a panic in the resolver surfacing as an error
func callDMBehavior(ctx context.Context, behavior runnerDMBehavior, profile *string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return behavior.GetUnauthorizedDMBehavior(ctx, gateway.PlatformTelegram, profile), nil
}

// Whether a message goes on to the trigger gates: the background service's access check decides, no verdict leaving the decision to its admission
func (g *adapterCore) isUserAuthorizedFromMessage(ctx context.Context, message *models.Message) bool {
	source := g.sourceFromMessageForAuth(message)
	if source.UserID == nil {
		return true
	}
	chatType, chatID := source.ChatType, source.ChatID
	verdict := g.base.IsSenderAuthorized(*source.UserID, &chatType, &chatID, gateway.AuthorizationOpts{IsBot: source.IsBot, ThreadID: source.ThreadID})
	if verdict == nil || *verdict {
		return true
	}
	return g.shouldPassUnauthorizedDMForPairing(ctx, source)
}

// Topic binding store of the session database
type topicBindingStore interface {
	DeleteTelegramTopicBinding(ctx context.Context, chatID, threadID, profileName string) (int, error)
}

// Session store exposing its live session database
type sessionDBStore interface {
	DB(ctx context.Context) (gateway.SessionStoreDB, error)
}

// Drops the stale DM topic binding of a thread Telegram confirmed deleted, never failing the send
func (g *adapterCore) pruneStaleDMTopicBinding(ctx context.Context, chatID, threadID any, metadata map[string]any) {
	if chatID == nil || threadID == nil {
		return
	}
	store, ok := g.base.SessionStore().(sessionDBStore)
	if !ok || store == nil {
		return
	}
	db, err := store.DB(ctx)
	if err != nil || db == nil {
		return
	}
	bindings, ok := db.(topicBindingStore)
	if !ok {
		return
	}
	profileName := "default"
	if p := metadata["profile"]; anyutil.IsTrue(p) {
		profileName = anyutil.String(p)
	} else if name := g.base.ProfileName(); name != "" {
		profileName = name
	}
	removed, err := bindings.DeleteTelegramTopicBinding(ctx, anyutil.String(chatID), anyutil.String(threadID), profileName)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("[%s] delete_telegram_topic_binding failed for chat=%s thread=%s — skipping prune",
			g.base.Name(), anyutil.String(chatID), anyutil.String(threadID)), "error", err)
		return
	}
	if removed != 0 {
		pkgLog().Info(fmt.Sprintf("[%s] Pruned stale Telegram DM topic binding chat=%s thread=%s (Bot API: thread not found)",
			g.base.Name(), anyutil.String(chatID), anyutil.String(threadID)))
	}
}

var _ topicBindingStore = (*state.SessionDB)(nil)

// Boolean config extra with string spellings, unknown strings reading as the default
func (g *adapterCore) coerceBoolExtra(key string, def bool) bool {
	var value any
	if g.base.Config != nil && len(g.base.Config.Extra) > 0 {
		value = g.base.Config.Extra[key]
	}
	if value == nil {
		return def
	}
	if s, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
		return def
	}
	return anyutil.IsTrue(value)
}

// Link preview options disabling previews when configured, nil otherwise
func linkPreviewKwargs(disable bool) *models.LinkPreviewOptions {
	if !disable {
		return nil
	}
	return &models.LinkPreviewOptions{IsDisabled: bot.True()}
}
