package slack

import (
	"context"
	"testing"
)

const (
	smokeBotUserID  = "U_TARGET_BOT"
	smokePeerUserID = "U_PEER_BOT"
	smokeTeamID     = "T_SMOKE"
	smokeChannelID  = "C_SMOKE"
	smokeThreadTS   = "1700000000.100000"
	smokeReplyTS    = "1700000000.200000"
)

func smokeEvent(text, user, ts, threadTS, botID string) map[string]any {
	event := map[string]any{"text": text, "user": user, "channel": smokeChannelID, "channel_type": "channel", "team": smokeTeamID, "ts": ts}
	if threadTS != "" {
		event["thread_ts"] = threadTS
	}
	if botID != "" {
		event["bot_id"] = botID
	}
	return event
}

// Adapter of the documented peer agent profile after its preflight checks pass
func smokeAdapter(t *testing.T) (*SlackAdapter, *fakeSlackAPI, *capturedEvents) {
	cleanMentionEnv(t)
	a, f, captured := newHarnessAdapter(t, map[string]any{"require_mention": true, "strict_mention": true, "allow_bots": "mentions", "allowed_channels": ""})
	bot := smokeBotUserID
	a.botUserID = &bot
	a.teamBotUserIDs = map[string]string{smokeTeamID: smokeBotUserID}
	ctx := context.Background()
	if !a.slackRequireMention(ctx) || !a.slackStrictMention(ctx) || a.slackAllowBots(ctx) != "mentions" || len(a.slackAllowedChannels(ctx)) != 0 || a.app == nil || a.app.client == nil {
		t.Fatal("peer agent preflight failed")
	}
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{map[string]any{"ts": smokeThreadTS, "user": "U_HUMAN", "text": "Parent mention"}}})
	return a, f, captured
}

func TestSlackPeerAgentSmokeHumanMessageWithCurrentMentionRoutes(t *testing.T) {
	a, f, captured := smokeAdapter(t)
	if err := a.handleSlackMessage(context.Background(), smokeEvent("<@"+smokeBotUserID+"> summarize the deploy status", "U_HUMAN", smokeReplyTS, "", ""), nil); err != nil {
		t.Fatal(err)
	}
	got := captured.all()
	if len(got) != 1 || got[0].Text != "summarize the deploy status" || deref(got[0].Source.ThreadID) != smokeReplyTS {
		t.Fatalf("dispatched %v", got)
	}
	if n := len(f.callsTo("conversations.replies")); n != 0 {
		t.Fatalf("thread context fetched %d times", n)
	}
}

func TestSlackPeerAgentSmokePeerBotWithCurrentExplicitMentionRoutes(t *testing.T) {
	a, f, captured := smokeAdapter(t)
	a.botMessageTS[smokeThreadTS] = true
	if err := a.handleSlackMessage(context.Background(), smokeEvent("<@"+smokeBotUserID+"> please take over this thread", smokePeerUserID, smokeReplyTS, smokeThreadTS, "B_PEER"), nil); err != nil {
		t.Fatal(err)
	}
	got := captured.all()
	if len(got) != 1 || got[0].Text != "please take over this thread" || len(a.mentionedThreads) != 0 {
		t.Fatalf("dispatched %v mentioned %v", got, a.mentionedThreads)
	}
	if n := len(f.callsTo("conversations.replies")); n != 1 {
		t.Fatalf("thread context fetched %d times", n)
	}
}
