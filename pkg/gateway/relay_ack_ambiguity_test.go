package gateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"
)

// Op of an outbound frame with a seal counted as its own key
func relayFrameKey(action map[string]any) string {
	if action["op"] == "draft" && action["final"] == true {
		return "seal"
	}
	return fmt.Sprint(action["op"])
}

// Frames of the capture matching the op and final flag
func relayFrames(c *relayCapture, op string, final bool) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, a := range c.sent {
		if a["op"] == op && (a["final"] == true) == final {
			out = append(out, a)
		}
	}
	return out
}

// Live cards adapter whose transport hangs every seal until cancelled while hanging is on
func relayHangOnSealAdapter(t *testing.T) (*RelayAdapter, *relayCapture, *bool) {
	t.Helper()
	a, c := relayCaptureAdapter(t, relayLiveCardsDesc)
	hangSeal := true
	c.result = func(map[string]any) (map[string]any, error) {
		return map[string]any{"success": true, "message_id": "ts.1"}, nil
	}
	c.hang = func(action map[string]any) bool { return relayFrameKey(action) == "seal" && hangSeal }
	return a, c, &hangSeal
}

// Sends the final under a context cancelled while its seal hangs
func relayCancelledFinal(t *testing.T, a *RelayAdapter, md map[string]any) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Send(ctx, "C1", "complete", "", maps.Clone(md))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled seal returned %v", err)
	}
}

func TestCancelDuringSealCancelMidSealRestoresOpenState(t *testing.T) {
	a, _, _ := relayHangOnSealAdapter(t)
	md := map[string]any{"message_id": "m.1"}
	a.SendDraft(context.Background(), "C1", 11, "partial", md)
	key := relayDraftKey("C1", md)
	relayCancelledFinal(t, a, md)
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.openDraftByChat.get(key); !ok || v != 11 {
		t.Fatal("cancelled seal must restore the open entry so abandon can close it")
	}
	if a.sealedDraftByChat.has(key) {
		t.Fatal("premature tombstone must not survive a cancelled seal")
	}
}

func TestCancelDuringSealAbandonAfterCancelledSealClosesRemoteStream(t *testing.T) {
	a, c, hangSeal := relayHangOnSealAdapter(t)
	md := map[string]any{"message_id": "m.2"}
	a.SendDraft(context.Background(), "C1", 12, "partial on screen", md)
	relayCancelledFinal(t, a, md)
	c.mu.Lock()
	*hangSeal = false
	c.mu.Unlock()
	r, err := a.AbandonOpenDraft(context.Background(), "C1", "partial on screen", maps.Clone(md))
	seals := relayFrames(c, "draft", true)
	if err != nil || !r.Success || seals[len(seals)-1]["content"] != "partial on screen" || a.openDraftByChat.size() != 0 {
		t.Fatalf("result %+v err %v seals %v", r, err, seals)
	}
}

// Live cards adapter whose transport loses the acks of the keys, a count limiting the losses per key
func relayAckLossAdapter(t *testing.T, loseTimes map[string]int, loseAlways ...string) (*RelayAdapter, *relayCapture) {
	t.Helper()
	a, c := relayCaptureAdapter(t, relayLiveCardsDesc)
	n := 0
	c.result = func(action map[string]any) (map[string]any, error) {
		key := relayFrameKey(action)
		lost := false
		for _, k := range loseAlways {
			lost = lost || k == key
		}
		if remaining, ok := loseTimes[key]; ok && remaining > 0 {
			loseTimes[key] = remaining - 1
			lost = true
		}
		if lost {
			return map[string]any{"success": false, "error": "relay outbound timed out", "ambiguous": true}, nil
		}
		n++
		return map[string]any{"success": true, "message_id": fmt.Sprintf("ts.%d", n)}, nil
	}
	return a, c
}

func TestSealAckLossLostSealAckRetriesIdempotentFrame(t *testing.T) {
	a, c := relayAckLossAdapter(t, map[string]int{"seal": 1})
	md := map[string]any{"message_id": "m.1"}
	a.SendDraft(context.Background(), "C1", 7, "partial", md)
	r, err := a.Send(context.Background(), "C1", "complete", "", maps.Clone(md))
	if err != nil || !r.Success || len(relayFrames(c, "draft", true)) != 2 || len(c.ops("send")) != 0 {
		t.Fatalf("result %+v err %v ops %v", r, err, c.sent)
	}
}

func TestSealAckLossSealAckLostTwiceReportsAmbiguousFailure(t *testing.T) {
	a, c := relayAckLossAdapter(t, nil, "seal")
	md := map[string]any{"message_id": "m.2"}
	a.SendDraft(context.Background(), "C1", 8, "partial", md)
	r, err := a.Send(context.Background(), "C1", "complete", "", maps.Clone(md))
	if err != nil || !r.Success || len(relayFrames(c, "draft", true)) == 0 || len(c.ops("send")) == 0 {
		t.Fatalf("result %+v err %v ops %v", r, err, c.sent)
	}
}

func TestFrameAckLossLostFrameAckKeepsInterceptionArmed(t *testing.T) {
	a, c := relayAckLossAdapter(t, map[string]int{"draft": 1})
	md := map[string]any{"message_id": "m.3"}
	r1, _ := a.SendDraft(context.Background(), "C1", 9, "partial", md)
	if r1.Success {
		t.Fatal("lost frame ack reported success")
	}
	if v, ok := a.openDraftByChat.get(relayDraftKey("C1", md)); !ok || v != 9 {
		t.Fatal("ambiguous frame result disarmed interception")
	}
	final, err := a.Send(context.Background(), "C1", "complete", "", maps.Clone(md))
	seals := relayFrames(c, "draft", true)
	if err != nil || !final.Success || len(seals) != 1 || seals[0]["content"] != "complete" {
		t.Fatalf("final %+v err %v seals %v", final, err, seals)
	}
}

func TestFrameAckLossDefiniteRejectionStillDisarms(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayLiveCardsDesc)
	c.result = func(action map[string]any) (map[string]any, error) {
		if action["op"] == "draft" {
			return map[string]any{"success": false, "error": "stream_gone"}, nil
		}
		return map[string]any{"success": true, "message_id": "m"}, nil
	}
	md := map[string]any{"message_id": "m.4"}
	r, _ := a.SendDraft(context.Background(), "C1", 10, "partial", md)
	if r.Success || a.openDraftByChat.size() != 0 {
		t.Fatal("definite rejection must disarm")
	}
	a.Send(context.Background(), "C1", "final", "", maps.Clone(md))
	if last, _ := c.last(t); last["op"] != "send" || last["final"] == true {
		t.Fatalf("last %v", last)
	}
}

func TestTransportTagsAmbiguityWsTransportTimeoutResultIsTagged(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv, _ := newRelayWSServerSilent(t)
	tr := relayConnectedTransport(t, srv.url, RelayTransportOptions{OutboundTimeoutS: relayFloatPtr(0.01)})
	defer relayDisconnectNow(tr)
	result, err := tr.SendOutbound(context.Background(), map[string]any{"op": "send"}, "")
	if err != nil || result["success"] != false || result["ambiguous"] != true {
		t.Fatalf("result %v err %v", result, err)
	}
	tr.mu.Lock()
	tr.closing = true
	tr.mu.Unlock()
	result2, err := tr.SendOutbound(context.Background(), map[string]any{"op": "send"}, "")
	if _, has := result2["ambiguous"]; err != nil || result2["success"] != false || has {
		t.Fatalf("fail-fast paths are definite: %v err %v", result2, err)
	}
}
