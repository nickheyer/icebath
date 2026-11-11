package gateway

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Host values a loopback bind accepts, the DNS rebinding defence of GHSA-ppp5-vxwm-4cf7
var LoopbackHostValues = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// Peers treated as loopback, the Starlette test client peer name included
var loopbackPeerHosts = map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true, "testclient": true}

// Largest desktop attachment frame a dashboard WebSocket accepts
const DesktopAttachmentWSMaxBytes = 384 * 1024 * 1024

// Scheme, host and port of an http or https loopback origin with its default port filled in
type LoopbackOrigin struct {
	Scheme, Host string
	Port         int
}

// Listener facts the boundary checks read, the app.state attributes of a dashboard or session listener
type ListenerState struct {
	AuthRequired         bool
	BoundHost            string
	BoundPort            int
	TrustedPublicHosts   map[string]bool
	TrustedPublicOrigins map[LoopbackOrigin]bool
	ListenerPublicURL    *string
}

// Handshake view the WebSocket boundary checks read, its headers and peer address
type BoundaryPeer interface {
	BoundaryHeader(name string) string
	BoundaryClient() (string, bool)
}

// Exact hostname of dashboard.public_url as the trusted public host set
func DashboardPublicHosts(ctx context.Context) map[string]bool {
	return PublicURLHosts(ResolvePublicURL(ctx, nil))
}

// Exact hostname a public URL declares, empty when unset or malformed
func PublicURLHosts(publicURL string) map[string]bool {
	out := map[string]bool{}
	if publicURL == "" {
		return out
	}
	parsed, err := pyurl.Parse(publicURL)
	if err != nil {
		return out
	}
	if h := parsed.Hostname(); h != "" {
		out[pystr.Lower(h)] = true
	}
	return out
}

// Lowered hostname of a valid Host authority, empty for ambiguous ports, bad brackets or URL syntax
func HostHeaderHostname(hostHeader string) string {
	value := pystr.Strip(hostHeader)
	if value == "" || strings.Contains(value, "://") || strings.ContainsAny(value, "\"'<> \n\r\t/?#@") {
		return ""
	}
	if strings.HasPrefix(value, "[") {
		closeAt := strings.Index(value, "]")
		if closeAt == -1 {
			return ""
		}
		hostname := value[1:closeAt]
		if !strings.Contains(hostname, ":") {
			return ""
		}
		suffix := value[closeAt+1:]
		if suffix != "" && !(len(suffix) > 1 && suffix[0] == ':' && pystr.All(suffix[1:], pystr.IsDecimal)) {
			return ""
		}
		return pystr.Lower(hostname)
	}
	if strings.Count(value, ":") > 1 {
		return ""
	}
	if i := strings.LastIndex(value, ":"); i >= 0 {
		hostname, port := value[:i], value[i+1:]
		if hostname == "" || !pystr.StrIsDigit(port) {
			return ""
		}
		return pystr.Lower(hostname)
	}
	return pystr.Lower(value)
}

// Whether the Host header targets the bound interface, a loopback alias of it, or a declared public host
func IsAcceptedHost(hostHeader, boundHost string, trustedPublicHosts map[string]bool) bool {
	hostOnly := HostHeaderHostname(hostHeader)
	if hostOnly == "" {
		return false
	}
	if trustedPublicHosts[hostOnly] || boundHost == "0.0.0.0" || boundHost == "::" {
		return true
	}
	boundLC := pystr.Lower(boundHost)
	if LoopbackHostValues[boundLC] {
		return LoopbackHostValues[hostOnly]
	}
	return hostOnly == boundLC
}

// Whether the auth gate must be active, true for every non-loopback bind
func ShouldRequireAuth(host string) bool {
	return !LoopbackHostValues[host]
}

// Whether a non-loopback public URL host engages the gate even on a loopback bind
func PublicHostsEngageGate(trustedPublicHosts map[string]bool) bool {
	for h := range trustedPublicHosts {
		if !LoopbackHostValues[h] {
			return true
		}
	}
	return false
}

// Whether a desktop owned loopback backend with an operator minted credential skips the public URL gate
func DesktopLoopbackAuthExempt(host, sshSessionToken, sshOwnerNonce string) bool {
	return LoopbackHostValues[host] && os.Getenv("ICEBATH_DESKTOP") == "1" &&
		(os.Getenv("ICEBATH_DASHBOARD_SESSION_TOKEN") != "" || sshSessionToken != "" || sshOwnerNonce != "")
}

// Auth gate decision of a listener with no SSH credentials, the gateway session listener
func ListenerAuthRequired(host string, trustedPublicHosts map[string]bool) bool {
	if DesktopLoopbackAuthExempt(host, "", "") {
		return ShouldRequireAuth(host)
	}
	return ShouldRequireAuth(host) || PublicHostsEngageGate(trustedPublicHosts)
}

// Ticket and outcome none, invalid or ok from an unambiguous gateway WebSocket protocol set
func GatewayWSTicketFromSubprotocol(ws BoundaryPeer) (string, string) {
	var protocols, tickets []string
	for _, value := range strings.Split(ws.BoundaryHeader("sec-websocket-protocol"), ",") {
		if v := pystr.Strip(value); v != "" {
			protocols = append(protocols, v)
			if strings.HasPrefix(v, GatewayWSTicketPrefix) {
				tickets = append(tickets, v)
			}
		}
	}
	if len(tickets) == 0 {
		return "", "none"
	}
	hasProtocol := false
	for _, p := range protocols {
		hasProtocol = hasProtocol || p == CanonicalGatewayProtocol
	}
	if !hasProtocol || len(tickets) != 1 {
		return "", "invalid"
	}
	if ticket := tickets[0][len(GatewayWSTicketPrefix):]; ticket != "" {
		return ticket, "ok"
	}
	return "", "invalid"
}

// Rejection token for the WebSocket peer address under the listener state, empty when allowed
func WSClientReason(ws BoundaryPeer, state *ListenerState) string {
	if state.AuthRequired {
		return ""
	}
	boundHost := pystr.Lower(pystr.Strip(state.BoundHost))
	if boundHost != "" && !loopbackPeerHosts[boundHost] {
		return ""
	}
	shown := boundHost
	if shown == "" {
		shown = "?"
	}
	clientHost, _ := ws.BoundaryClient()
	if clientHost == "" {
		return "missing_or_empty_peer bound=" + shown
	}
	if loopbackPeerHosts[clientHost] {
		return ""
	}
	return fmt.Sprintf("peer_not_loopback peer=%s bound=%s", clientHost, shown)
}

// Scheme, host and filled in port of an http or https loopback origin, false otherwise
func loopbackOriginOf(origin string) (LoopbackOrigin, bool) {
	parsed, err := pyurl.Parse(origin)
	if err != nil {
		return LoopbackOrigin{}, false
	}
	port, present, err := parsed.Port()
	if err != nil {
		return LoopbackOrigin{}, false
	}
	if !present || port == 0 {
		port = map[string]int{"http": 80, "https": 443}[parsed.Scheme]
	}
	hostname := parsed.Hostname()
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || !LoopbackHostValues[hostname] {
		return LoopbackOrigin{}, false
	}
	return LoopbackOrigin{Scheme: parsed.Scheme, Host: hostname, Port: port}, true
}

// Loopback origin a public URL declares as the trusted origin set, empty for unset, malformed or remote URLs
func PublicURLOrigins(publicURL string) map[LoopbackOrigin]bool {
	out := map[LoopbackOrigin]bool{}
	if publicURL == "" {
		return out
	}
	if origin, ok := loopbackOriginOf(publicURL); ok {
		out[origin] = true
	}
	return out
}

// Trusted loopback origin set of dashboard.public_url
func DashboardPublicOrigins(ctx context.Context) map[LoopbackOrigin]bool {
	return PublicURLOrigins(ResolvePublicURL(ctx, nil))
}

// Whether a loopback origin is on the bound port, the configured public URL, or the desktop dev renderer
func IsTrustedLocalOrigin(origin string, state *ListenerState) bool {
	loopback, ok := loopbackOriginOf(origin)
	if !ok {
		return false
	}
	if state.BoundPort != 0 && loopback.Port == state.BoundPort {
		return true
	}
	if state.TrustedPublicOrigins[loopback] {
		return true
	}
	dev, ok := loopbackOriginOf(os.Getenv("ICEBATH_DESKTOP_DEV_SERVER"))
	return ok && dev == loopback
}

// Host or origin mismatch token of a WebSocket handshake under the listener state, empty when allowed
func WSHostOriginReason(ws BoundaryPeer, state *ListenerState) string {
	boundHost := state.BoundHost
	if boundHost == "" {
		return ""
	}
	hostHeader := ws.BoundaryHeader("host")
	if !IsAcceptedHost(hostHeader, boundHost, state.TrustedPublicHosts) {
		shown := hostHeader
		if shown == "" {
			shown = "?"
		}
		return fmt.Sprintf("host_mismatch host=%s bound=%s", shown, boundHost)
	}
	origin := ws.BoundaryHeader("origin")
	if origin == "" {
		return ""
	}
	parsed, err := pyurl.Parse(origin)
	if err == nil && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	mismatch := fmt.Sprintf("origin_mismatch origin=%s bound=%s", origin, boundHost)
	if err != nil || parsed.Netloc == "" || !IsAcceptedHost(parsed.Netloc, boundHost, state.TrustedPublicHosts) {
		return mismatch
	}
	if LoopbackHostValues[parsed.Hostname()] && pystr.Lower(parsed.Netloc) != pystr.Lower(pystr.Strip(hostHeader)) &&
		!IsTrustedLocalOrigin(origin, state) {
		return mismatch
	}
	return ""
}
