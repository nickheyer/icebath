package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Platform value of a source for logs and stores, unknown when unset
func sourcePlatformName(source *gateway.SessionSource) string {
	if source.Platform == "" {
		return "unknown"
	}
	return string(source.Platform)
}

// User id of a source, empty when unset
func sourceUserID(source *gateway.SessionSource) string {
	if source.UserID == nil {
		return ""
	}
	return *source.UserID
}

// DMs an unauthorized sender a pairing code or the rate limited notice, silent while the sender is rate limited
func (r *GatewayRunner) hmOfferPairingCode(ctx context.Context, source *gateway.SessionSource) error {
	platformName := sourcePlatformName(source)
	store := r.pairingStoreFor(source)
	if store == nil {
		inboundLog().Error(fmt.Sprintf("Cannot offer pairing code on %s: no pairing store", platformName))
		return nil
	}
	userID := sourceUserID(source)
	limited, err := store.IsRateLimited(ctx, platformName, userID)
	if err != nil {
		return err
	}
	if limited {
		return nil
	}
	userName := ""
	if source.UserName != nil {
		userName = *source.UserName
	}
	code, ok, err := store.GenerateCode(ctx, platformName, userID, userName)
	if err != nil {
		inboundLog().Warn(fmt.Sprintf("Could not record a pairing code on %s", platformName), "exc_info", err)
		return nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	reply := pairingRateLimitedReply(ctx)
	if ok && code != "" {
		reply = pairingCodeReply(ctx, platformName, code, pairingProfileArg(store))
	}
	if adapter != nil {
		if _, err := adapter.Send(ctx, source.ChatID, reply, "", nil); err != nil {
			return err
		}
	}
	if !ok || code == "" {
		return store.RecordRateLimit(ctx, platformName, userID)
	}
	return nil
}

// One short refusal per sender per dedupe window under the decline behaviour, then silence
func (r *GatewayRunner) hmSendUnauthorizedDecline(ctx context.Context, source *gateway.SessionSource) error {
	platformName := sourcePlatformName(source)
	store := r.pairingStoreFor(source)
	if store == nil {
		return nil
	}
	userID := sourceUserID(source)
	recent, err := store.HasRecentDecline(ctx, platformName, userID)
	if err != nil {
		return err
	}
	if recent {
		return nil
	}
	if err := store.RecordDecline(ctx, platformName, userID); err != nil {
		inboundLog().Warn(fmt.Sprintf("Could not record an unauthorized-DM decline on %s", platformName), "exc_info", err)
		return nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	if adapter == nil {
		return nil
	}
	text := ""
	if r.config != nil {
		text = pystr.Strip(r.config.UnauthorizedDMDeclineMessage)
	}
	if text == "" {
		text = gateway.DefaultUnauthorizedDMDeclineMessage
	}
	if _, err := adapter.Send(ctx, source.ChatID, text, "", nil); err != nil {
		inboundLog().Warn(fmt.Sprintf("Failed to deliver unauthorized-DM decline on %s", platformName), "exc_info", err)
	}
	return nil
}
