package gateway

import (
	"context"
	"strings"
	"testing"
)

const relayOpaqueIdentityToken = "Zq8vXw3LmN0pR5tUyB2cD7eF9gH1jK4sA6-_oQ"

// Relay env of the transient recovery suite fronting slack against the fake connector
func relayRecoveryEnv(t *testing.T) *relayFakeConn {
	t.Helper()
	relayCleanEnv(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "slack")
	c := newRelayFakeConn(t)
	c.install(t)
	return c
}

// Sleep recording each requested delay without waiting
func relayRecordingSleep(slept *[]float64) func(context.Context, float64) error {
	return func(_ context.Context, s float64) error {
		*slept = append(*slept, s)
		return nil
	}
}

func relayFloatsEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProvisionRetryPacesTransientFailuresAndThrottlesRefusals(t *testing.T) {
	c := relayRecoveryEnv(t)
	c.provision = []relayFakeScript{relayProxy403(), relayRefusal(403, "enterprise identity lifecycle is not confirmed"), relayRefusal(503, "upstream unavailable")}
	var slept []float64
	var provisioned []bool
	ok, err := RetrySelfProvisionWith(context.Background(), func() { provisioned = append(provisioned, true) }, relayRecordingSleep(&slept))
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	delays, refusal := relayProvisionRetryDelays, relayProvisionRefusalRetry
	if want := []float64{delays[0], delays[1], refusal, delays[len(delays)-1]}; !relayFloatsEqual(slept, want) {
		t.Fatalf("slept %v want %v", slept, want)
	}
	if refusal <= max(delays[0], delays[1], delays[2], delays[3]) {
		t.Fatal("a reasoned refusal must be retried less often than a blip")
	}
	if c.provisionCalls != 4 || len(provisioned) != 1 {
		t.Fatalf("calls %d provisioned %v", c.provisionCalls, provisioned)
	}
}

func TestProvisionFailureLogNamesTheProxyAndNeverTheToken(t *testing.T) {
	c := relayRecoveryEnv(t)
	c.provision = []relayFakeScript{relayProxy403()}
	logs := captureLogs(t)
	if ok, err := SelfProvisionRelay(context.Background()); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	text := logs.String()
	for _, want := range []string{"server=squid/6.10", "x-squid-error=ERR_DNS_FAIL 0", "ERROR: The requested URL could not be retrieved"} {
		if !strings.Contains(text, want) {
			t.Fatalf("log lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "connector returned HTTP 403") || strings.Contains(text, c.identityToken) || len(c.bearersSeen) == 0 {
		t.Fatalf("log %s bearers %v", text, c.bearersSeen)
	}
	for _, b := range c.bearersSeen {
		if strings.Contains(text, b) {
			t.Fatalf("bearer %q logged", b)
		}
	}
}

func TestProvisionFailureLogNeverCarriesTheIdentityTokenAHopEchoed(t *testing.T) {
	for id, spec := range map[string]relayFakeScript{
		"refusal-reason": relayEchoing(401, `{"error":"token {token} rejected"}`),
		"proxy-json":     relayEchoing(403, `{"message":"token {token} rejected"}`),
		"proxy-text":     relayEchoing(502, "bad gateway for token {token} rejected"),
	} {
		t.Run(id, func(t *testing.T) {
			c := relayRecoveryEnv(t)
			c.identityToken = relayOpaqueIdentityToken
			c.provision = []relayFakeScript{spec}
			logs := captureLogs(t)
			if ok, err := SelfProvisionRelay(context.Background()); ok || err != nil {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if len(c.bearersSeen) != 1 || c.bearersSeen[0] != "Bearer "+relayOpaqueIdentityToken {
				t.Fatalf("bearers %v", c.bearersSeen)
			}
			if !strings.Contains(logs.String(), "rejected") || strings.Contains(logs.String(), relayOpaqueIdentityToken) {
				t.Fatalf("log %s", logs.String())
			}
		})
	}
}

func TestProvisionFailureLogNeverCarriesATokenAHopEchoedIntoAHeader(t *testing.T) {
	c := relayRecoveryEnv(t)
	c.identityToken = relayOpaqueIdentityToken
	c.provision = []relayFakeScript{func(token string) *relayFakeSpec {
		return &relayFakeSpec{status: 403, headers: map[string]string{"Content-Type": "text/html", "Proxy-Status": "egress; error=denied; token=" + token},
			body: "<html>denied</html>"}
	}}
	logs := captureLogs(t)
	if ok, err := SelfProvisionRelay(context.Background()); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	text := logs.String()
	if !strings.Contains(text, "proxy-status=egress") || strings.Contains(text, relayOpaqueIdentityToken) || strings.Contains(text, relayOpaqueIdentityToken[:12]) {
		t.Fatalf("log %s", text)
	}
}

func TestDiagnosticsScrubTheExactBearerTheCallerSent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	bearer := RelayMakeUpgradeToken("gw-x", strings.Repeat("s", 64), relayDefaultUpgradeTTLSeconds)
	exc := &RelayHTTPError{Code: 403, Reason: "err", Body: []byte(`{"message":"token ` + bearer + ` rejected"}`)}
	text := RelayDescribeHTTPError(exc, RelayReadErrorBody(exc), bearer, "")
	if strings.Contains(text, bearer) || !strings.Contains(text, "rejected") {
		t.Fatalf("description %s", text)
	}
}

func TestJSON403FromANamedProxyGetsTheFastScheduleNotRefusalPacing(t *testing.T) {
	for id, headers := range map[string]map[string]string{
		"proxy-status":      {"Proxy-Status": "egress; error=dns_timeout", "Server": "envoy"},
		"via":               {"Via": "1.1 edge-proxy"},
		"x-squid-error":     {"X-Squid-Error": "ERR_ACCESS_DENIED 0"},
		"server-cloudflare": {"Server": "cloudflare"},
	} {
		t.Run(id, func(t *testing.T) {
			c := relayRecoveryEnv(t)
			c.provision = []relayFakeScript{relayJSONProxy403(headers), relayRefusal(403, "enterprise identity lifecycle is not confirmed")}
			var slept []float64
			ok, err := RetrySelfProvisionWith(context.Background(), nil, relayRecordingSleep(&slept))
			if !ok || err != nil {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			delays := relayProvisionRetryDelays
			if want := []float64{delays[0], delays[1], relayProvisionRefusalRetry}; !relayFloatsEqual(slept, want) {
				t.Fatalf("slept %v want %v", slept, want)
			}
		})
	}
}

func TestConnectorRefusalBehindANonProxyEdgeIsStillARefusal(t *testing.T) {
	c := relayRecoveryEnv(t)
	spec := relayRefusal(403, "binding owned elsewhere")("")
	spec.headers["Server"] = "railway-edge"
	c.provision = []relayFakeScript{relayFixed(spec)}
	if outcome, err := relaySelfProvision(context.Background()); outcome != relayRefused || err != nil {
		t.Fatalf("outcome %q err %v", outcome, err)
	}
}
