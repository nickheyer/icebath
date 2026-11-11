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

// Answers 413 for a public auth POST whose declared or streamed body passes the limit
func AuthBodyLimitMiddleware(app ASGIApp) ASGIApp {
	return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		if scope.Type != "http" || scope.Method != "POST" || !strings.HasPrefix(scope.Path, "/auth/") || !pathIsPublic(scope.Path) {
			return app(ctx, scope, receive, send)
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
			m, err := receive(ctx)
			if err != nil {
				return m, err
			}
			if m.Type == "http.request" {
				received += len(m.Body)
				if received > AuthBodyLimit {
					return m, &authBodyTooLarge{NewHTTPException(413, "Request body too large")}
				}
			}
			return m, nil
		}
		tracked := func(ctx context.Context, m Message) error {
			if m.Type == "http.response.start" {
				started = true
			}
			return send(ctx, m)
		}
		err := app(ctx, scope, limited, tracked)
		var tooLarge *authBodyTooLarge
		if errors.As(err, &tooLarge) && !started {
			return response.Serve(ctx, scope, receive, send)
		}
		return err
	}
}

// Big integer of an int64
func bigInt(n int64) *big.Int { return big.NewInt(n) }
