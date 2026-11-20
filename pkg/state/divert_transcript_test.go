package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestDivertSessionTranscriptJSONLAppends(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	msg := pyjson.NewObject()
	msg.Set("role", "user")
	msg.Set("content", "hello-jsonl")
	path, err := DivertSessionTranscriptJSONL(context.Background(), "sess-jsonl", []any{msg, nil, 42})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "sessions", "sess-jsonl.jsonl") {
		t.Fatalf("path %s", path)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var first, last map[string]any
	if len(lines) != 2 || json.Unmarshal([]byte(lines[0]), &first) != nil || json.Unmarshal([]byte(lines[1]), &last) != nil {
		t.Fatalf("lines %q", lines)
	}
	if first["content"] != "hello-jsonl" || last["content"] != "42" {
		t.Fatalf("records %v %v", first, last)
	}
	if path, err := DivertSessionTranscriptJSONL(context.Background(), "sess-jsonl", nil); path != "" || err != nil {
		t.Fatalf("empty messages must write nothing, got %q %v", path, err)
	}
}
