package telegram

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

// Reaction stand in with an emoji and a custom emoji id, nil standing for an absent attribute
func reactionValue(emoji, customEmojiID any) map[string]any {
	return map[string]any{"emoji": emoji, "custom_emoji_id": customEmojiID}
}

// Update carrying a message_reaction with the reactions
func reactionUpdate(reactions []any, chatID, messageID any) map[string]any {
	return map[string]any{"message_reaction": map[string]any{"chat": map[string]any{"id": chatID}, "message_id": messageID,
		"new_reaction": reactions}}
}

func TestNormalizePlatformEventStandardEmojiReactionNormalized(t *testing.T) {
	got := normalizePlatformEvent(reactionUpdate([]any{reactionValue("\U0001F44E", nil)}, 123, 456))
	want := map[string]any{"platform": "telegram", "event_type": "reaction", "payload": map[string]any{
		"emojis": []string{"\U0001F44E"}, "custom_emoji_ids": []string{}, "chat_id": "123", "message_id": "456", "thread_id": nil}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestNormalizePlatformEventCustomEmojiReactionNormalized(t *testing.T) {
	payload := normalizePlatformEvent(reactionUpdate([]any{reactionValue(nil, "555123")}, 123, 456))["payload"].(map[string]any)
	if len(payload["emojis"].([]string)) != 0 || !reflect.DeepEqual(payload["custom_emoji_ids"], []string{"555123"}) {
		t.Fatalf("got %#v", payload)
	}
}

func TestNormalizePlatformEventMixedReactionsSplitCorrectly(t *testing.T) {
	payload := normalizePlatformEvent(reactionUpdate([]any{reactionValue("\U0001F44D", nil), reactionValue(nil, "555"),
		reactionValue("\U0001F525", nil)}, 123, 456))["payload"].(map[string]any)
	if !reflect.DeepEqual(payload["emojis"], []string{"\U0001F44D", "\U0001F525"}) || !reflect.DeepEqual(payload["custom_emoji_ids"], []string{"555"}) {
		t.Fatalf("got %#v", payload)
	}
}

func TestNormalizePlatformEventMalformedValuesNeverEscapeAsLiveObjects(t *testing.T) {
	event := normalizePlatformEvent(reactionUpdate([]any{reactionValue(map[string]any{}, []any{})}, 123, 456))
	if event == nil {
		t.Fatal("want an event")
	}
	payload := event["payload"].(map[string]any)
	if len(payload["emojis"].([]string)) != 0 || len(payload["custom_emoji_ids"].([]string)) != 0 {
		t.Fatalf("got %#v", payload)
	}
	if _, err := json.Marshal(event); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizePlatformEventReactionCountAndStringLengthsAreBounded(t *testing.T) {
	var reactions []any
	for range 100 {
		reactions = append(reactions, reactionValue(strings.Repeat("x", 100), strings.Repeat("9", 200)))
	}
	payload := normalizePlatformEvent(reactionUpdate(reactions, strings.Repeat("c", 200), strings.Repeat("m", 200)))["payload"].(map[string]any)
	emojis, customs := payload["emojis"].([]string), payload["custom_emoji_ids"].([]string)
	if len(emojis) != 64 || len(customs) != 64 {
		t.Fatalf("got %d %d", len(emojis), len(customs))
	}
	for i := range emojis {
		if len(emojis[i]) != 64 || len(customs[i]) != 128 {
			t.Fatal("unbounded value")
		}
	}
	if len(payload["chat_id"].(string)) != 128 || len(payload["message_id"].(string)) != 128 {
		t.Fatal("unbounded identity")
	}
}

func TestNormalizePlatformEventNonReactionUpdateReturnsNone(t *testing.T) {
	if normalizePlatformEvent(map[string]any{"message_reaction": nil, "edited_message": nil}) != nil {
		t.Fatal("want nil")
	}
}

// Edited message update with the identities and text, a private chat and no topic
func editedUpdate(chatID, messageID, text any) map[string]any {
	return map[string]any{"edited_message": map[string]any{"chat": map[string]any{"id": chatID, "type": "private", "is_forum": false},
		"message_id": messageID, "text": text, "caption": nil, "message_thread_id": nil, "is_topic_message": false,
		"from": map[string]any{"id": 777, "username": "editor"}}}
}

func TestNormalizeMessageEditedEditedMessageNormalized(t *testing.T) {
	got := normalizePlatformEvent(editedUpdate(123, 456, "fixed typo"))
	want := map[string]any{"platform": "telegram", "event_type": "message_edited", "payload": map[string]any{"chat_id": "123",
		"message_id": "456", "thread_id": nil, "text": "fixed typo", "edited_at": nil}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestNormalizeMessageEditedCaptionFallsBackWhenNoText(t *testing.T) {
	update := editedUpdate(123, 456, nil)
	update["edited_message"].(map[string]any)["caption"] = "new caption"
	if got := normalizePlatformEvent(update)["payload"].(map[string]any)["text"]; got != "new caption" {
		t.Fatalf("got %v", got)
	}
}

func TestNormalizeMessageEditedForumTopicThreadIdIncluded(t *testing.T) {
	update := editedUpdate(123, 456, "fixed typo")
	m := update["edited_message"].(map[string]any)
	m["chat"].(map[string]any)["type"], m["chat"].(map[string]any)["is_forum"] = "supergroup", true
	m["message_thread_id"], m["is_topic_message"] = 42, true
	if got := normalizePlatformEvent(update)["payload"].(map[string]any)["thread_id"]; got != "42" {
		t.Fatalf("got %v", got)
	}
}

func TestNormalizeMessageEditedEditDateSerializedIso(t *testing.T) {
	update := editedUpdate(123, 456, "fixed typo")
	update["edited_message"].(map[string]any)["edit_date"] = time.Date(2026, 8, 12, 10, 30, 0, 0, time.UTC).Unix()
	if got := normalizePlatformEvent(update)["payload"].(map[string]any)["edited_at"]; got != "2026-08-12T10:30:00+00:00" {
		t.Fatalf("got %v", got)
	}
}

func TestNormalizeMessageEditedMalformedIdentitiesReturnNone(t *testing.T) {
	if normalizePlatformEvent(editedUpdate(map[string]any{}, 456, "x")) != nil || normalizePlatformEvent(editedUpdate(123, nil, "x")) != nil {
		t.Fatal("malformed identities must yield nil")
	}
}

func TestNormalizeMessageEditedTextIsBoundedAndJsonSafe(t *testing.T) {
	event := normalizePlatformEvent(editedUpdate(123, 456, strings.Repeat("x", 20000)))
	if len(event["payload"].(map[string]any)["text"].(string)) != 8192 {
		t.Fatal("text not bounded")
	}
	if _, err := json.Marshal(event); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizePlatformEventFromSDKUpdate(t *testing.T) {
	fields, err := updateFields(&models.Update{ID: 1, MessageReaction: &models.MessageReactionUpdated{Chat: models.Chat{ID: 123},
		MessageID: 456, NewReaction: []models.ReactionType{{Type: models.ReactionTypeTypeEmoji,
			ReactionTypeEmoji: &models.ReactionTypeEmoji{Type: models.ReactionTypeTypeEmoji, Emoji: "\U0001F44E"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	payload := normalizePlatformEvent(fields)["payload"].(map[string]any)
	if !reflect.DeepEqual(payload["emojis"], []string{"\U0001F44E"}) || payload["chat_id"] != "123" || payload["message_id"] != "456" {
		t.Fatalf("got %#v", payload)
	}
}
