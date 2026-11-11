package dashboard

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Rejects requests whose Host header names neither the bound interface nor a configured public host
func hostHeaderMiddleware(app *FastAPI) DispatchFunc {
	return func(ctx context.Context, req *Request, callNext CallNext) (ASGIResponse, error) {
		bound := app.State.String("bound_host")
		trusted, _ := app.State.Get("trusted_public_hosts").(map[string]bool)
		if bound != "" && !gateway.IsAcceptedHost(req.Headers().Get("host"), bound, trusted) {
			return MustJSONResponse(map[string]any{"detail": "Invalid Host header. Dashboard requests must use the " +
				"bound hostname or the configured public hostname."}, 400), nil
		}
		return callNext(ctx, req)
	}
}

// OAuth gate between the host check and the token middleware, a pass through on a loopback bind
func dashboardAuthGate(ctx context.Context, req *Request, callNext CallNext) (ASGIResponse, error) {
	return GatedAuthMiddleware(ctx, req, callNext)
}

// Outermost auth seam, the native ticket then bearer token auth for opted in routes in the ticket home
func tokenAuthSeam(ctx context.Context, req *Request, callNext CallNext) (ASGIResponse, error) {
	rejection, err := authenticateNativeHTTP(ctx, req)
	if err != nil {
		return nil, err
	}
	if rejection != nil {
		return rejection, nil
	}
	return TokenAuthMiddleware(nativeProfileScope(ctx, &req.HTTPConnection), req, callNext)
}

// Counts unhandled errors and 5xx responses into the health window without altering them
func dashboardHealthMiddleware(ctx context.Context, req *Request, callNext CallNext) (ASGIResponse, error) {
	resp, err := callNext(ctx, req)
	if err != nil {
		DashboardHealthState.RecordError(pyTypeNameOf(err), req.URL().Path())
		return nil, err
	}
	if resp.Status() >= 500 {
		DashboardHealthState.RecordError(fmt.Sprintf("http_%d", resp.Status()), req.URL().Path())
	}
	return resp, nil
}

// Python exception class name of an error, its Go type name when it carries none
func pyTypeNameOf(err error) string {
	if named, ok := err.(interface{ PyTypeName() string }); ok {
		return named.PyTypeName()
	}
	return fmt.Sprintf("%T", err)
}

// Whether an origin is one of the app's trusted loopback origins, the WebSocket Origin rule
func isTrustedLocalOrigin(app *FastAPI, origin string) bool {
	return gateway.IsTrustedLocalOrigin(origin, listenerStateOf(app.State))
}

// CORS for the trusted loopback origins only, every method and header allowed for them
func localOriginCORSMiddleware(app *FastAPI) Middleware {
	return CORSMiddleware(CORSOptions{AllowMethods: []string{"*"}, AllowHeaders: []string{"*"}, MaxAge: 600,
		IsAllowedOriginCheck: func(origin string) bool { return isTrustedLocalOrigin(app, origin) }})
}

// One authenticated in process request against a cheap database route, recorded as the self test result
func dashboardSelftestOnce(ctx context.Context, app *FastAPI) {
	status, err := inProcessGet(ctx, app, "/api/sessions", "limit=1", Header{lowerHeaderName(sessionHeaderName), SessionToken()})
	if err != nil {
		DashboardHealthState.RecordSelftest(false, nil)
		return
	}
	DashboardHealthState.RecordSelftest(status == 200, &status)
}

func lowerHeaderName(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// Status of a GET the app serves in process to a loopback client like the httpx ASGI transport
func inProcessGet(ctx context.Context, app *FastAPI, path, query string, headers ...Header) (int, error) {
	scope := &Scope{Type: "http", HTTPVersion: "1.1", Method: "GET", Scheme: "http", Path: path, RawPath: path,
		QueryString: query, Headers: append([]Header{{"host", "127.0.0.1"}}, headers...),
		Client: &Address{Host: "127.0.0.1", Port: 123}, Server: &Address{Host: "127.0.0.1", Port: 80},
		Extensions: map[string]any{}}
	sent := false
	receive := func(context.Context) (Message, error) {
		if sent {
			return Message{Type: "http.disconnect"}, nil
		}
		sent = true
		return Message{Type: "http.request"}, nil
	}
	status := 0
	send := func(_ context.Context, m Message) error {
		if m.Type == "http.response.start" {
			status = m.Status
		}
		return nil
	}
	if err := app.ASGI(ctx, scope, receive, send); err != nil {
		return 0, err
	}
	return status, nil
}

// Periodic self test driver of the lifespan, skipped while the auth gate engages
func dashboardSelftestLoop(ctx context.Context, app *FastAPI) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(dashboardSelftestInterval):
		}
		if app.State.Bool("auth_required") {
			continue
		}
		dashboardSelftestOnce(ctx, app)
	}
}

// Whether a path bound file ticket or, on the query token paths, the session token authorizes the request
func hasValidQueryToken(req *Request, path string) bool {
	if redeemFileTicket(req) {
		return true
	}
	if !queryTokenAPIPaths[path] {
		return false
	}
	token := req.QueryParams().Get("token")
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(SessionToken())) == 1
}

// Requires the session token on every API route outside the public list unless another seam already authenticated it
func authMiddleware(ctx context.Context, req *Request, callNext CallNext) (ASGIResponse, error) {
	path := req.URL().Path()
	if !req.State().Bool("token_authenticated") && req.State().Get("native_http_principal") == nil &&
		!req.App().State.Bool("auth_required") && strings.HasPrefix(path, "/api/") && !PublicAPIPaths[path] &&
		!strings.HasPrefix(path, "/api/mcp/oauth/callback/") && !hasValidSessionToken(req) && !hasValidQueryToken(req, path) {
		return MustJSONResponse(map[string]any{"detail": "Unauthorized"}, 401), nil
	}
	return callNext(ctx, req)
}
