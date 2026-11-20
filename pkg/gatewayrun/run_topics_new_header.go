package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
)

// New session header of a Telegram topic lane, false for every other source
func (r *GatewayRunner) telegramTopicNewHeader(ctx context.Context, source *gateway.SessionSource) (string, bool) {
	if !r.isTelegramTopicLane(ctx, source) {
		return "", false
	}
	return i18n.T(ctx, "gateway.topic.new_header", "", nil), true
}
