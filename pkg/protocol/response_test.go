package protocol

import (
	"encoding/json"
	"errors"
	"iter"
	"testing"
)

func types(events []Event) []EventType {
	out := make([]EventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func TestBuilderAssemblesTextThinkingAndToolCall(t *testing.T) {
	var b Builder
	b.Start("msg_1", "m")
	b.Start("", "")
	b.OpenThinking("0", false)
	if err := b.Delta("0", "hmm"); err != nil {
		t.Fatal(err)
	}
	if err := b.Signature("0", "sig"); err != nil {
		t.Fatal(err)
	}
	if err := b.Close("0"); err != nil {
		t.Fatal(err)
	}
	b.OpenText("1")
	for _, d := range []string{"Hel", "lo"} {
		if err := b.Delta("1", d); err != nil {
			t.Fatal(err)
		}
	}
	b.OpenToolCall("2", "call_1", "lookup")
	for _, d := range []string{`{"q":`, `"x"}`} {
		if err := b.Delta("2", d); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Finish(StopToolUse, "tool_use", Usage{Input: 3, Output: 4}); err != nil {
		t.Fatal(err)
	}
	events := b.Events()
	want := []EventType{EventStart, EventThinkingStart, EventThinkingDelta, EventThinkingEnd, EventTextStart, EventTextDelta, EventTextDelta, EventToolCallStart, EventToolCallDelta, EventToolCallDelta, EventTextEnd, EventToolCallEnd, EventDone}
	got := types(events)
	if len(got) != len(want) {
		t.Fatalf("events %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s want %s", i, got[i], want[i])
		}
	}
	final := events[len(events)-1].Response
	if final.ID != "msg_1" || final.Model != "m" || final.StopReason != StopToolUse || final.Usage.Output != 4 {
		t.Fatalf("response %+v", final)
	}
	if th := final.Content[0].(Thinking); th.Text != "hmm" || th.Signature != "sig" {
		t.Fatalf("thinking %+v", th)
	}
	if tx := final.Content[1].(Text); tx.Text != "Hello" {
		t.Fatalf("text %+v", tx)
	}
	tc := final.Content[2].(ToolCall)
	if tc.ID != "call_1" || tc.Name != "lookup" || string(tc.Args) != `{"q":"x"}` {
		t.Fatalf("tool call %+v", tc)
	}
	if events[5].Response.Content[1].(Text).Text != "Hel" {
		t.Fatal("event snapshots must not alias later mutations")
	}
	if len(b.Events()) != 0 {
		t.Fatal("events not drained")
	}
}

func TestBuilderRejectsUnknownKeysAndBadArguments(t *testing.T) {
	var b Builder
	if b.Delta("x", "a") == nil || b.Close("x") == nil || b.Signature("x", "s") == nil {
		t.Fatal("unopened block accepted")
	}
	b.OpenText("t")
	if b.Signature("t", "s") == nil {
		t.Fatal("signature on text accepted")
	}
	b.OpenToolCall("c", "id", "f")
	if err := b.Delta("c", `{"a":`); err != nil {
		t.Fatal(err)
	}
	if err := b.Finish(StopEnd, "end_turn", Usage{}); err == nil {
		t.Fatal("truncated arguments accepted")
	}
	var c Builder
	c.OpenToolCall("c", "id", "f")
	if err := c.Close("c"); err != nil {
		t.Fatal(err)
	}
	if string(c.Response.Content[0].(ToolCall).Args) != "{}" {
		t.Fatal("empty arguments must become an empty object")
	}
}

func TestParseArgs(t *testing.T) {
	if _, err := ParseArgs(`[1]`); err == nil {
		t.Fatal("array accepted")
	}
	if got, _ := ParseArgs(" "); string(got) != "{}" {
		t.Fatal("blank")
	}
	if got, err := ParseArgs(`{"a":1}`); err != nil || string(got) != `{"a":1}` {
		t.Fatal("object")
	}
}

func TestCollect(t *testing.T) {
	seq := func(events []Event, err error) iter.Seq2[Event, error] {
		return func(yield func(Event, error) bool) {
			for _, e := range events {
				if !yield(e, nil) {
					return
				}
			}
			if err != nil {
				yield(Event{}, err)
			}
		}
	}
	done := Event{Type: EventDone, Response: Response{ID: "r"}}
	if r, err := Collect(seq([]Event{{Type: EventStart}, done}, nil)); err != nil || r.ID != "r" {
		t.Fatalf("%v %v", r, err)
	}
	if _, err := Collect(seq([]Event{{Type: EventStart}}, nil)); err == nil {
		t.Fatal("missing done accepted")
	}
	boom := errors.New("boom")
	if _, err := Collect(seq(nil, boom)); !errors.Is(err, boom) {
		t.Fatal("error not propagated")
	}
}

func TestMarshalBodyMergesExtra(t *testing.T) {
	type req struct {
		Model string         `json:"model"`
		Cfg   map[string]any `json:"cfg"`
	}
	body, err := MarshalBody(req{Model: "m<1>", Cfg: map[string]any{"a": 1, "b": 2}}, nil)
	if err != nil || string(body) != `{"model":"m<1>","cfg":{"a":1,"b":2}}` {
		t.Fatalf("%s %v", body, err)
	}
	body, err = MarshalBody(req{Model: "m", Cfg: map[string]any{"a": 1, "b": 2}}, map[string]json.RawMessage{
		"cfg":   json.RawMessage(`{"b":3,"c":{"d":12345678901234567890}}`),
		"extra": json.RawMessage(`true`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"cfg":{"a":1,"b":3,"c":{"d":12345678901234567890}},"extra":true,"model":"m"}` {
		t.Fatalf("%s", body)
	}
	if _, err := MarshalBody(req{}, map[string]json.RawMessage{"x": json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid extra accepted")
	}
}

func TestBuilderEmitsStartBeforeAnyBlock(t *testing.T) {
	var b Builder
	b.OpenText("t")
	if err := b.Finish(StopEnd, "stop", Usage{}); err != nil {
		t.Fatal(err)
	}
	got := types(b.Events())
	if got[0] != EventStart || got[len(got)-1] != EventDone {
		t.Fatalf("events %v", got)
	}
	var c Builder
	if err := c.Finish(StopEnd, "stop", Usage{}); err != nil {
		t.Fatal(err)
	}
	if got := types(c.Events()); len(got) != 2 || got[0] != EventStart {
		t.Fatalf("empty finish %v", got)
	}
}

func TestBuilderToolDeltaCarriesPartialArguments(t *testing.T) {
	var b Builder
	b.OpenToolCall("c", "id", "f")
	for _, d := range []string{`{"a":`, `1}`} {
		if err := b.Delta("c", d); err != nil {
			t.Fatal(err)
		}
	}
	events := b.Events()
	if events[2].Partial != `{"a":` || events[3].Partial != `{"a":1}` {
		t.Fatalf("partials %q %q", events[2].Partial, events[3].Partial)
	}
}

func TestBuilderUsageEventAndMetrics(t *testing.T) {
	var b Builder
	b.Usage(Usage{Input: 10})
	b.Metrics(Metrics{Latency: 5})
	if err := b.Finish(StopEnd, "stop", Usage{Input: 10, Output: 2}); err != nil {
		t.Fatal(err)
	}
	events := b.Events()
	if got := types(events); got[0] != EventStart || got[1] != EventUsage {
		t.Fatalf("events %v", got)
	}
	if events[1].Response.Usage.Input != 10 || events[2].Response.Metrics.Latency != 5 || events[2].Response.Usage.Output != 2 {
		t.Fatalf("usage %+v metrics %+v", events[2].Response.Usage, events[2].Response.Metrics)
	}
}

func TestBuilderStampsOriginOnSignedParts(t *testing.T) {
	b := Builder{Origin: AnthropicMessages}
	b.OpenThinking("0", true)
	if err := b.Signature("0", "abc"); err != nil {
		t.Fatal(err)
	}
	b.OpenToolCall("1", "id", "f")
	if err := b.Signature("1", "sig"); err != nil {
		t.Fatal(err)
	}
	b.OpenServerToolCall("2", "srv", "web_search")
	if err := b.Delta("2", `{"query":"x"}`); err != nil {
		t.Fatal(err)
	}
	b.AddServerToolResult(ServerToolResult{CallID: "srv", Name: "web_search", Content: json.RawMessage(`[]`)})
	if err := b.Finish(StopEnd, "end_turn", Usage{}); err != nil {
		t.Fatal(err)
	}
	r := b.Response
	if th := r.Content[0].(Thinking); th.Origin != AnthropicMessages || !th.Redacted || th.Signature != "abc" {
		t.Fatalf("thinking %+v", th)
	}
	if tc := r.Content[1].(ToolCall); tc.Origin != AnthropicMessages || tc.Signature != "sig" {
		t.Fatalf("tool call %+v", tc)
	}
	if sc := r.Content[2].(ServerToolCall); sc.Origin != AnthropicMessages || string(sc.Args) != `{"query":"x"}` {
		t.Fatalf("server call %+v", sc)
	}
	if sr := r.Content[3].(ServerToolResult); sr.Origin != AnthropicMessages || sr.CallID != "srv" {
		t.Fatalf("server result %+v", sr)
	}
	got := types(b.Events())
	want := []EventType{EventStart, EventThinkingStart, EventToolCallStart, EventServerToolCallStart, EventServerToolCallDelta, EventServerToolResult, EventThinkingEnd, EventToolCallEnd, EventServerToolCallEnd, EventDone}
	if len(got) != len(want) {
		t.Fatalf("events %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s want %s", i, got[i], want[i])
		}
	}
	if !Replayable("", OllamaChat) || !Replayable(AnthropicMessages, AnthropicMessages) || Replayable(AnthropicMessages, OllamaChat) {
		t.Fatal("replayable")
	}
}

func TestBuilderCitations(t *testing.T) {
	var b Builder
	b.OpenText("t")
	if err := b.Delta("t", "The sky is blue."); err != nil {
		t.Fatal(err)
	}
	c := Citation{Title: "Sky", URL: "https://example.com", Start: 0, End: 16, Cited: "blue"}
	if err := b.Cite("t", c); err != nil {
		t.Fatal(err)
	}
	b.OpenToolCall("c", "id", "f")
	if b.Cite("c", c) == nil || b.Cite("missing", c) == nil {
		t.Fatal("citation on non text accepted")
	}
	if err := b.Finish(StopEnd, "stop", Usage{}); err != nil {
		t.Fatal(err)
	}
	events := b.Events()
	if events[3].Type != EventCitation || events[3].Citation.Title != "Sky" || events[3].Index != 0 {
		t.Fatalf("citation event %+v", events[3])
	}
	if got := b.Response.Content[0].(Text).Citations; len(got) != 1 || got[0].URL != "https://example.com" {
		t.Fatalf("citations %+v", got)
	}
}

func TestParseArgsRejectsNull(t *testing.T) {
	if _, err := ParseArgs("null"); err == nil {
		t.Fatal("null accepted")
	}
}

func TestResponseHelpersAndUsageMath(t *testing.T) {
	r := Response{Content: []Part{Text{Text: "a"}, ToolCall{ID: "1", Name: "f"}, Text{Text: "b"}, ToolCall{ID: "2", Name: "g"}}}
	if r.Text() != "ab" {
		t.Fatalf("text %q", r.Text())
	}
	if calls := r.ToolCalls(); len(calls) != 2 || calls[1].Name != "g" {
		t.Fatalf("calls %+v", calls)
	}
	u := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, Reasoning: 5}
	if u.Context() != 8 || u.Total() != 10 {
		t.Fatalf("context %d total %d", u.Context(), u.Total())
	}
	if s := u.Add(u); s.Input != 2 || s.Reasoning != 10 || s.CacheWrite != 8 {
		t.Fatalf("sum %+v", s)
	}
}
