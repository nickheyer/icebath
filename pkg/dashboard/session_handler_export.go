package dashboard

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Swaps the handler an admitted native session socket runs, the attribute patch of tui_gateway.ws.handle_ws
func ReplaceSessionHandleWS(handler func(ctx context.Context, ws tuiserver.ServedSocket, authIdentity map[string]any, subprotocol *string, operator bool) error) {
	sessionHandleWS = handler
}
