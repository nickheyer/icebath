package gateway

import (
	"net/http"

	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
)

// InvalidStatusError is the websockets InvalidStatus of a handshake the server answered with a non 101 HTTP status
type InvalidStatusError = pywebsockets.InvalidStatus

// Dial error as websockets connect raises it, a refused upgrade naming the HTTP status it got
func HandshakeError(resp *http.Response, err error) error {
	return pywebsockets.HandshakeError(resp, err)
}
