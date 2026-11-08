package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Assistant call and tool result of one skill_manage patch whose result carries the given change
func skillToolMessages(t *testing.T, change any) []map[string]any {
	ops := []any{map[string]any{"action": "replace", "content": "x", "old_text": "y"}}
	return []map[string]any{
		{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
			"name": "skill_manage", "arguments": jsonText(t, map[string]any{"action": "patch", "name": "my-skill", "operations": ops}),
		}}}},
		{"role": "tool", "tool_call_id": "call_1", "content": jsonText(t, map[string]any{"success": true,
			"message": "Skill 'my-skill' patched.", "_change": change})},
	}
}

// Assistant call and tool result of one memory add whose result carries the given operations field
func memoryToolMessages(t *testing.T, operations any) []map[string]any {
	return []map[string]any{
		{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_2", "type": "function", "function": map[string]any{
			"name": "memory", "arguments": jsonText(t, map[string]any{"action": "add", "target": "memory"}),
		}}}},
		{"role": "tool", "tool_call_id": "call_2", "content": jsonText(t, map[string]any{"success": true, "message": "Entry added.",
			"operations": operations})},
	}
}

func TestBOperationsAsNoneTreatedAsEmpty(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	actions, err := summarizeBackgroundReviewActions(context.Background(), memoryToolMessages(t, nil), nil, "verbose")
	if err != nil || actions == nil {
		t.Fatalf("actions %v err %v", actions, err)
	}
}

func TestCOperationsContainsNonDictEntries(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ops := []any{"raw-string-no-fields", map[string]any{"action": "add", "content": "valid entry"}, nil,
		map[string]any{"action": "replace", "content": "another", "old_text": "thing"}}
	actions, err := summarizeBackgroundReviewActions(context.Background(), memoryToolMessages(t, ops), nil, "verbose")
	if err != nil || len(actions) < 1 {
		t.Fatalf("expected at least one action line, got %v err %v", actions, err)
	}
}

func TestDDetailNonDictReplacedWithEmpty(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var msgs []map[string]any
	for _, m := range skillToolMessages(t, map[string]any{}) {
		if m["role"] != "assistant" {
			msgs = append(msgs, m)
		}
	}
	actions, err := summarizeBackgroundReviewActions(context.Background(), msgs, nil, "verbose")
	if err != nil || actions == nil {
		t.Fatalf("actions %v err %v", actions, err)
	}
}

// Assistant skill_manage call with args and its tool result data
func skillCallMessages(t *testing.T, args, data any) []map[string]any {
	return []map[string]any{
		{"role": "assistant", "tool_calls": []any{map[string]any{"id": "skill", "function": map[string]any{
			"name": "skill_manage", "arguments": jsonText(t, args)}}}},
		{"role": "tool", "tool_call_id": "skill", "content": jsonText(t, data)},
	}
}

func TestAppliedSkillOperationsNotifyWithNames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	name := "notify-contract"
	content := "---\nname: " + name + "\ndescription: Use when checking notices. Verify applied writes.\n---\nRead the sample before editing.\n"
	operations := []map[string]any{
		{"name": name, "action": "create", "content": content},
		{"name": name, "action": "patch", "old_string": "sample", "new_string": "example"},
		{"name": name, "action": "write_file", "file_path": "references/a.md", "file_content": "Check the example."},
		{"name": name, "action": "remove_file", "file_path": "references/a.md"},
		{"name": name, "action": "delete"},
	}
	scope := skills.ScopeFromEnv(&tools.Env{Home: dir})
	for _, op := range operations {
		var data map[string]any
		raw := scope.SkillManage(ctx, map[string]any{"action": "", "name": "", "operations": []any{op}}, "", "")
		if err := json.Unmarshal([]byte(raw), &data); err != nil || data["success"] != true {
			t.Fatalf("skill_manage %v: %s", op, raw)
		}
		messages := skillCallMessages(t, map[string]any{"operations": []any{op}}, data)
		for _, mode := range []string{"on", "verbose"} {
			actions, err := summarizeBackgroundReviewActions(ctx, messages, nil, mode)
			if err != nil || len(actions) == 0 {
				t.Fatalf("%s %v: actions %v err %v", mode, op["action"], actions, err)
			}
			for _, line := range actions {
				if !strings.Contains(line, name) || strings.Contains(line, "?") {
					t.Fatalf("%s %v: line %q", mode, op["action"], line)
				}
			}
		}
		if actions, _ := summarizeBackgroundReviewActions(ctx, messages, nil, "off"); len(actions) != 0 {
			t.Fatalf("off mode notified %v", actions)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", name)); !os.IsNotExist(err) {
		t.Fatalf("deleted skill still on disk: %v", err)
	}
}

func TestUnappliedSkillOperationsNeverNotify(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	args := map[string]any{"operations": []any{map[string]any{"name": "pending", "action": "create"}}}
	for _, data := range []map[string]any{
		{"success": true, "staged": true, "message": "Write staged for approval."},
		{"success": false, "results": []any{map[string]any{"success": true, "name": "pending", "action": "create"}}},
		{"success": true, "operations_applied": 0, "results": []any{}},
		{"success": true, "operations_applied": 1, "results": []any{map[string]any{"success": false, "name": "pending", "action": "create"}}},
	} {
		for _, mode := range []string{"on", "verbose"} {
			actions, err := summarizeBackgroundReviewActions(context.Background(), skillCallMessages(t, args, data), nil, mode)
			if err != nil || !reflect.DeepEqual(actions, []string{}) {
				t.Fatalf("%v %s: actions %v err %v", data, mode, actions, err)
			}
		}
	}
}

func TestGroundingRuleIsInBothMemoryPromptsAndNotTheSkillPrompt(t *testing.T) {
	for _, marker := range []string{"GROUNDED in this conversation", "never turn a one-off request into a standing preference"} {
		if !strings.Contains(memoryReviewPrompt, marker) || !strings.Contains(combinedReviewPrompt, marker) {
			t.Fatalf("memory prompts lack %q", marker)
		}
		if strings.Contains(skillReviewPrompt, marker) {
			t.Fatalf("skill prompt carries %q", marker)
		}
	}
}
