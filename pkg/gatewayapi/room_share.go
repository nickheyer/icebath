package gatewayapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Route a room home pushes shared room pages to and pulls queued operations from
func (a *APIServerAdapter) roomShareHTTPRoutes() []httpRoute {
	return []httpRoute{{"POST", "/v1/rooms/sync", a.requireAuth(a.handleRoomShareSync)}}
}

// Stores one sync of a room home and answers cursors and the operations local users queued for it
func (a *APIServerAdapter) handleRoomShareSync(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	result, err := gateway.RoomShareSync(ctx, a.GatewayRunner, body)
	if err != nil {
		var refused *gateway.RoomShareSyncError
		if errors.As(err, &refused) {
			return jsonError(refused.Msg, "invalid_room_sync", http.StatusBadRequest), nil
		}
		return nil, err
	}
	return jsonResponse(result, http.StatusOK, nil), nil
}
