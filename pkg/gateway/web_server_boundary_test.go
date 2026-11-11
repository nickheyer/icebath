package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const haIngressDashboardPrefix = "/api/hassio_ingress/8AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEf/dashboard"

// Count of captured warning lines that mention X-Forwarded-Prefix
func prefixWarnings(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "X-Forwarded-Prefix") {
			out = append(out, line)
		}
	}
	return out
}

func TestHomeAssistantIngressPrefixWithSubpathIsAccepted(t *testing.T) {
	ResetDashboardPrefixWarnings()
	if len(haIngressDashboardPrefix) <= 64 {
		t.Fatal("fixture prefix must exceed 64 characters")
	}
	logs := captureLogs(t)
	if got := NormalisePrefix(haIngressDashboardPrefix); got != haIngressDashboardPrefix {
		t.Fatalf("got %q", got)
	}
	if w := prefixWarnings(logs.String()); len(w) != 0 {
		t.Fatalf("unexpected warnings %v", w)
	}
}

func TestOverlongPrefixIsRejectedWithDeduplicatedWarning(t *testing.T) {
	ResetDashboardPrefixWarnings()
	tooLong := "/" + strings.Repeat("a", 257)
	logs := captureLogs(t)
	for range 3 {
		if got := NormalisePrefix(tooLong); got != "" {
			t.Fatalf("got %q", got)
		}
	}
	w := prefixWarnings(logs.String())
	if len(w) != 1 || !strings.Contains(w[0], "longer than 256 characters") {
		t.Fatalf("warnings %v", w)
	}
}

func TestZeroZeroBindAcceptsAnything(t *testing.T) {
	for _, host := range []string{"10.0.0.5", "evil.example", "my-server.corp.net"} {
		if !IsAcceptedHost(host, "0.0.0.0", nil) || !IsAcceptedHost(host+":9119", "0.0.0.0", nil) {
			t.Fatalf("0.0.0.0 bind refused %q", host)
		}
	}
}

func TestExplicitNonLoopbackBindRequiresExactMatch(t *testing.T) {
	if !IsAcceptedHost("my-server.corp.net", "my-server.corp.net", nil) ||
		!IsAcceptedHost("my-server.corp.net:9119", "my-server.corp.net", nil) {
		t.Fatal("exact host refused")
	}
	if IsAcceptedHost("evil.example", "my-server.corp.net", nil) || IsAcceptedHost("localhost", "my-server.corp.net", nil) {
		t.Fatal("foreign host accepted")
	}
}

func TestTrustedPublicHostIsExactMatchOnly(t *testing.T) {
	trusted := map[string]bool{"dashboard.example.test": true}
	if !IsAcceptedHost("dashboard.example.test:9443", "127.0.0.1", trusted) {
		t.Fatal("trusted host refused")
	}
	if IsAcceptedHost("dashboard.example.test.evil.test", "127.0.0.1", trusted) || IsAcceptedHost("evil.test", "127.0.0.1", trusted) {
		t.Fatal("untrusted host accepted")
	}
}

func TestMalformedHostAuthoritiesFailClosed(t *testing.T) {
	trusted := map[string]bool{"dashboard.example.test": true}
	for _, malformed := range []string{"http://dashboard.example.test:9443", "dashboard.example.test:",
		"dashboard.example.test:notaport", "[::1].evil.test", "[::1]:notaport", "[localhost]"} {
		if IsAcceptedHost(malformed, "127.0.0.1", trusted) {
			t.Fatalf("malformed %q accepted", malformed)
		}
	}
}

type boundaryPeerFake struct{ headers map[string]string }

func (p boundaryPeerFake) BoundaryHeader(name string) string { return p.headers[name] }
func (p boundaryPeerFake) BoundaryClient() (string, bool)    { return "", false }

func TestAllInterfacesBindsKeepTheLoopbackPortCheck(t *testing.T) {
	for _, bound := range []string{"0.0.0.0", "::"} {
		for _, c := range []struct {
			host, origin string
			allowed      bool
		}{
			{"localhost:9119", "http://localhost:9119", true},
			{"192.168.1.5:9119", "http://192.168.1.5:9119", true},
			{"localhost:9119", "http://localhost:3000", false},
		} {
			state := &ListenerState{BoundHost: bound, BoundPort: 9119, AuthRequired: true}
			sock := boundaryPeerFake{map[string]string{"host": c.host, "origin": c.origin}}
			if got := WSHostOriginReason(sock, state) == ""; got != c.allowed {
				t.Errorf("%s %s %s allowed=%v", bound, c.host, c.origin, got)
			}
		}
	}
}

func TestTheWebappAndItsSignInGateAgreeOnWhichHostsStayOnThisMachine(t *testing.T) {
	for _, c := range []struct{ host, bindHost string }{
		{"127.0.0.1", "127.0.0.1"}, {"'::1'", "::1"}, {"localhost", "localhost"}, {"LocalHost", "localhost"},
		{"'0:0:0:0:0:0:0:1'", "::1"}, {"'0000::0001'", "::1"}, {"0.0.0.0", "0.0.0.0"},
		{"'0:0:0:0:0:0:0:0'", "::"}, {"127.0.0.2", ""}, {"127.255.255.254", ""}, {"'::ffff:127.0.0.1'", ""},
	} {
		h := filepath.Join(t.TempDir(), "home")
		os.MkdirAll(h, 0o755)
		os.WriteFile(filepath.Join(h, "config.yaml"), []byte("webapp:\n  enabled: true\n  host: "+c.host+"\n"), 0o644)
		settings := WebappSettingsOf(context.Background(), h)
		host, port := settings.Bind()
		if c.bindHost == "" {
			if host != "127.0.0.1" || port != 0 || !strings.Contains(settings.Reason, "webapp.host must be 127.0.0.1, ::1, localhost, 0.0.0.0 or ::") {
				t.Errorf("%s: bind %s:%d reason %q", c.host, host, port, settings.Reason)
			}
			continue
		}
		if host != c.bindHost || port != webappDefaultPort {
			t.Errorf("%s: bind %s:%d", c.host, host, port)
		}
		if ShouldRequireAuth(settings.Host) != webappIsWildcard(settings.Host) {
			t.Errorf("%s: gate and wildcard disagree", c.host)
		}
	}
}
