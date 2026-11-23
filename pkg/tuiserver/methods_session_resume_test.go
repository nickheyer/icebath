package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const (
	mintedKey   = "20260828_053121_3427a9"
	existingKey = "20260827_224618_956bf8"
)

func resumeCall(t *testing.T, params map[string]any) map[string]any {
	t.Helper()
	return handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.resume", "params": params})
}

func assertResumedOK(t *testing.T, resp map[string]any, target string) {
	t.Helper()
	result, isResult := resp["result"].(map[string]any)
	if !isResult || result["resumed"] != target || !truthy(result["session_id"]) {
		t.Fatalf("resume %v", resp)
	}
}

// Count of tui rows the store lists
func tuiRows(t *testing.T, db *state.DB) int {
	t.Helper()
	rows, err := db.ListSessionsRich(context.Background(), state.ListSessionsRichOptions{Source: "tui"})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}
