package slack

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
)

// Clears the mention env the hermes autouse fixture deletes
func cleanMentionEnv(t *testing.T) {
	for _, k := range []string{"SLACK_REQUIRE_MENTION", "SLACK_REQUIRE_MENTION_CHANNELS", "SLACK_FREE_RESPONSE_CHANNELS", "SLACK_STRICT_MENTION", "SLACK_THREAD_REQUIRE_MENTION"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestRequireMentionChannelsCSVAndList(t *testing.T) {
	cleanMentionEnv(t)
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

func TestYAMLBridgeSetsEnv(t *testing.T) {
	cleanMentionEnv(t)
	if _, err := applyYAMLConfig(context.Background(), map[string]any{}, map[string]any{"require_mention_channels": []any{"C1", "C2"}}); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("SLACK_REQUIRE_MENTION_CHANNELS"); got != "C1,C2" {
		t.Fatalf("bridged env %q", got)
	}
}

func TestForcedChannelWakeChecksStillApply(t *testing.T) {
	cleanMentionEnv(t)
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
