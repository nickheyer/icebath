package telegram

import (
	"context"
	"math"
	"strings"
	"testing"
)

// Typing cooldown deadline of a chat, ok false when the chat is not cooling down
func typingCooldown(a *TelegramAdapter, chatID string) (float64, bool) {
	a.pacer.mu.Lock()
	defer a.pacer.mu.Unlock()
	until, ok := a.pacer.typingCooldownUntil[chatID]
	return until, ok
}

func TestTypingTransientFailureEntersCooldown(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	a.pacer.typingCooldownSeconds = 30
	fake.respond = func(method string, _ map[string]string) (int, string) {
		if method == "sendChatAction" {
			return botError(502, "Bad Gateway: temporary telegram network failure")
		}
		return 0, ""
	}
	ctx := context.Background()
	before := monotonicNow()
	_ = a.SendTyping(ctx, "123", nil)
	_ = a.SendTyping(ctx, "123", nil)
	if n := len(fake.callsOf("sendChatAction")); n != 1 {
		t.Fatalf("sendChatAction calls %d", n)
	}
	until, ok := typingCooldown(a, "123")
	if !ok || math.Abs(until-before-30) > 1 {
		t.Fatalf("cooldown until %v (start %v)", until, before)
	}
	a.pacer.mu.Lock()
	a.pacer.typingCooldownUntil["123"] = monotonicNow() - 1
	a.pacer.mu.Unlock()
	fake.respond = nil
	_ = a.SendTyping(ctx, "123", nil)
	if n := len(fake.callsOf("sendChatAction")); n != 2 {
		t.Fatalf("sendChatAction calls %d", n)
	}
	if _, ok := typingCooldown(a, "123"); ok {
		t.Fatal("a landed typing action must clear the cooldown")
	}
}

func TestTypingDMTopicFallbackSuccessDoesNotCoolDown(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	fake.respond = func(method string, params map[string]string) (int, string) {
		if _, threaded := params["message_thread_id"]; method == "sendChatAction" && threaded {
			return botError(400, "Bad Request: message thread not found")
		}
		return 0, ""
	}
	_ = a.SendTyping(context.Background(), "123", map[string]any{"thread_id": "99", "telegram_dm_topic_reply_fallback": true})
	if n := len(fake.callsOf("sendChatAction")); n != 2 {
		t.Fatalf("sendChatAction calls %d", n)
	}
	if _, ok := typingCooldown(a, "123"); ok {
		t.Fatal("a successful fallback must not cool down")
	}
}

func TestTypingBadThreadFailureDoesNotCoolDown(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	fake.respond = func(method string, _ map[string]string) (int, string) {
		if strings.EqualFold(method, "sendChatAction") {
			return botError(400, "Bad Request: message thread not found")
		}
		return 0, ""
	}
	ctx := context.Background()
	_ = a.SendTyping(ctx, "123", map[string]any{"thread_id": "99"})
	_ = a.SendTyping(ctx, "123", map[string]any{"thread_id": "99"})
	if n := len(fake.callsOf("sendChatAction")); n != 2 {
		t.Fatalf("sendChatAction calls %d", n)
	}
	if _, ok := typingCooldown(a, "123"); ok {
		t.Fatal("a bad thread is not transient")
	}
}
