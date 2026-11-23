package slack

import (
	"context"
	"testing"
)

// Adapter of the thread require mention cases, bot UBOT in workspace T1
func threadMentionAdapter(t *testing.T, extra map[string]any) (*SlackAdapter, *capturedEvents) {
	a, _, captured := newHarnessAdapter(t, extra)
	bot := "UBOT"
	a.botUserID = &bot
	a.teamBotUserIDs["T1"] = "UBOT"
	return a, captured
}

func threadMentionEvent(text, ts, threadTS string) map[string]any {
	event := map[string]any{"type": "message", "channel": "C123", "channel_type": "channel", "team": "T1", "user": "U123", "text": text, "ts": ts}
	if threadTS != "" {
		event["thread_ts"] = threadTS
	}
	return event
}

func threadMentionExtra() map[string]any {
	return map[string]any{"allowed_channels": []any{"C123"}, "require_mention": false, "thread_require_mention": true, "reply_in_thread": true}
}

func TestThreadRequireMentionParsesYAML(t *testing.T) {
	ctx := context.Background()
	check := func(extra map[string]any, want bool) {
		t.Helper()
		a, _ := threadMentionAdapter(t, extra)
		if got := a.slackThreadRequireMention(ctx); got != want {
			t.Fatalf("config %v gave %v", extra, got)
		}
	}
	check(nil, false)
	check(map[string]any{"thread_require_mention": "false"}, false)
	check(map[string]any{"thread_require_mention": true}, true)
}

func TestThreadRequireMentionAllowsTopLevelFreeResponse(t *testing.T) {
	a, captured := threadMentionAdapter(t, threadMentionExtra())
	if err := a.handleSlackMessage(context.Background(), threadMentionEvent("vpn is broken", "100.000", ""), nil); err != nil {
		t.Fatal(err)
	}
	got := captured.all()
	if len(got) != 1 || got[0].Text != "vpn is broken" || deref(got[0].Source.ThreadID) != "100.000" {
		t.Fatalf("handled %v", got)
	}
}

func TestThreadRequireMentionBlocksUnmentionedThreadReply(t *testing.T) {
	a, captured := threadMentionAdapter(t, threadMentionExtra())
	if err := a.handleSlackMessage(context.Background(), threadMentionEvent("we found another 403", "101.000", "100.000"), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(captured.all()); n != 0 {
		t.Fatalf("handled %d", n)
	}
}

func TestThreadRequireMentionAllowsMentionedThreadReplyWithoutStickyThread(t *testing.T) {
	a, captured := threadMentionAdapter(t, threadMentionExtra())
	ctx := context.Background()
	if err := a.handleSlackMessage(ctx, threadMentionEvent("<@UBOT> update this", "101.000", "100.000"), nil); err != nil {
		t.Fatal(err)
	}
	got := captured.all()
	if len(got) != 1 || got[0].Text != "update this" {
		t.Fatalf("handled %v", got)
	}
	for k := range a.mentionedThreads {
		if k == "100.000" || k == [2]string{"T1", "100.000"} {
			t.Fatalf("thread became sticky %v", a.mentionedThreads)
		}
	}
	if err := a.handleSlackMessage(ctx, threadMentionEvent("follow-up without mention", "102.000", "100.000"), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(captured.all()); n != 1 {
		t.Fatalf("handled %d", n)
	}
}
