package gateway

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
)

// Descriptor of a fronted platform in the multi platform cases
func relayDescFor(platform string, ops ...string) CapabilityDescriptor {
	return relayTestDesc(func(d *CapabilityDescriptor) {
		relayLiveCardsDesc(d)
		d.Platform, d.Label, d.MaxMessageLength = platform, strings.ToUpper(platform[:1])+platform[1:], 4096
		d.MarkdownDialect = "slack"
		if platform == "telegram" {
			d.MarkdownDialect = "markdown_v2"
		}
		if len(ops) > 0 {
			d.SupportedOps = ops
		}
	})
}

// Adapter of the primary platform over a recording transport negotiating every listed platform
func relayMultiAdapter(t *testing.T, primary string, others ...string) (*RelayAdapter, *relayCapture) {
	t.Helper()
	mod := relayLiveCardsDesc
	if primary != "slack" {
		mod = func(d *CapabilityDescriptor) {
			relayLiveCardsDesc(d)
			d.Platform, d.MarkdownDialect, d.SupportedOps = primary, "markdown_v2", []string{"send", "edit", "typing", "draft"}
		}
	}
	a, c := relayCaptureAdapter(t, mod)
	for _, p := range append([]string{primary}, others...) {
		c.byPlatform[p] = relayDescFor(p)
	}
	c.result = func(map[string]any) (map[string]any, error) {
		return map[string]any{"success": true, "message_id": "ts.1"}, nil
	}
	return a, c
}

// Draft frames sent with final set
func relaySealFrames(c *relayCapture) []map[string]any {
	var out []map[string]any
	for _, o := range c.ops("draft") {
		if o["final"] == true {
			out = append(out, o)
		}
	}
	return out
}

// Streams a partial draft then sends the final with the same turn metadata
func relayDraftThenFinal(t *testing.T, a *RelayAdapter, chatID string, draftID int, md map[string]any, final string) SendResult {
	t.Helper()
	ctx := context.Background()
	if _, err := a.SendDraft(ctx, chatID, draftID, "partial", md); err != nil {
		t.Fatal(err)
	}
	r, err := a.Send(ctx, chatID, final, "", maps.Clone(md))
	if err != nil || !r.Success {
		t.Fatalf("final %+v err %v", r, err)
	}
	return r
}

func TestPerChatStreamSemanticsSlackPrimaryTelegramChatGetsRealFinal(t *testing.T) {
	a, c := relayMultiAdapter(t, "slack", "telegram")
	a.platformByChat["TG1"] = "telegram"
	relayDraftThenFinal(t, a, "TG1", 3, map[string]any{"message_id": "m.1"}, "complete")
	if len(relaySealFrames(c)) != 0 || len(c.ops("send")) == 0 {
		t.Fatalf("telegram chat sealed by the Slack primary's semantic: %v", c.sent)
	}
}

func TestPerChatStreamSemanticsSlackPrimarySlackChatStillSeals(t *testing.T) {
	a, c := relayMultiAdapter(t, "slack", "telegram")
	a.platformByChat["C1"] = "slack"
	relayDraftThenFinal(t, a, "C1", 4, map[string]any{"message_id": "m.2"}, "complete")
	if len(relaySealFrames(c)) == 0 || len(c.ops("send")) != 0 {
		t.Fatalf("ops %v", c.sent)
	}
}

func TestPerChatStreamSemanticsTelegramPrimarySlackChatGetsNativeStreaming(t *testing.T) {
	a, _ := relayMultiAdapter(t, "telegram", "slack")
	a.platformByChat["C9"] = "slack"
	if !a.SupportsDraftStreaming("", nil, "C9") || !a.StreamIsMessageForChat("C9") {
		t.Fatal("slack chat of a telegram primary must stream natively")
	}
	a.platformByChat["TG9"] = "telegram"
	if a.StreamIsMessageForChat("TG9") {
		t.Fatal("telegram chat must not take the seal semantic")
	}
}

func TestPerChatStreamSemanticsUnknownChatFallsBackToScalar(t *testing.T) {
	a, _ := relayMultiAdapter(t, "slack", "telegram")
	if !a.StreamIsMessageForChat("UNSEEN") {
		t.Fatal("unseen chat must follow the slack primary")
	}
}

func TestPerChatStreamSemanticsCapabilityGateRespectsChatDescriptor(t *testing.T) {
	a, c := relayMultiAdapter(t, "slack")
	c.byPlatform["telegram"] = relayDescFor("telegram", "send", "edit", "typing")
	a.platformByChat["TG2"] = "telegram"
	_, err := a.SendDraft(context.Background(), "TG2", 5, "x", map[string]any{})
	var pe *PyError
	if !errors.As(err, &pe) || pe.Kind != "NotImplementedError" {
		t.Fatalf("err %v", err)
	}
}

func TestStreamIsMessageGatingTelegramFinalIsARealSendNotASeal(t *testing.T) {
	a, c := relayCaptureAdapter(t, func(d *CapabilityDescriptor) {
		relayLiveCardsDesc(d)
		d.Platform, d.MarkdownDialect, d.SupportedOps = "telegram", "markdown_v2", []string{"send", "edit", "typing", "draft"}
	})
	c.result = func(map[string]any) (map[string]any, error) {
		return map[string]any{"success": true, "message_id": "m.1"}, nil
	}
	relayDraftThenFinal(t, a, "T1", 3, map[string]any{"reply_to_message_id": "evt.1"}, "complete answer")
	partial, plain := false, false
	for _, o := range c.sent {
		final, hasFinal := o["final"]
		partial = partial || (o["op"] == "draft" && final == false)
		plain = plain || (o["op"] == "send" && !hasFinal)
	}
	if !partial || !plain || len(relaySealFrames(c)) != 0 {
		t.Fatalf("telegram-shaped connector must never receive draft(final=true) as the turn-final: %v", c.sent)
	}
}

func TestStreamIsMessageGatingSlackFinalStillSeals(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayLiveCardsDesc)
	c.result = func(map[string]any) (map[string]any, error) {
		return map[string]any{"success": true, "message_id": "m.1"}, nil
	}
	relayDraftThenFinal(t, a, "C1", 4, map[string]any{"reply_to_message_id": "evt.2"}, "complete answer")
	if len(relaySealFrames(c)) == 0 || len(c.ops("send")) != 0 {
		t.Fatalf("ops %v", c.sent)
	}
}
