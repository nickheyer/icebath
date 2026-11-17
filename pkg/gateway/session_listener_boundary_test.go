package gateway

import (
	"testing"
)

func TestExplicitNonLoopbackBindRequiresExactMatch(t *testing.T) {
	if !IsAcceptedHost("my-server.corp.net", "my-server.corp.net") ||
		!IsAcceptedHost("my-server.corp.net:9119", "my-server.corp.net") {
		t.Fatal("exact host refused")
	}
	if IsAcceptedHost("evil.example", "my-server.corp.net") || IsAcceptedHost("localhost", "my-server.corp.net") {
		t.Fatal("foreign host accepted")
	}
}

func TestLoopbackBindAcceptsOnlyLoopbackAliases(t *testing.T) {
	for _, host := range []string{"127.0.0.1:4100", "localhost:4100", "[::1]:4100", "LOCALHOST"} {
		if !IsAcceptedHost(host, "127.0.0.1") {
			t.Fatalf("loopback alias %q refused", host)
		}
	}
	for _, host := range []string{"10.0.0.5:4100", "evil.example", "localhost.evil.test"} {
		if IsAcceptedHost(host, "127.0.0.1") {
			t.Fatalf("foreign host %q accepted", host)
		}
	}
}

func TestMalformedHostAuthoritiesFailClosed(t *testing.T) {
	for _, malformed := range []string{"http://localhost:9443", "localhost:", "localhost:notaport", "[::1].evil.test",
		"[::1]:notaport", "[localhost]"} {
		if IsAcceptedHost(malformed, "127.0.0.1") {
			t.Fatalf("malformed %q accepted", malformed)
		}
	}
}

type boundaryPeerFake struct {
	headers map[string]string
	peer    string
}

func (p boundaryPeerFake) BoundaryHeader(name string) string { return p.headers[name] }
func (p boundaryPeerFake) BoundaryClient() (string, bool)    { return p.peer, p.peer != "" }

func TestSessionListenerHostAndPeerChecks(t *testing.T) {
	state := &ListenerState{BoundHost: "127.0.0.1"}
	for _, c := range []struct {
		host, peer         string
		hostOK, peerOK     bool
		hostWant, peerWant string
	}{
		{"127.0.0.1:4100", "127.0.0.1", true, true, "", ""},
		{"localhost:4100", "::1", true, true, "", ""},
		{"evil.example:4100", "127.0.0.1", false, true, "host_mismatch host=evil.example:4100 bound=127.0.0.1", ""},
		{"", "10.0.0.5", false, false, "host_mismatch host=? bound=127.0.0.1", "peer_not_loopback peer=10.0.0.5 bound=127.0.0.1"},
		{"127.0.0.1:4100", "", true, false, "", "missing_or_empty_peer bound=127.0.0.1"},
	} {
		sock := boundaryPeerFake{headers: map[string]string{"host": c.host}, peer: c.peer}
		if got := WSHostReason(sock, state); got != c.hostWant || (got == "") != c.hostOK {
			t.Errorf("host %q reason %q", c.host, got)
		}
		if got := WSClientReason(sock, state); got != c.peerWant || (got == "") != c.peerOK {
			t.Errorf("peer %q reason %q", c.peer, got)
		}
	}
}

func TestGatewayTicketNeedsTheCanonicalProtocolAndOneTicket(t *testing.T) {
	for _, c := range []struct {
		header, ticket, outcome string
	}{
		{"", "", "none"},
		{CanonicalGatewayProtocol, "", "none"},
		{CanonicalGatewayProtocol + ", " + GatewayWSTicketPrefix + "abc", "abc", "ok"},
		{GatewayWSTicketPrefix + "abc", "", "invalid"},
		{CanonicalGatewayProtocol + "," + GatewayWSTicketPrefix + "a," + GatewayWSTicketPrefix + "b", "", "invalid"},
		{CanonicalGatewayProtocol + "," + GatewayWSTicketPrefix, "", "invalid"},
	} {
		ticket, outcome := GatewayWSTicketFromSubprotocol(boundaryPeerFake{headers: map[string]string{"sec-websocket-protocol": c.header}})
		if ticket != c.ticket || outcome != c.outcome {
			t.Errorf("%q gave %q %q", c.header, ticket, outcome)
		}
	}
}
