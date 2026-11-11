package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func tokenAuthLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.dashboard_auth.token_auth")
}

// One provider's verdict on a token, a panic or error reported as the error
func verifyTokenGuarded(ctx context.Context, p gateway.DashboardAuthProvider, token string) (principal *gateway.TokenPrincipal, err error) {
	defer func() {
		if r := recover(); r != nil {
			principal, err = nil, fmt.Errorf("%v", r)
		}
	}()
	return gateway.DashboardAuthVerifyToken(ctx, p, token)
}

// Principal of the request's bearer token across the token providers, else the first unreachable provider name
func authenticateToken(ctx context.Context, conn *HTTPConnection) (*gateway.TokenPrincipal, string) {
	token := extractBearer(conn)
	if token == "" {
		return nil, ""
	}
	unreachable := ""
	for _, provider := range gateway.ListTokenDashboardAuthProviders(ctx) {
		principal, err := verifyTokenGuarded(ctx, provider, token)
		var perr *gateway.ProviderError
		if errors.As(err, &perr) {
			tokenAuthLog().Warn(fmt.Sprintf("dashboard-auth: token provider %s unreachable during verify: %s", pyrepr.Repr(provider.Name()), err))
			if unreachable == "" {
				unreachable = provider.Name()
			}
			continue
		}
		if err != nil {
			tokenAuthLog().Warn(fmt.Sprintf("dashboard-auth: token provider %s raised during verify: %s", pyrepr.Repr(provider.Name()), err))
			continue
		}
		if principal != nil {
			return principal, ""
		}
	}
	return nil, unreachable
}

// Owns a registered token route, attaching the principal on a valid token, 503 when unreachable and 401 otherwise
func TokenAuthMiddleware(ctx context.Context, req *Request, callNext CallNext) (ASGIResponse, error) {
	path := req.URL().Path()
	if !gateway.IsTokenRoute(path) {
		return callNext(ctx, req)
	}
	conn := &req.HTTPConnection
	principal, unreachable := authenticateToken(ctx, conn)
	if principal != nil {
		req.State().Set("token_principal", principal)
		req.State().Set("token_authenticated", true)
		return callNext(ctx, req)
	}
	if unreachable != "" {
		auditLog(ctx, AuditTokenAuthFailure, "provider", unreachable, "reason", "provider_unreachable", "path", path, "ip", clientIP(conn))
		return unreachableResponse(unreachable), nil
	}
	auditLog(ctx, AuditTokenAuthFailure, "reason", "no_provider_recognises_token", "path", path, "ip", clientIP(conn))
	return MustJSONResponse(pyjson.ObjectOf("error", "unauthenticated", "detail", "Unauthorized"), 401), nil
}
