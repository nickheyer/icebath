package slack

import (
	"context"
	"testing"
)

const replyThread = "1700000000.000010"

func replyMessage(text, ts string, extra map[string]any) map[string]any {
	m := map[string]any{"channel": ignoreChannelID, "channel_type": "channel", "user": "U_HUMAN", "text": text, "ts": ts}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestAdmittedMessageCarriesWhetherItWasAddressed(t *testing.T) {
	yes, no := true, false
	dm := map[string]any{"channel": "D0001", "channel_type": "im"}
	inThread := map[string]any{"thread_ts": replyThread}
	cases := []struct {
		id    string
		extra map[string]any
		event map[string]any
		want  *bool
	}{
		{"dm", nil, replyMessage("hi", "1.1", dm), &yes},
		{"mention", nil, replyMessage("<@"+ignoreBotUserID+"> hi", "1.2", nil), &yes},
		{"reaction", nil, replyMessage("reaction:added:eyes", "1.3", map[string]any{"thread_ts": replyThread, "_hermes_force_process": true}), &yes},
		{"thread-followup", nil, replyMessage("done?", "1.4", inThread), nil},
		{"peer-addressed", nil, replyMessage("<@"+ignoreOtherUserID+"> can you check?", "1.5", inThread), &no},
		{"free-channel-top-level", map[string]any{"free_response_channels": ignoreChannelID}, replyMessage("side chatter", "1.6", nil), &no},
		{"flat-channel-followup", map[string]any{"free_response_channels": ignoreChannelID, "reply_in_thread": false}, replyMessage("any update?", "1.7", nil), nil},
		{"peer-led-but-mentions-bot", nil, replyMessage("<@"+ignoreOtherUserID+"> and <@"+ignoreBotUserID+"|icebath> both look", "1.8", inThread), &yes},
		{"thread-followup-silence-on", map[string]any{"thread_followup_silence": true}, replyMessage("done?", "1.9", inThread), &no},
		{"thread-followup-silence-off", map[string]any{"thread_followup_silence": false}, replyMessage("done?", "1.10", inThread), nil},
		{"thread-followup-silence-on-mention", map[string]any{"thread_followup_silence": true}, replyMessage("<@"+ignoreBotUserID+"> done?", "1.11", inThread), &yes},
		{"thread-followup-silence-on-dm", map[string]any{"thread_followup_silence": true}, replyMessage("hi", "1.12", map[string]any{"channel": "D0001", "channel_type": "im", "thread_ts": replyThread}), &yes},
		{"thread-followup-silence-on-flat-channel", map[string]any{"thread_followup_silence": true, "free_response_channels": ignoreChannelID, "reply_in_thread": false},
			replyMessage("any update?", "1.13", nil), nil},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			a, captured := ignoreAdapter(t)
			for k, v := range c.extra {
				a.Config.Extra[k] = v
			}
			a.mentionedThreads[replyThread] = true
			if err := a.handleSlackMessage(context.Background(), c.event, nil); err != nil {
				t.Fatal(err)
			}
			got := captured.all()
			if len(got) != 1 {
				t.Fatalf("dispatched %d", len(got))
			}
			if (got[0].ReplyExpected == nil) != (c.want == nil) || (c.want != nil && *got[0].ReplyExpected != *c.want) {
				t.Fatalf("reply_expected %v want %v", got[0].ReplyExpected, c.want)
			}
		})
	}
}
