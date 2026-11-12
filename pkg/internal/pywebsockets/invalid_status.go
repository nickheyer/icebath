package pywebsockets

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"
)

// Handshake the server answered with a non 101 HTTP status, the websockets InvalidStatus exception
type InvalidStatus struct{ StatusCode int }

func (e *InvalidStatus) Error() string {
	return fmt.Sprintf("server rejected WebSocket connection: HTTP %d", e.StatusCode)
}

func (e *InvalidStatus) PyTypeName() string { return "InvalidStatus" }

func (e *InvalidStatus) Unwrap() error { return websocket.ErrBadHandshake }

// Dial error as websockets connect raises it, a refused upgrade naming the HTTP status it got
func HandshakeError(resp *http.Response, err error) error {
	if resp != nil && errors.Is(err, websocket.ErrBadHandshake) {
		return &InvalidStatus{StatusCode: resp.StatusCode}
	}
	return err
}
