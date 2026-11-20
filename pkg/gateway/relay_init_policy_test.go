package gateway

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
)

// JSON text of a policy object for whole value comparison
func relayPolicyJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestProjectionMapsRequireMentionAndFreeResponse(t *testing.T) {
	h := relayCleanEnv(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "discord")
	relayWriteConfig(t, h, "discord:\n  require_mention: true\n  free_response_channels: [c-support, c-help]\n")
	got := relayPolicyJSON(t, RelayRelevancePolicy(context.Background(), nil))
	want := `{"platform":"discord","requireAddress":true,"freeResponseScopes":["c-support","c-help"],"allowOtherBots":false}`
	if got != want {
		t.Fatalf("policy %s want %s", got, want)
	}
}

func TestProjectionDeclaresExplicitRequireMentionFalse(t *testing.T) {
	h := relayCleanEnv(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "discord")
	relayWriteConfig(t, h, "discord:\n  require_mention: false\n")
	got := relayPolicyJSON(t, RelayRelevancePolicy(context.Background(), nil))
	want := `{"platform":"discord","requireAddress":false,"freeResponseScopes":[],"allowOtherBots":false}`
	if got != want {
		t.Fatalf("policy %s want %s", got, want)
	}
}

// Relay env with pinned credentials fronting discord at the dial URL
func relayPolicyArm(t *testing.T, dialURL string) {
	t.Helper()
	t.Setenv("GATEWAY_RELAY_URL", dialURL)
	t.Setenv("GATEWAY_RELAY_ID", "gw-x")
	t.Setenv("GATEWAY_RELAY_SECRET", strings.Repeat("s", 48))
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "discord")
}

func TestSendPostsProjectedPolicyWithToken(t *testing.T) {
	h := relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	relayPolicyArm(t, c.dialURL())
	relayWriteConfig(t, h, "discord:\n  require_mention: true\n  free_response_channels: [c-support]\n")
	ok, err := SendRelayPolicy(context.Background())
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(c.policies) != 1 || !strings.HasPrefix(c.policyAuth[0], "Bearer ") || len(c.policyAuth[0]) <= len("Bearer ") {
		t.Fatalf("policies %v auth %v", c.policies, c.policyAuth)
	}
	if c.policies[0]["requireAddress"] != true || relayPolicyJSON(t, c.policies[0]["freeResponseScopes"]) != `["c-support"]` {
		t.Fatalf("policy %v", c.policies[0])
	}
}

func TestSendSkipsWhenNoSecret(t *testing.T) {
	h := relayCleanEnv(t)
	c := newRelayFakeConnector(t)
	t.Setenv("GATEWAY_RELAY_URL", c.dialURL())
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "discord")
	relayWriteConfig(t, h, "discord:\n  require_mention: true\n")
	ok, err := SendRelayPolicy(context.Background())
	if ok || err != nil || len(c.policies) != 0 {
		t.Fatalf("ok=%v err=%v policies=%v", ok, err, c.policies)
	}
}

func TestSendFailSoftOnTransportError(t *testing.T) {
	h := relayCleanEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	relayPolicyArm(t, "ws://"+addr+"/relay")
	relayWriteConfig(t, h, "discord:\n  require_mention: true\n")
	ok, err := SendRelayPolicy(context.Background())
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}
