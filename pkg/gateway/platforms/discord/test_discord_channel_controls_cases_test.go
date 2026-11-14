package discord

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/i18n"
)

// Thread creation requests the fake API received
func threadPosts(f *fakeDiscordAPI) []fakeCall {
	var out []fakeCall
	for _, c := range f.matching("POST", "/threads") {
		out = append(out, c)
	}
	return out
}

func TestIgnoredChannelBlocksEvenWithMention(t *testing.T) {
	a, _, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "true")
	t.Setenv("DISCORD_IGNORED_CHANNELS", "500")
	bot := a.selfUser()
	a.handleDiscordMessage(t.Context(), makeMessage(textChannel(t, a, "500"), "<@999> hello", bot), false, false)
	if len(c.all()) != 0 {
		t.Fatal("ignored channel dispatched")
	}
}

func TestNonIgnoredChannelProcessesNormally(t *testing.T) {
	a, _, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "false")
	t.Setenv("DISCORD_IGNORED_CHANNELS", "500,600")
	a.handleDiscordMessage(t.Context(), makeMessage(textChannel(t, a, "700"), "hello"), false, false)
	if len(c.all()) != 1 {
		t.Fatalf("dispatched %d", len(c.all()))
	}
}

func TestIgnoredChannelsEmptyStringIgnoresNothing(t *testing.T) {
	a, _, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "false")
	t.Setenv("DISCORD_IGNORED_CHANNELS", "")
	a.handleDiscordMessage(t.Context(), makeMessage(textChannel(t, a, "500"), "hello"), false, false)
	if len(c.all()) != 1 {
		t.Fatalf("dispatched %d", len(c.all()))
	}
}

func TestNoThreadChannelSkipsAutoThread(t *testing.T) {
	a, f, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "false")
	t.Setenv("DISCORD_NO_THREAD_CHANNELS", "800")
	a.handleDiscordMessage(t.Context(), makeMessage(textChannel(t, a, "800"), "hello"), false, false)
	if len(threadPosts(f)) != 0 {
		t.Fatal("auto thread created in a no-thread channel")
	}
	if len(c.all()) != 1 || c.all()[0].Source.ChatType != "group" {
		t.Fatal("expected one group event")
	}
}

func TestAutoThreadFailureSkipsAgentAndNotifiesUser(t *testing.T) {
	a, f, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "false")
	t.Setenv("DISCORD_AUTO_THREAD", "true")
	f.fail = func(call fakeCall, _ int) (int, string) {
		if call.Method == "POST" && strings.HasSuffix(call.Path, "/threads") {
			return 500, `{"message":"boom","code":0}`
		}
		return 0, ""
	}
	a.handleDiscordMessage(t.Context(), makeMessage(textChannel(t, a, "800"), "hello"), false, false)
	if len(c.all()) != 0 {
		t.Fatal("Agent must NOT be invoked when the routing target failed")
	}
	sent := f.sentContents("800")
	visible := i18n.T(t.Context(), "platform.discord.thread.auto_create_failed", "", nil)
	hidden := i18n.T(t.Context(), "platform.discord.thread.auto_create_failed_generic", "", nil)
	if len(threadPosts(f)) == 0 || len(sent) == 0 || (sent[len(sent)-1] != visible && sent[len(sent)-1] != hidden) {
		t.Fatalf("expected thread attempts and a visible notice, sends %q", sent)
	}
}
