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
