package telegram

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

func TestReactionsFollowTheSettingOfTheProfileInScope(t *testing.T) {
	a := newTestAdapter(t, nil)
	if a.ReactionsEnabled(context.Background()) {
		t.Fatal("reactions must be off until the setting turns them on")
	}
	setSetting(t, settings.TelegramReactions.Key, true)
	if !a.ReactionsEnabled(context.Background()) {
		t.Fatal("the profile's setting must turn reactions on")
	}
	if a.ReactionsEnabled(home.WithOverride(context.Background(), t.TempDir())) {
		t.Fatal("another profile must read its own setting, not the default profile's")
	}
}

func TestSetReactionCallsBotAPI(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	if !a.setReaction(context.Background(), "123", "456", "\U0001f440") {
		t.Fatal("want true")
	}
	calls := fake.callsOf("setMessageReaction")
	if len(calls) != 1 || calls[0].params["chat_id"] != "123" || calls[0].params["message_id"] != "456" ||
		calls[0].params["reaction"] != `[{"type":"emoji","emoji":"👀"}]` {
		t.Fatalf("calls %+v", calls)
	}
}

func TestOnProcessingStartHandlesMissingIDs(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	setSetting(t, settings.TelegramReactions.Key, true)
	event := gateway.NewMessageEvent("hello")
	event.Source = &gateway.SessionSource{}
	if err := a.OnProcessingStart(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.callsOf("setMessageReaction")); n != 0 {
		t.Fatalf("reaction calls %d", n)
	}
}

func TestOnProcessingCompleteCancelledClearsReaction(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	setSetting(t, settings.TelegramReactions.Key, true)
	if err := a.OnProcessingComplete(context.Background(), privateEvent("123", "456"), gateway.ProcessingCancelled); err != nil {
		t.Fatal(err)
	}
	calls := fake.callsOf("setMessageReaction")
	if len(calls) != 1 || calls[0].params["chat_id"] != "123" || calls[0].params["message_id"] != "456" {
		t.Fatalf("calls %+v", calls)
	}
	if reaction, ok := calls[0].params["reaction"]; ok && reaction != "[]" {
		t.Fatalf("clear must send no reaction, got %q", reaction)
	}
}

func TestClearReactionsHandlesAPIErrorGracefully(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	fake.respond = func(string, map[string]string) (int, string) { return botError(400, "Bad Request: no perms") }
	if a.clearReactions(context.Background(), "123", "456") {
		t.Fatal("want false")
	}
}
