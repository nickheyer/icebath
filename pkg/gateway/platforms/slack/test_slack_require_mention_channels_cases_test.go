package slack

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// Clears the mention env the autouse fixture deletes
func TestRequireMentionChannelsCSVAndList(t *testing.T) {
	for _, raw := range []any{"C1, C2", []any{"C1", "C2"}} {
		a, _, _ := newHarnessAdapter(t, map[string]any{"require_mention_channels": raw})
		var got []string
		for k := range a.slackRequireMentionChannels(context.Background()) {
			got = append(got, k)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != "C1,C2" {
			t.Fatalf("%v gave %v", raw, got)
		}
	}
}

func TestForcedChannelWakeChecksStillApply(t *testing.T) {
	a, _, captured := newHarnessAdapter(t, map[string]any{"require_mention": false, "require_mention_channels": "C_FORCED"})
	a.mentionedThreads["100.000"] = true
	event := map[string]any{"type": "message", "channel": "C_FORCED", "channel_type": "channel", "user": "U_HUMAN", "text": "follow-up", "ts": "101.000", "thread_ts": "100.000"}
	if err := a.handleSlackMessage(context.Background(), event, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(captured.all()); n != 1 {
		t.Fatalf("dispatched %d", n)
	}
}
