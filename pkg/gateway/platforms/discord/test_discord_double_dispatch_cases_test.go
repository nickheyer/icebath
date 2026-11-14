package discord

import (
	"testing"
)

func TestThreadStarterDedupThreadStarterDuplicateDropped(t *testing.T) {
	a, f, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "false")
	t.Setenv("DISCORD_AUTO_THREAD", "true")
	ch := textChannel(t, a, "100")
	f.thread = func(map[string]any) string { return `{"id":"55555","type":11,"parent_id":"100","name":"hello bot"}` }
	m := makeMessage(ch, "hello bot")
	m.ID = "42"
	a.handleDiscordMessage(t.Context(), m, false, false)
	if len(c.all()) != 1 {
		t.Fatalf("Expected handle_message to be called exactly once, got %d", len(c.all()))
	}
	if !a.dedup.IsDuplicate("55555") {
		t.Fatal("Thread starter message (id == thread.id) should be in dedup cache after _auto_create_thread returns")
	}
	if len(c.all()) != 1 {
		t.Fatal("handle_message should only be called once")
	}
}

func TestThreadStarterDedupNoDedupSeedWhenThreadCreationFails(t *testing.T) {
	a, f, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "false")
	t.Setenv("DISCORD_AUTO_THREAD", "true")
	ch := textChannel(t, a, "100")
	f.fail = func(call fakeCall, _ int) (int, string) {
		if call.Method == "POST" && len(call.Path) > 8 && call.Path[len(call.Path)-8:] == "/threads" {
			return 500, `{"message":"boom","code":0}`
		}
		return 0, ""
	}
	m := makeMessage(ch, "hello")
	m.ID = "42"
	a.handleDiscordMessage(t.Context(), m, false, false)
	if len(c.all()) != 0 {
		t.Fatal("agent must not run when thread creation fails")
	}
	if a.dedup.Contains("55555") {
		t.Fatal("thread.id should NOT be pre-seeded when thread creation fails")
	}
}

func TestDirectDoubleDispatchSameMessageIDNotDispatchedTwiceViaDedup(t *testing.T) {
	a, _, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "false")
	t.Setenv("DISCORD_AUTO_THREAD", "false")
	m := makeMessage(textChannel(t, a, "100"), "hello")
	m.ID = "42"
	if a.dedup.IsDuplicate(m.ID) {
		t.Fatal("first delivery marked duplicate")
	}
	a.handleDiscordMessage(t.Context(), m, false, false)
	if len(c.all()) != 1 {
		t.Fatal("handle_message not called once")
	}
	if !a.dedup.IsDuplicate(m.ID) || len(c.all()) != 1 {
		t.Fatal("Second delivery with same message.id must be dropped by dedup")
	}
}

func TestDedupCacheIntegrityPreseedDoesNotBlockLegitimateNewMessages(t *testing.T) {
	a, f, c := inboundAdapter(t)
	t.Setenv("DISCORD_REQUIRE_MENTION", "false")
	t.Setenv("DISCORD_AUTO_THREAD", "true")
	ch := textChannel(t, a, "100")
	f.thread = func(map[string]any) string { return `{"id":"22222","type":11,"parent_id":"100","name":"first"}` }
	first := makeMessage(ch, "first")
	first.ID = "10"
	a.handleDiscordMessage(t.Context(), first, false, false)
	if len(c.all()) != 1 {
		t.Fatalf("dispatched %d", len(c.all()))
	}
	if a.dedup.IsDuplicate("20") {
		t.Fatal("A new message with a different ID should not be blocked")
	}
}
