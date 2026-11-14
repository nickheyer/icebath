package gateway

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Telegram descriptor advertising every interactive op
func relayInteractiveDesc(d *CapabilityDescriptor) {
	d.SupportedOps = []string{"send", "edit", "typing", "get_chat_info", "send_media", "prompt", "react"}
}

// Prompt answer event of a telegram DM
func relayPromptEvent(promptResponse map[string]any) *MessageEvent {
	e := relayInbound(Platform("telegram"), "c1", "dm", func(s *SessionSource) { s.UserID = relayStrPtr("u1") })
	e.Text, e.MessageType, e.PromptResponse = "/once", MessageTypeCommand, promptResponse
	return e
}

func relayOptionIDs(action map[string]any) []string {
	var ids []string
	for _, o := range action["options"].([]any) {
		ids = append(ids, o.(map[string]any)["id"].(string))
	}
	return ids
}

func relayConsume(t *testing.T, a *RelayAdapter, e *MessageEvent) {
	t.Helper()
	if consumed, err := a.consumePromptResponse(context.Background(), e); err != nil || !consumed {
		t.Fatalf("consumed %v err %v", consumed, err)
	}
}

// Response a resolved clarify handed its waiter
func relayClarifyResponse(clarifyID string) string {
	if r := tools.WaitForGatewayClarify(context.Background(), nil, clarifyID, time.Second); r != nil {
		return *r
	}
	return ""
}

func TestExecApprovalRendersFullOptionSet(t *testing.T) {
	a, c := relayConnectorAdapter(t, relayInteractiveDesc)
	desc := "deletes files"
	result, err := a.SendExecApproval(context.Background(), "c1", "rm -rf /tmp/x", "sess:1", &desc, nil, true, true, false, nil)
	if err != nil || !result.Success || result.MessageID != "pm1" {
		t.Fatalf("result %+v err %v", result, err)
	}
	action, _ := c.last(t)
	content := action["content"].(string)
	if action["op"] != "prompt" || action["prompt_kind"] != "approval" || !slices.Equal(relayOptionIDs(action), []string{"once", "session", "always", "deny"}) ||
		!strings.Contains(content, "rm -rf /tmp/x") || !strings.Contains(content, "deletes files") {
		t.Fatalf("action %v", action)
	}
	state := a.pendingPrompts[action["prompt_id"].(string)]
	if state == nil || state["kind"] != "exec_approval" || state["session_key"] != "sess:1" {
		t.Fatalf("state %v", state)
	}
}

func TestExecApprovalSmartDeniedAndFlagGating(t *testing.T) {
	a, c := relayConnectorAdapter(t, relayInteractiveDesc)
	ctx := context.Background()
	a.SendExecApproval(ctx, "c1", "cmd", "s", nil, nil, true, true, true, nil)
	if action, _ := c.last(t); !slices.Equal(relayOptionIDs(action), []string{"once", "deny"}) {
		t.Fatalf("smart deny options %v", relayOptionIDs(action))
	}
	a.SendExecApproval(ctx, "c1", "cmd", "s", nil, nil, false, true, false, nil)
	if action, _ := c.last(t); !slices.Equal(relayOptionIDs(action), []string{"once", "session", "deny"}) {
		t.Fatalf("session only options %v", relayOptionIDs(action))
	}
}

func TestSlashConfirmRendersThreeOptions(t *testing.T) {
	a, c := relayConnectorAdapter(t, relayInteractiveDesc)
	result, err := a.SendSlashConfirm(context.Background(), "c1", "Reload MCP", "This invalidates the prompt cache.", "sess:1", "cf-9", nil)
	if err != nil || !result.Success {
		t.Fatalf("result %+v err %v", result, err)
	}
	action, _ := c.last(t)
	state := a.pendingPrompts[action["prompt_id"].(string)]
	if !slices.Equal(relayOptionIDs(action), []string{"once", "always", "cancel"}) || !strings.Contains(action["content"].(string), "Reload MCP") ||
		state["kind"] != "slash_confirm" || state["confirm_id"] != "cf-9" || state["session_key"] != "sess:1" {
		t.Fatalf("action %v state %v", action, state)
	}
}

func TestClarifyRendersChoicesPlusOtherWithPositionalIds(t *testing.T) {
	a, c := relayConnectorAdapter(t, relayInteractiveDesc)
	result, err := a.SendClarify(context.Background(), "c1", "Which environment?", []string{"staging — the safe one", "production"}, "cl-1", "sess:1", nil)
	if err != nil || !result.Success {
		t.Fatalf("result %+v err %v", result, err)
	}
	action, _ := c.last(t)
	label := action["options"].([]any)[0].(map[string]any)["label"].(string)
	state := a.pendingPrompts[action["prompt_id"].(string)]
	if action["prompt_kind"] != "clarify" || !slices.Equal(relayOptionIDs(action), []string{"c0", "c1", "other"}) ||
		!strings.HasPrefix(label, "staging") || !relayDeepEqual(state["choices"], []any{"staging — the safe one", "production"}) {
		t.Fatalf("action %v state %v", action, state)
	}
}

func TestPromptResponseResolvesClarifyChoiceAndOther(t *testing.T) {
	a, c := relayConnectorAdapter(t, relayInteractiveDesc)
	ctx := context.Background()
	tools.RegisterGatewayClarify("cl-9", "s", "Which?", []string{"alpha", "beta"}, false)
	a.SendClarify(ctx, "c1", "Which?", []string{"alpha", "beta"}, "cl-9", "s", nil)
	action, _ := c.last(t)
	relayConsume(t, a, relayPromptEvent(map[string]any{"prompt_id": action["prompt_id"], "option_id": "c1"}))
	if got := relayClarifyResponse("cl-9"); got != "beta" {
		t.Fatalf("resolved %q", got)
	}
	entry := tools.RegisterGatewayClarify("cl-10", "s", "Which?", []string{"a"}, false)
	defer tools.ClearClarifySession("s")
	a.SendClarify(ctx, "c1", "Which?", []string{"a"}, "cl-10", "s", nil)
	action, _ = c.last(t)
	relayConsume(t, a, relayPromptEvent(map[string]any{"prompt_id": action["prompt_id"], "option_id": "other"}))
	if pending := tools.GetPendingClarify("cl-10"); pending != entry || !pending.AwaitingText {
		t.Fatalf("cl-10 not marked awaiting text")
	}
}

func TestDiscordComponentInteractionDecodesPromptToken(t *testing.T) {
	a, _ := relayConnectorAdapter(t, relayInteractiveDesc)
	fwd := &PassthroughForward{Platform: "discord", Method: "POST", Path: "/interactions/bot1", Body: []byte(`{"type": 3, "id": "i1", "channel_id": "ch1", "guild_id": "g1",` +
		` "message": {"id": "pm55"}, "member": {"user": {"id": "u1", "username": "ben"}}, "data": {"custom_id": "hp1:a1b2c3d4:deny"}}`)}
	event, err := a.discordInteractionToEvent(fwd)
	if err != nil || event == nil {
		t.Fatalf("event %v err %v", event, err)
	}
	if !relayMetaEqual(event.PromptResponse, map[string]any{"prompt_id": "a1b2c3d4", "option_id": "deny", "prompt_message_id": "pm55"}) ||
		event.Text != "/deny" || event.MessageType != MessageTypeCommand {
		t.Fatalf("event %+v", event)
	}
}

func relayReactableEvent(platform Platform) *MessageEvent {
	e := relayInbound(platform, "ch1", "channel", func(s *SessionSource) { s.UserID, s.MessageID = relayStrPtr("u1"), relayStrPtr("m42") })
	e.Text, e.MessageID = "do something", relayStrPtr("m42")
	return e
}

// Emoji and remove flag of every react frame
func relayReacts(c *relayCapture) [][2]any {
	var out [][2]any
	for _, r := range c.ops("react") {
		out = append(out, [2]any{r["emoji"], r["remove"]})
	}
	return out
}

func relayReactEmojis(c *relayCapture) []any {
	var out []any
	for _, r := range relayReacts(c) {
		out = append(out, r[0])
	}
	return out
}

var relayTelegramAllowedReactions = func() map[string]bool {
	out := map[string]bool{"❤‍\U0001f525": true, "\U0001f468‍\U0001f4bb": true, "\U0001f937‍♂": true, "\U0001f937‍♀": true}
	for _, e := range strings.Fields("❤ 👍 👎 🔥 🥰 👏 😁 🤔 🤯 😱 🤬 😢 🎉 🤩 🤮 💩 🙏 👌 🕊 🤡 🥱 🥴 😍 🐳 🌚 🌭 💯 🤣 ⚡ 🍌 🏆 💔 🤨 😐 🍓 🍾 💋 🖕 😈 😴 😭 🤓 👻 👀 🎃 🙈 😇 😨 🤝 ✍ 🤗 🫡 🎅 🎄 ☃ 💅 🤪 🗿 🆒 💘 🙉 🦄 😘 💊 🙊 😎 👾 🤷 😡") {
		out[e] = true
	}
	return out
}()

func TestProcessingLifecycleReactsEyesThenCheck(t *testing.T) {
	a, c := relayConnectorAdapter(t, relayInteractiveDesc)
	ctx := context.Background()
	event := relayReactableEvent(PlatformDiscord)
	a.OnProcessingStart(ctx, event)
	a.OnProcessingComplete(ctx, event, ProcessingSuccess)
	want := [][2]any{{"\U0001f440", false}, {"\U0001f440", true}, {"✅", false}}
	if got := relayReacts(c); !slices.Equal(got, want) {
		t.Fatalf("reacts %v", got)
	}
	for _, r := range c.ops("react") {
		if r["message_id"] != "m42" || r["chat_id"] != "ch1" {
			t.Fatalf("react %v", r)
		}
	}
}

func TestTelegramAckUsesAnEmojiTelegramActuallyAllows(t *testing.T) {
	for outcome, expected := range map[ProcessingOutcome]string{ProcessingSuccess: "\U0001f44d", ProcessingFailure: "\U0001f44e"} {
		a, c := relayConnectorAdapter(t, relayInteractiveDesc)
		event := relayReactableEvent(PlatformTelegram)
		a.OnProcessingStart(context.Background(), event)
		a.OnProcessingComplete(context.Background(), event, outcome)
		emojis := relayReactEmojis(c)
		if !slices.Equal(emojis, []any{"\U0001f440", "\U0001f440", expected}) {
			t.Fatalf("%s emojis %v", outcome, emojis)
		}
		for _, e := range emojis {
			if !relayTelegramAllowedReactions[e.(string)] {
				t.Fatalf("%q is not in Telegram's ReactionTypeEmoji vocabulary", e)
			}
		}
	}
}

func TestFreeFormReactionPlatformsKeepTheCheckMark(t *testing.T) {
	for _, platform := range []Platform{PlatformSlack, PlatformDiscord, PlatformMatrix, PlatformSignal} {
		a, c := relayConnectorAdapter(t, relayInteractiveDesc)
		a.OnProcessingComplete(context.Background(), relayReactableEvent(platform), ProcessingSuccess)
		if emojis := relayReactEmojis(c); !slices.Equal(emojis, []any{"\U0001f440", "✅"}) {
			t.Fatalf("%s should keep the check mark, got %v", platform, emojis)
		}
	}
}

// Event decoded from a wire frame with the source fields given
func relayWireEvent(t *testing.T, source *pyjson.Object) *MessageEvent {
	t.Helper()
	e, err := relayEventFromWire(context.Background(), pyjson.ObjectOf("text", "hi", "message_id", "m42", "source", source))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAckPlatformFallsBackThroughTheRealWireDecode(t *testing.T) {
	for _, platform := range []any{nil, "relay", "not_a_platform"} {
		a, c := relayConnectorAdapter(t, relayInteractiveDesc)
		source := pyjson.ObjectOf("chat_id", "ch1", "chat_type", "channel", "user_id", "u1")
		if platform != nil {
			source.Set("platform", platform)
		}
		event := relayWireEvent(t, source)
		if event.Source.Platform != PlatformRelay {
			t.Fatalf("%v decoded to %q", platform, event.Source.Platform)
		}
		a.OnProcessingComplete(context.Background(), event, ProcessingSuccess)
		if emojis := relayReactEmojis(c); !slices.Equal(emojis, []any{"\U0001f440", "\U0001f44d"}) {
			t.Fatalf("%v fell back to %v", platform, emojis)
		}
	}
}

func TestAckPlatformPrefersTheChatLaneOverThePrimary(t *testing.T) {
	a, c := relayConnectorAdapter(t, func(d *CapabilityDescriptor) { relayInteractiveDesc(d); d.Platform, d.Label = "slack", "Slack" })
	a.platformByChat["ch1"] = "telegram"
	a.OnProcessingComplete(context.Background(), relayWireEvent(t, pyjson.ObjectOf("chat_id", "ch1", "chat_type", "channel")), ProcessingSuccess)
	if emojis := relayReactEmojis(c); !slices.Equal(emojis, []any{"\U0001f440", "\U0001f44d"}) {
		t.Fatalf("emojis %v", emojis)
	}
}

func TestAckOnAGenuinelyGenericRelayKeepsTheCheckMark(t *testing.T) {
	a, c := relayConnectorAdapter(t, func(d *CapabilityDescriptor) { relayInteractiveDesc(d); d.Platform, d.Label = "relay", "Relay" })
	a.OnProcessingComplete(context.Background(), relayWireEvent(t, pyjson.ObjectOf("chat_id", "ch1", "chat_type", "channel")), ProcessingSuccess)
	if emojis := relayReactEmojis(c); !slices.Equal(emojis, []any{"\U0001f440", "✅"}) {
		t.Fatalf("emojis %v", emojis)
	}
}

func TestSiblingGatewayIgnoresAnotherInstancesPromptAnswer(t *testing.T) {
	owner, ownerStub := relayConnectorAdapter(t, relayInteractiveDesc)
	sibling, siblingStub := relayConnectorAdapter(t, relayInteractiveDesc)
	tools.RegisterGatewayClarify("cl-1", "s", "Which?", []string{"alpha", "beta"}, false)
	defer tools.ClearClarifySession("s")
	owner.SendClarify(context.Background(), "c1", "Which?", []string{"alpha", "beta"}, "cl-1", "s", nil)
	action, _ := ownerStub.last(t)
	event := relayPromptEvent(map[string]any{"prompt_id": action["prompt_id"], "option_id": "c1"})
	relayConsume(t, sibling, event)
	time.Sleep(50 * time.Millisecond)
	if tools.GetPendingClarify("cl-1") == nil || len(siblingStub.ops("send"))+len(siblingStub.ops("prompt")) != 0 {
		t.Fatal("the sibling resolved or answered another instance's prompt")
	}
	relayConsume(t, owner, event)
	if got := relayClarifyResponse("cl-1"); got != "beta" {
		t.Fatalf("resolved %q", got)
	}
}

func TestRepeatAnswerForResolvedPromptIsIgnored(t *testing.T) {
	a, c := relayConnectorAdapter(t, relayInteractiveDesc)
	entry := tools.RegisterGatewayClarify("cl-2", "s", "Which?", []string{"alpha", "beta"}, false)
	defer tools.ClearClarifySession("s")
	a.SendClarify(context.Background(), "c1", "Which?", []string{"alpha", "beta"}, "cl-2", "s", nil)
	action, _ := c.last(t)
	event := relayPromptEvent(map[string]any{"prompt_id": action["prompt_id"], "option_id": "c0"})
	relayConsume(t, a, event)
	if tools.GetPendingClarify("cl-2") != nil || entry == nil {
		t.Fatal("first answer did not resolve")
	}
	relayWaitUntil(func() bool { return len(c.ops("send")) == 1 })
	sentAfterFirst := len(c.ops("send"))
	relayConsume(t, a, event)
	time.Sleep(50 * time.Millisecond)
	if got := relayClarifyResponse("cl-2"); got != "alpha" || len(c.ops("send")) != sentAfterFirst {
		t.Fatalf("resolved %q sends %d", got, len(c.ops("send")))
	}
}

func TestExpiredOwnPromptNotifiesInsteadOfUnknownCommand(t *testing.T) {
	a, c := relayConnectorAdapter(t, relayInteractiveDesc)
	promptID := a.mintPrompt("clarify", map[string]any{"chat_id": "c1"}, -1.0)
	relayConsume(t, a, relayPromptEvent(map[string]any{"prompt_id": promptID, "option_id": "c1"}))
	relayWaitUntil(func() bool { return len(c.ops("send")) >= 1 })
	time.Sleep(50 * time.Millisecond)
	notices := c.ops("send")
	if len(notices) != 1 || !strings.Contains(notices[0]["content"].(string), "no longer waiting") {
		t.Fatalf("notices %v", notices)
	}
}

func TestMintedPromptIdsAreInstanceScopedAndCallbackSafe(t *testing.T) {
	a, _ := relayConnectorAdapter(t, relayInteractiveDesc)
	b, _ := relayConnectorAdapter(t, relayInteractiveDesc)
	idA := a.mintPrompt("clarify", map[string]any{"chat_id": "c1"}, 3600)
	idB := b.mintPrompt("clarify", map[string]any{"chat_id": "c1"}, 3600)
	if !regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,32}$`).MatchString(idA) || len("hp1:"+idA+":option_id_up_to_32_chars_here") > 64 {
		t.Fatalf("id %q", idA)
	}
	if !a.mintedHere(idA) || b.mintedHere(idA) || a.mintedHere(idB) || !a.mintedHere("a1b2c3d4") {
		t.Fatal("prompt ownership is not instance scoped")
	}
}
