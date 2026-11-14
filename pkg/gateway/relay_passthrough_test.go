package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Connected discord adapter over a capture transport, its passthrough handler wired
func relayPassthroughAdapter(t *testing.T) (*RelayAdapter, *relayCapture) {
	t.Helper()
	a, c := relayCaptureAdapter(t, relayDiscordDesc)
	if _, err := a.Connect(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	return a, c
}

// Forwarded discord interaction of the JSON payload with an optional routed profile
func relayInteractionForward(t *testing.T, payload map[string]any, profile *string) *PassthroughForward {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &PassthroughForward{Platform: "discord", BotID: "appShared", Method: "POST", Path: "/interactions/discord/appShared",
		Headers: [][2]string{{"content-type", "application/json"}}, Body: body, Profile: profile}
}

func relayOnlyEvent(t *testing.T, seen func() []*MessageEvent) *MessageEvent {
	t.Helper()
	events := relaySettledEvents(seen, 1)
	if len(events) != 1 {
		t.Fatalf("handled %d events", len(events))
	}
	return events[0]
}

func TestPassthroughFromWireBytePreservesBody(t *testing.T) {
	original, _ := json.Marshal(map[string]any{"type": 2, "data": map[string]any{"name": "ping"}, "guild_id": "g1"})
	wire := pyjson.ObjectOf("platform", "discord", "botId", "appShared", "method", "POST", "path", "/interactions/discord/appShared",
		"headers", []any{[]any{"content-type", "application/json"}}, "bodyB64", base64.StdEncoding.EncodeToString(original))
	fwd := relayPassthroughFromWire(wire)
	if fwd.Platform != "discord" || fwd.BotID != "appShared" || !bytes.Equal(fwd.Body, original) ||
		len(fwd.Headers) != 1 || fwd.Headers[0] != [2]string{"content-type", "application/json"} {
		t.Fatalf("forward %+v", fwd)
	}
}

func TestPassthroughFromWireStampsRoutedProfile(t *testing.T) {
	wire := pyjson.ObjectOf("platform", "discord", "botId", "appShared", "method", "POST", "path", "/interactions/discord/appShared",
		"headers", []any{}, "bodyB64", "", "profile", "reviewer")
	if fwd := relayPassthroughFromWire(wire); fwd.Profile == nil || *fwd.Profile != "reviewer" {
		t.Fatalf("profile %v", fwd.Profile)
	}
}

func TestDiscordInteractionRoutesThroughHandleMessage(t *testing.T) {
	a, c := relayPassthroughAdapter(t)
	seen := relayTapEvents(a)
	c.pushPassthrough(t, relayInteractionForward(t, map[string]any{"id": "interaction-1", "type": 2, "channel_id": "chan-9",
		"guild_id": "guild-7", "data": map[string]any{"name": "summarize"},
		"member": map[string]any{"user": map[string]any{"id": "user-3", "username": "ben"}}}, nil))
	ev := relayOnlyEvent(t, seen)
	command, _ := ev.GetCommand()
	src := ev.Source
	if ev.Text != "/summarize" || !ev.IsCommand() || command != "summarize" || src.ChatID != "chan-9" ||
		src.ScopeID == nil || *src.ScopeID != "guild-7" || src.UserID == nil || *src.UserID != "user-3" ||
		src.Platform != PlatformDiscord || src.ChatType != "group" || !src.DeliveredViaUpstreamRelay {
		t.Fatalf("event %+v source %+v", ev, src)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.scopeByChat["chan-9"] != "guild-7" || a.platformByChat["chan-9"] != "discord" {
		t.Fatalf("scope %q platform %q", a.scopeByChat["chan-9"], a.platformByChat["chan-9"])
	}
}

func TestDiscordInteractionStampsRoutedProfile(t *testing.T) {
	a, c := relayPassthroughAdapter(t)
	seen := relayTapEvents(a)
	c.pushPassthrough(t, relayInteractionForward(t, map[string]any{"id": "interaction-2", "type": 2, "channel_id": "chan-9",
		"guild_id": "guild-7", "data": map[string]any{"name": "summarize"},
		"member": map[string]any{"user": map[string]any{"id": "user-3", "username": "ben"}}}, relayStrPtr("reviewer")))
	if ev := relayOnlyEvent(t, seen); ev.Source.Profile == nil || *ev.Source.Profile != "reviewer" {
		t.Fatalf("profile %v", ev.Source.Profile)
	}
}

func TestApplicationCommandSubcommandNestingRendersNamesThenValues(t *testing.T) {
	a, c := relayPassthroughAdapter(t)
	seen := relayTapEvents(a)
	c.pushPassthrough(t, relayInteractionForward(t, map[string]any{"id": "i-sub", "type": 2, "channel_id": "c5", "guild_id": "g5",
		"data": map[string]any{"name": "skill", "options": []any{map[string]any{"name": "run", "type": 1,
			"options": []any{map[string]any{"name": "target", "type": 3, "value": "deploy"}}}}},
		"member": map[string]any{"user": map[string]any{"id": "u5", "username": "ben"}}}, nil))
	ev := relayOnlyEvent(t, seen)
	command, _ := ev.GetCommand()
	if ev.Text != "/skill run deploy" || !ev.IsCommand() || command != "skill" || ev.GetCommandArgs() != "run deploy" {
		t.Fatalf("event %q command %q args %q", ev.Text, command, ev.GetCommandArgs())
	}
}

func TestDmInteractionKeysAsDiscordDm(t *testing.T) {
	a, c := relayPassthroughAdapter(t)
	seen := relayTapEvents(a)
	c.pushPassthrough(t, relayInteractionForward(t, map[string]any{"id": "i-dm", "type": 2, "channel_id": "dm-chan-1",
		"data": map[string]any{"name": "sethome"}, "user": map[string]any{"id": "u9", "username": "ben"}}, nil))
	src := relayOnlyEvent(t, seen).Source
	if src.Platform != PlatformDiscord || src.ChatType != "dm" || src.ScopeID != nil || src.UserID == nil || *src.UserID != "u9" ||
		!src.DeliveredViaUpstreamRelay {
		t.Fatalf("source %+v", src)
	}
}

func TestRoutedProfileRoundTripsOnEveryEgressFrame(t *testing.T) {
	a, c := relayPassthroughAdapter(t)
	relayTapEvents(a)
	ctx := context.Background()
	c.pushPassthrough(t, relayInteractionForward(t, map[string]any{"id": "interaction-3", "type": 2, "channel_id": "chan-9",
		"guild_id": "guild-7", "data": map[string]any{"name": "summarize"},
		"member": map[string]any{"user": map[string]any{"id": "user-3", "username": "ben"}}}, relayStrPtr("reviewer")))
	a.Send(ctx, "chan-9", "done", "", nil)
	a.SendFollowUp(ctx, "agent:reviewer:discord:group:chan-9", "discord.interaction_token", "x", nil)
	sent, _ := c.last(t)
	if sent["metadata"].(map[string]any)["profile"] != "reviewer" || c.followUps[len(c.followUps)-1]["metadata"].(map[string]any)["profile"] != "reviewer" {
		t.Fatalf("sent %v follow up %v", sent, c.followUps)
	}
	c.pushPassthrough(t, relayInteractionForward(t, map[string]any{"id": "interaction-4", "type": 2, "channel_id": "chan-1",
		"guild_id": "guild-7", "data": map[string]any{"name": "summarize"}, "member": map[string]any{"user": map[string]any{"id": "user-3"}}}, nil))
	a.Send(ctx, "chan-1", "done", "", nil)
	a.SendFollowUp(ctx, "agent:main:discord:group:chan-1", "discord.interaction_token", "x", nil)
	sent, _ = c.last(t)
	_, sentHas := sent["metadata"].(map[string]any)["profile"]
	_, followHas := c.followUps[len(c.followUps)-1]["metadata"].(map[string]any)["profile"]
	if sentHas || followHas {
		t.Fatalf("sent %v follow up %v", sent, c.followUps)
	}
}

func TestModalSubmitRendersItsSubmittedValues(t *testing.T) {
	a, c := relayPassthroughAdapter(t)
	seen := relayTapEvents(a)
	c.pushPassthrough(t, relayInteractionForward(t, map[string]any{"id": "i-modal", "type": 5, "channel_id": "th1", "guild_id": "g1",
		"channel": map[string]any{"type": 11, "parent_id": "c1"}, "member": map[string]any{"user": map[string]any{"id": "u1", "username": "ben"}},
		"data": map[string]any{"custom_id": "feedback", "components": []any{
			map[string]any{"type": 1, "components": []any{map[string]any{"type": 4, "custom_id": "title", "value": "Café ☕ outage"}}},
			map[string]any{"type": 1, "components": []any{map[string]any{"type": 4, "custom_id": "notes", "value": ""}}},
			map[string]any{"type": 18, "component": map[string]any{"type": 4, "custom_id": "steps", "value": "1. deploy\n2. \U0001f4a5"}},
		}}}, nil))
	ev := relayOnlyEvent(t, seen)
	src := ev.Source
	if ev.Text != "feedback\ntitle: Café ☕ outage\nsteps: 1. deploy\n2. \U0001f4a5" || src.ChatType != "thread" ||
		src.ThreadID == nil || *src.ThreadID != "th1" || src.ParentChatID == nil || *src.ParentChatID != "c1" {
		t.Fatalf("text %q source %+v", ev.Text, src)
	}
}
