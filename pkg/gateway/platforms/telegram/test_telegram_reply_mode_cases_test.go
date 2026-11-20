package telegram

import (
	"context"
	"strings"
	"testing"
)

// Adapter of the reply_to_mode attached to a loopback bot
func replyModeAdapter(t *testing.T, mode string) (*TelegramAdapter, *scriptedBot) {
	a, fake := newScriptedAdapter(t, nil)
	if mode != "" {
		a.Config.ReplyToMode = mode
		a.replyToMode = mode
	}
	a.richSendDisabled.Store(true)
	return a, fake
}

func TestDefaultModeIsFirst(t *testing.T) {
	a := newTestAdapter(t, nil)
	if a.replyToMode != "first" {
		t.Fatalf("mode %q", a.replyToMode)
	}
}

func TestReplyToModeConfigOffMode(t *testing.T) {
	cfgAdapter := newTestAdapter(t, nil)
	cfgAdapter.Config.ReplyToMode = "off"
	a := NewTelegramAdapter(context.Background(), cfgAdapter.Config)
	if a.replyToMode != "off" {
		t.Fatalf("mode %q", a.replyToMode)
	}
}

func TestNoReplyToReturnsFalse(t *testing.T) {
	empty := ""
	if shouldThreadReply(nil, 0, "first") || shouldThreadReply(&empty, 0, "first") {
		t.Fatal("no reply target never threads")
	}
}

func TestOffModeNeverThreads(t *testing.T) {
	target := "msg-123"
	for _, index := range []int{0, 1, 5} {
		if shouldThreadReply(&target, index, "off") {
			t.Fatalf("chunk %d threaded in off mode", index)
		}
	}
}

func TestOffModeNoReplyThreading(t *testing.T) {
	a, fake := replyModeAdapter(t, "off")
	if _, err := a.Send(context.Background(), "12345", strings.Repeat("x", 10000), "999", nil); err != nil {
		t.Fatal(err)
	}
	calls := fake.callsOf("sendMessage")
	if len(calls) != 3 {
		t.Fatalf("calls %d", len(calls))
	}
	for i, c := range calls {
		if _, anchored := c.params["reply_parameters"]; anchored {
			t.Fatalf("chunk %d anchored in off mode", i)
		}
	}
}

func TestFirstModeOnlyFirstChunkThreads(t *testing.T) {
	a, fake := replyModeAdapter(t, "first")
	if _, err := a.Send(context.Background(), "12345", strings.Repeat("x", 10000), "999", nil); err != nil {
		t.Fatal(err)
	}
	calls := fake.callsOf("sendMessage")
	if len(calls) != 3 || calls[0].params["reply_parameters"] != `{"message_id":999}` {
		t.Fatalf("calls %+v", calls)
	}
	for _, c := range calls[1:] {
		if _, anchored := c.params["reply_parameters"]; anchored {
			t.Fatal("only the first chunk threads")
		}
	}
}
