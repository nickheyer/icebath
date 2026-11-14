package provider

import (
	"encoding/json"
	"net/http"
	"testing"
)

// SSE data frame for a JSON object, raw bytes or the [DONE] sentinel
func sseDataLine(obj any) []byte {
	switch v := obj.(type) {
	case []byte:
		return append(append([]byte("data: "), v...), "\n\n"...)
	case string:
		if v == "[DONE]" {
			return []byte("data: [DONE]\n\n")
		}
	}
	raw, _ := json.Marshal(obj)
	return append(append([]byte("data: "), raw...), "\n\n"...)
}

func TestCompleteStreamWithoutDoneAppends(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "LONGCAT_OK"}}}}))
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{}, "lastOne": true, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1}}))
	if !tracker.ShouldAppendDone() || tracker.SawDone {
		t.Fatalf("append=%t sawDone=%t", tracker.ShouldAppendDone(), tracker.SawDone)
	}
	if string(DoneSSEFrame[:12]) != "data: [DONE]" {
		t.Fatalf("frame %q", DoneSSEFrame)
	}
}

func TestFinishReasonAloneIsEnoughWithoutLastOne(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "SOLAR_OK"}}}}))
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	if !tracker.ShouldAppendDone() {
		t.Fatal("finish_reason alone must unlock DONE")
	}
}

func TestLastOneAloneIsEnoughWithoutFinishReason(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "x"}}}}))
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{}, "lastOne": true}))
	if !tracker.ShouldAppendDone() {
		t.Fatal("lastOne alone must unlock DONE")
	}
}

func TestExistingDoneIsNotDuplicated(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	tracker.Feed(sseDataLine("[DONE]"))
	if tracker.ShouldAppendDone() || !tracker.SawDone {
		t.Fatal("an upstream [DONE] must not be duplicated")
	}
}

func TestErrorEventBlocksDoneSynthesis(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "partial"}}}}))
	tracker.Feed(sseDataLine(map[string]any{"error": map[string]any{"message": "upstream failed", "type": "api_error"}}))
	if tracker.ShouldAppendDone() {
		t.Fatal("an error event must block DONE")
	}
}

func TestErrorFinishReasonBlocksDoneSynthesis(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "error"}}}))
	if tracker.ShouldAppendDone() {
		t.Fatal("an error finish reason must block DONE")
	}
}

func TestMalformedEventBlocksDoneSynthesis(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	tracker.Feed([]byte("data: {\"choices\": [MALFORMED]}\n\n"))
	if tracker.ShouldAppendDone() {
		t.Fatal("a malformed event must block DONE")
	}
}

func TestTruncatedTrailingEventBlocksDoneSynthesis(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	tracker.Feed([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"tail\"}}]"))
	if tracker.ShouldAppendDone() {
		t.Fatal("a truncated trailing event must block DONE")
	}
}

func TestInterruptedStreamNeverAppendsDone(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	tracker.MarkInterrupted()
	if tracker.ShouldAppendDone() {
		t.Fatal("an interrupted stream must not get DONE")
	}
}

func TestIncompleteStreamWithoutTerminalMarkerDoesNotAppend(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "mid"}}}}))
	if tracker.ShouldAppendDone() {
		t.Fatal("no terminal marker must mean no DONE")
	}
}

func TestFeedPreservesChunkBoundariesAcrossSplitLines(t *testing.T) {
	tracker := &SseDoneTracker{}
	payload := sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}})
	mid := len(payload) / 2
	tracker.Feed(payload[:mid])
	tracker.Feed(payload[mid:])
	if !tracker.ShouldAppendDone() {
		t.Fatal("a frame split across chunks must still be detected")
	}
}

func TestContentTypeIsSse(t *testing.T) {
	if !contentTypeIsSSE(http.Header{"Content-Type": {"text/event-stream"}}) {
		t.Fatal("text/event-stream not recognised")
	}
	if !contentTypeIsSSE(map[string][]string{"content-type": {"text/event-stream; charset=utf-8"}}) {
		t.Fatal("lowercase header not recognised")
	}
	if contentTypeIsSSE(http.Header{"Content-Type": {"application/json"}}) {
		t.Fatal("application/json taken for SSE")
	}
}

func TestMultilineDataEventJoinsPerSseSpec(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed([]byte("data: {\"choices\":[{\"delta\":{},\ndata: \"finish_reason\":\"stop\"}]}\n\n"))
	if tracker.SawMalformedEvent || !tracker.ShouldAppendDone() {
		t.Fatalf("malformed=%t append=%t", tracker.SawMalformedEvent, tracker.ShouldAppendDone())
	}
}

func TestMultilineMalformedEventStillDisablesSynthesis(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed([]byte("data: {broken\ndata: json!\n\n"))
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	if tracker.ShouldAppendDone() {
		t.Fatal("a malformed multiline event must block DONE")
	}
}

func TestTruthyIntegerLastOneCountsAsTerminal(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed(sseDataLine(map[string]any{"choices": []any{}, "lastOne": 1, "usage": map[string]any{}}))
	if !tracker.ShouldAppendDone() {
		t.Fatal("lastOne 1 must count as terminal")
	}
}

func TestFinalEventWithoutTrailingBlankLineStillDispatches(t *testing.T) {
	tracker := &SseDoneTracker{}
	tracker.Feed([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n"))
	if !tracker.ShouldAppendDone() {
		t.Fatal("a final event without its blank line must still dispatch")
	}
}
