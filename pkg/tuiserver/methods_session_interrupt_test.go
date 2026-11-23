package tuiserver

import (
	"context"
	"testing"
)

const removedProvider = "Unknown provider 'custom:llama-local'."

func interruptStatus(t *testing.T) any {
	t.Helper()
	resp := lookupMethod("session.interrupt")(context.Background(), 1, map[string]any{"session_id": "stop-" + t.Name()})
	result, _ := resp["result"].(map[string]any)
	if result["status"] != "interrupted" {
		t.Fatalf("interrupt %v", resp)
	}
	return result["status"]
}
