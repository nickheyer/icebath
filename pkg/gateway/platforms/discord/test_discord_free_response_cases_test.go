package discord

import (
	"context"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Ids the tracker state file holds on disk
func trackerFileIDs(t *testing.T, home string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "gateway", "discord_nonconversational_messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range data.([]any) {
		out = append(out, v.(string))
	}
	return out
}

func TestNonConversationalTrackerOffloadMarkManyPersistRunsOffEventLoopThread(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	tracker := newNonConversationalTracker(context.Background(), nonConversationalMaxTracked)
	tracker.markMany(context.Background(), []string{"999"})
	if !tracker.contains("999") {
		t.Fatal("999 not tracked")
	}
	if got := trackerFileIDs(t, home); !slices.Equal(got, []string{"999"}) {
		t.Fatalf("persisted %v", got)
	}
}

func TestNonConversationalTrackerOffloadConcurrentMarkManyPersistsLandInOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	tracker := newNonConversationalTracker(context.Background(), nonConversationalMaxTracked)
	var wg sync.WaitGroup
	for _, id := range []string{"1", "2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			tracker.markMany(context.Background(), []string{id})
		}(id)
	}
	wg.Wait()
	got := trackerFileIDs(t, home)
	slices.Sort(got)
	if !slices.Equal(got, []string{"1", "2"}) {
		t.Fatalf("last write %v", got)
	}
}

func TestNonconversationalFallbackRequiresSelfImprovementEmoji(t *testing.T) {
	if !looksLikeNonconversationalHistoryMessage("💾 Self-improvement review: Memory updated") {
		t.Fatal("status bump not recognized")
	}
	if looksLikeNonconversationalHistoryMessage("Self-improvement review: this is a normal assistant heading") {
		t.Fatal("plain heading treated as status")
	}
}

// One gated dispatch of a message in a channel, returning the thread creations and the events
func gateRun(t *testing.T, env map[string]string, channelID, content string, setup func(*DiscordAdapter, *fakeDiscordAPI)) (*DiscordAdapter, *fakeDiscordAPI, *eventCapture) {
	a, f, c := inboundAdapter(t)
	for k, v := range env {
		t.Setenv(k, v)
	}
	ch := textChannel(t, a, channelID)
	if setup != nil {
		setup(a, f)
	}
	a.handleDiscordMessage(t.Context(), makeMessage(ch, content), false, false)
	return a, f, c
}

func onlyEvent(t *testing.T, c *eventCapture) *gatewayEventView {
	t.Helper()
	all := c.all()
	if len(all) != 1 {
		t.Fatalf("handle_message awaited %d times", len(all))
	}
	ev := all[0]
	return &gatewayEventView{text: ev.Text, chatID: ev.Source.ChatID, chatType: ev.Source.ChatType}
}

type gatewayEventView struct{ text, chatID, chatType string }

func TestDiscordFreeResponseInServerChannels(t *testing.T) {
	_, _, c := gateRun(t, map[string]string{"DISCORD_REQUIRE_MENTION": "false", "DISCORD_AUTO_THREAD": "false"}, "123", "hello from channel", nil)
	if ev := onlyEvent(t, c); ev.text != "hello from channel" || ev.chatID != "123" || ev.chatType != "group" {
		t.Fatalf("%+v", ev)
	}
}

func TestDiscordAcceptsAndStripsBotMentionsWhenRequired(t *testing.T) {
	a, _, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "true")
	t.Setenv("DISCORD_AUTO_THREAD", "false")
	a.handleDiscordMessage(t.Context(), makeMessage(textChannel(t, a, "321"), "<@999> hello with mention", a.selfUser()), false, false)
	if ev := onlyEvent(t, c); ev.text != "hello with mention" {
		t.Fatalf("%+v", ev)
	}
}

func TestDiscordReplyMessageSkipsAutoThread(t *testing.T) {
	a, f, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "true")
	t.Setenv("DISCORD_FREE_RESPONSE_CHANNELS", "123")
	m := makeMessage(textChannel(t, a, "123"), "reply without mention")
	m.Type = discordgo.MessageTypeReply
	a.handleDiscordMessage(t.Context(), m, false, false)
	if len(threadPosts(f)) != 0 {
		t.Fatal("reply auto-threaded")
	}
	if ev := onlyEvent(t, c); ev.text != "reply without mention" || ev.chatID != "123" || ev.chatType != "group" {
		t.Fatalf("%+v", ev)
	}
}

func TestDiscordVoiceLinkedChannelSkipsMentionRequirementAndAutoThread(t *testing.T) {
	_, f, c := gateRun(t, map[string]string{"DISCORD_REQUIRE_MENTION": "true"}, "789", "follow-up from voice text chat",
		func(a *DiscordAdapter, _ *fakeDiscordAPI) { a.voiceTextChannels[111] = 789 })
	if len(threadPosts(f)) != 0 {
		t.Fatal("voice linked channel auto-threaded")
	}
	if ev := onlyEvent(t, c); ev.text != "follow-up from voice text chat" || ev.chatType != "group" {
		t.Fatalf("%+v", ev)
	}
}

func TestDiscordFreeResponseChannelSkipsAutoThread(t *testing.T) {
	_, f, c := gateRun(t, map[string]string{"DISCORD_REQUIRE_MENTION": "true", "DISCORD_FREE_RESPONSE_CHANNELS": "789"}, "789", "casual chat in free-response channel", nil)
	if len(threadPosts(f)) != 0 {
		t.Fatal("free response channel auto-threaded")
	}
	if ev := onlyEvent(t, c); ev.text != "casual chat in free-response channel" || ev.chatType != "group" {
		t.Fatalf("%+v", ev)
	}
}

func TestDiscordFreeResponseAutoThreadOptIn(t *testing.T) {
	_, f, c := gateRun(t, map[string]string{"DISCORD_REQUIRE_MENTION": "true", "DISCORD_FREE_RESPONSE_CHANNELS": "789", "DISCORD_FREE_RESPONSE_AUTO_THREAD": "true"},
		"789", "thread this one please", func(_ *DiscordAdapter, f *fakeDiscordAPI) {
			f.thread = func(map[string]any) string { return `{"id":"456","type":11,"parent_id":"789","name":"auto-thread"}` }
		})
	if len(threadPosts(f)) != 1 {
		t.Fatalf("thread posts %d", len(threadPosts(f)))
	}
	if ev := onlyEvent(t, c); ev.chatType != "thread" || ev.chatID != "456" {
		t.Fatalf("%+v", ev)
	}
}

func TestDiscordNoThreadChannelsWinsOverFreeResponseAutoThread(t *testing.T) {
	a, f, c := gateRun(t, map[string]string{"DISCORD_REQUIRE_MENTION": "true", "DISCORD_FREE_RESPONSE_CHANNELS": "789", "DISCORD_FREE_RESPONSE_AUTO_THREAD": "true"},
		"789", "threaded by opt-in", func(_ *DiscordAdapter, f *fakeDiscordAPI) {
			f.thread = func(map[string]any) string { return `{"id":"456","type":11,"parent_id":"789","name":"t"}` }
		})
	if len(threadPosts(f)) != 1 {
		t.Fatal("opt-in did not thread")
	}
	t.Setenv("DISCORD_NO_THREAD_CHANNELS", "789")
	m := makeMessage(a.stateChannel("789"), "explicitly inline")
	m.ID = "124"
	a.handleDiscordMessage(t.Context(), m, false, false)
	if len(threadPosts(f)) != 1 {
		t.Fatal("no_thread_channels must win")
	}
	if all := c.all(); all[len(all)-1].Source.ChatType != "group" {
		t.Fatal("expected inline group event")
	}
}

func TestDiscordVoiceLinkedChannelIgnoresFreeResponseAutoThread(t *testing.T) {
	_, f, c := gateRun(t, map[string]string{"DISCORD_REQUIRE_MENTION": "true", "DISCORD_FREE_RESPONSE_AUTO_THREAD": "true"}, "789", "voice follow-up",
		func(a *DiscordAdapter, _ *fakeDiscordAPI) { a.voiceTextChannels[111] = 789 })
	if len(threadPosts(f)) != 0 || onlyEvent(t, c).chatType != "group" {
		t.Fatal("voice linked channel must stay unthreaded")
	}
}

func TestDiscordFreeResponseAutoThreadRespectsGlobalDisable(t *testing.T) {
	_, f, c := gateRun(t, map[string]string{"DISCORD_REQUIRE_MENTION": "true", "DISCORD_FREE_RESPONSE_CHANNELS": "789",
		"DISCORD_FREE_RESPONSE_AUTO_THREAD": "true", "DISCORD_AUTO_THREAD": "false"}, "789", "no threads anywhere", nil)
	if len(threadPosts(f)) != 0 || onlyEvent(t, c).chatType != "group" {
		t.Fatal("global disable must win")
	}
}

// History channel 123 served by the fake API
func historyAdapter(t *testing.T, msgs ...*discordgo.Message) (*DiscordAdapter, *discordgo.Channel, *fakeDiscordAPI) {
	a, f, _ := inboundAdapter(t)
	ch := textChannel(t, a, "123")
	f.history = map[string][]*discordgo.Message{"123": msgs}
	return a, ch, f
}

var (
	botSelf  = &discordgo.User{ID: "999", Username: "hermes", Bot: true}
	alice    = &discordgo.User{ID: "56", Username: "Alice", GlobalName: "Alice"}
	geminiBt = &discordgo.User{ID: "55", Username: "Gemini", GlobalName: "Gemini", Bot: true}
)

func TestFetchChannelContextStopsAtSelfMessageAndReversesToChronologicalOrder(t *testing.T) {
	bob := &discordgo.User{ID: "57", Username: "Bob"}
	a, ch, _ := historyAdapter(t, historyMessage(alice, "latest human note", "4"), historyMessage(geminiBt, "latest bot note", "3"),
		historyMessage(botSelf, "our prior response", "2"), historyMessage(bob, "older than boundary", "1"))
	t.Setenv("DISCORD_ALLOW_BOTS", "all")
	a.Config.Extra["history_backfill_limit"] = 10
	got := a.fetchChannelContext(t.Context(), ch, makeMessage(ch, "trigger").Message, "")
	if want := "[Recent channel messages]\n[Gemini [bot]] latest bot note\n[Alice] latest human note"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestFetchChannelContextHydratesAroundReplyTarget(t *testing.T) {
	carol := &discordgo.User{ID: "58", Username: "Carol"}
	a, ch, _ := historyAdapter(t, historyMessage(alice, "latest note", "6"), historyMessage(botSelf, "our prior response", "5"),
		historyMessage(botSelf, "the bot answer being replied to", "3"), historyMessage(carol, "older question", "2"), historyMessage(alice, "even older", "1"))
	t.Setenv("DISCORD_ALLOW_BOTS", "all")
	a.Config.Extra["history_backfill_limit"] = 10
	got := a.fetchChannelContext(t.Context(), ch, makeMessage(ch, "follow-up about that").Message, "3")
	for _, want := range []string{"[Context around the replied-to message]", "the bot answer being replied to", "older question", "[Recent channel messages]", "latest note"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Index(got, "[Context around the replied-to message]") > strings.Index(got, "[Recent channel messages]") {
		t.Fatal("reply context must come first")
	}
}

func TestFetchChannelContextReplyTargetInPrimaryWindowNotDuplicated(t *testing.T) {
	a, ch, _ := historyAdapter(t, historyMessage(alice, "recent reply target", "4"), historyMessage(alice, "another recent", "3"),
		historyMessage(botSelf, "our prior response", "2"))
	t.Setenv("DISCORD_ALLOW_BOTS", "all")
	a.Config.Extra["history_backfill_limit"] = 10
	got := a.fetchChannelContext(t.Context(), ch, makeMessage(ch, "re: that").Message, "4")
	if strings.Contains(got, "[Context around the replied-to message]") || strings.Count(got, "recent reply target") != 1 {
		t.Fatalf("got %q", got)
	}
}

func TestUnmentionedBotChunksJoinRecentTagBatch(t *testing.T) {
	a, _, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "true")
	t.Setenv("DISCORD_AUTO_THREAD", "false")
	t.Setenv("DISCORD_ALLOW_BOTS", "mentions")
	a.textBatchDelaySeconds, a.textBatchSplitDelaySeconds = 0.6, 2.0
	a.SetTextBatchDelays(0.6, 2.0)
	ch := textChannel(t, a, "321")
	clock := 1000.0
	orig := monotonic
	monotonic = func() float64 { return clock }
	t.Cleanup(func() { monotonic = orig })
	bot := &discordgo.User{ID: "42", Username: "Jezza", Bot: true}
	tagged := makeMessage(ch, "<@999> first chunk", a.selfUser())
	tagged.Author = bot
	second := makeMessage(ch, "second chunk")
	second.ID, second.Author = "124", bot
	third := makeMessage(ch, "third chunk")
	third.ID, third.Author = "125", bot
	for n, m := range []*inboundMessage{tagged, second, third} {
		if n > 0 {
			clock += 1.5
		}
		if !a.dispatchDiscordMessage(t.Context(), m) {
			t.Fatalf("chunk %d not admitted", n+1)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a.WaitTextBatches(ctx)
	all := c.all()
	if len(all) != 1 || all[0].Text != "first chunk\nsecond chunk\nthird chunk" {
		t.Fatalf("events %d", len(all))
	}
}

func TestShortTaggedBotChunkWaitsForFollowupWindow(t *testing.T) {
	a, _, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "true")
	t.Setenv("DISCORD_AUTO_THREAD", "false")
	a.textBatchDelaySeconds, a.textBatchSplitDelaySeconds = 0.01, 0.08
	a.SetTextBatchDelays(0.01, 0.08)
	tagged := makeMessage(textChannel(t, a, "321"), "<@999> short chunk", a.selfUser())
	tagged.Author = &discordgo.User{ID: "42", Username: "Jezza", Bot: true}
	a.recordBotTagDebounce(tagged)
	if !a.handleDiscordMessage(t.Context(), tagged, true, false) {
		t.Fatal("not dispatched")
	}
	if len(c.all()) != 0 {
		t.Fatal("handle_message must wait for the batch window")
	}
	key, _ := a.TextBatchKey(t.Context(), &gateway.MessageEvent{Source: a.BuildSource(t.Context(), "321", gateway.SourceFields{ChatType: "group", UserID: strPtr("42")})})
	if pending := a.PendingTextBatch(key); pending == nil || a.TextBatchDelayFor(pending) != 0.08 {
		t.Fatal("tagged bot batch must wait the split delay")
	}
	a.WaitTextBatches(t.Context())
	if len(c.all()) != 1 {
		t.Fatal("handle_message not awaited once")
	}
}
