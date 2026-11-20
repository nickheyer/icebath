package gatewayrun

import (
	"fmt"
	"reflect"
	"testing"
)

// Failed flag and overflow verdict as the gateway computes them
func classifyFailure(agentResult map[string]any, historyLen int) (bool, bool) {
	failed, _ := agentResult["failed"].(bool)
	return failed, isContextOverflowFailureResult(agentResult, historyLen)
}

// test_transient_failure_transcript.py test_compression_exhausted_is_context_overflow
func TestCompressionExhaustedIsContextOverflow(t *testing.T) {
	failed, overflow := classifyFailure(map[string]any{"failed": true, "compression_exhausted": true,
		"error": "Request payload too large: max compression attempts reached."}, 100)
	if !failed || !overflow {
		t.Fatalf("failed %v overflow %v", failed, overflow)
	}
}

// test_transient_failure_transcript.py test_rate_limit_429_is_not_context_overflow
func TestRateLimit429IsNotContextOverflow(t *testing.T) {
	failed, overflow := classifyFailure(map[string]any{"failed": true,
		"error": "API call failed after 3 retries: 429 Too Many Requests — rate limit exceeded"}, 10)
	if !failed || overflow {
		t.Fatalf("failed %v overflow %v", failed, overflow)
	}
}

// test_transient_failure_transcript.py test_read_timeout_is_not_context_overflow
func TestReadTimeoutIsNotContextOverflow(t *testing.T) {
	failed, overflow := classifyFailure(map[string]any{"failed": true,
		"error": "ReadTimeout: HTTPSConnectionPool(host='api.z.ai'): Read timed out."}, 10)
	if !failed || overflow {
		t.Fatalf("failed %v overflow %v", failed, overflow)
	}
}

// test_transient_failure_transcript.py test_successful_result_neither_failed_nor_overflow
func TestSuccessfulResultNeitherFailedNorOverflow(t *testing.T) {
	failed, overflow := classifyFailure(map[string]any{"final_response": "Hello!",
		"messages": []any{map[string]any{"role": "assistant", "content": "Hello!"}}}, 10)
	if failed || overflow {
		t.Fatalf("failed %v overflow %v", failed, overflow)
	}
}

const hygieneHardLimit = 20

// Transcript with a setup prefix, a tool group straddling the tail cut and a newest tail
func makeBoundProbeTranscript(total int) []map[string]any {
	rows := []map[string]any{{"role": "session_meta", "tools": []any{}, "model": "m", "platform": "telegram", "timestamp": "t0"}}
	for len(rows) < total-20 {
		role := "assistant"
		if len(rows)%2 == 1 {
			role = "user"
		}
		rows = append(rows, map[string]any{"role": role, "content": fmt.Sprintf("old-%d", len(rows)), "timestamp": fmt.Sprintf("t%d", len(rows))})
	}
	rows = append(rows,
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "t"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "old tool output 1"},
		map[string]any{"role": "tool", "tool_call_id": "c2", "content": "old tool output 2"},
		map[string]any{"role": "user", "content": "newest ask", "timestamp": "t-ask"},
		map[string]any{"role": "assistant", "content": "newest reply", "timestamp": "t-reply"},
	)
	for len(rows) < total {
		role := "assistant"
		if len(rows)%2 == 1 {
			role = "user"
		}
		rows = append(rows, map[string]any{"role": role, "content": fmt.Sprintf("tail-%d", len(rows)), "timestamp": fmt.Sprintf("t%d", len(rows))})
	}
	return rows
}

// Same backing rows and length, the identity of a returned list
func sameRows(a, b []map[string]any) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// Same map object, the identity of a returned row
func sameRow(a, b map[string]any) bool {
	return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

// test_session_hygiene.py test_bound_model_input_without_hygiene_is_deterministic_and_fail_closed
func TestBoundModelInputWithoutHygieneIsDeterministicAndFailClosed(t *testing.T) {
	rows := makeBoundProbeTranscript(60)
	snapshot := make([]map[string]any, len(rows))
	for i, r := range rows {
		c := map[string]any{}
		for k, v := range r {
			c[k] = v
		}
		snapshot[i] = c
	}
	bounded := boundModelInputWithoutHygiene(rows, hygieneHardLimit)
	if len(bounded) > hygieneHardLimit || len(bounded) >= len(rows) {
		t.Fatalf("bounded to %d of %d", len(bounded), len(rows))
	}
	if !sameRow(bounded[0], rows[0]) || !sameRow(bounded[len(bounded)-1], rows[len(rows)-1]) {
		t.Fatal("setup head or newest row lost")
	}
	if bounded[1]["role"] == "tool" {
		t.Fatal("the kept tail starts on an orphan tool result")
	}
	if !reflect.DeepEqual(rows, snapshot) {
		t.Fatal("the source transcript was mutated")
	}
	if !reflect.DeepEqual(boundModelInputWithoutHygiene(rows, hygieneHardLimit), bounded) {
		t.Fatal("the cut is not deterministic")
	}
	if !sameRows(boundModelInputWithoutHygiene(rows, len(rows)), rows) || !sameRows(boundModelInputWithoutHygiene(rows, len(rows)+5), rows) {
		t.Fatal("an in limit transcript was copied")
	}
}
