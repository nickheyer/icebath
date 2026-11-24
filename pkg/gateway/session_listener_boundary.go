package gateway

import (
	"fmt"
	"strings"

	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Host values a loopback bind accepts, the DNS rebinding defence of GHSA-ppp5-vxwm-4cf7
var LoopbackHostValues = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// Peer hosts treated as loopback
var loopbackPeerHosts = map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true}

// Bound host of the session listener the boundary checks read
type ListenerState struct {
	BoundHost string
}

// Handshake view the WebSocket boundary checks read, its headers and peer address
type BoundaryPeer interface {
	BoundaryHeader(name string) string
	BoundaryClient() (string, bool)
}

// Lowered hostname of a valid Host authority, empty for ambiguous ports, bad brackets or URL syntax
func HostHeaderHostname(hostHeader string) string {
	value := strings.TrimSpace(hostHeader)
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
		if suffix != "" && !(len(suffix) > 1 && suffix[0] == ':' && textutil.IsDigits(suffix[1:])) {
			return ""
		}
		return strings.ToLower(hostname)
	}
	if strings.Count(value, ":") > 1 {
		return ""
	}
	if i := strings.LastIndex(value, ":"); i >= 0 {
		hostname, port := value[:i], value[i+1:]
		if hostname == "" || !textutil.IsDigits(port) {
			return ""
		}
		return strings.ToLower(hostname)
	}
	return strings.ToLower(value)
}

// Whether the Host header targets the bound interface or a loopback alias of a loopback bind
func IsAcceptedHost(hostHeader, boundHost string) bool {
	hostOnly := HostHeaderHostname(hostHeader)
	if hostOnly == "" {
		return false
	}
	boundLC := strings.ToLower(boundHost)
	if LoopbackHostValues[boundLC] {
		return LoopbackHostValues[hostOnly]
	}
	return hostOnly == boundLC
}

// Ticket and outcome none, invalid or ok from an unambiguous gateway WebSocket protocol set
func GatewayWSTicketFromSubprotocol(ws BoundaryPeer) (string, string) {
	var protocols, tickets []string
	for _, value := range strings.Split(ws.BoundaryHeader("sec-websocket-protocol"), ",") {
		if v := strings.TrimSpace(value); v != "" {
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
	boundHost := strings.ToLower(strings.TrimSpace(state.BoundHost))
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

// Host mismatch token of a WebSocket handshake under the listener state, empty when allowed
func WSHostReason(ws BoundaryPeer, state *ListenerState) string {
	boundHost := state.BoundHost
	if boundHost == "" {
		return ""
	}
	hostHeader := ws.BoundaryHeader("host")
	if IsAcceptedHost(hostHeader, boundHost) {
		return ""
	}
	shown := hostHeader
	if shown == "" {
		shown = "?"
	}
	return fmt.Sprintf("host_mismatch host=%s bound=%s", shown, boundHost)
}
