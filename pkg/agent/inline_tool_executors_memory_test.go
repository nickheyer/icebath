package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

type memoryWriteSink struct {
	memory.BaseProvider
	mu     sync.Mutex
	writes [][3]string
	meta   []map[string]any
}

func (s *memoryWriteSink) Name() string                                                 { return "test-sink" }
func (s *memoryWriteSink) Available(context.Context) bool                               { return true }
func (s *memoryWriteSink) Initialize(context.Context, string, memory.InitOptions) error { return nil }
func (s *memoryWriteSink) ToolSchemas(context.Context) ([]map[string]any, error)        { return nil, nil }
func (s *memoryWriteSink) OnMemoryWrite(_ context.Context, action, target, content string, metadata map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, [3]string{action, target, content})
	s.meta = append(s.meta, metadata)
	return nil
}

func (s *memoryWriteSink) last() ([3]string, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) == 0 {
		return [3]string{}, nil
	}
	return s.writes[len(s.writes)-1], s.meta[len(s.meta)-1]
}

// test_inline_tool_executors_memory_args.py test_memory_alias_persists_with_content_precedence
func TestMemoryAliasPersistsWithContentPrecedence(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	env := &tools.Env{Home: tmp, Values: &sync.Map{}}
	store := memory.NewStore(home.MemoriesDir(tmp), 500, 300, true, true)
	sink := &memoryWriteSink{}
	manager := memory.NewMemoryManager(context.Background(), env, nil)
	if err := manager.AddProvider(context.Background(), sink); err != nil {
		t.Fatal(err)
	}
	a := &AIAgent{env: env, memoryStore: store, memoryManager: manager, sessionID: "sess-1", platform: "cli",
		memoryWriteOrigin: "assistant_tool", memoryWriteContext: "foreground"}
	ictx := tools.InlineToolContext{TaskID: "task-1", ToolCallID: "call-1"}
	call := func(args map[string]any) map[string]any {
		t.Helper()
		out, err := memoryInlineExecutor(context.Background(), agentExecutorHost{a}, args, ictx)
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("result %q: %v", out, err)
		}
		return parsed
	}
	if r := call(map[string]any{"action": "add", "target": "user", "content": "Household owns an estate."}); r["success"] != true {
		t.Fatalf("add %v", r)
	}
	r := call(map[string]any{"action": "patch", "target": "user", "old_text": "Household owns an estate.", "new_text": "Household owns a saloon."})
	if r["success"] != true {
		t.Fatalf("patch %v", r)
	}
	path := filepath.Join(tmp, "memories", "USER.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Household owns a saloon.") {
		t.Fatalf("USER.md %q", raw)
	}
	if got, meta := sink.last(); got != [3]string{"replace", "user", "Household owns a saloon."} {
		t.Fatalf("last write %v", got)
	} else if meta["task_id"] != "task-1" || meta["tool_call_id"] != "call-1" || meta["session_id"] != "sess-1" || meta["write_origin"] != "assistant_tool" {
		t.Fatalf("metadata %v", meta)
	}
	r = call(map[string]any{"action": "patch", "target": "user", "old_text": "Household owns a saloon.",
		"content": "Household owns a hatchback.", "new_text": "Household owns a van."})
	if r["success"] != true {
		t.Fatalf("patch with content %v", r)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted := string(raw)
	if !strings.Contains(persisted, "Household owns a hatchback.") || strings.Contains(persisted, "van") || strings.Contains(persisted, "saloon") {
		t.Fatalf("USER.md %q", persisted)
	}
	if got, _ := sink.last(); got != [3]string{"replace", "user", "Household owns a hatchback."} {
		t.Fatalf("last write %v", got)
	}
	r = call(map[string]any{"target": "user", "operations": []any{map[string]any{"action": "patch", "old_text": "Household owns a hatchback.",
		"new_text": "Household owns a coupe."}}})
	if r["success"] != true {
		t.Fatalf("batch patch %v", r)
	}
	if got, _ := sink.last(); got != [3]string{"replace", "user", "Household owns a coupe."} {
		t.Fatalf("last write %v", got)
	}
}
