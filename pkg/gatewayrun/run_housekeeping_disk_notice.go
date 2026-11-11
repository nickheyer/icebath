package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Drops the sticky auto prune notice of a local session viewer once autoprune was answered there
func clearLocalAutoPruneNotice(ctx context.Context, r *GatewayRunner, source *gateway.SessionSource) error {
	if source == nil || source.Platform != gateway.PlatformLocal {
		return nil
	}
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return err
	}
	if authority == nil {
		return nil
	}
	if live := authority.Sessions[source.ChatID]; live != nil {
		live.EventStream.Publish(source.ChatID, pyjson.ObjectOf("key", state.AutoPruneNoticeKey), "notification.clear", nil)
	}
	return nil
}
