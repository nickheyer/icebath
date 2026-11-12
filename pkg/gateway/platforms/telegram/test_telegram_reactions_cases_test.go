package telegram

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestExplicitEnvWinsOverMaterializedYamlDefault(t *testing.T) {
	a := newTestAdapter(t, map[string]any{"reactions": false})
	t.Setenv("TELEGRAM_REACTIONS", "true")
	if !a.ReactionsEnabled(context.Background()) {
		t.Fatal("explicit env must win over the materialized YAML default")
	}
}

func TestScopedMissDoesNotLeakDefaultProfileEnv(t *testing.T) {
	a := newTestAdapter(t, map[string]any{"reactions": false})
	t.Setenv("TELEGRAM_REACTIONS", "true")
	ctx := provider.WithMultiplexContext(provider.WithSecretScope(context.Background(), map[string]string{"TELEGRAM_BOT_TOKEN": "222:b2"}, ""), true)
	if a.ReactionsEnabled(ctx) {
		t.Fatal("a scoped miss must read this profile's YAML, not the default profile env")
	}
}

func TestSetReactionCallsBotAPI(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	t.Setenv("TELEGRAM_REACTIONS", "true")
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
	t.Setenv("TELEGRAM_REACTIONS", "true")
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
	t.Setenv("TELEGRAM_REACTIONS", "true")
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
	t.Setenv("TELEGRAM_REACTIONS", "true")
	fake.respond = func(string, map[string]string) (int, string) { return botError(400, "Bad Request: no perms") }
	if a.clearReactions(context.Background(), "123", "456") {
		t.Fatal("want false")
	}
}
