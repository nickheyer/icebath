package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func replayUser(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

func replayAssistantTC(name string) map[string]any {
	return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": "{}"}},
	}}
}

func replayTool(content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": "c1", "content": content}
}

func TestMixedDanglingBatchUsesTruthfulPerCallWording(t *testing.T) {
	assistant := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"id": "read", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
		map[string]any{"id": "write", "function": map[string]any{"name": "write_file", "arguments": "{}"}},
	}}
	out := StripDanglingToolCallTail([]map[string]any{replayUser("hi"), assistant})
	read, write := out[len(out)-2], out[len(out)-1]
	readText := strings.ToLower(read["content"].(string))
	if read["effect_disposition"] != "none" || !strings.Contains(readText, "no effect") || strings.Contains(readText, "unknown") {
		t.Fatalf("read %v", read)
	}
	if write["effect_disposition"] != "unknown" || !strings.Contains(strings.ToLower(write["content"].(string)), "unknown") {
		t.Fatalf("write %v", write)
	}
}

func TestSanitizeReplayHistoryCombinesBoth(t *testing.T) {
	history := []map[string]any{
		replayUser("first"), replayAssistantTC("terminal"), replayTool("[Command interrupted]"),
		replayUser("second"), replayAssistantTC("read_file"),
	}
	out := SanitizeReplayHistory(history)
	if !reflect.DeepEqual(out[:2], []map[string]any{replayUser("first"), replayAssistantTC("terminal")}) {
		t.Fatalf("prefix %v", out[:2])
	}
	if out[2]["effect_disposition"] != "unknown" || !reflect.DeepEqual(out[len(out)-1], replayUser("second")) {
		t.Fatalf("out %v", out)
	}
}

func TestSanitizeReplayHistoryNoopOnCleanHistory(t *testing.T) {
	history := []map[string]any{replayUser("hi"), {"role": "assistant", "content": "hello"}}
	if out := SanitizeReplayHistory(history); !reflect.DeepEqual(out, history) {
		t.Fatalf("out %v", out)
	}
}

func TestUntrustworthyConfirmationStampFailsClosed(t *testing.T) {
	now := 10_000.0
	for _, untrusted := range []any{"nan", 96_400.0, 4_000_000_000.0} {
		row := []map[string]any{{"role": "user", "content": "confirm reboot", "timestamp": untrusted, "api_content": "confirm reboot"}}
		out := CanonicalizeReplayHistory(row, &now)
		if _, has := out[0]["api_content"]; !strings.Contains(out[0]["content"].(string), "EXPIRED") || has {
			t.Fatalf("%v: %v", untrusted, out[0])
		}
		if row[0]["content"] != "confirm reboot" {
			t.Fatal("the input must never be modified")
		}
	}
	late := 1e9
	if out := CanonicalizeReplayHistory([]map[string]any{{"role": "user", "content": "confirm reboot"}}, &late); out[0]["content"] != "confirm reboot" {
		t.Fatalf("an unstamped row is left alone, got %v", out[0])
	}
}

func TestInterruptMarkerMustEndAFailedOutput(t *testing.T) {
	cases := map[string]bool{
		"[Command interrupted]": true,
		`{"output": "[execution interrupted — user stop]", "exit_code": -1}`:    true,
		`{"output": "a.txt: [Command interrupted]\nb.txt: ok", "exit_code": 0}`: false,
		`{"output": "[Command interrupted]", "exit_code": 0}`:                   false,
		"docs:\n[Command interrupted]\n(the marker above is documentation)":     false,
		`{"output": "[Command interrupted - Modal sandbox]", "exit_code": 130}`: true,
	}
	for content, want := range cases {
		if got := IsInterruptedToolResult(content); got != want {
			t.Errorf("%q: %v", content, got)
		}
	}
}

func TestCanonicalizeReplayHistoryKeepsUnchangedRowPointers(t *testing.T) {
	keep := &state.Message{Role: "user", Content: "hello", Timestamp: 9_000}
	stale := &state.Message{Role: "user", Content: "confirm reboot", Timestamp: 9_000, APIContent: "confirm reboot"}
	out := canonicalizeReplayHistory([]*state.Message{keep, stale}, 10_000)
	if out[0] != keep || out[1] == stale || !strings.Contains(out[1].Text(), "EXPIRED") || out[1].APIContent != "" {
		t.Fatalf("out %+v", out)
	}
	if stale.Content != "confirm reboot" {
		t.Fatal("the input row must not be rewritten")
	}
}
