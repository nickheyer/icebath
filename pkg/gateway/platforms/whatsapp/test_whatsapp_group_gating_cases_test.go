package whatsapp

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Adapter with the policies and allowlists of the given values set on it and their mention settings stored in the home of the test
func gatingAdapter(t *testing.T, values map[string]any) *WhatsAppAdapter {
	t.Helper()
	a := newBareAdapter(t, "/tmp/test-bridge", t.TempDir(), 3000)
	policy := func(key string) string {
		if v, ok := values[key]; ok {
			return strings.ToLower(strings.TrimSpace(v.(string)))
		}
		return "pairing"
	}
	a.DMPolicy, a.GroupPolicy = policy("dm_policy"), policy("group_policy")
	a.AllowFrom, a.GroupAllowFrom = CoerceAllowList(values["allow_from"]), CoerceAllowList(values["group_allow_from"])
	for key, unset := range map[string]any{settings.WhatsappRequireMention.Key: false, settings.WhatsappMentionPatterns.Key: []any{},
		settings.WhatsappFreeResponseChats.Key: []any{}} {
		value, ok := values[strings.TrimPrefix(key, "whatsapp.")]
		if !ok {
			value = unset
		}
		if _, err := settings.Set(context.Background(), key, value, settings.Global); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// Group message payload with the bot ids of the Python harness
func groupMessage(body string, overrides map[string]any) map[string]any {
	data := map[string]any{"isGroup": true, "body": body, "chatId": "120363001234567890@g.us", "mentionedIds": []any{},
		"botIds": []any{"15551230000@s.whatsapp.net", "15551230000@lid"}, "quotedParticipant": ""}
	maps.Copy(data, overrides)
	return data
}

// DM payload of the Python harness
func dmMessage(body string, overrides map[string]any) map[string]any {
	data := map[string]any{"isGroup": false, "body": body, "senderId": "6281234567890@s.whatsapp.net",
		"from": "6281234567890@s.whatsapp.net", "botIds": []any{}, "mentionedIds": []any{}}
	maps.Copy(data, overrides)
	return data
}

// Gate verdict, failing the case on error
func gate(t *testing.T, a *WhatsAppAdapter, data map[string]any) bool {
	t.Helper()
	ok, err := a.ShouldProcessMessage(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestGroupMessagesCanRequireDirectTriggerViaConfig(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"require_mention": true, "group_policy": "open"})
	if gate(t, a, groupMessage("hello everyone", nil)) {
		t.Fatal("unmentioned group message passed")
	}
	if !gate(t, a, groupMessage("hi there", map[string]any{"mentionedIds": []any{"15551230000@s.whatsapp.net"}})) {
		t.Fatal("mention did not pass")
	}
	if !gate(t, a, groupMessage("replying", map[string]any{"quotedParticipant": "15551230000@lid"})) {
		t.Fatal("reply to bot did not pass")
	}
	if !gate(t, a, groupMessage("/status", nil)) {
		t.Fatal("slash command did not pass")
	}
}

func TestRegexMentionPatternsAllowCustomWakeWords(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"require_mention": true, "mention_patterns": []any{`^\s*chompy\b`}, "group_policy": "open"})
	if !gate(t, a, groupMessage("chompy status", nil)) || !gate(t, a, groupMessage("   chompy help", nil)) || gate(t, a, groupMessage("hey chompy", nil)) {
		t.Fatal("wake word gating wrong")
	}
}

func TestInvalidRegexPatternsAreIgnored(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"require_mention": true, "mention_patterns": []any{`(`, `^\s*chompy\b`}, "group_policy": "open"})
	if !gate(t, a, groupMessage("chompy status", nil)) || gate(t, a, groupMessage("hello everyone", nil)) {
		t.Fatal("invalid pattern broke gating")
	}
}

func TestFreeResponseChatsBypassMentionGating(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"require_mention": true, "free_response_chats": []any{"120363001234567890@g.us"}, "group_policy": "open"})
	if !gate(t, a, groupMessage("hello everyone", nil)) {
		t.Fatal("free response chat gated")
	}
}

func TestFreeResponseChatsFollowTheSetting(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"free_response_chats": []any{"123@g.us"}})
	if chats := a.WhatsAppFreeResponseChats(context.Background()); len(chats) != 1 || !chats["123@g.us"] {
		t.Fatalf("chats = %v", chats)
	}
	if _, err := settings.Set(context.Background(), settings.WhatsappFreeResponseChats.Key, []any{}, settings.Global); err != nil {
		t.Fatal(err)
	}
	if chats := a.WhatsAppFreeResponseChats(context.Background()); len(chats) != 0 {
		t.Fatalf("chats = %v", chats)
	}
}

func TestFreeResponseChatsDoesNotBypassOtherGroups(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"require_mention": true, "free_response_chats": []any{"999999999999@g.us"}, "group_policy": "open"})
	if gate(t, a, groupMessage("hello everyone", nil)) {
		t.Fatal("another group's free response leaked")
	}
}

func TestMentionStrippingRemovesBotPhoneFromBody(t *testing.T) {
	data := groupMessage("@15551230000 what is the weather?", nil)
	cleaned := CleanBotMentionText(data["body"].(string), data)
	if strings.Contains(cleaned, "15551230000") || !strings.Contains(cleaned, "weather") {
		t.Fatalf("cleaned = %q", cleaned)
	}
}

func TestDmPolicyDisabledStillAllowsGroups(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"dm_policy": "disabled", "require_mention": false, "group_policy": "open"})
	if !gate(t, a, groupMessage("hello", nil)) {
		t.Fatal("group dropped by the DM policy")
	}
}

func TestStatusBroadcastChatsAreAlwaysDropped(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"dm_policy": "open"})
	if gate(t, a, dmMessage("[video received]", map[string]any{"chatId": "status@broadcast", "senderId": "34612345678@s.whatsapp.net"})) {
		t.Fatal("status broadcast passed")
	}
	if gate(t, a, dmMessage("check out our latest post", map[string]any{"chatId": "120363999999999999@newsletter", "senderId": "120363999999999999@newsletter"})) {
		t.Fatal("newsletter passed")
	}
}

func TestBroadcastFilterRunsBeforeAllowlist(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"dm_policy": "allowlist", "allow_from": []any{"34612345678@s.whatsapp.net"}})
	if gate(t, a, dmMessage("[image received]", map[string]any{"chatId": "status@broadcast", "senderId": "34612345678@s.whatsapp.net"})) {
		t.Fatal("allowlisted status broadcast passed")
	}
}

func TestDeviceQualifiedBotIdsMatchBareMentionAndQuoteIds(t *testing.T) {
	a := gatingAdapter(t, map[string]any{"require_mention": true, "group_policy": "open"})
	device := []any{"447999674698:14@s.whatsapp.net", "116342762025117:14@lid"}
	if !gate(t, a, groupMessage("hi there", map[string]any{"botIds": device, "mentionedIds": []any{"116342762025117@lid"}})) {
		t.Fatal("bare mention of a device qualified bot id missed")
	}
	if !gate(t, a, groupMessage("and this?", map[string]any{"botIds": device, "quotedParticipant": "447999674698@s.whatsapp.net"})) {
		t.Fatal("bare quote of a device qualified bot id missed")
	}
	if gate(t, a, groupMessage("hello everyone", map[string]any{"botIds": device})) {
		t.Fatal("unmentioned message passed")
	}
}
