package provider

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func anthInterleavedReply() map[string]any {
	return anthMessage("tool_use",
		map[string]any{"type": "thinking", "thinking": "Plan: inspect a.py first.", "signature": "sig-AAA"},
		anthText("Reading a.py first."),
		anthToolUse("toolu_1", "read_file", map[string]any{"path": "a.py"}),
		map[string]any{"type": "thinking", "thinking": "Now b.py.", "signature": "sig-BBB"},
		anthToolUse("toolu_2", "read_file", map[string]any{"path": "b.py"}))
}

func anthAssistantWireBlocks(t *testing.T, body map[string]any) []any {
	t.Helper()
	msgs, _ := asList(body["messages"])
	for _, m := range msgs {
		mm, _ := asMap(m)
		if asStr(mm["role"]) != "assistant" {
			continue
		}
		blocks, _ := asList(deepCopyAny(mm["content"]))
		for _, b := range blocks {
			bm, _ := asMap(b)
			delete(bm, "cache_control")
		}
		return blocks
	}
	t.Fatalf("no assistant turn on the wire %v", body["messages"])
	return nil
}

func TestAnthropicContentBlocksSidecarIsLiveOnlyAcrossASessionReload(t *testing.T) {
	s := newAnthServer(t, anthReplyJSON(200, anthInterleavedReply()), anthReplyJSON(200, anthMessage("end_turn", anthText("done"))),
		anthReplyJSON(200, anthMessage("end_turn", anthText("done"))))
	c := s.client(s.route("https://api.anthropic.com", "sk-ant-api03-key"))
	user := &state.Message{Role: "user", Content: "Inspect a.py and b.py."}
	resp, err := c.Complete(t.Context(), &Request{Messages: []*state.Message{user}, MaxTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	anthMustEqualJSON(t, anthJSON(t, string(resp.Message.ReasoningDetails)), anthJSON(t,
		`[{"type":"thinking","thinking":"Plan: inspect a.py first.","signature":"sig-AAA"},{"type":"thinking","thinking":"Now b.py.","signature":"sig-BBB"}]`))
	live := messageToDict(resp.Message)
	ordered := `[{"type":"thinking","thinking":"Plan: inspect a.py first.","signature":"sig-AAA"},{"type":"text","text":"Reading a.py first."},
		{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.py"}},{"type":"thinking","thinking":"Now b.py.","signature":"sig-BBB"},
		{"type":"tool_use","id":"toolu_2","name":"read_file","input":{"path":"b.py"}}]`
	anthMustEqualJSON(t, live["anthropic_content_blocks"], anthJSON(t, ordered))
	if _, has := bedrockMessageDicts([]*state.Message{resp.Message})[0]["anthropic_content_blocks"]; !has {
		t.Fatal("the Converse request view lost the live ordered sidecar")
	}
	shadow, _ := messageShadow(resp.Message)
	if _, has := shadow["reasoning_details"]; has {
		t.Fatalf("estimate priced the unpriced reasoning details %v", shadow)
	}
	anthMustEqualJSON(t, shadow["anthropic_content_blocks"], anthJSON(t, ordered))

	results := []*state.Message{{Role: "tool", ToolCallID: "toolu_1", Content: "a.py: ok"}, {Role: "tool", ToolCallID: "toolu_2", Content: "b.py: ok"}}
	history := append([]*state.Message{user, resp.Message}, results...)
	if _, err := c.Complete(t.Context(), &Request{Messages: history, MaxTokens: 1024}); err != nil {
		t.Fatal(err)
	}
	anthMustEqualJSON(t, anthAssistantWireBlocks(t, s.requests()[1].Body), anthJSON(t, ordered))

	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := stateRoundTrip(t, db, "s1", history)
	if err != nil || len(reloaded) != 4 {
		t.Fatal(reloaded, err)
	}
	if strings.Contains(string(reloaded[1].ReasoningDetails), "tool_use") || strings.Contains(string(reloaded[1].ReasoningDetails), "\"text\"") {
		t.Fatalf("ordered blocks reached the session store %s", reloaded[1].ReasoningDetails)
	}
	if _, has := messageToDict(reloaded[1])["anthropic_content_blocks"]; has {
		t.Fatal("reloaded message carries the live only sidecar")
	}
	if shadow, _ := messageShadow(reloaded[1]); shadow["anthropic_content_blocks"] != nil || shadow["reasoning_details"] != nil {
		t.Fatalf("reloaded estimate priced a replay carrier %v", shadow)
	}
	if _, err := c.Complete(t.Context(), &Request{Messages: reloaded, MaxTokens: 1024}); err != nil {
		t.Fatal(err)
	}
	anthMustEqualJSON(t, anthAssistantWireBlocks(t, s.requests()[2].Body), anthJSON(t,
		`[{"type":"thinking","thinking":"Plan: inspect a.py first.","signature":"sig-AAA"},{"type":"thinking","thinking":"Now b.py.","signature":"sig-BBB"},
		{"type":"text","text":"Reading a.py first."},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.py"}},
		{"type":"tool_use","id":"toolu_2","name":"read_file","input":{"path":"b.py"}}]`))
}

func TestAnthropicUnsignedOrToolFreeTurnsCarryNoOrderedSidecar(t *testing.T) {
	c := &anthropicClient{route: Route{Provider: "anthropic", Model: "claude-opus-4-8"}, transport: newAnthDirectTransport(Route{})}
	for name, msg := range map[string]map[string]any{
		"unsigned thinking": anthMessage("tool_use", map[string]any{"type": "thinking", "thinking": "plan"}, anthToolUse("t1", "x", map[string]any{})),
		"no tool use":       anthMessage("end_turn", map[string]any{"type": "thinking", "thinking": "plan", "signature": "s"}, anthText("hi")),
	} {
		resp, err := c.normalizeResponse(msg, 200, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := anthropicLiveBlocksOf(resp.Message); ok {
			t.Fatalf("%s: ordered sidecar attached", name)
		}
	}
	redacted := anthMessage("tool_use", map[string]any{"type": "redacted_thinking", "data": "opaque"}, anthToolUse("t1", "x", map[string]any{}))
	resp, err := c.normalizeResponse(redacted, 200, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if blocks, ok := anthropicLiveBlocksOf(resp.Message); !ok || len(blocks) != 2 {
		t.Fatalf("redacted thinking with tool use keeps the ordered sidecar %v", blocks)
	}
}

func stateRoundTrip(t *testing.T, db *state.DB, sessionID string, msgs []*state.Message) ([]*state.Message, error) {
	t.Helper()
	dicts := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		dicts[i] = state.MessageToDict(m)
	}
	if _, err := db.AppendMessagesBatch(t.Context(), sessionID, dicts, state.AppendGuards{}, nil); err != nil {
		return nil, err
	}
	rows, err := db.GetMessagesAsConversation(t.Context(), sessionID, state.ConversationOptions{})
	out := make([]*state.Message, len(rows))
	for i, r := range rows {
		out[i] = state.MessageFromDict(r)
	}
	return out, err
}
