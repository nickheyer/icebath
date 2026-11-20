package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestIdentitiesMultiPlatformKeyedMap(t *testing.T) {
	relayCleanEnv(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "discord, telegram")
	t.Setenv("GATEWAY_RELAY_BOT_IDS", `{"discord": {"botId": "app-1"}, "telegram": {"botId": "bot-9", "username": "@my_bot"}}`)
	if got := RelayPlatformIdentities(); !slices.Equal(got, [][2]string{{"discord", "app-1"}, {"telegram", "bot-9"}}) {
		t.Fatalf("identities %v", got)
	}
	if got := RelayPlatformIdentity(); got != [2]string{"discord", "app-1"} {
		t.Fatalf("identity %v", got)
	}
}

func TestBotIdsMalformedJSONDegradesToEmpty(t *testing.T) {
	relayCleanEnv(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "discord")
	t.Setenv("GATEWAY_RELAY_BOT_IDS", "{not valid json")
	if got := RelayPlatformIdentities(); !slices.Equal(got, [][2]string{{"discord", ""}}) {
		t.Fatalf("identities %v", got)
	}
}

func TestSelfProvisionLoopsPerPlatform(t *testing.T) {
	relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	c.provision = func(_ int, body map[string]any) (int, http.Header, string) {
		out, _ := json.Marshal(map[string]any{"secret": strings.Repeat("s", 64), "deliveryKey": strings.Repeat("d", 64),
			"tenant": "t", "gatewayId": body["gatewayId"]})
		return 200, nil, string(out)
	}
	c.arm(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "discord,telegram")
	t.Setenv("GATEWAY_RELAY_BOT_IDS", `{"discord": {"botId": "app-1"}, "telegram": {"botId": "bot-9"}}`)
	ok, err := SelfProvisionRelay(context.Background())
	if err != nil || !ok {
		t.Fatalf("provisioned %v err %v", ok, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var pairs [][2]any
	gateways := map[any]bool{}
	for _, p := range c.provisions {
		pairs = append(pairs, [2]any{p["platform"], p["botId"]})
		gateways[p["gatewayId"]] = true
	}
	if !slices.Equal(pairs, [][2]any{{"discord", "app-1"}, {"telegram", "bot-9"}}) || len(gateways) != 1 {
		t.Fatalf("provisions %v", c.provisions)
	}
	if got := os.Getenv("GATEWAY_RELAY_SECRET"); got != strings.Repeat("s", 64) {
		t.Fatalf("secret %q", got)
	}
}

func TestAdapterStampsPerFramePlatformFromInbound(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	desc := NewCapabilityDescriptor(RelayContractVersion, "relay", "Relay", 4096, false, true, false, "plain", "chars")
	c := &relayCapture{desc: &desc, result: relayConnectorResult, byPlatform: map[string]CapabilityDescriptor{},
		identities: [][2]string{{"discord", "app-1"}, {"telegram", "bot-9"}}}
	a := NewRelayAdapter(NewPlatformConfig(), desc, c)
	ctx := context.Background()
	if _, err := a.Connect(ctx, false); err != nil {
		t.Fatal(err)
	}
	c.handler(ctx, relayInbound(PlatformTelegram, "tg-1", "dm", func(s *SessionSource) { s.UserID = relayStrPtr("u-1") }))
	a.Send(ctx, "tg-1", "a telegram reply", "", nil)
	if _, platform := c.last(t); platform != "telegram" {
		t.Fatalf("platform %q", platform)
	}
	c.handler(ctx, relayInbound(PlatformDiscord, "dc-1", "channel", func(s *SessionSource) { s.ScopeID = relayStrPtr("g-1") }))
	a.Send(ctx, "dc-1", "a discord reply", "", nil)
	if _, platform := c.last(t); platform != "discord" {
		t.Fatalf("platform %q", platform)
	}
}
