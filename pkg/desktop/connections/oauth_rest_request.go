package connections

import (
	"context"
	"errors"
	"strings"
)

// OauthRestRequestDeps are the bearer and cookie halves of an OAuth gated REST call
type OauthRestRequestDeps[T any] struct {
	EnsureNativeAccessToken func(ctx context.Context, baseURL string, o NativeAccessTokenOptions) (*string, error)
	RequestWithBearer       func(accessToken string) (T, error)
	RequestWithCookie       func() (T, error)
}

// Cookie retry after a failed native refresh, the native failure surfacing when the cookie is rejected too
func cookieFallback[T any](request func() (T, error), nativeErr error) (T, error) {
	var zero T
	if isNativeAuthChanged(nativeErr) {
		return zero, nativeErr
	}
	out, err := request()
	if err == nil {
		return out, nil
	}
	if IsGatewayAuthRejection(err) {
		return zero, nativeErr
	}
	return zero, err
}

// Marks a rejected native bearer as the stale app token the ticket failure explains
func markStaleAppToken(err error) error {
	var gateway *GatewayError
	if errors.As(err, &gateway) {
		gateway.AppTokenRejected = true
		return err
	}
	return &GatewayError{Message: err.Error(), Status: ReadStatusCode(err), AppTokenRejected: true, Cause: err}
}

// RequestWithOauthFallback prefers the native bearer and falls back to the cookie partition
func RequestWithOauthFallback[T any](ctx context.Context, baseURL string, deps OauthRestRequestDeps[T]) (T, error) {
	token, err := deps.EnsureNativeAccessToken(ctx, baseURL, NativeAccessTokenOptions{})
	if err != nil {
		return cookieFallback(deps.RequestWithCookie, err)
	}
	if token != nil && *token != "" {
		return deps.RequestWithBearer(*token)
	}
	return deps.RequestWithCookie()
}

type interactiveLoginKey struct{}

// WithoutInteractiveOauthLogin scopes work so no cookie retry may show a login window
func WithoutInteractiveOauthLogin(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveLoginKey{}, false)
}

// CanShowInteractiveOauthLogin reports a scope outside WithoutInteractiveOauthLogin
func CanShowInteractiveOauthLogin(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	allowed, ok := ctx.Value(interactiveLoginKey{}).(bool)
	return !ok || allowed
}

// RetryCookie401WithLogin replays a gate refused cookie request once after a silent login
func RetryCookie401WithLogin[T any](ctx context.Context, err error, method string, replayOn401 bool, clearCookies func(), login func() error, retry func() (T, error)) (T, error) {
	var zero T
	if !CanShowInteractiveOauthLogin(ctx) || !ShouldReplayAfterCookie401(err, method, replayOn401) {
		return zero, err
	}
	clearCookies()
	if login() != nil {
		return zero, err
	}
	return retry()
}

// ShouldReplayAfterCookie401 reports the auth gate's own 401 refusal of an idempotent or vouched request
func ShouldReplayAfterCookie401(err error, method string, replayOn401 bool) bool {
	if ReadStatusCode(err) != 401 {
		return false
	}
	body := ReadJSONErrorBody(err)
	if body == nil {
		return false
	}
	code := body["error"]
	reason, _ := body["reason"].(string)
	if (code != "unauthenticated" && code != "session_expired") || reason == "" {
		return false
	}
	verb := strings.ToUpper(orGet(method))
	return verb == "GET" || verb == "HEAD" || replayOn401
}

// MintGatewayWsTicketDeps are the token, bearer request and cookie request a ticket mint runs over
type MintGatewayWsTicketDeps struct {
	EnsureNativeAccessToken  func(ctx context.Context, baseURL string, o NativeAccessTokenOptions) (*string, error)
	FetchJSON                func(ctx context.Context, url string, o JSONOptions) (any, error)
	FetchJSONViaOauthSession func(ctx context.Context, url string, o JSONOptions) (any, error)
}

// MintOauthGatewayWsTicket mints a one use socket ticket, native bearer first with one forced rotation, cookie second
func MintOauthGatewayWsTicket(ctx context.Context, baseURL string, deps MintGatewayWsTicketDeps, headers map[string]string) (string, error) {
	url := baseURL + "/api/auth/ws-ticket"
	options := JSONOptions{Method: "POST", TimeoutMs: 8_000, Headers: headers, ReplayOn401: true}
	ticketFrom := func(body any, err error) (string, error) {
		if err != nil {
			return "", err
		}
		ticket, ok := jsObject(body)["ticket"].(string)
		if !ok || ticket == "" {
			return "", errors.New("Gateway did not return a WS ticket.")
		}
		return ticket, nil
	}
	mintWithBearer := func(bearer string) (string, error) {
		o := options
		o.Bearer = bearer
		return ticketFrom(deps.FetchJSON(ctx, url, o))
	}
	requestWithCookie := func() (string, error) { return ticketFrom(deps.FetchJSONViaOauthSession(ctx, url, options)) }
	return RequestWithOauthFallback(ctx, baseURL, OauthRestRequestDeps[string]{
		EnsureNativeAccessToken: deps.EnsureNativeAccessToken,
		RequestWithCookie:       requestWithCookie,
		RequestWithBearer: func(nativeAt string) (string, error) {
			ticket, err := func() (string, error) {
				ticket, err := mintWithBearer(nativeAt)
				if err == nil {
					return ticket, nil
				}
				if !ShouldRotateNativeTokenAfterRejection(err) {
					return "", err
				}
				rotated, rotateErr := deps.EnsureNativeAccessToken(ctx, baseURL, NativeAccessTokenOptions{ForceRefresh: true, RejectedAccessToken: nativeAt})
				if rotateErr != nil {
					return "", rotateErr
				}
				if rotated == nil || *rotated == "" || *rotated == nativeAt {
					return "", err
				}
				return mintWithBearer(*rotated)
			}()
			if err == nil {
				return ticket, nil
			}
			if ReadStatusCode(err) == 401 {
				err = markStaleAppToken(err)
			}
			return cookieFallback(requestWithCookie, err)
		},
	})
}
