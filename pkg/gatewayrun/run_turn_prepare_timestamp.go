package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// True when gateway.message_timestamps.enabled is opted in, a bare boolean shorthand accepted
func messageTimestampsEnabled(userConfig map[string]any) bool {
	if userConfig == nil {
		return false
	}
	gw, ok := userConfig["gateway"].(map[string]any)
	if !ok {
		return false
	}
	if mt, isDict := gw["message_timestamps"].(map[string]any); isDict {
		enabled, present := mt["enabled"]
		return present && pyTruthy(enabled)
	}
	return pyTruthy(gw["message_timestamps"])
}

// Python truthiness of a decoded config value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return x.Len() > 0
	}
	return true
}

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
		if messageTimestampsEnabled(gateway.LoadGatewayConfigMap(ctx, "")) {
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
		runLog().Debug(fmt.Sprintf("Message timestamp injection failed (non-fatal): %v", err))
	}
	return messageText, persistUserMessage, persistUserTimestamp
}
