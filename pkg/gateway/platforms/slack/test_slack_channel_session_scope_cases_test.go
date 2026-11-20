package slack

import (
	"context"
	"testing"
)

func scopeEvent(text, ts, threadTS string) map[string]any {
	event := map[string]any{"channel": "C_CHAN", "channel_type": "channel", "user": "U_USER", "text": text, "ts": ts}
	if threadTS != "" {
		event["thread_ts"] = threadTS
	}
	return event
}

// Thread id of the single event the scope case dispatched
func scopeThreadID(t *testing.T, replyInThread bool, event map[string]any) *string {
	t.Helper()
	a, _, captured := newHarnessAdapter(t, map[string]any{"reply_in_thread": replyInThread})
	if err := a.handleSlackMessage(context.Background(), event, nil); err != nil {
		t.Fatal(err)
	}
	got := captured.all()
	if len(got) != 1 {
		t.Fatalf("handler dropped the channel mention, dispatched %d", len(got))
	}
	return got[0].Source.ThreadID
}

func TestChannelSessionScopeDefaultTopLevelMapsToTsWhenReplyInThreadTrue(t *testing.T) {
	if got := scopeThreadID(t, true, scopeEvent("<@U_BOT> hello", "1700000000.000001", "")); deref(got) != "1700000000.000001" {
		t.Fatalf("thread id %v", got)
	}
}

func TestChannelSessionScopeSharedTopLevelMapsToNoneWhenReplyInThreadFalse(t *testing.T) {
	if got := scopeThreadID(t, false, scopeEvent("<@U_BOT> hello", "1700000000.000003", "")); got != nil {
		t.Fatalf("thread id %q", *got)
	}
}

func TestChannelSessionScopeSharedThreadReplyKeyedByThreadTs(t *testing.T) {
	for _, replyInThread := range []bool{true, false} {
		if got := scopeThreadID(t, replyInThread, scopeEvent("<@U_BOT> thread reply", "1700000000.000010", "1700000000.000009")); deref(got) != "1700000000.000009" {
			t.Fatalf("reply_in_thread %v thread id %v", replyInThread, got)
		}
	}
}
