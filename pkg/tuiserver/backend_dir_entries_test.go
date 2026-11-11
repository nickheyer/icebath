package tuiserver

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Swaps the terminal tool for one recording each request and answering the given payload
func fakeTerminalTool(t *testing.T, payload map[string]any, seen *tools.TerminalRequest) {
	t.Helper()
	saved := terminalTool
	terminalTool = func(_ context.Context, _ *tools.Env, req tools.TerminalRequest) string {
		if seen != nil {
			*seen = req
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	t.Cleanup(func() { terminalTool = saved })
}

func TestBackendDirEntriesPreconfirmsInternalListing(t *testing.T) {
	var seen tools.TerminalRequest
	fakeTerminalTool(t, map[string]any{"output": "src/\nREADME.md\n", "exit_code": 0}, &seen)
	entries := backendDirEntries(context.Background(), "/workspace", "sess")
	if !seen.Force {
		t.Fatal("listing was not pre-confirmed")
	}
	if want := []dirEntry{{"README.md", false}, {"src", true}}; !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries %v", entries)
	}
}

func TestBackendDirEntriesKeepsSessionRouting(t *testing.T) {
	var seen tools.TerminalRequest
	fakeTerminalTool(t, map[string]any{"output": "src/\nREADME.md\n", "exit_code": 0}, &seen)
	backendDirEntries(context.Background(), "~/proj", "sess-42")
	if seen.TaskID != "sess-42" {
		t.Fatalf("task id %q", seen.TaskID)
	}
}

func TestBackendDirEntriesSwallowsFailedListings(t *testing.T) {
	for _, payload := range []map[string]any{{"output": "boom", "exit_code": 1}, {"error": "backend unreachable"}} {
		fakeTerminalTool(t, payload, nil)
		if entries := backendDirEntries(context.Background(), "/workspace", "sess"); len(entries) != 0 {
			t.Fatalf("%v -> %v", payload, entries)
		}
	}
}
