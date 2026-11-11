package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Whether the source is a user created Telegram private chat topic lane its chat has not moved past
func (r *GatewayRunner) isTelegramTopicLane(ctx context.Context, source *gateway.SessionSource) bool {
	tid := optText(source.ThreadID)
	return gateway.DetachedRouteOf(source) == nil && isTelegramDM(source) && r.telegramTopicModeEnabled(ctx, source) &&
		tid != "" && !telegramGeneralTopicIDs[tid]
}

// Persists the Telegram topic to session binding of a topic lane
func (r *GatewayRunner) recordTelegramTopicBinding(ctx context.Context, source *gateway.SessionSource, entry *gateway.SessionEntry) error {
	db := r.syncSessionDB(ctx)
	if db == nil || source.ChatID == "" || optText(source.ThreadID) == "" {
		return nil
	}
	return db.BindTelegramTopic(ctx, state.TelegramTopicBinding{
		ChatID: source.ChatID, ThreadID: optText(source.ThreadID), UserID: optText(source.UserID),
		SessionKey: entry.SessionKey, SessionID: entry.SessionID, ManagedMode: "auto",
		ProfileName: telegramTopicProfileName(source),
	})
}

// Points a topic lane's binding at the entry's session so a rotated parent never reloads
func (r *GatewayRunner) syncTelegramTopicBinding(ctx context.Context, source *gateway.SessionSource, entry *gateway.SessionEntry, reason string) {
	if !r.isTelegramTopicLane(ctx, source) {
		return
	}
	if err := r.recordTelegramTopicBinding(ctx, source, entry); err != nil {
		runLog().Debug(fmt.Sprintf("telegram topic binding refresh failed (%s)", reason), "error", err)
	}
}
