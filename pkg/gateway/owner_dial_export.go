package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// Largest attachment the room store accepts in bytes
const MaxAttachmentBytes = maxAttachmentBytes

// Whether the name is a per turn client surface that carries a model bound note
func IsTurnSurface(name string) bool { return turnSurfaces[name] }

// Socket on the ready endpoint dialed with this home's ticket of the scope, never through a proxy
func DialOwnerSocket(ctx context.Context, profileHome string, endpoint *GatewayEndpoint, scope string, maxSize int64) (GatewaySocket, error) {
	ticket, err := sessionTicket(profileHome, endpoint, "", scope, "", false)
	if err != nil {
		return nil, err
	}
	target, protocols := GatewayWSTarget(endpoint, ticket)
	dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 10 * time.Second, Subprotocols: protocols}
	conn, _, err := dialer.DialContext(ctx, target, http.Header{})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxSize)
	return &wsSocket{conn: conn}, nil
}
