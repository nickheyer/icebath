package discord

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
)

type seenEvent struct {
	event  map[string]any
	source *gateway.SessionSource
}

// Adapter whose hook probe reports a subscriber, with a cached guild text channel and thread
func platformEventsAdapter(t *testing.T) *DiscordAdapter {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prev := hasHook
	hasHook = func(context.Context, string) bool { return true }
	t.Cleanup(func() { hasHook = prev })
	a := newTestAdapter(t, nil)
	attachClient(t, a, "1", []string{"999"},
		&discordgo.Channel{ID: "555", GuildID: "999", Type: discordgo.ChannelTypeGuildText},
		&discordgo.Channel{ID: "888", GuildID: "999", ParentID: "555", Type: discordgo.ChannelTypeGuildPublicThread})
	return a
}

func capture(a *DiscordAdapter) *[]seenEvent {
	seen := &[]seenEvent{}
	a.SetPlatformEventHandler(func(_ context.Context, event map[string]any, source *gateway.SessionSource) error {
		*seen = append(*seen, seenEvent{event, source})
		return nil
	})
	return seen
}

func eventMessage(channelID string, bot bool, content string) *discordgo.Message {
	return &discordgo.Message{ID: "456", ChannelID: channelID, GuildID: "999", Content: content,
		Author: &discordgo.User{ID: "777", Bot: bot, Username: "user"}}
}

func threadObj(name, ownerID string) *discordgo.Channel {
	return &discordgo.Channel{ID: "321", Name: name, OwnerID: ownerID, ParentID: "555", GuildID: "999", Type: discordgo.ChannelTypeGuildPublicThread}
}

func TestMessageEditedEditNormalizedAndFired(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformMessageEdit(context.Background(), eventMessage("555", false, "hello world"), eventMessage("555", false, "edited!"))
	if len(*seen) != 1 {
		t.Fatalf("seen %d", len(*seen))
	}
	got := (*seen)[0]
	want := map[string]any{"platform": "discord", "event_type": "message_edited", "payload": map[string]any{
		"chat_id": "555", "message_id": "456", "thread_id": nil, "text": "edited!", "edited_at": nil}}
	if !reflect.DeepEqual(got.event, want) {
		t.Fatalf("event %v", got.event)
	}
	if _, err := json.Marshal(got.event); err != nil {
		t.Fatal(err)
	}
	if got.source.UserID == nil || *got.source.UserID != "777" || got.source.ChatID != "555" {
		t.Fatalf("source %+v", got.source)
	}
}

func TestMessageEditedEditInThreadCarriesThreadID(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformMessageEdit(context.Background(), nil, eventMessage("888", false, "hello world"))
	got := (*seen)[0]
	payload := got.event["payload"].(map[string]any)
	if payload["thread_id"] != "888" || payload["chat_id"] != "888" || got.source.ThreadID == nil || *got.source.ThreadID != "888" {
		t.Fatalf("payload %v source %+v", payload, got.source)
	}
}

func TestMessageEditedBotAuthoredEditDropped(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformMessageEdit(context.Background(), nil, eventMessage("555", true, "hello world"))
	if len(*seen) != 0 {
		t.Fatal("bot edit fired")
	}
}

func TestMessageEditedEditedAtSerialized(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	m := eventMessage("555", false, "hello world")
	ts := time.Date(2026, 8, 12, 10, 30, 0, 0, time.UTC)
	m.EditedTimestamp = &ts
	a.onPlatformMessageEdit(context.Background(), nil, m)
	if got := (*seen)[0].event["payload"].(map[string]any)["edited_at"]; got != "2026-08-12T10:30:00+00:00" {
		t.Fatalf("edited_at %v", got)
	}
}

func TestMessageEditedNoSubscriberSkipsEverything(t *testing.T) {
	a := platformEventsAdapter(t)
	hasHook = func(context.Context, string) bool { return false }
	called := false
	a.SetPlatformEventHandler(func(context.Context, map[string]any, *gateway.SessionSource) error {
		called = true
		return nil
	})
	a.onPlatformMessageEdit(context.Background(), nil, eventMessage("555", false, "hello world"))
	if called {
		t.Fatal("handler awaited without a subscriber")
	}
}

func TestMessageEditedNoGatewayCallbackFailsClosed(t *testing.T) {
	a := platformEventsAdapter(t)
	a.onPlatformMessageEdit(context.Background(), nil, eventMessage("555", false, "hello world"))
}

func TestMessageEditedMissingIdsDrop(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformMessageEdit(context.Background(), nil, eventMessage("", false, "hello world"))
	if len(*seen) != 0 {
		t.Fatal("channelless edit fired")
	}
}

func TestMessageEditedDispatchErrorIsSwallowed(t *testing.T) {
	a := platformEventsAdapter(t)
	a.SetPlatformEventHandler(func(context.Context, map[string]any, *gateway.SessionSource) error {
		return errors.New("plugin boom")
	})
	a.onPlatformMessageEdit(context.Background(), nil, eventMessage("555", false, "hello world"))
}

func TestMessageDeletedDeleteNormalizedAndFired(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformMessageDelete(context.Background(), eventMessage("555", false, "hello world"))
	got := (*seen)[0]
	want := map[string]any{"platform": "discord", "event_type": "message_deleted", "payload": map[string]any{
		"chat_id": "555", "message_id": "456", "thread_id": nil, "author_id": "777"}}
	if !reflect.DeepEqual(got.event, want) || *got.source.UserID != "777" {
		t.Fatalf("event %v", got.event)
	}
}

func TestMessageDeletedBotAuthoredDeleteDropped(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformMessageDelete(context.Background(), eventMessage("555", true, "hello world"))
	if len(*seen) != 0 {
		t.Fatal("bot delete fired")
	}
}

func TestMessageDeletedMissingAuthorFailsClosed(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	m := eventMessage("555", false, "hello world")
	m.Author = nil
	a.onPlatformMessageDelete(context.Background(), m)
	if len(*seen) != 0 {
		t.Fatal("authorless delete fired")
	}
}

func TestThreadCreatedThreadCreateNormalizedAndFired(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformThreadCreate(context.Background(), threadObj("my thread", "777"))
	got := (*seen)[0]
	want := map[string]any{"platform": "discord", "event_type": "thread_created", "payload": map[string]any{
		"thread_id": "321", "parent_chat_id": "555", "name": "my thread", "owner_id": "777"}}
	if !reflect.DeepEqual(got.event, want) || *got.source.ThreadID != "321" || *got.source.UserID != "777" {
		t.Fatalf("event %v", got.event)
	}
}

func TestThreadCreatedMissingOwnerFailsClosed(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformThreadCreate(context.Background(), threadObj("my thread", ""))
	if len(*seen) != 0 {
		t.Fatal("ownerless thread fired")
	}
}

func TestThreadRenamedRenameNormalizedAndFired(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformThreadUpdate(context.Background(), threadObj("old name", "777"), threadObj("new name", "777"))
	want := map[string]any{"platform": "discord", "event_type": "thread_renamed", "payload": map[string]any{
		"thread_id": "321", "parent_chat_id": "555", "old_name": "old name", "new_name": "new name"}}
	if !reflect.DeepEqual((*seen)[0].event, want) {
		t.Fatalf("event %v", (*seen)[0].event)
	}
}

func TestThreadRenamedNonRenameUpdateDropped(t *testing.T) {
	a := platformEventsAdapter(t)
	seen := capture(a)
	a.onPlatformThreadUpdate(context.Background(), threadObj("same", "777"), threadObj("same", "777"))
	if len(*seen) != 0 {
		t.Fatal("non rename fired")
	}
}
