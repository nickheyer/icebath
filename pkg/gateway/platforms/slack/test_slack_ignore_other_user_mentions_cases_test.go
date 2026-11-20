package slack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	ignoreBotUserID   = "U_BOT_123"
	ignoreOtherUserID = "U_OTHER_456"
	ignoreChannelID   = "C0AQWDLHY9M"
)

var ignoreSelfUIDs = map[string]bool{ignoreBotUserID: true}

func TestIgnoreOtherUserMentionsDefaultsOff(t *testing.T) {
	t.Setenv("SLACK_IGNORE_OTHER_USER_MENTIONS", "")
	os.Unsetenv("SLACK_IGNORE_OTHER_USER_MENTIONS")
	a, _, _ := newHarnessAdapter(t, nil)
	if a.slackIgnoreOtherUserMentions(context.Background()) {
		t.Fatal("ignore_other_user_mentions must default off")
	}
}

func TestAddressedEmptyAndBlank(t *testing.T) {
	if slackMessageAddressedToOtherUser("", ignoreSelfUIDs) || slackMessageAddressedToOtherUser("   ", ignoreSelfUIDs) {
		t.Fatal("blank text addresses nobody")
	}
}

func TestAddressedChannelAndBroadcastTokensAreNotUsers(t *testing.T) {
	if slackMessageAddressedToOtherUser("<!here> standup in 5", ignoreSelfUIDs) ||
		slackMessageAddressedToOtherUser("<#C0000000000|general> see here", ignoreSelfUIDs) {
		t.Fatal("room tokens address no person")
	}
}

func TestMentionsSelfPlainForm(t *testing.T) {
	if !slackMessageMentionsSelf("hello <@"+ignoreBotUserID+">", ignoreSelfUIDs) {
		t.Fatal("plain self mention missed")
	}
}

func TestMentionsSelfIDPrefixIsNotAMatch(t *testing.T) {
	if slackMessageMentionsSelf("hello <@"+ignoreBotUserID+"X>", ignoreSelfUIDs) {
		t.Fatal("an id prefix must not count as a self mention")
	}
}

// Adapter of the live pipeline cases with the mention env cleared
func ignoreAdapter(t *testing.T) (*SlackAdapter, *capturedEvents) {
	for _, k := range []string{"SLACK_IGNORE_OTHER_USER_MENTIONS", "SLACK_FREE_RESPONSE_CHANNELS", "SLACK_REQUIRE_MENTION"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	a, _, captured := newHarnessAdapter(t, nil)
	bot := ignoreBotUserID
	a.botUserID = &bot
	return a, captured
}

func ignoreEvent(text, ts, threadTS string) map[string]any {
	event := map[string]any{"channel": ignoreChannelID, "channel_type": "channel", "user": "U_HUMAN", "text": text, "ts": ts}
	if threadTS != "" {
		event["thread_ts"] = threadTS
	}
	return event
}

func TestFreeResponseRepliesWhenBotAlsoMentioned(t *testing.T) {
	a, captured := ignoreAdapter(t)
	a.Config.Extra["free_response_channels"] = ignoreChannelID
	a.Config.Extra["ignore_other_user_mentions"] = true
	if err := a.handleSlackMessage(context.Background(), ignoreEvent("<@"+ignoreOtherUserID+"> and <@"+ignoreBotUserID+"> please compare", "1700000000.000003", ""), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(captured.all()); n != 1 {
		t.Fatalf("dispatched %d", n)
	}
}

func TestFreeResponseRepliesWhenBotMentionedInPipeForm(t *testing.T) {
	a, captured := ignoreAdapter(t)
	a.Config.Extra["free_response_channels"] = ignoreChannelID
	a.Config.Extra["ignore_other_user_mentions"] = true
	if err := a.handleSlackMessage(context.Background(), ignoreEvent("<@"+ignoreOtherUserID+"|rasha> and <@"+ignoreBotUserID+"|icebath> please compare", "1700000000.000004", ""), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(captured.all()); n != 1 {
		t.Fatalf("dispatched %d", n)
	}
}

func TestMentionedThreadIgnoresFollowupAddressedToOtherUser(t *testing.T) {
	a, captured := ignoreAdapter(t)
	threadTS := "1700000000.000010"
	a.mentionedThreads[threadTS] = true
	a.Config.Extra["ignore_other_user_mentions"] = true
	if err := a.handleSlackMessage(context.Background(), ignoreEvent("<@"+ignoreOtherUserID+"> check this out", "1700000000.000011", threadTS), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(captured.all()); n != 0 {
		t.Fatalf("dispatched %d", n)
	}
}

// Writes a slack block config.yaml under a fresh home and loads the gateway config through it
func loadSlackYAML(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("slack:\n  ignore_other_user_mentions: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", home)
	prev := plugins.InstalledServices()
	plugins.InstallServices(&plugins.Services{Platforms: gateway.PlatformRegistryService()})
	t.Cleanup(func() { plugins.InstallServices(prev) })
	if _, err := gateway.LoadGatewayConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConfigBridgesSlackIgnoreOtherUserMentions(t *testing.T) {
	t.Setenv("SLACK_IGNORE_OTHER_USER_MENTIONS", "")
	os.Unsetenv("SLACK_IGNORE_OTHER_USER_MENTIONS")
	loadSlackYAML(t)
	if got := os.Getenv("SLACK_IGNORE_OTHER_USER_MENTIONS"); got != "true" {
		t.Fatalf("bridged env %q", got)
	}
}

func TestIgnoreOtherUserMentionsEnvWinsOverYAML(t *testing.T) {
	t.Setenv("SLACK_IGNORE_OTHER_USER_MENTIONS", "false")
	loadSlackYAML(t)
	if got := os.Getenv("SLACK_IGNORE_OTHER_USER_MENTIONS"); got != "false" {
		t.Fatalf("explicit env overwritten to %q", got)
	}
}
