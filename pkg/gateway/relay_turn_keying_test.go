package gateway

import (
	"context"
	"fmt"
	"maps"
	"testing"
)

// Live cards adapter over a transport answering with an increasing message ts
func relayKeyingAdapter(t *testing.T) (*RelayAdapter, *relayCapture) {
	t.Helper()
	a, c := relayCaptureAdapter(t, relayLiveCardsDesc)
	n := 0
	c.result = func(map[string]any) (map[string]any, error) {
		n++
		return map[string]any{"success": true, "message_id": fmt.Sprintf("ts.%d", n)}, nil
	}
	return a, c
}

func relayDraft(t *testing.T, a *RelayAdapter, chatID string, draftID int, content string, md map[string]any) {
	t.Helper()
	if _, err := a.SendDraft(context.Background(), chatID, draftID, content, md); err != nil {
		t.Fatal(err)
	}
}

func relayFinal(t *testing.T, a *RelayAdapter, chatID, content string, md map[string]any) {
	t.Helper()
	if r, err := a.Send(context.Background(), chatID, content, "", maps.Clone(md)); err != nil || !r.Success {
		t.Fatalf("final %+v err %v", r, err)
	}
}

// Draft id and content of every seal frame
func relaySealSet(c *relayCapture) map[[2]any]bool {
	out := map[[2]any]bool{}
	for _, s := range relayFrames(c, "draft", true) {
		out[[2]any{s["draft_id"], s["content"]}] = true
	}
	return out
}

func relayLastOp(t *testing.T, c *relayCapture) any {
	t.Helper()
	last, _ := c.last(t)
	return last["op"]
}

func TestSameThreadParallelTurnsTwoTurnsInOneThreadDoNotCollide(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	mdA := map[string]any{"thread_ts": "1700.100", "message_id": "1700.111"}
	mdB := map[string]any{"thread_ts": "1700.100", "message_id": "1700.222"}
	relayDraft(t, a, "C1", 11, "A partial", mdA)
	relayDraft(t, a, "C1", 12, "B partial", mdB)
	relayFinal(t, a, "C1", "A final", mdA)
	relayFinal(t, a, "C1", "B final", mdB)
	if got := relaySealSet(c); !maps.Equal(got, map[[2]any]bool{{11, "A final"}: true, {12, "B final"}: true}) || len(c.ops("send")) != 0 {
		t.Fatalf("seals %v ops %v", got, c.sent)
	}
}

func TestSameThreadParallelTurnsCardIdsDistinctPerTurnInOneThread(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	tasks := []map[string]string{{"id": "t1", "title": "x", "status": "in_progress"}}
	for _, md := range []map[string]any{{"thread_ts": "1700.100", "message_id": "1700.111"}, {"thread_ts": "1700.100", "message_id": "1700.222"}} {
		a.SendNativeTaskCardProgress(context.Background(), "C1", tasks, "", nil, md, "")
	}
	ids := map[any]bool{}
	for _, card := range c.ops("task_card") {
		ids[card["card_id"]] = true
	}
	if len(ids) != 2 {
		t.Fatalf("cards %v", c.ops("task_card"))
	}
}

func TestSameThreadParallelTurnsCardStopUsesSameKeyAsSend(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	md := map[string]any{"thread_ts": "1700.100", "message_id": "1700.111"}
	a.SendNativeTaskCardProgress(context.Background(), "C1", []map[string]string{{"id": "t1", "title": "x", "status": "in_progress"}}, "", nil, md, "")
	a.StopNativeTaskCardProgress(context.Background(), "C1", nil, md)
	ids := map[any]bool{}
	for _, card := range append(c.ops("task_card"), c.ops("task_card_stop")...) {
		ids[card["card_id"]] = true
	}
	if len(ids) != 1 {
		t.Fatalf("ops %v", c.sent)
	}
}

func TestFlatDmKeyingFlatDmWithoutThreadMetadataStillSeals(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	md := map[string]any{"reply_to_message_id": "evt.1"}
	relayDraft(t, a, "D1", 5, "partial", md)
	relayFinal(t, a, "D1", "final", md)
	if seals := relayFrames(c, "draft", true); len(seals) != 1 || seals[0]["content"] != "final" {
		t.Fatalf("seals %v", seals)
	}
}

func TestFlatDmKeyingParallelFlatDmTurnsKeyedByMessageID(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	mdA, mdB := map[string]any{"reply_to_message_id": "evt.a"}, map[string]any{"reply_to_message_id": "evt.b"}
	relayDraft(t, a, "D1", 21, "A partial", mdA)
	relayDraft(t, a, "D1", 22, "B partial", mdB)
	relayFinal(t, a, "D1", "A final", mdA)
	relayFinal(t, a, "D1", "B final", mdB)
	if got := relaySealSet(c); !maps.Equal(got, map[[2]any]bool{{21, "A final"}: true, {22, "B final"}: true}) {
		t.Fatalf("seals %v", got)
	}
}

func TestLegacyPlacementOnlyCallersSingleOpenStreamAbsorbsIdentitylessFinal(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	relayDraft(t, a, "C1", 31, "partial", map[string]any{"thread_ts": "1700.9", "message_id": "1700.910"})
	relayFinal(t, a, "C1", "final", nil)
	if seals := relayFrames(c, "draft", true); len(seals) != 1 || seals[0]["draft_id"] != 31 {
		t.Fatalf("seals %v", seals)
	}
}

func TestLegacyPlacementOnlyCallersThreadAnchoredPlacementOnlyFinalSeals(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	relayDraft(t, a, "C1", 32, "partial", map[string]any{"thread_ts": "1700.9", "message_id": "1700.910"})
	relayFinal(t, a, "C1", "final", map[string]any{"thread_ts": "1700.9"})
	if seals := relayFrames(c, "draft", true); len(seals) != 1 || seals[0]["draft_id"] != 32 || len(c.ops("send")) != 0 || a.openDraftByChat.size() != 0 {
		t.Fatalf("placement-only final must seal the single open stream: %v", c.sent)
	}
}

func TestLegacyPlacementOnlyCallersMultipleOpenStreamsIdentitylessSendStaysPlain(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	relayDraft(t, a, "C1", 41, "A", map[string]any{"message_id": "m.1"})
	relayDraft(t, a, "C1", 42, "B", map[string]any{"message_id": "m.2"})
	relayFinal(t, a, "C1", "ambiguous final", nil)
	if relayLastOp(t, c) != "send" || len(relayFrames(c, "draft", true)) != 0 {
		t.Fatalf("ops %v", c.sent)
	}
}

func TestLegacyPlacementOnlyCallersPlacementOnlyWithMultipleOpenStreamsStaysPlain(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	relayDraft(t, a, "C1", 43, "A", map[string]any{"thread_ts": "1700.9", "message_id": "m.3"})
	relayDraft(t, a, "C1", 44, "B", map[string]any{"thread_ts": "1700.9", "message_id": "m.4"})
	relayFinal(t, a, "C1", "ambiguous", map[string]any{"thread_ts": "1700.9"})
	if relayLastOp(t, c) != "send" {
		t.Fatalf("ops %v", c.sent)
	}
}

func TestLegacyPlacementOnlyCallersMessageIDMismatchNeverFallsBack(t *testing.T) {
	a, c := relayKeyingAdapter(t)
	relayDraft(t, a, "C1", 45, "A", map[string]any{"message_id": "m.5"})
	relayFinal(t, a, "C1", "other turn", map[string]any{"message_id": "m.OTHER"})
	if relayLastOp(t, c) != "send" || a.openDraftByChat.size() == 0 {
		t.Fatalf("stream must survive the mismatch: %v", c.sent)
	}
}
