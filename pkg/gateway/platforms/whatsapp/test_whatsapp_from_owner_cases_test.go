package whatsapp

import (
	"context"
	"encoding/json"
	"maps"
	"testing"
)

// Owner DM payload of the Python harness with overrides
func ownerPayload(t *testing.T, overrides map[string]any) map[string]any {
	t.Helper()
	data := map[string]any{"messageId": "M1", "chatId": "6281234567890@s.whatsapp.net", "senderId": "6281234567890@s.whatsapp.net",
		"senderName": "Customer", "chatName": "Customer", "isGroup": false, "body": "hi from the linked phone", "hasMedia": false,
		"mediaType": "", "mediaUrls": []any{}, "mentionedIds": []any{}, "quotedParticipant": "", "botIds": []any{}, "timestamp": 0}
	maps.Copy(data, overrides)
	return data
}

// Builds the event of a payload map
func buildFrom(t *testing.T, a *WhatsAppAdapter, data map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(data)
	event := a.buildMessageEvent(context.Background(), payload(t, string(raw)))
	if event == nil {
		t.Fatal("event dropped")
	}
	if event.Metadata["whatsapp_from_owner"] != true {
		t.Fatalf("metadata = %v", event.Metadata)
	}
	return event.Text
}

func TestMetadataFlagSetWhenPayloadHasFromOwner(t *testing.T) {
	if text := buildFrom(t, openAdapter(t), ownerPayload(t, map[string]any{"fromOwner": true})); text != "[owner reply] hi from the linked phone" {
		t.Fatalf("text = %q", text)
	}
}

func TestFromOwnerDoesNotDoublePrefixWhenAlreadyTagged(t *testing.T) {
	if text := buildFrom(t, openAdapter(t), ownerPayload(t, map[string]any{"fromOwner": true, "body": "[owner reply] already tagged"})); text != "[owner reply] already tagged" {
		t.Fatalf("text = %q", text)
	}
}
