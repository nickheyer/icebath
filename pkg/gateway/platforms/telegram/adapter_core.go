package telegram

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Base platform state the auth, pairing, config and topic binding helpers read
type adapterCore struct {
	base *gateway.BasePlatformAdapter
}

// Runner authorizing a full session source through its whole gateway chain
type runnerUserAuthorizer interface {
	IsUserAuthorized(ctx context.Context, source *gateway.SessionSource, allowAdapterDelegation bool) (bool, error)
}

// Runner reporting the unauthorized DM behavior of a platform for a profile
type runnerDMBehavior interface {
	GetUnauthorizedDMBehavior(ctx context.Context, platform gateway.Platform, profile *string) string
}

// Runner auth chain of the bound message handler owner, nil for a closure handler
func (g *adapterCore) legacyRunnerAuthFn() runnerUserAuthorizer {
	if auth, ok := g.base.MessageHandlerOwner().(runnerUserAuthorizer); ok && auth != nil {
		return auth
	}
	return nil
}

// Whether an inline button caller may run gated actions, failing closed without an allowlist
func (g *adapterCore) isCallbackUserAuthorized(ctx context.Context, userID string, chatID, chatType, threadID, userName *string) bool {
	normalizedUserID := pystr.Strip(userID)
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
	if g.base.HasAuthorizationCheck() {
		if injected := g.base.IsSenderAuthorized(normalizedUserID, &normalizedChatType, &effectiveChatID,
			gateway.AuthorizationOpts{ThreadID: threadID}); injected != nil {
			return *injected
		}
	}
	if auth := g.legacyRunnerAuthFn(); auth != nil {
		var name *string
		if userName != nil && *userName != "" {
			stripped := pystr.Strip(*userName)
			name = &stripped
		}
		uid := normalizedUserID
		source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: effectiveChatID, ChatType: normalizedChatType,
			UserID: &uid, UserName: name, ThreadID: threadID}
		ok, err := auth.IsUserAuthorized(ctx, source, true)
		if err == nil {
			return ok
		}
		slog.Debug(fmt.Sprintf("[Telegram] Falling back to env-only callback auth for user %s", normalizedUserID), "error", err)
	}
	decision := envAllowlistDecision(ctx, normalizedUserID)
	if decision == nil {
		switch pystr.Lower(gateway.PlatformGateEnv(ctx, "GATEWAY_ALLOW_ALL_USERS", "")) {
		case "true", "1", "yes":
			return true
		}
		return false
	}
	return *decision
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

// Whether an unauthorized DM must still reach the gateway for a pairing code or a one time decline
func (g *adapterCore) shouldPassUnauthorizedDMForPairing(ctx context.Context, source *gateway.SessionSource) bool {
	if source.ChatType != "dm" {
		return false
	}
	runner := g.base.MessageHandlerOwner()
	if runner == nil {
		runner = g.base.GatewayRunner
	}
	if behavior, ok := runner.(runnerDMBehavior); ok && behavior != nil {
		profile := source.Profile
		if profile == nil || *profile == "" {
			if owner := g.base.OwnerProfile(); owner != "" {
				profile = &owner
			} else {
				profile = nil
			}
		}
		if resolved, err := callDMBehavior(ctx, behavior, profile); err == nil {
			return resolved != "ignore"
		} else {
			slog.Debug("[Telegram] Failed to resolve unauthorized DM behavior; falling back to adapter-local override", "error", err)
		}
	}
	var raw any
	if g.base.Config != nil {
		raw = g.base.Config.Extra["unauthorized_dm_behavior"]
	}
	text := ""
	if raw != nil {
		text = pyrepr.Str(raw)
	}
	switch pystr.Lower(pystr.Strip(text)) {
	case "pair", "decline":
		return true
	}
	return false
}

// Unauthorized DM behavior from the runner, a panic in the resolver surfacing as an error
func callDMBehavior(ctx context.Context, behavior runnerDMBehavior, profile *string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return behavior.GetUnauthorizedDMBehavior(ctx, gateway.PlatformTelegram, profile), nil
}

// Intake auth prefilter that rejects only when it can make the runner's context aware decision
func (g *adapterCore) isUserAuthorizedFromMessage(ctx context.Context, message *models.Message) bool {
	source := g.sourceFromMessageForAuth(message)
	if source.UserID == nil {
		return true
	}
	userID := *source.UserID
	var authorized *bool
	key := "allow_from"
	switch source.ChatType {
	case "group", "forum", "channel":
		key = "group_allow_from"
	}
	if g.base.Config != nil && g.base.Config.Extra != nil {
		if raw, ok := g.base.Config.Extra[key]; ok && raw != nil {
			allowed := gateway.CoerceAllowSet(raw)
			decision := allowed[userID] || allowed["*"]
			authorized = &decision
		}
	}
	if authorized == nil {
		auth := g.legacyRunnerAuthFn()
		hasCallback := g.base.HasAuthorizationCheck()
		if hasCallback || auth != nil {
			if !telegramAuthEnvConfigured(ctx) {
				return true
			}
			var decision *bool
			if hasCallback {
				chatType, chatID := source.ChatType, source.ChatID
				decision = g.base.IsSenderAuthorized(userID, &chatType, &chatID,
					gateway.AuthorizationOpts{IsBot: source.IsBot, ThreadID: source.ThreadID})
			}
			if decision != nil {
				authorized = decision
			} else if auth != nil {
				if ok, err := auth.IsUserAuthorized(ctx, source, true); err == nil {
					authorized = &ok
				} else {
					slog.Debug(fmt.Sprintf("[Telegram] Falling back to env-only auth for user %s", userID), "error", err)
				}
			}
		}
	}
	if authorized == nil {
		authorized = envAllowlistDecision(ctx, userID)
		if authorized == nil {
			return true
		}
	}
	if *authorized {
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
	if p := metadata["hermes_profile"]; pyTruthy(p) {
		profileName = pyrepr.Str(p)
	} else if name := g.base.ProfileName(); name != "" {
		profileName = name
	}
	removed, err := bindings.DeleteTelegramTopicBinding(ctx, pyrepr.Str(chatID), pyrepr.Str(threadID), profileName)
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] delete_telegram_topic_binding failed for chat=%s thread=%s — skipping prune",
			g.base.Name(), pyrepr.Str(chatID), pyrepr.Str(threadID)), "error", err)
		return
	}
	if removed != 0 {
		slog.Info(fmt.Sprintf("[%s] Pruned stale Telegram DM topic binding chat=%s thread=%s (Bot API: thread not found)",
			g.base.Name(), pyrepr.Str(chatID), pyrepr.Str(threadID)))
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
		switch pystr.Lower(pystr.Strip(s)) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
		return def
	}
	return pyTruthy(value)
}

// Link preview options disabling previews when configured, nil otherwise
func linkPreviewKwargs(disable bool) *models.LinkPreviewOptions {
	if !disable {
		return nil
	}
	return &models.LinkPreviewOptions{IsDisabled: bot.True()}
}
