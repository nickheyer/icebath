package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Discord slash command interaction carrying the guild it ran in, falsy outside a guild
type interactionGuildCarrier interface{ InteractionGuildID() any }

// Discord message carrying its guild, false for a direct message
type messageGuildCarrier interface{ MessageGuildID() (int64, bool) }

// Guild id of a Discord event, a slash interaction's guild first then a regular message's guild
func getGuildID(event *gateway.MessageEvent) (int64, bool, error) {
	raw := event.RawMessage
	if interaction, ok := raw.(interactionGuildCarrier); ok {
		if id := interaction.InteractionGuildID(); id != nil && id != "" && id != int64(0) {
			n, err := pyint.Of(id)
			if err != nil {
				return 0, false, err
			}
			return n.Int64(), true, nil
		}
	}
	if message, ok := raw.(messageGuildCarrier); ok {
		id, found := message.MessageGuildID()
		return id, found, nil
	}
	return 0, false, nil
}
