package slack

import (
	"context"
	"testing"
)

const (
	wakeBotUserID = "U_BOT_OWN"
	wakeChannelID = "C_incident"
	wakeUserID    = "U_engineer"
	wakeThreadTS  = "1700000000.000100"
)

// Adapter whose legacy wake checks all fail, the post restart state, with the bot root cached when asked
func wakeAdapter(t *testing.T, botAuthoredRoot bool) (*SlackAdapter, *fakeSlackAPI) {
	a, f, _ := newHarnessAdapter(t, map[string]any{"require_mention": true, "strict_mention": false})
	bot := wakeBotUserID
	a.botUserID = &bot
	if botAuthoredRoot {
		a.threadContextCache.Set(threadCacheKey(wakeChannelID, wakeThreadTS, ""),
			&threadContextCache{content: "ctx", fetchedAt: 0, messageCount: 1, parentText: "root", parentUserID: wakeBotUserID})
	}
	return a, f
}

func TestWakeDecisionReturnsFalseWhenNotThreadReply(t *testing.T) {
	a, _ := wakeAdapter(t, true)
	if a.shouldWakeOnUnmentionedMessage(context.Background(), "", wakeChannelID, wakeUserID, false, "", "group") {
		t.Fatal("a top level message must not wake the bot under require_mention")
	}
}

func TestWakeDecisionReturnsTrueWhenBotAuthoredThreadRoot(t *testing.T) {
	a, _ := wakeAdapter(t, true)
	if !a.shouldWakeOnUnmentionedMessage(context.Background(), wakeThreadTS, wakeChannelID, wakeUserID, true, "", "group") {
		t.Fatal("human reply in a thread whose root was bot-posted (not via gateway send) should wake the bot — #63530")
	}
}

func TestBotAuthoredThreadRootFetchesOnCacheMiss(t *testing.T) {
	a, f := wakeAdapter(t, false)
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{
		map[string]any{"ts": wakeThreadTS, "user": wakeBotUserID, "text": "bot-posted root"}}})
	if !a.botAuthoredThreadRoot(context.Background(), wakeChannelID, wakeThreadTS, "") {
		t.Fatal("a fetched root authored by the bot must report true")
	}
	if n := len(f.callsTo("conversations.replies")); n != 1 {
		t.Fatalf("conversations.replies called %d times, want once", n)
	}
}

func TestBotAuthoredThreadRootUsesPerTeamBotID(t *testing.T) {
	a, _ := wakeAdapter(t, false)
	a.teamBotUserIDs = map[string]string{"T2": "U_BOT_T2"}
	key := threadCacheKey(wakeChannelID, wakeThreadTS, "T2")
	a.threadContextCache.Set(key, &threadContextCache{content: "ctx", messageCount: 1, parentText: "root", parentUserID: "U_BOT_T2"})
	if !a.botAuthoredThreadRoot(context.Background(), wakeChannelID, wakeThreadTS, "T2") {
		t.Fatal("the team's own bot id must match the root author")
	}
	entry, _ := a.threadContextCache.Get(key)
	entry.parentUserID = wakeBotUserID
	if a.botAuthoredThreadRoot(context.Background(), wakeChannelID, wakeThreadTS, "T2") {
		t.Fatal("the primary bot id must not count for another workspace")
	}
}
