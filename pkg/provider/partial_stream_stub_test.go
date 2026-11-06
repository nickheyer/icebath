package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func partialStubDo(t *testing.T, events []StreamEvent, cause error, o DoOptions) *Response {
	t.Helper()
	t.Setenv("ICEBATH_STREAM_RETRIES", "0")
	route := testRoute()
	client := &fakeClient{route: route, queue: []fakeCall{{events: events, err: cause}}}
	o.Sleep, o.Stream = noSleep, func(StreamEvent) error { return nil }
	resp, err := Do(t.Context(), client, &Request{Model: route.Model}, o)
	if err != nil || !IsPartialStreamStub(resp) || resp.FinishReason != "length" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	return resp
}

func stubRaw(t *testing.T, resp *Response) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(resp.Raw, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPartialStreamOverflowErrorReturnsTerminalStub(t *testing.T) {
	text := "Here's my long partial answer ..."
	resp := partialStubDo(t, []StreamEvent{{Kind: EventTextDelta, Text: text}},
		errors.New("This model's maximum context length is 128000 tokens. However, you requested 140000 tokens."), DoOptions{StreamedText: func() string { return text }})
	if !PartialStreamOverflowTerminal(resp) || resp.Message.Content != nil {
		t.Fatalf("overflow stub %s", resp.Raw)
	}
	choices, _ := stubRaw(t, resp)["choices"].([]any)
	if msg := choices[0].(map[string]any)["message"].(map[string]any); msg["content"] != nil {
		t.Fatalf("recovered text seeded %v", msg)
	}
}

func TestPayloadTooLargePartialIsNotMadeTerminal(t *testing.T) {
	text := "some media-heavy partial output ..."
	resp := partialStubDo(t, []StreamEvent{{Kind: EventTextDelta, Text: text}},
		errors.New("Request payload too large (413): 5MB exceeds the 4MB limit"), DoOptions{StreamedText: func() string { return text }})
	if PartialStreamOverflowTerminal(resp) || resp.Message.Content != text || stubRaw(t, resp)[ccRawOverflowTerminal] != false {
		t.Fatalf("payload too large stub %s", resp.Raw)
	}
}

func TestPartialStreamStalledMidToolCallWarnsAndNamesTheDroppedCalls(t *testing.T) {
	var warned []string
	events := []StreamEvent{{Kind: EventTextDelta, Text: "Let me look."}}
	for _, n := range []string{"read_file", "search_files", "terminal", "patch"} {
		events = append(events, StreamEvent{Kind: EventToolCallStart, ToolCall: &state.ToolCall{ID: "c_" + n, Function: state.ToolCallFunction{Name: n}}})
	}
	logs := captureLogs(t)
	resp := partialStubDo(t, events, &APIError{Message: "bad request body", StatusCode: 400}, DoOptions{Warning: func(s string) { warned = append(warned, s) }})
	warn := "\n\n⚠ Stream stalled mid tool-call (read_file, search_files, terminal, +1 more); the action was not executed. Ask me to retry if you want to continue."
	if resp.Message.Content != "Let me look."+warn || len(warned) != 1 || warned[0] != warn {
		t.Fatalf("content %q warned %q", resp.Message.Content, warned)
	}
	if got := PartialStreamDroppedToolNames(resp); strings.Join(got, ",") != "read_file,search_files,terminal,patch" {
		t.Fatalf("dropped %v", got)
	}
	if logCount(logs, "Partial stream dropped tool call(s) ['read_file', 'search_files', 'terminal', 'patch'] after") != 1 ||
		logCount(logs, "returning length-truncated stub") != 0 {
		t.Fatal("tool call drop log")
	}
}

func TestContentPolicyPartialStubIsTaggedForFallback(t *testing.T) {
	resp := partialStubDo(t, []StreamEvent{{Kind: EventTextDelta, Text: "Sure, here"}},
		&APIError{StatusCode: 400, Message: "Your request was flagged by our safety system: content_filter"}, DoOptions{})
	if !ResponseContentFilterTerminated(resp) || PartialStreamOverflowTerminal(resp) {
		t.Fatalf("content filter stub %s", resp.Raw)
	}
}

func TestAnthropicPartialStubPassesTransportValidation(t *testing.T) {
	for _, c := range []struct {
		content  string
		overflow bool
	}{{"partial answer", false}, {"", true}} {
		stub := buildPartialStreamStub(Route{Provider: "anthropic", Mode: ModeAnthropicMessages}, c.content, "", "m", nil, nil,
			partialStubOptions{overflowTerminal: c.overflow, apiMode: ModeAnthropicMessages})
		raw := stubRaw(t, stub)
		tr := GetTransport(string(ModeAnthropicMessages))
		if !tr.ValidateResponse(raw) || raw["stop_reason"] != "max_tokens" || tr.MapFinishReason("max_tokens") != "length" || raw[ccRawOverflowTerminal] != c.overflow {
			t.Fatalf("anthropic stub %s", stub.Raw)
		}
		blocks, _ := raw["content"].([]any)
		if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != c.content {
			t.Fatalf("one text block %v", blocks)
		}
	}
}
