package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Thread ids of the Telegram General topic, the lobby of a topic mode DM
var telegramGeneralTopicIDs = map[string]bool{"": true, "1": true}

// Profile namespace of topic mode rows, the profile stamped on the routed source else default
func telegramTopicProfileName(source *gateway.SessionSource) string {
	if p := strings.TrimSpace(optText(source.Profile)); p != "" {
		return p
	}
	return "default"
}

// Synchronous session database of the active scope, nil when unavailable
func (r *GatewayRunner) syncSessionDB(ctx context.Context) *state.SessionDB {
	db := r.sessionDB(ctx)
	if db == nil {
		return nil
	}
	return db.DB()
}

// Whether the source is a Telegram private chat
func isTelegramDM(source *gateway.SessionSource) bool {
	return source.Platform == gateway.PlatformTelegram && source.ChatType == "dm"
}

// Whether Telegram DM topic mode is active for the chat, off on any read failure
func (r *GatewayRunner) telegramTopicModeEnabled(ctx context.Context, source *gateway.SessionSource) bool {
	if !isTelegramDM(source) {
		return false
	}
	db := r.syncSessionDB(ctx)
	if db == nil {
		return false
	}
	userID := "None"
	if source.UserID != nil {
		userID = *source.UserID
	}
	enabled, err := db.IsTelegramTopicModeEnabled(ctx, source.ChatID, userID, telegramTopicProfileName(source))
	if err != nil {
		runLog().Debug("Failed to read Telegram topic mode state", "error", err)
		return false
	}
	return enabled
}

// Whether the source is the main Telegram DM or General topic that topic mode turned into a lobby
func (r *GatewayRunner) isTelegramTopicRootLobby(ctx context.Context, source *gateway.SessionSource) bool {
	return isTelegramDM(source) && r.telegramTopicModeEnabled(ctx, source) && telegramGeneralTopicIDs[optText(source.ThreadID)]
}

// Whether the receiving bot opted into moving root DM messages into a fresh topic
func (r *GatewayRunner) telegramDMTopicAutoTransferConfigured(ctx context.Context, source *gateway.SessionSource) bool {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil || adapter.Base() == nil || adapter.Base().Config == nil {
		return false
	}
	return provider.IsTruthyValue(adapter.Base().Config.Extra["dm_topic_auto_transfer"])
}

// Whether auto transfer is on and the source is a topic mode root DM
func (r *GatewayRunner) telegramTopicAutoTransferEnabled(ctx context.Context, source *gateway.SessionSource) bool {
	return r.telegramDMTopicAutoTransferConfigured(ctx, source) && r.isTelegramTopicRootLobby(ctx, source)
}

// Lobby recovery thread id of the source, nil when auto transfer owns the root DM or nothing applies
func (r *GatewayRunner) recoverTelegramTopicThreadID(ctx context.Context, source *gateway.SessionSource) *string {
	if r.telegramTopicAutoTransferEnabled(ctx, source) {
		return nil
	}
	return r.recoverTelegramLobbyThreadID(ctx, source)
}

// Newest bound topic of the user for a lobby shaped topic mode DM reply, nil leaves the source alone
func (r *GatewayRunner) recoverTelegramLobbyThreadID(ctx context.Context, source *gateway.SessionSource) *string {
	if !isTelegramDM(source) || source.ChatID == "" || source.UserID == nil || *source.UserID == "" || !r.telegramTopicModeEnabled(ctx, source) {
		return nil
	}
	inbound := optText(source.ThreadID)
	if inbound != "" && !telegramGeneralTopicIDs[inbound] {
		return nil
	}
	db := r.syncSessionDB(ctx)
	if db == nil {
		return nil
	}
	bindings, err := db.ListTelegramTopicBindingsForChat(ctx, source.ChatID, telegramTopicProfileName(source))
	if err != nil {
		runLog().Debug("topic-recover: read failed", "error", err)
		return nil
	}
	for _, b := range bindings {
		userID, _ := b.Get("user_id")
		if bindingText(userID) == *source.UserID {
			threadID, _ := b.Get("thread_id")
			recovered := bindingText(threadID)
			if recovered != "" && recovered != inbound {
				return &recovered
			}
			return nil
		}
	}
	return nil
}

// Text of a binding column, empty for a null or falsy value
func bindingText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Source with the Telegram DM topic recovery applied, the key every session scoped override derives from
func (r *GatewayRunner) normalizeSourceForSessionKey(ctx context.Context, source *gateway.SessionSource) (normalized *gateway.SessionSource) {
	defer func() {
		if rec := recover(); rec != nil {
			normalized = source
		}
	}()
	recovered := r.recoverTelegramTopicThreadID(ctx, source)
	if recovered == nil {
		return source
	}
	cp := *source
	cp.ThreadID = recovered
	return &cp
}
