package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Pops the adapter's deferred post delivery callback for the key when the generation owns it, nil without an adapter
func popPostDeliveryCallback(adapter gateway.PlatformAdapter, key string, generation *int) gateway.PostDeliveryCallback {
	if adapter == nil {
		return nil
	}
	return adapter.Base().PopPostDeliveryCallback(key, generation)
}

// Stops the typing indicator of this turn's own message with its thread routing metadata, never failing the turn
func (r *GatewayRunner) hmwaStopTypingForTurn(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource) {
	defer func() { _ = recover() }()
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil {
		return
	}
	metadata := gateway.TypingMetadataForTurn(adapter, source.ChatID, gateway.ReplyAnchorForEvent(event), r.eventThreadMetadata(event, source))
	_ = gateway.StopTypingWithMetadata(ctx, adapter, source.ChatID, metadata)
}
