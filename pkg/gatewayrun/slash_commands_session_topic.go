package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Optional bool of a capability flag, nil unless the flag is a bool
func capabilityFlag(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

// Whether a capability flag is exactly False
func capabilityFalse(v any) bool {
	b, ok := v.(bool)
	return ok && !b
}

// Manages Telegram DM user topic sessions, enabling topic mode and restoring sessions into topics
func (r *GatewayRunner) handleTopicCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	if source.Platform != gateway.PlatformTelegram || source.ChatType != "dm" {
		return i18n.T(ctx, "gateway.topic.not_telegram_dm", "", nil), nil
	}
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return sessionDBUnavailableReply(ctx), nil
	}
	if authorized, err := r.isUserAuthorizedForSource(ctx, source, false); err != nil {
		runLog().Debug("Topic auth check failed", "exc_info", err)
	} else if !authorized {
		return i18n.T(ctx, "gateway.topic.unauthorized", "", nil), nil
	}
	args := pystr.Strip(event.GetCommandArgs())
	switch pystr.Lower(args) {
	case "help", "?", "-h", "--help":
		return telegramTopicHelpText(ctx), nil
	case "off", "disable", "stop":
		return r.disableTelegramTopicModeForChat(ctx, source), nil
	}
	if args != "" {
		if sattr(source.ThreadID) == "" {
			return i18n.T(ctx, "gateway.topic.restore_needs_topic", "", nil), nil
		}
		return r.restoreTelegramTopicSession(ctx, event, args)
	}
	capabilities := r.getTelegramTopicCapabilities(ctx, source)
	if pyrepr.Truthy(capabilities["checked"]) {
		blockedKey := ""
		if capabilityFalse(capabilities["has_topics_enabled"]) {
			blockedKey = "gateway.topic.topics_disabled"
		} else if capabilityFalse(capabilities["allows_users_to_create_topics"]) {
			blockedKey = "gateway.topic.topics_user_disallowed"
		}
		if blockedKey != "" {
			if r.shouldSendTelegramCapabilityHint(source) {
				r.sendTelegramTopicSetupImage(ctx, source)
			}
			return i18n.T(ctx, blockedKey, "", nil), nil
		}
	}
	profileName := telegramTopicProfileName(source)
	if _, err := state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) (struct{}, error) {
		return struct{}{}, d.EnableTelegramTopicMode(ctx, state.TelegramTopicMode{ChatID: source.ChatID, UserID: strOrNone(source.UserID),
			ProfileName: profileName, HasTopicsEnabled: capabilityFlag(capabilities["has_topics_enabled"]),
			AllowsUsersToCreateTopics: capabilityFlag(capabilities["allows_users_to_create_topics"])})
	}); err != nil {
		runLog().Error("Failed to enable Telegram topic mode", "exc_info", err)
		return i18n.T(ctx, "gateway.topic.enable_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	if sattr(source.ThreadID) == "" {
		r.ensureTelegramSystemTopic(ctx, source)
		return r.telegramTopicRootStatusMessage(ctx, source), nil
	}
	binding, err := state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) (*pyjson.Object, error) {
		return d.GetTelegramTopicBinding(ctx, source.ChatID, *source.ThreadID, profileName)
	})
	if err != nil {
		runLog().Debug("Failed to read Telegram topic binding", "exc_info", err)
		binding = nil
	}
	if binding == nil || binding.Len() == 0 {
		return i18n.T(ctx, "gateway.topic.thread_ready", "", nil), nil
	}
	sessionID := rowStr(binding, "session_id")
	title, _, err := asyncDB.DB().GetSessionTitle(ctx, sessionID)
	if err != nil || title == "" {
		title = i18n.T(ctx, "gateway.topic.untitled_session", "", nil)
	}
	return i18n.T(ctx, "gateway.topic.bound_status", "", map[string]any{"label": title, "session_id": sessionID}), nil
}
