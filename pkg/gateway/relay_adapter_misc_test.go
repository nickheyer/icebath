package gateway

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
)

// Discord descriptor of the interrupt and follow up cases
func relayDiscordDesc(d *CapabilityDescriptor) {
	d.Platform, d.Label, d.MaxMessageLength, d.SupportsDraftStreaming = "discord", "Discord", 2000, false
	d.MarkdownDialect, d.LenUnit = "discord", "chars"
}

// Slack descriptor of the live card cases, draft and task card advertised
func relayLiveCardsDesc(d *CapabilityDescriptor) {
	relaySlackDedupeDesc(d)
	d.SupportedOps = []string{"send", "edit", "typing", "draft", "task_card"}
}

func TestInterruptSetsOnlyTargetSessionEvent(t *testing.T) {
	a, _ := relayCaptureAdapter(t, relayDiscordDesc)
	keyA, keyB := "agent:main:discord:group:chanA:userX", "agent:main:discord:group:chanB:userY"
	evA, evB := NewInterruptEvent(), NewInterruptEvent()
	a.mu.Lock()
	lazyAttr(&a.activeSessions)[keyA] = evA
	a.activeSessions[keyB] = evB
	a.mu.Unlock()
	if err := a.OnInterrupt(context.Background(), keyA, "chanA"); err != nil {
		t.Fatal(err)
	}
	if !evA.IsSet() || evB.IsSet() {
		t.Fatalf("target set %v sibling set %v", evA.IsSet(), evB.IsSet())
	}
}

func TestFollowUpWireActionCarriesNoCredential(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayDiscordDesc)
	ctx := context.Background()
	if _, err := a.Connect(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SendFollowUp(ctx, "sess-1", "discord.interaction_token", "x", map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(c.followUps[0]))
	if !slices.Equal(keys, []string{"content", "kind", "metadata", "op", "session_key"}) {
		t.Fatalf("keys %v", keys)
	}
}

func TestSealedTombstonesAreBounded(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayLiveCardsDesc)
	c.result = func(map[string]any) (map[string]any, error) { return map[string]any{"success": true, "message_id": "ts.1"}, nil }
	ctx := context.Background()
	for n := range relayDraftStateCap + 300 {
		md := map[string]any{"message_id": fmt.Sprintf("evt.%d", n)}
		if _, err := a.SendDraft(ctx, "C1", 1000+n, "x", md); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Send(ctx, "C1", "final", "", maps.Clone(md)); err != nil {
			t.Fatal(err)
		}
	}
	newest := relayDraftKey("C1", map[string]any{"message_id": fmt.Sprintf("evt.%d", relayDraftStateCap+299)})
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sealedDraftByChat.size() > relayDraftStateCap || a.openDraftByChat.size() > relayDraftStateCap || !a.sealedDraftByChat.has(newest) {
		t.Fatalf("sealed %d open %d newest %v", a.sealedDraftByChat.size(), a.openDraftByChat.size(), a.sealedDraftByChat.has(newest))
	}
}

func TestOpenDraftsAreBounded(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayLiveCardsDesc)
	c.result = func(map[string]any) (map[string]any, error) { return map[string]any{"success": true, "message_id": "ts.1"}, nil }
	for n := range relayDraftStateCap + 300 {
		if _, err := a.SendDraft(context.Background(), "C1", 2000+n, "x", map[string]any{"message_id": fmt.Sprintf("evt.%d", n)}); err != nil {
			t.Fatal(err)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.openDraftByChat.size() > relayDraftStateCap {
		t.Fatalf("open %d", a.openDraftByChat.size())
	}
}
