package dashboard

import "context"

// Refuses plugin api websocket upgrades the origin gate, native grant or scope consent bars before the route runs
func PluginAPIWebSocketGate(app ASGIApp) ASGIApp {
	return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		name := ""
		if scope.Type == "websocket" {
			name = pluginAPIName(scope)
		}
		if name == "" {
			return app(ctx, scope, receive, send)
		}
		if !wsRequestIsAllowed(NewWebSocket(scope, receive, send)) {
			return send(ctx, Message{Type: "websocket.close", Code: 4403})
		}
		sctx, admitted, err := nativeWSScope(ctx, scope)
		if err != nil {
			return err
		}
		sctx, release, err := pluginRequestScope(sctx, scope)
		if err != nil {
			return err
		}
		if admitted && !pluginAPIRequestRefused(sctx, name) {
			refused, err := refuseSecondaryPluginSocket(sctx, scope, receive, send, name)
			if err != nil || refused {
				release()
				return err
			}
			defer release()
			return app(sctx, scope, receive, send)
		}
		release()
		return send(ctx, Message{Type: "websocket.close", Code: 1008})
	}
}

// Whether a plugin socket native grant was admitted with its home and secret scope bound, true without the header
func nativeWSScope(ctx context.Context, scope *Scope) (context.Context, bool, error) {
	conn := &HTTPConnection{Scope: scope}
	if !conn.Headers().Contains(nativeHTTPHeader) {
		return ctx, true, nil
	}
	grant, err := redeemNativeWS(conn)
	if err != nil {
		return ctx, false, err
	}
	if grant == nil {
		return ctx, false, nil
	}
	conn.State().Set("native_http_principal", grant)
	sctx, _, err := configProfileScope(nativeProfileScope(ctx, conn), "")
	if err != nil {
		return ctx, false, err
	}
	return sctx, true, nil
}
