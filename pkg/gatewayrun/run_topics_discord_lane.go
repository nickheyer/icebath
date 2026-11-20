package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Whether the source is a Discord thread this gateway just auto created
func (r *GatewayRunner) isDiscordAutoThreadLane(source *gateway.SessionSource) bool {
	return source.Platform == gateway.PlatformDiscord && source.ChatType == "thread" && source.AutoThreadCreated &&
		source.ThreadID != nil && *source.ThreadID != "" && source.AutoThreadInitialName != nil && *source.AutoThreadInitialName != ""
}

// The source with the auto thread markers of the session's opening event when that event created this same thread
func (r *GatewayRunner) recoverDiscordAutoThreadSource(ctx context.Context, source *gateway.SessionSource, sessionKey string) *gateway.SessionSource {
	if source.Platform != gateway.PlatformDiscord || source.ChatType != "thread" || source.ThreadID == nil || *source.ThreadID == "" ||
		source.DeliveredViaUpstreamRelay || r.isDiscordAutoThreadLane(source) {
		return source
	}
	entry := r.sessionStore.LookupBySessionKey(ctx, sessionKey)
	if entry == nil {
		return source
	}
	opensThisThread := func(origin *gateway.SessionSource) bool {
		return origin != nil && origin.ThreadID != nil && *origin.ThreadID == *source.ThreadID && r.isDiscordAutoThreadLane(origin)
	}
	origin := entry.Origin
	if !opensThisThread(origin) {
		origin = nil
		if db := r.sessionStore.DBForKey(ctx, sessionKey); db != nil {
			row, err := db.GetSession(ctx, entry.SessionID)
			if err != nil {
				panic(err)
			}
			if row != nil {
				if raw := row.GetOr("origin_json", nil); pyrepr.Truthy(raw) {
					if parsed, err := pyjson.Loads(pyrepr.Str(raw)); err == nil {
						if data, ok := pyjson.Plain(parsed).(map[string]any); ok {
							if recovered, err := gateway.SessionSourceFromDict(ctx, data); err == nil {
								origin = recovered
							}
						}
					}
				}
			}
		}
		if !opensThisThread(origin) {
			return source
		}
	}
	name := origin.AutoThreadInitialName
	return gateway.ReplaceSource(source, func(s *gateway.SessionSource) {
		s.AutoThreadCreated = true
		s.AutoThreadInitialName = name
	})
}

// Whether a relay delivered Discord channel event's reply may be auto threaded by the connector
func (r *GatewayRunner) isRelayDiscordChannelLane(source *gateway.SessionSource) bool {
	return source.Platform == gateway.PlatformDiscord && source.ChatID != "" && (source.ThreadID == nil || *source.ThreadID == "") &&
		(source.ChatType == "group" || source.ChatType == "channel") && source.DeliveredViaUpstreamRelay
}
