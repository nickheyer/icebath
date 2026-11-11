package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var relayInitEnvKeys = []string{
	"GATEWAY_RELAY_URL", "GATEWAY_RELAY_ID", "GATEWAY_RELAY_SECRET", "GATEWAY_RELAY_DELIVERY_KEY",
	"GATEWAY_RELAY_ENDPOINT", "GATEWAY_RELAY_ROUTE_KEYS", "GATEWAY_RELAY_PLATFORM", "GATEWAY_RELAY_BOT_ID",
	"GATEWAY_RELAY_INSTANCE_ID", "GATEWAY_RELAY_WAKE_URL", "GATEWAY_RELAY_PLATFORMS", "GATEWAY_RELAY_BOT_IDS",
	"GATEWAY_RELAY_DISPLAY_NAME", "DISCORD_ALLOW_BOTS", "GATEWAY_RELAY_IDP_TOKEN_URL", "GATEWAY_RELAY_IDP_CLIENT_ID",
	"GATEWAY_RELAY_IDP_CLIENT_SECRET", "GATEWAY_RELAY_IDP_SCOPE",
}

// Fresh profile with every relay variable unset and restored after the case
func relayCleanEnv(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(h, "no-managed"))
	for _, k := range relayInitEnvKeys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return h
}

// Writes the profile config.yaml the effective config reader loads
func relayWriteConfig(t *testing.T, h, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Loopback connector recording provision and policy posts and serving an ambient identity token
type relayFakeConnector struct {
	srv        *httptest.Server
	mu         sync.Mutex
	provisions []map[string]any
	provAuth   []string
	policies   []map[string]any
	policyAuth []string
	provision  func(n int, body map[string]any) (int, http.Header, string)
	policy     func(body map[string]any) int
	token      string
}

func newRelayFakeConnector(t *testing.T) *relayFakeConnector {
	t.Helper()
	c := &relayFakeConnector{token: "tok-" + strings.Repeat("t", 40), policy: func(map[string]any) int { return 200 }}
	c.provision = func(_ int, body map[string]any) (int, http.Header, string) {
		out, _ := json.Marshal(map[string]any{"secret": strings.Repeat("a", 64), "deliveryKey": strings.Repeat("b", 64),
			"tenant": "org-tenant-x", "gatewayId": body["gatewayId"], "routeKeys": body["routeKeys"]})
		return 200, nil, string(out)
	}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		c.mu.Lock()
		switch r.URL.Path {
		case "/access-token":
			c.mu.Unlock()
			io.WriteString(w, c.token)
			return
		case "/relay/provision":
			c.provisions = append(c.provisions, body)
			c.provAuth = append(c.provAuth, r.Header.Get("Authorization"))
			n := len(c.provisions)
			c.mu.Unlock()
			status, headers, text := c.provision(n, body)
			for k, v := range headers {
				w.Header()[k] = v
			}
			w.WriteHeader(status)
			io.WriteString(w, text)
			return
		case "/relay/policy":
			c.policies = append(c.policies, body)
			c.policyAuth = append(c.policyAuth, r.Header.Get("Authorization"))
			c.mu.Unlock()
			w.WriteHeader(c.policy(body))
			return
		}
		c.mu.Unlock()
		w.WriteHeader(404)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// ws dial URL of the fake connector
func (c *relayFakeConnector) dialURL() string {
	return "ws" + strings.TrimPrefix(c.srv.URL, "http") + "/relay"
}

// Points the relay URL and the ambient identity endpoint at the fake connector
func (c *relayFakeConnector) arm(t *testing.T) {
	t.Helper()
	t.Setenv("GATEWAY_RELAY_URL", c.dialURL())
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", c.srv.URL+"/access-token")
}

func (c *relayFakeConnector) provisionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.provisions)
}

func (c *relayFakeConnector) lastProvision() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.provisions[len(c.provisions)-1]
}

func TestProvisionURLMapsWsToHTTP(t *testing.T) {
	relayCleanEnv(t)
	for in, want := range map[string]string{
		"wss://c.example/relay": "https://c.example/relay/provision",
		"ws://c.example/relay":  "http://c.example/relay/provision",
		"https://c.example":     "https://c.example/relay/provision",
	} {
		if got := relayProvisionURL(in); got != want {
			t.Errorf("%s -> %s want %s", in, got, want)
		}
	}
}

func TestSkipsWhenRelayNotConfigured(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", c.srv.URL+"/access-token")
	ok, err := SelfProvisionRelay(context.Background())
	if ok || err != nil || c.provisionCount() != 0 {
		t.Fatalf("ok=%v err=%v posts=%d", ok, err, c.provisionCount())
	}
}

func TestSkipsWhenSecretAlreadyPinned(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.arm(t)
	t.Setenv("GATEWAY_RELAY_ID", "gw-pinned")
	t.Setenv("GATEWAY_RELAY_SECRET", "deadbeef")
	ok, err := SelfProvisionRelay(context.Background())
	if ok || err != nil || c.provisionCount() != 0 {
		t.Fatalf("ok=%v err=%v posts=%d", ok, err, c.provisionCount())
	}
	if id, secret := RelayConnectionAuth(context.Background()); id != "gw-pinned" || secret != "deadbeef" {
		t.Fatalf("pinned auth changed to %q %q", id, secret)
	}
}

func TestProvisionsAndSetsEnvInProcess(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.arm(t)
	t.Setenv("GATEWAY_RELAY_ENDPOINT", "https://gw.example.com/inbound")
	t.Setenv("GATEWAY_RELAY_ROUTE_KEYS", "guild-1,guild-2")
	ok, err := SelfProvisionRelay(context.Background())
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	body := c.lastProvision()
	if c.provAuth[0] != "Bearer "+c.token || body["gatewayEndpoint"] != "https://gw.example.com/inbound" {
		t.Fatalf("auth %q body %v", c.provAuth[0], body)
	}
	if keys, _ := json.Marshal(body["routeKeys"]); string(keys) != `["guild-1","guild-2"]` {
		t.Fatalf("route keys %s", keys)
	}
	if id, secret := RelayConnectionAuth(context.Background()); id == "" || secret != strings.Repeat("a", 64) {
		t.Fatalf("auth %q %q", id, secret)
	}
	if os.Getenv("GATEWAY_RELAY_DELIVERY_KEY") != strings.Repeat("b", 64) {
		t.Fatalf("delivery key %q", os.Getenv("GATEWAY_RELAY_DELIVERY_KEY"))
	}
}

func TestOutboundOnlyWhenNoEndpoint(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.arm(t)
	ok, err := SelfProvisionRelay(context.Background())
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	body := c.lastProvision()
	if body["gatewayEndpoint"] != "" {
		t.Fatalf("endpoint %v", body["gatewayEndpoint"])
	}
	if keys, _ := json.Marshal(body["routeKeys"]); string(keys) != `[]` {
		t.Fatalf("route keys %s", keys)
	}
	if _, secret := RelayConnectionAuth(context.Background()); secret != strings.Repeat("a", 64) {
		t.Fatalf("secret %q", secret)
	}
}

func TestPostProvisionBodyIncludesInstanceIDOnlyWhenSet(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	args := relayProvisionArgs{provisionURL: c.srv.URL + "/relay/provision", accessToken: "tok", gatewayID: "gw-1",
		platform: "discord", botID: "app", routeKeys: []string{}, instanceID: "inst-abc"}
	if _, err := relayPostProvision(args); err != nil {
		t.Fatal(err)
	}
	if got := c.lastProvision()["instanceId"]; got != "inst-abc" {
		t.Fatalf("instanceId %v", got)
	}
	args.instanceID = ""
	if _, err := relayPostProvision(args); err != nil {
		t.Fatal(err)
	}
	if _, has := c.lastProvision()["instanceId"]; has {
		t.Fatalf("instanceId present without a value: %v", c.lastProvision())
	}
}

func TestForwardsWakeURLToProvision(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.arm(t)
	t.Setenv("GATEWAY_RELAY_WAKE_URL", "https://wake.example/poke")
	if ok, err := SelfProvisionRelay(context.Background()); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got := c.lastProvision()["wakeUrl"]; got != "https://wake.example/poke" {
		t.Fatalf("wakeUrl %v", got)
	}
}

func TestNoNasTokenIsNonFatal(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	t.Setenv("GATEWAY_RELAY_URL", c.dialURL())
	ok, err := SelfProvisionRelay(context.Background())
	if ok || err != nil || c.provisionCount() != 0 {
		t.Fatalf("ok=%v err=%v posts=%d", ok, err, c.provisionCount())
	}
	if id, secret := RelayConnectionAuth(context.Background()); id != "" || secret != "" {
		t.Fatalf("auth %q %q", id, secret)
	}
}

func TestRelayDisplayNameSuppressesStockBrand(t *testing.T) {
	relayCleanEnv(t)
	if got := RelayDisplayName(context.Background()); got != "" {
		t.Fatalf("stock brand forwarded as %q", got)
	}
}

// Relay env of two fronted platforms against the fake connector
func relayTwoPlatforms(t *testing.T, c *relayFakeConnector) {
	t.Helper()
	c.arm(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "telegram,discord")
	t.Setenv("GATEWAY_RELAY_BOT_IDS", `{"telegram": {"botId": "BOT_T"}, "discord": {"botId": "BOT_D"}}`)
}

func TestSecretlessProvisionResponseIsNotAnError(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.provision = func(int, map[string]any) (int, http.Header, string) {
		return 200, nil, `{"secretIssued": false, "tenant": "org-x", "gatewayId": "gw-1", "routeKeys": ["k"]}`
	}
	payload, err := relayPostProvision(relayProvisionArgs{provisionURL: c.srv.URL + "/relay/provision", accessToken: "tok",
		gatewayID: "gw-1", platform: "telegram", botID: "BOT", routeKeys: []string{"k"}})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := payload.Get("secretIssued"); v != false {
		t.Fatalf("secretIssued %v", v)
	}
	if _, has := payload.Get("secret"); has {
		t.Fatal("secret present")
	}
	if v, _ := payload.Get("gatewayId"); v != "gw-1" {
		t.Fatalf("gatewayId %v", v)
	}
}

func TestMalformedResponseStillRaises(t *testing.T) {
	for id, body := range map[string]string{
		"no-discriminator-pre-f004": `{"tenant": "org-x", "gatewayId": "gw-1"}`,
		"issued-true-but-absent":    `{"secretIssued": true, "gatewayId": "gw-1"}`,
		"string-false":              `{"secretIssued": "false", "gatewayId": "gw-1"}`,
		"falsy-non-bool":            `{"secretIssued": 0, "gatewayId": "gw-1"}`,
		"explicit-null":             `{"secretIssued": null, "gatewayId": "gw-1"}`,
	} {
		t.Run(id, func(t *testing.T) {
			relayCleanEnv(t)
			c := newRelayFakeConnector(t)
			c.provision = func(int, map[string]any) (int, http.Header, string) { return 200, nil, body }
			_, err := relayPostProvision(relayProvisionArgs{provisionURL: c.srv.URL + "/relay/provision", accessToken: "tok",
				gatewayID: "gw-1", platform: "telegram", botID: "BOT", routeKeys: []string{"k"}})
			var rt *RelayRuntimeError
			if err == nil || !strings.Contains(err.Error(), "no secret") || !errors.As(err, &rt) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestMultiplatformBootSurvivesASecretlessSecondPlatform(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.provision = func(n int, body map[string]any) (int, http.Header, string) {
		resp := map[string]any{"secretIssued": false, "tenant": "org-x", "gatewayId": body["gatewayId"], "routeKeys": body["routeKeys"]}
		if n == 1 {
			resp = map[string]any{"secretIssued": true, "secret": strings.Repeat("a", 64), "deliveryKey": strings.Repeat("b", 64),
				"tenant": "org-x", "gatewayId": body["gatewayId"], "routeKeys": body["routeKeys"]}
		}
		out, _ := json.Marshal(resp)
		return 200, nil, string(out)
	}
	relayTwoPlatforms(t, c)
	ok, err := SelfProvisionRelay(context.Background())
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if c.provisions[0]["platform"] != "telegram" || c.provisions[1]["platform"] != "discord" || len(c.provisions) != 2 {
		t.Fatalf("calls %v", c.provisions)
	}
	if os.Getenv("GATEWAY_RELAY_SECRET") != strings.Repeat("a", 64) || os.Getenv("GATEWAY_RELAY_DELIVERY_KEY") != strings.Repeat("b", 64) {
		t.Fatal("credentials of the first platform lost")
	}
}

func TestASecretlessFirstPlatformDoesNotWriteEmptyCreds(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.provision = func(n int, body map[string]any) (int, http.Header, string) {
		resp := map[string]any{"secretIssued": false, "tenant": "org-x", "gatewayId": "gw-from-withheld", "routeKeys": body["routeKeys"]}
		if n == 2 {
			resp = map[string]any{"secretIssued": true, "secret": strings.Repeat("c", 64), "deliveryKey": strings.Repeat("d", 64),
				"tenant": "org-x", "gatewayId": "gw-self", "routeKeys": body["routeKeys"]}
		}
		out, _ := json.Marshal(resp)
		return 200, nil, string(out)
	}
	relayTwoPlatforms(t, c)
	ok, err := SelfProvisionRelay(context.Background())
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if os.Getenv("GATEWAY_RELAY_SECRET") != strings.Repeat("c", 64) || os.Getenv("GATEWAY_RELAY_DELIVERY_KEY") != strings.Repeat("d", 64) || os.Getenv("GATEWAY_RELAY_ID") != "gw-self" {
		t.Fatalf("env %q %q %q", os.Getenv("GATEWAY_RELAY_SECRET"), os.Getenv("GATEWAY_RELAY_DELIVERY_KEY"), os.Getenv("GATEWAY_RELAY_ID"))
	}
}

func TestAWithheldResponseNeverOverwritesCredentialsInHand(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.provision = func(_ int, body map[string]any) (int, http.Header, string) {
		out, _ := json.Marshal(map[string]any{"secretIssued": false, "tenant": "org-x", "gatewayId": "gw-connector-chosen", "routeKeys": body["routeKeys"]})
		return 200, nil, string(out)
	}
	relayTwoPlatforms(t, c)
	ok, err := SelfProvisionRelay(context.Background())
	if err != nil || len(c.provisions) != 2 || c.provisions[0]["platform"] != "telegram" || c.provisions[1]["platform"] != "discord" {
		t.Fatalf("err=%v calls=%v", err, c.provisions)
	}
	for _, k := range []string{"GATEWAY_RELAY_SECRET", "GATEWAY_RELAY_DELIVERY_KEY", "GATEWAY_RELAY_ID"} {
		if os.Getenv(k) != "" {
			t.Fatalf("%s written %q", k, os.Getenv(k))
		}
	}
	if ok {
		t.Fatal("withheld boot reported provisioned")
	}
}

func TestAllPlatformsWithheldWarnsWithTheRecoveryPath(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.provision = func(_ int, body map[string]any) (int, http.Header, string) {
		out, _ := json.Marshal(map[string]any{"secretIssued": false, "tenant": "org-x", "gatewayId": body["gatewayId"], "routeKeys": []any{}})
		return 200, nil, string(out)
	}
	c.arm(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "telegram")
	t.Setenv("GATEWAY_RELAY_BOT_IDS", `{"telegram": {"botId": "BOT_T"}}`)
	logs := captureLogs(t)
	ok, err := SelfProvisionRelay(context.Background())
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	var warnings []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") || strings.Contains(line, "level=ERROR") {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) == 0 || !strings.Contains(warnings[len(warnings)-1], "/relay/rotate") {
		t.Fatalf("warnings %v", warnings)
	}
	if strings.Contains(logs.String(), "self-provisioned (") {
		t.Fatalf("withheld boot logged a provision: %s", logs.String())
	}
}
