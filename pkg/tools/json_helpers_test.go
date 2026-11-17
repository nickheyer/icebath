package tools

import (
	"encoding/json"
	"testing"
)

// Decodes a tool result into a map, failing the test when it is not a JSON object
func mustJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return m
}
