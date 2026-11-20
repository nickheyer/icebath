package gateway

import (
	"math/big"
	"testing"
)

// Row kwargs keep assistant only keys on assistant rows, fall back on ids and timestamps, and bind text safely
func TestTranscriptAppendKwargsBindsRowFields(t *testing.T) {
	huge, _ := new(big.Int).SetString("99999999999999999999", 10)
	user := transcriptAppendKwargs("s1", map[string]any{"role": "user", "content": "hi", "reasoning": "r",
		"message_id": "m-1", "tool_call_id": map[string]any{"bad": 1}, "api_content": 7}, 12.5)
	if user["reasoning"] != nil || user["platform_message_id"] != "m-1" || user["timestamp"] != 12.5 || user["api_content"] != nil {
		t.Fatalf("user row %v", user)
	}
	if user["tool_call_id"] != `{"bad": 1}` || user["observed"] != false || user["session_id"] != "s1" {
		t.Fatalf("bound text %v", user["tool_call_id"])
	}
	assistant := transcriptAppendKwargs("s1", map[string]any{"role": "assistant", "reasoning": "why", "timestamp": 0.0,
		"platform_message_id": "p-1", "message_id": "m-1", "observed": 1, "api_content": "exact"}, 99.0)
	if assistant["reasoning"] != "why" || assistant["timestamp"] != 0.0 || assistant["platform_message_id"] != "p-1" ||
		assistant["observed"] != true || assistant["api_content"] != "exact" {
		t.Fatalf("assistant row %v", assistant)
	}
	if unknown := transcriptAppendKwargs("s1", map[string]any{"content": huge}, nil); unknown["role"] != "unknown" || unknown["content"] != huge {
		t.Fatalf("unknown role row %v", unknown)
	}
	if got := bindableText(huge); got != "99999999999999999999" {
		t.Fatalf("oversized int %v", got)
	}
	if got := bindableText(int64(5)); got != int64(5) {
		t.Fatalf("int %v", got)
	}
	if got := bindableText([]any{"a", 1}); got != `["a", 1]` {
		t.Fatalf("list %v", got)
	}
}

// Marked sessions join the spooled drop set and an empty mark is a no op
func TestMarkSpooledDropSessions(t *testing.T) {
	store := &SessionStore{spooledDropSessions: map[string]bool{}}
	store.markSpooledDropSessions(nil)
	store.markSpooledDropSessions(map[string]bool{"sess-1": true})
	if !store.spooledDropSessions["sess-1"] || len(store.spooledDropSessions) != 1 {
		t.Fatalf("spooled %v", store.spooledDropSessions)
	}
}
