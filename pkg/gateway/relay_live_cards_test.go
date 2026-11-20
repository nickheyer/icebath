package gateway

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// Live cards adapter over the scripted connector with the ops overridden when given
func relayLiveCardsAdapter(t *testing.T, ops ...string) (*RelayAdapter, *relayCapture) {
	t.Helper()
	return relayConnectorAdapter(t, func(d *CapabilityDescriptor) {
		relayLiveCardsDesc(d)
		if ops != nil {
			d.SupportedOps = ops
		}
	})
}

// Scripted connector result with the draft op answered by the given result
func relayDraftAnswer(draft map[string]any) func(map[string]any) (map[string]any, error) {
	return func(action map[string]any) (map[string]any, error) {
		if action["op"] == "draft" {
			return draft, nil
		}
		return relayConnectorResult(action)
	}
}

func relayLiveTasks() []map[string]string {
	return []map[string]string{{"id": "call_1", "title": "terminal", "status": "in_progress", "details": "ls -la"},
		{"id": "call_2", "title": "web_search", "status": "complete", "details": "slack startStream"}}
}

func TestDraftOpSendDraftEmitsDraftOp(t *testing.T) {
	a, c := relayLiveCardsAdapter(t)
	r, err := a.SendDraft(context.Background(), "C1", 7, "hel", nil)
	frames := c.ops("draft")
	if err != nil || !r.Success || len(frames) != 1 || frames[0]["chat_id"] != "C1" || frames[0]["draft_id"] != 7 || frames[0]["content"] != "hel" || frames[0]["final"] != false {
		t.Fatalf("result %+v err %v frames %v", r, err, frames)
	}
}

func TestDraftOpSendDraftRequiresDescriptorOp(t *testing.T) {
	a, c := relayLiveCardsAdapter(t, "send", "edit", "typing")
	_, err := a.SendDraft(context.Background(), "C1", 1, "x", nil)
	var pe *PyError
	if !errors.As(err, &pe) || pe.Kind != "NotImplementedError" || len(c.ops("draft")) != 0 {
		t.Fatalf("err %v frames %v", err, c.ops("draft"))
	}
}

func TestDraftOpSupportsDraftStreamingRequiresBothFlagAndOp(t *testing.T) {
	both, _ := relayLiveCardsAdapter(t)
	flagOnly, _ := relayLiveCardsAdapter(t, "send", "edit", "typing")
	legacy, _ := relayLiveCardsAdapter(t, []string{}...)
	legacy.applyDescriptor(func() CapabilityDescriptor { d := legacy.Descriptor(); d.SupportedOps = []string{}; return d }())
	if !both.SupportsDraftStreaming("", nil, "") || flagOnly.SupportsDraftStreaming("", nil, "") || legacy.SupportsDraftStreaming("", nil, "") {
		t.Fatal("draft streaming must need both the flag and the op")
	}
}

func TestDraftOpFinalSendSealsOpenDraft(t *testing.T) {
	a, c := relayLiveCardsAdapter(t)
	ctx := context.Background()
	a.SendDraft(ctx, "C1", 7, "hel", nil)
	a.SendDraft(ctx, "C1", 7, "hello wor", nil)
	c.mu.Lock()
	c.result = relayDraftAnswer(map[string]any{"success": true, "message_id": "1723600000.1"})
	c.mu.Unlock()
	r, err := a.Send(ctx, "C1", "hello world", "", nil)
	finals := relayFrames(c, "draft", true)
	if err != nil || !r.Success || slices.Contains(relayOpsOf(c), "send") || len(finals) != 1 || finals[0]["content"] != "hello world" || r.MessageID != "1723600000.1" {
		t.Fatalf("result %+v err %v ops %v", r, err, c.sent)
	}
}

func TestDraftOpSendWithoutOpenDraftIsNormal(t *testing.T) {
	a, c := relayLiveCardsAdapter(t)
	a.Send(context.Background(), "C1", "plain", "", nil)
	if !slices.Equal(relayOpsOf(c), []any{"send"}) {
		t.Fatalf("ops %v", relayOpsOf(c))
	}
}

func TestDraftOpDraftStateClearsAfterSeal(t *testing.T) {
	a, c := relayLiveCardsAdapter(t)
	ctx := context.Background()
	a.SendDraft(ctx, "C1", 7, "a", nil)
	a.Send(ctx, "C1", "a!", "", nil)
	a.Send(ctx, "C1", "followup", "", nil)
	if !slices.Equal(relayOpsOf(c), []any{"draft", "draft", "send"}) {
		t.Fatalf("ops %v", relayOpsOf(c))
	}
}

func TestDraftOpDraftFailureResultPropagates(t *testing.T) {
	a, c := relayLiveCardsAdapter(t)
	c.result = relayDraftAnswer(map[string]any{"success": false, "error": "stream_gone"})
	ctx := context.Background()
	if r, _ := a.SendDraft(ctx, "C1", 1, "x", nil); r.Success {
		t.Fatal("draft failure reported success")
	}
	a.Send(ctx, "C1", "final", "", nil)
	if ops := relayOpsOf(c); ops[len(ops)-1] != "send" {
		t.Fatalf("ops %v", ops)
	}
}

func TestDraftOpDraftTransportExceptionKeepsInterceptionArmed(t *testing.T) {
	a, c := relayLiveCardsAdapter(t)
	calls := 0
	c.result = func(map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &PyError{Kind: "ConnectionError", Msg: "mid-write drop"}
		}
		return map[string]any{"success": true, "message_id": "ts.9"}, nil
	}
	ctx := context.Background()
	if r, _ := a.SendDraft(ctx, "C1", 2, "x", nil); r.Success {
		t.Fatal("transport exception reported success")
	}
	final, err := a.Send(ctx, "C1", "final", "", nil)
	if err != nil || !final.Success || final.MessageID != "ts.9" {
		t.Fatalf("final %+v err %v", final, err)
	}
}

func TestDraftOpDraftsArePerChat(t *testing.T) {
	a, c := relayLiveCardsAdapter(t)
	a.SendDraft(context.Background(), "C1", 1, "a", nil)
	a.Send(context.Background(), "D9", "other", "", nil)
	if sends := c.ops("send"); len(sends) != 1 || sends[0]["chat_id"] != "D9" {
		t.Fatalf("ops %v", c.sent)
	}
}

func TestTaskCardOpsProgressEmitsTaskCardOp(t *testing.T) {
	a, c := relayLiveCardsAdapter(t)
	r, err := a.SendNativeTaskCardProgress(context.Background(), "C1", relayLiveTasks(), "", relayStrPtr("1700.100"), nil, "")
	frames := c.ops("task_card")
	if err != nil || !r.Success || len(frames) != 1 || frames[0]["card_id"] != "turn:1700.100" {
		t.Fatalf("result %+v err %v frames %v", r, err, frames)
	}
	chunks := frames[0]["chunks"].([]any)
	if len(chunks) != 2 || chunks[0].(map[string]any)["id"] != "call_1" || chunks[1].(map[string]any)["id"] != "call_2" || chunks[1].(map[string]any)["status"] != "complete" {
		t.Fatalf("chunks %v", chunks)
	}
}

func TestTaskCardOpsStopEmitsTaskCardStop(t *testing.T) {
	a, c := relayLiveCardsAdapter(t)
	a.SendNativeTaskCardProgress(context.Background(), "C1", relayLiveTasks(), "", relayStrPtr("1700.100"), nil, "")
	a.StopNativeTaskCardProgress(context.Background(), "C1", relayStrPtr("1700.100"), nil)
	if !slices.Equal(relayOpsOf(c), []any{"task_card", "task_card_stop"}) {
		t.Fatalf("ops %v", relayOpsOf(c))
	}
}

func TestTaskCardOpsTaskCardGatedOnDescriptor(t *testing.T) {
	a, c := relayLiveCardsAdapter(t, "send", "edit", "typing", "draft")
	r, _ := a.SendNativeTaskCardProgress(context.Background(), "C1", relayLiveTasks(), "", relayStrPtr("t"), nil, "")
	if a.SupportsNativeTaskCards() || r.Success || len(c.sent) != 0 {
		t.Fatalf("result %+v sent %v", r, c.sent)
	}
}

func TestTaskCardOpsTaskCardLegacyEmptyOpsNotFailOpen(t *testing.T) {
	legacy, _ := relayLiveCardsAdapter(t)
	legacy.applyDescriptor(func() CapabilityDescriptor { d := legacy.Descriptor(); d.SupportedOps = []string{}; return d }())
	if legacy.SupportsNativeTaskCards() {
		t.Fatal("legacy empty ops must not fail open")
	}
}
