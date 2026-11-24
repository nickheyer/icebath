package gateway

import (
	"net/http"

	websocketutil "github.com/nickheyer/icebath/pkg/utils/websocket"
)

// InvalidStatusError is the websockets InvalidStatus of a handshake the server answered with a non 101 HTTP status
type InvalidStatusError = websocketutil.HandshakeError

// Dial error as websockets connect raises it, a refused upgrade naming the HTTP status it got
func HandshakeError(resp *http.Response, err error) error {
	return websocketutil.WrapDialError(resp, err)
}
