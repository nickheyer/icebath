package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Seconds between two root DM lobby reminders in one Telegram chat
const telegramLobbyReminderCooldownS = 30.0

// Cooldown key of a profile and chat pair, empty when the source names no chat
func telegramTopicCooldownKey(source *gateway.SessionSource) string {
	if source.ChatID == "" {
		return ""
	}
	return telegramTopicProfileName(source) + ":" + source.ChatID
}

// True and stamps now when the per profile and chat window has elapsed since the last stamp
func telegramCooldownElapsed(stamps *map[string]float64, source *gateway.SessionSource, cooldownS float64) bool {
	if *stamps == nil {
		*stamps = map[string]float64{}
	}
	key := telegramTopicCooldownKey(source)
	if key == "" {
		return true
	}
	now := pythread.Monotonic()
	if now-(*stamps)[key] < cooldownS {
		return false
	}
	(*stamps)[key] = now
	return true
}

// Rate limits root DM lobby reminders to one per cooldown window
func (r *GatewayRunner) shouldSendTelegramLobbyReminder(source *gateway.SessionSource) bool {
	return telegramCooldownElapsed(&r.telegramLobbyReminderTS, source, telegramLobbyReminderCooldownS)
}

// Reply telling a user the root DM is reserved for system commands
func (r *GatewayRunner) telegramTopicRootLobbyMessage(ctx context.Context) string {
	return i18n.T(ctx, "gateway.topic.root_lobby", "", nil)
}

// Reply telling a user how to open a parallel chat from the root DM
func (r *GatewayRunner) telegramTopicRootNewMessage(ctx context.Context) string {
	return i18n.T(ctx, "gateway.topic.root_new", "", nil)
}
