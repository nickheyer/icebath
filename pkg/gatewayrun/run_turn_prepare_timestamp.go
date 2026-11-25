package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Message text for the model with the event time captured, the persisted text stripped of stamps and the trigger note
func (r *GatewayRunner) hmwaApplyMessageTimestamp(ctx context.Context, event *gateway.MessageEvent, messageText any) (any, any, any) {
	var persistUserMessage, persistUserTimestamp any
	apply := func() error {
		tz, _ := config.TimeZone(ctx)
		text, isText := messageText.(string)
		if !isText || text == "" {
			return nil
		}
		clean, embedded := gateway.StripLeadingMessageTimestamps(text, tz)
		persistUserMessage = stripDiscordTriggeringNote(event, clean)
		var eventTime any
		if event != nil && !event.Timestamp.IsZero() {
			eventTime = event.Timestamp
		}
		epoch, ok, err := gateway.CoerceMessageTimestamp(eventTime, tz)
		if err != nil {
			return err
		}
		switch {
		case ok:
			persistUserTimestamp = epoch
		case embedded != nil:
			persistUserTimestamp = *embedded
		}
		if settings.GatewayMessageTimestampsEnabled.Get(ctx) {
			rendered, err := gateway.RenderUserContentWithTimestamp(clean, persistUserTimestamp, tz)
			if err != nil {
				return err
			}
			messageText = rendered
		} else {
			messageText = clean
		}
		return nil
	}
	if err := apply(); err != nil {
		pkgLog().Debug(fmt.Sprintf("Message timestamp injection failed (non-fatal): %v", err))
	}
	return messageText, persistUserMessage, persistUserTimestamp
}
