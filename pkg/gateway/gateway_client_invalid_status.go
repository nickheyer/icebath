package gateway

import (
	"fmt"

	"github.com/gorilla/websocket"
)

// InvalidStatusError is the websockets InvalidStatus of a handshake the server answered with a non 101 HTTP status
type InvalidStatusError struct{ StatusCode int }

func (e *InvalidStatusError) Error() string {
	return fmt.Sprintf("server rejected WebSocket connection: HTTP %d", e.StatusCode)
}

func (e *InvalidStatusError) PyTypeName() string { return "InvalidStatus" }

func (e *InvalidStatusError) Unwrap() error { return websocket.ErrBadHandshake }
