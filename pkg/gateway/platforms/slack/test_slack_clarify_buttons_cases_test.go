package slack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears the clarify registry entries a case registers once it ends
func clearClarifyState(t *testing.T, sessions ...string) {
	t.Cleanup(func() {
		for _, s := range sessions {
			tools.ClearClarifySession(s)
		}
	})
}

func TestSlackSendClarifyMultiChoiceRendersButtonsAndOther(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1234.5678"})
	result, err := a.SendClarify(context.Background(), "C1", "Which environment?", []string{"staging", "production"}, "cid1", "sk1", nil)
	if err != nil || !result.Success || result.MessageID != "1234.5678" {
		t.Fatalf("result %+v err %v", result, err)
	}
	if v, ok := a.clarifyResolved.Get("1234.5678"); !ok || v {
		t.Fatalf("clarify resolved entry %v %v", v, ok)
	}
	blocks := blocksOf(t, f.callsTo("chat.postMessage")[0].Args)
	section := blocks[0].(map[string]any)
	if section["type"] != "section" || !strings.Contains(section["text"].(map[string]any)["text"].(string), "Which environment?") {
		t.Fatalf("section %v", section)
	}
	actions := blocks[1].(map[string]any)
	elements := actions["elements"].([]any)
	if actions["type"] != "actions" || len(elements) != 3 {
		t.Fatalf("actions %v", actions)
	}
	el := func(i int) map[string]any { return elements[i].(map[string]any) }
	if el(0)["action_id"] != "hermes_clarify_choice_0" || el(0)["value"] != "cid1|0" || el(1)["action_id"] != "hermes_clarify_choice_1" ||
		el(1)["value"] != "cid1|1" || el(0)["text"].(map[string]any)["text"] != "staging" || el(2)["action_id"] != "hermes_clarify_other" || el(2)["value"] != "cid1|other" {
		t.Fatalf("elements %v", elements)
	}
	for _, b := range blocks {
		block := b.(map[string]any)
		if block["type"] != "actions" {
			continue
		}
		seen := map[any]bool{}
		for _, e := range block["elements"].([]any) {
			id := e.(map[string]any)["action_id"]
			if seen[id] {
				t.Fatalf("duplicate action id %v", id)
			}
			seen[id] = true
		}
	}
}

func TestSlackSendClarifyMrkdwnEscapesQuestion(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1.1"})
	if _, err := a.SendClarify(context.Background(), "C1", "Use <A> & <B>?", []string{"yes"}, "cid2", "sk2", nil); err != nil {
		t.Fatal(err)
	}
	text := blocksOf(t, f.callsTo("chat.postMessage")[0].Args)[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	if strings.Contains(text, "<A>") || !strings.Contains(text, "&lt;A&gt;") || !strings.Contains(text, "&amp;") {
		t.Fatalf("section %q", text)
	}
}

func TestSlackSendClarifyRetireClarifyCardDropsButtonsAndMakesALateClickANoop(t *testing.T) {
	a, f := approvalAdapter(t)
	attachAuthRunner(a, nil)
	clearClarifyState(t, "sk-retire")
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1.2"})
	tools.RegisterGatewayClarify("cid-retire", "sk-retire", "Which environment?", []string{"staging", "production"}, false)
	ctx := context.Background()
	if _, err := a.SendClarify(ctx, "C1", "Which environment?", []string{"staging", "production"}, "cid-retire", "sk-retire", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.RetireClarifyCard(ctx, "cid-retire", "⏳ expired"); err != nil {
		t.Fatal(err)
	}
	updates := f.callsTo("chat.update")
	if len(updates) != 1 {
		t.Fatalf("chat.update calls %d", len(updates))
	}
	args := updates[0].Args
	if args["channel"] != "C1" || args["ts"] != "1.2" || args["text"] != "⏳ expired" {
		t.Fatalf("update %v", args)
	}
	for _, b := range blocksOf(t, args) {
		if b.(map[string]any)["type"] == "actions" {
			t.Fatal("retired card still carries buttons")
		}
	}
	a.handleClarifyAction(ctx, (&ackRecorder{}).ack, map[string]any{"message": map[string]any{"ts": "1.2", "blocks": args["blocks"]},
		"channel": map[string]any{"id": "C1"}, "user": map[string]any{"name": "norbert", "id": "U_N"}},
		map[string]any{"action_id": "hermes_clarify_choice_0", "value": "cid-retire|0"})
	if n := len(f.callsTo("chat.update")); n != 1 {
		t.Fatalf("late click rewrote the card, chat.update calls %d", n)
	}
	if tools.GetPendingClarify("cid-retire") == nil {
		t.Fatal("a late click must not resolve the clarify")
	}
}

func TestSlackSendClarifyOtherClickKeepsTheCardRetirableUntilTheClarifyEnds(t *testing.T) {
	a, f := approvalAdapter(t)
	attachAuthRunner(a, nil)
	clearClarifyState(t, "sk-other")
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1.2"})
	tools.RegisterGatewayClarify("cid-other", "sk-other", "Which environment?", []string{"staging", "production"}, false)
	ctx := context.Background()
	if _, err := a.SendClarify(ctx, "C1", "Which environment?", []string{"staging", "production"}, "cid-other", "sk-other", nil); err != nil {
		t.Fatal(err)
	}
	a.handleClarifyAction(ctx, (&ackRecorder{}).ack, map[string]any{"message": map[string]any{"ts": "1.2", "blocks": []any{}},
		"channel": map[string]any{"id": "C1"}, "user": map[string]any{"name": "norbert", "id": "U_N"}},
		map[string]any{"action_id": "hermes_clarify_other", "value": "cid-other|other"})
	updates := f.callsTo("chat.update")
	if len(updates) != 1 || !strings.Contains(updates[0].Args["text"].(string), "Awaiting typed answer") {
		t.Fatalf("updates %v", updates)
	}
	tools.ClearClarifySession("sk-other")
	if err := a.RetireClarifyCard(ctx, "cid-other", "⏳ expired"); err != nil {
		t.Fatal(err)
	}
	updates = f.callsTo("chat.update")
	if len(updates) != 2 || updates[1].Args["text"] != "⏳ expired" {
		t.Fatalf("updates %v", updates)
	}
}

func TestSlackClarifyChoiceActionUnauthorizedClickIgnored(t *testing.T) {
	a, _ := approvalAdapter(t)
	attachAuthRunner(a, func(*gateway.SessionSource) bool { return false })
	clearClarifyState(t, "sk-auth")
	tools.RegisterGatewayClarify("cidAuth", "sk-auth", "Pick", []string{"a", "b"}, false)
	a.clarifyResolved.Set("2.2", false)
	a.handleClarifyAction(context.Background(), (&ackRecorder{}).ack, map[string]any{"message": map[string]any{"ts": "2.2", "blocks": []any{}},
		"channel": map[string]any{"id": "C1"}, "user": map[string]any{"name": "mallory", "id": "U_BAD"}},
		map[string]any{"action_id": "hermes_clarify_choice", "value": "cidAuth|0"})
	if tools.GetPendingClarify("cidAuth") == nil {
		t.Fatal("an unauthorized click must leave the clarify pending")
	}
}

func TestSlackClarifyOtherFlowOtherFlipsToTextModeThenTypedReplyResolves(t *testing.T) {
	a, f := approvalAdapter(t)
	attachAuthRunner(a, nil)
	clearClarifyState(t, "sk-other")
	tools.RegisterGatewayClarify("cidO", "sk-other", "Pick", []string{"x", "y"}, false)
	a.clarifyResolved.Set("4.4", false)
	ctx := context.Background()
	a.handleClarifyAction(ctx, (&ackRecorder{}).ack, map[string]any{"message": map[string]any{"ts": "4.4", "blocks": []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "❓ Pick"}},
		map[string]any{"type": "actions", "elements": []any{}}}},
		"channel": map[string]any{"id": "C1"}, "user": map[string]any{"name": "norbert", "id": "U_N"}},
		map[string]any{"action_id": "hermes_clarify_other", "value": "cidO|other"})
	pending := tools.GetPendingClarifyForSession("sk-other", false)
	if pending == nil || pending.ClarifyID != "cidO" || !pending.AwaitingText {
		t.Fatalf("pending %+v", pending)
	}
	if tools.GetPendingClarify("cidO") == nil {
		t.Fatal("the Other click must not resolve the clarify")
	}
	updates := f.callsTo("chat.update")
	if len(updates) == 0 || !strings.Contains(strings.ToLower(updates[len(updates)-1].Args["text"].(string)), "awaiting") {
		t.Fatalf("updates %v", updates)
	}
	if !tools.ResolveClarifyTextResponse("sk-other", "my custom answer") {
		t.Fatal("typed reply must resolve the clarify")
	}
	if tools.GetPendingClarify("cidO") != nil {
		t.Fatal("the clarify must be resolved")
	}
	got := tools.WaitForGatewayClarify(ctx, &tools.Env{Home: t.TempDir()}, "cidO", time.Second)
	if got == nil || *got != "my custom answer" {
		t.Fatalf("response %v", got)
	}
}
