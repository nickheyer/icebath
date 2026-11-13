package dashboard

import (
	"context"
	"errors"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Largest body a public auth POST may carry
const AuthBodyLimit = 64 * 1024

// HTTP 413 raised from the limited receive, an HTTPException subclass
type authBodyTooLarge struct{ *HTTPException }

func (e *authBodyTooLarge) Unwrap() error { return e.HTTPException }

// HTTP 413 with the request body too large detail
func newAuthBodyTooLarge() *authBodyTooLarge {
	return &authBodyTooLarge{NewHTTPException(413, "Request body too large")}
}

// Middleware bounding the body of public auth POSTs ahead of app
type authBodyLimitMiddleware struct{ app ASGIApp }

// Wraps app so a public auth POST whose declared or streamed body passes the limit answers 413
func AuthBodyLimitMiddleware(app ASGIApp) ASGIApp { return (&authBodyLimitMiddleware{app: app}).ASGI }

// Answers 413 for a public auth POST whose declared or streamed body passes the limit
func (m *authBodyLimitMiddleware) ASGI(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	if scope.Type != "http" || scope.Method != "POST" || !strings.HasPrefix(scope.Path, "/auth/") || !pathIsPublic(scope.Path) {
		return m.app(ctx, scope, receive, send)
	}
	response := MustJSONResponse(jsonObj("detail", "Request body too large"), 413)
	for _, h := range scope.Headers {
		if h[0] != "content-length" {
			continue
		}
		n, ok := pystr.IntText(latin1Decode(h[1]))
		if !ok {
			continue
		}
		if n.Cmp(bigInt(AuthBodyLimit)) > 0 {
			return response.Serve(ctx, scope, receive, send)
		}
	}
	received := 0
	started := false
	limited := func(ctx context.Context) (Message, error) {
		msg, err := receive(ctx)
		if err != nil {
			return msg, err
		}
		if msg.Type == "http.request" {
			received += len(msg.Body)
			if received > AuthBodyLimit {
				return msg, newAuthBodyTooLarge()
			}
		}
		return msg, nil
	}
	tracked := func(ctx context.Context, msg Message) error {
		if msg.Type == "http.response.start" {
			started = true
		}
		return send(ctx, msg)
	}
	err := m.app(ctx, scope, limited, tracked)
	var tooLarge *authBodyTooLarge
	if errors.As(err, &tooLarge) && !started {
		return response.Serve(ctx, scope, receive, send)
	}
	return err
}

// Big integer of an int64
func bigInt(n int64) *big.Int { return big.NewInt(n) }
