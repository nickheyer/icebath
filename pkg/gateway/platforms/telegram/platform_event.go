package telegram

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"github.com/go-telegram/bot/models"
)

// Update decoded into plain JSON values, absent fields missing like PTB None attributes
func updateFields(update *models.Update) (map[string]any, error) {
	raw, err := json.Marshal(update)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Integer value of a decoded JSON field, false for anything that is not an integer
func jsonInt(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}

// Whether a value is a string or an integer, never a bool or a nested object
func isIDLike(value any) bool {
	if _, ok := value.(string); ok {
		return true
	}
	_, ok := jsonInt(value)
	return ok
}

// Text of an id like value, the Python str of a string or an integer
func idText(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	n, _ := jsonInt(value)
	return strconv.FormatInt(n, 10)
}

// Field of a decoded object, nil when the value is not an object or lacks the key
func field(obj any, key string) any {
	m, ok := obj.(map[string]any)
	if !ok {
		return nil
	}
	return m[key]
}

// Stable gateway_platform_event envelope of a reaction or edited message update, nil for other types
func normalizePlatformEvent(update map[string]any) map[string]any {
	if update["message_reaction"] != nil {
		return normalizeReactionEvent(update)
	}
	if update["edited_message"] != nil {
		return normalizeMessageEditedEvent(update)
	}
	return nil
}

// Reaction envelope with unicode emojis, custom emoji ids and bounded identities
func normalizeReactionEvent(update map[string]any) map[string]any {
	mr := update["message_reaction"]
	if mr == nil {
		return nil
	}
	newReaction := field(mr, "new_reaction")
	reactions, isList := newReaction.([]any)
	if pyTruthy(newReaction) && !isList {
		return nil
	}
	chatID := field(field(mr, "chat"), "id")
	messageID := field(mr, "message_id")
	if !isIDLike(chatID) || !isIDLike(messageID) {
		return nil
	}
	emojis, customEmojiIDs := []string{}, []string{}
	if len(reactions) > 64 {
		reactions = reactions[:64]
	}
	for _, r := range reactions {
		if emoji, ok := field(r, "emoji").(string); ok && emoji != "" {
			emojis = append(emojis, clipRunes(emoji, 64))
		}
		if customID := field(r, "custom_emoji_id"); isIDLike(customID) {
			customEmojiIDs = append(customEmojiIDs, clipRunes(idText(customID), 128))
		}
	}
	return map[string]any{"platform": "telegram", "event_type": "reaction", "payload": map[string]any{
		"emojis": emojis, "custom_emoji_ids": customEmojiIDs, "chat_id": clipRunes(idText(chatID), 128),
		"message_id": clipRunes(idText(messageID), 128), "thread_id": nil}}
}

// Edited message envelope with the bounded edited text or caption and the ISO edit time
func normalizeMessageEditedEvent(update map[string]any) map[string]any {
	message := update["edited_message"]
	if message == nil {
		return nil
	}
	chatID := field(field(message, "chat"), "id")
	messageID := field(message, "message_id")
	if !isIDLike(chatID) || !isIDLike(messageID) {
		return nil
	}
	var text any = field(message, "text")
	if !pyTruthy(text) {
		text = field(message, "caption")
	}
	textStr, isStr := text.(string)
	var threadID any
	if raw := field(message, "message_thread_id"); isIDLike(raw) && pyTruthy(field(message, "is_topic_message")) {
		threadID = clipRunes(idText(raw), 128)
	}
	var editedAt any
	if stamp, ok := jsonInt(field(message, "edit_date")); ok && stamp != 0 {
		editedAt = clipRunes(time.Unix(stamp, 0).UTC().Format("2006-01-02T15:04:05+00:00"), 64)
	}
	var payloadText any
	if isStr {
		payloadText = clipRunes(textStr, 8192)
	}
	return map[string]any{"platform": "telegram", "event_type": "message_edited", "payload": map[string]any{
		"chat_id": clipRunes(idText(chatID), 128), "message_id": clipRunes(idText(messageID), 128), "thread_id": threadID,
		"text": payloadText, "edited_at": editedAt}}
}
