package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Telegram adapter surface that opens a forum topic in a private chat, nil when no topic was created
type dmTopicCreator interface {
	CreateDMTopic(ctx context.Context, chatID int64, name string, iconColor *int, iconCustomEmojiID *string) (*int64, error)
}

// Topic a root DM message moves into, created true for a fresh topic, ok false when nothing applies
func (r *GatewayRunner) transferTelegramRootDM(ctx context.Context, source *gateway.SessionSource) (threadID string, created bool, ok bool, err error) {
	if source.ChatID == "" || !r.telegramDMTopicAutoTransferConfigured(ctx, source) {
		return "", false, false, nil
	}
	if !r.isTelegramTopicRootLobby(ctx, source) {
		return "", false, false, nil
	}
	adapter, _ := r.deliveryAdapterFor(ctx, source)
	var thread *int64
	if creator, has := adapter.(dmTopicCreator); has && adapter != nil {
		thread, err = func() (*int64, error) {
			chatID, err := anyutil.ToInt(source.ChatID)
			if err != nil {
				return nil, err
			}
			return creator.CreateDMTopic(ctx, chatID, i18n.T(ctx, "gateway.topic.default_title", "", nil), nil, nil)
		}()
		if err != nil {
			pkgLog().Debug("Failed to auto-transfer Telegram root DM into a new topic", "exc_info", err)
		}
	}
	if thread != nil && *thread != 0 {
		pkgLog().Info(fmt.Sprintf("telegram topic auto-transfer: chat=%s user=%s root -> new topic %d", source.ChatID, anyutil.String(optAny(source.UserID)), *thread))
		return fmt.Sprint(*thread), true, true, nil
	}
	recovered := r.recoverTelegramLobbyThreadID(ctx, source)
	if recovered == nil || *recovered == "" {
		return "", false, false, nil
	}
	return *recovered, false, true, nil
}
