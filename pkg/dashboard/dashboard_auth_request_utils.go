package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Post login targets that would loop through the auth flow or land on raw JSON
var nextDenyPrefixes = []string{"/login", "/auth/", "/api/auth/"}

// Listener boundary state the gateway checks read, taken from the serving app's state
func listenerStateOf(state *State) *gateway.ListenerState {
	if state == nil {
		return nil
	}
	ls := &gateway.ListenerState{AuthRequired: state.Bool("auth_required"), BoundHost: state.String("bound_host")}
	if port, ok := state.Get("bound_port").(int); ok {
		ls.BoundPort = port
	}
	if hosts, ok := state.Get("trusted_public_hosts").(map[string]bool); ok {
		ls.TrustedPublicHosts = hosts
	}
	if origins, ok := state.Get("trusted_public_origins").(map[gateway.LoopbackOrigin]bool); ok {
		ls.TrustedPublicOrigins = origins
	}
	if own, ok := state.Get("listener_public_url").(string); ok {
		ls.ListenerPublicURL = &own
	}
	return ls
}

// Peer address of the connection for rate limits and audit, never a client supplied forwarded header
func clientIP(conn *HTTPConnection) string {
	if c := conn.Client(); c != nil {
		return c.Host
	}
	return ""
}

// Bearer token of the authorization header with a case insensitive scheme, empty when absent
func extractBearer(conn *HTTPConnection) string {
	scheme, token, found := strings.Cut(conn.Headers().Get("authorization"), " ")
	if found && strings.ToLower(pystr.Strip(scheme)) == "bearer" {
		return pystr.Strip(token)
	}
	return ""
}

// Scheme, host and effective port of one serialized HTTP origin
type httpOrigin struct {
	scheme, host string
	port         int
}

// Parses one serialized HTTP origin, false for an opaque or URL like value
func parseHTTPOrigin(value string) (httpOrigin, bool) {
	if value == "" {
		return httpOrigin{}, false
	}
	for _, c := range value {
		if c <= 32 || c >= 127 || strings.ContainsRune("\\,?#%", c) {
			return httpOrigin{}, false
		}
	}
	parsed, err := pyurl.Split(value)
	if err != nil {
		return httpOrigin{}, false
	}
	userinfo := strings.LastIndex(parsed.Netloc, "@") >= 0
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || userinfo ||
		parsed.Path != "" || strings.HasSuffix(parsed.Netloc, ":") {
		return httpOrigin{}, false
	}
	port, hasPort, err := parsed.Port()
	if err != nil {
		return httpOrigin{}, false
	}
	if !hasPort {
		port = 80
		if parsed.Scheme == "https" {
			port = 443
		}
	}
	return httpOrigin{parsed.Scheme, parsed.Hostname(), port}, true
}

// Whether a cookie write comes from the listener's own origin, by Fetch Metadata first and the public URL otherwise
func cookieOriginIsAllowed(ctx context.Context, req *Request) bool {
	appState := req.App().State
	if bound := appState.String("bound_host"); bound != "" {
		trusted, _ := appState.Get("trusted_public_hosts").(map[string]bool)
		if !gateway.IsAcceptedHost(req.Headers().Get("host"), bound, trusted) {
			return false
		}
	}
	origins := req.Headers().GetList("origin")
	fetchSites := req.Headers().GetList("sec-fetch-site")
	if len(origins) == 0 {
		return len(fetchSites) == 0 || slices.Equal(fetchSites, []string{"none"})
	}
	if len(origins) != 1 {
		return false
	}
	origin, ok := parseHTTPOrigin(origins[0])
	if !ok {
		return false
	}
	if len(fetchSites) > 0 {
		return slices.Equal(fetchSites, []string{"same-origin"})
	}
	targetURL := gateway.ResolvePublicURL(ctx, listenerStateOf(appState))
	if targetURL == "" {
		targetURL = req.URL().String()
	}
	target, err := pyurl.Split(targetURL)
	if err != nil {
		return false
	}
	want, ok := parseHTTPOrigin(target.Scheme + "://" + target.Netloc)
	return ok && origin == want
}

// Whether a post login target is a same origin path outside the auth flow and the API
func isSafeNextPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return false
	}
	for _, p := range nextDenyPrefixes {
		if path == p || strings.HasPrefix(path, p) {
			return false
		}
	}
	return !(path == "/api" || strings.HasPrefix(path, "/api/"))
}

// Cookie Max Age of the access token, seconds to expiry floored at sixty
func accessTokenMaxAge(session *gateway.DashboardSession) int64 {
	return max(60, session.ExpiresAt-time.Now().Unix())
}

// 503 for a transient identity provider outage, never a forced re login
func unreachableResponse(providerName string) *Response {
	return MustJSONResponse(map[string]any{"detail": fmt.Sprintf("Auth provider %s unreachable", pyrepr.Repr(providerName))}, 503)
}

// Hooks of a session provider scan, a swallowed error rejecting only that candidate
type providerScan struct {
	phase         string
	log           *slog.Logger
	swallow       func(error) bool
	onSwallow     func(gateway.DashboardAuthProvider)
	onUnreachable func(gateway.DashboardAuthProvider)
}

// First non nil result of call across the session providers, the hinted one first, a ProviderError naming the first unreachable
func scanSessionProviders[T any](ctx context.Context, providerHint string, call func(gateway.DashboardAuthProvider) (*T, error), scan providerScan) (*T, error) {
	providers := gateway.ListSessionDashboardAuthProviders(ctx)
	if providerHint != "" {
		slices.SortStableFunc(providers, func(a, b gateway.DashboardAuthProvider) int {
			ka, kb := a.Name() != providerHint, b.Name() != providerHint
			switch {
			case ka == kb:
				return 0
			case !ka:
				return -1
			}
			return 1
		})
	}
	unreachable := ""
	found := false
	for _, provider := range providers {
		result, err := call(provider)
		if err != nil {
			if scan.swallow != nil && scan.swallow(err) {
				if scan.onSwallow != nil {
					scan.onSwallow(provider)
				}
				continue
			}
			var perr *gateway.ProviderError
			if !errors.As(err, &perr) {
				return nil, err
			}
			scan.log.Warn(fmt.Sprintf("dashboard-auth: provider %s unreachable during %s: %s", pyrepr.Repr(provider.Name()), scan.phase, err))
			if scan.onUnreachable != nil {
				scan.onUnreachable(provider)
			}
			if !found {
				unreachable, found = provider.Name(), true
			}
			continue
		}
		if result != nil {
			return result, nil
		}
	}
	if found {
		return nil, &gateway.ProviderError{Msg: unreachable}
	}
	return nil, nil
}
