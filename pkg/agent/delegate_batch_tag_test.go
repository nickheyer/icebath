package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Empties the per conversation batch ordinal table for one test
func freshBatchOrdinals(t *testing.T) {
	t.Helper()
	batchOrdinalsMu.Lock()
	prev := batchOrdinals
	batchOrdinals = map[string]map[string]int{}
	batchOrdinalsMu.Unlock()
	t.Cleanup(func() {
		batchOrdinalsMu.Lock()
		batchOrdinals = prev
		batchOrdinalsMu.Unlock()
	})
}

type treeSpinner struct {
	mu    sync.Mutex
	lines []string
}

func (s *treeSpinner) Stop(string) {}
func (s *treeSpinner) PrintAbove(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, line)
}
func (s *treeSpinner) UpdateText(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, "<spin>"+text)
}

// test_delegate_batch_tag.py test_format_batch_tag_assigns_stable_ordinals_per_batch
func TestFormatBatchTagAssignsStableOrdinalsPerBatch(t *testing.T) {
	freshBatchOrdinals(t)
	for _, tc := range []struct{ id, want string }{{"deleg_6a664903", "set 1"}, {"deleg_b2ac1234", "set 2"}, {"deleg_6a664903", "set 1"}, {"", ""}} {
		if got := formatBatchTag(tc.id, nil); got != tc.want {
			t.Fatalf("tag of %q = %q want %q", tc.id, got, tc.want)
		}
	}
}

// test_delegate_batch_tag.py test_batch_ordinals_are_scoped_per_parent_conversation
func TestBatchOrdinalsAreScopedPerParentConversation(t *testing.T) {
	freshBatchOrdinals(t)
	a, b := &AIAgent{sessionID: "conv-a"}, &AIAgent{sessionID: "conv-b"}
	for _, tc := range []struct {
		id     string
		parent *AIAgent
		want   string
	}{{"deleg_a1", a, "set 1"}, {"deleg_b1", b, "set 1"}, {"deleg_b2", b, "set 2"}, {"deleg_a2", a, "set 2"}, {"deleg_a1", a, "set 1"}} {
		if got := formatBatchTag(tc.id, tc.parent); got != tc.want {
			t.Fatalf("tag of %q in %s = %q want %q", tc.id, tc.parent.sessionID, got, tc.want)
		}
	}
}

// test_delegate_batch_tag.py test_child_tree_lines_and_relayed_events_carry_batch_tag
func TestChildTreeLinesAndRelayedEventsCarryBatchTag(t *testing.T) {
	freshBatchOrdinals(t)
	var relayed []map[string]any
	spinner := &treeSpinner{}
	parent := &AIAgent{}
	parent.delegateSpinner = spinner
	parent.toolProgressCallback = func(_, _ string, _ any, _ any, extra map[string]any) { relayed = append(relayed, extra) }
	ref, refMu := map[string]any{}, &sync.Mutex{}
	cb := buildChildProgressCallback(2, "triage cluster", parent, 9, "sa-2", "", nil, "", nil, ref, refMu)
	refMu.Lock()
	ref["delegation_id"] = "deleg_6a664903"
	ref["session_id"] = "child-sess"
	refMu.Unlock()

	callProgressShape(cb, "subagent.start", "", "", nil, nil)
	callProgressShape(cb, "tool.started", "terminal", "ls", nil, nil)

	if len(spinner.lines) < 2 || !strings.HasPrefix(spinner.lines[0], " [set 1 · 3/9] ├─ 🔀 triage cluster") ||
		!strings.HasPrefix(spinner.lines[1], " [set 1 · 3/9] ├─ ") {
		t.Fatalf("tree lines %q", spinner.lines)
	}
	for _, kw := range relayed {
		if kw["delegation_id"] != "deleg_6a664903" || kw["child_session_id"] != "child-sess" {
			t.Fatalf("relayed %v", kw)
		}
	}
}

// test_delegate_batch_tag.py test_batch_completion_lines_are_attributable_across_two_batches
func TestBatchCompletionLinesAreAttributableAcrossTwoBatches(t *testing.T) {
	freshBatchOrdinals(t)
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	var mu sync.Mutex
	var lines []string
	parent := &AIAgent{sessionID: "root", model: "m", printFn: func(line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, line)
	}}
	prevRun, prevBuild, prevCreds := runSingleChildFn, buildChildPreservingParentToolsFn, resolveDelegationCredentialsFn
	t.Cleanup(func() {
		runSingleChildFn, buildChildPreservingParentToolsFn, resolveDelegationCredentialsFn = prevRun, prevBuild, prevCreds
	})
	runSingleChildFn = func(_ context.Context, taskIndex int, _ string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		return pyjson.ObjectOf("task_index", taskIndex, "status", "completed", "summary", "ok", "error", nil, "api_calls", 1, "duration_seconds", 1)
	}
	buildChildPreservingParentToolsFn = func(context.Context, childBuildSpec) (*AIAgent, error) { return &AIAgent{}, nil }
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return map[string]any{"model": "m", "provider": "openrouter", "base_url": "https://x/v1", "api_key": "k", "api_mode": "chat_completions"}, nil
	}

	for _, n := range []int{3, 9} {
		tasks := make([]any, n)
		for i := range tasks {
			tasks[i] = map[string]any{"goal": fmt.Sprintf("batch of %d: worker task number %d", n, i)}
		}
		res, err := delegateTask(context.Background(), parent, map[string]any{"tasks": tasks})
		if err != nil || strings.Contains(pyStrPrefix(res, 20), "error") {
			t.Fatalf("delegate_task %s %v", res, err)
		}
	}
	header := regexp.MustCompile(`^\s*🔀 \[(set \d+)\] delegating (\d+) tasks`)
	var headers []string
	done, set1, set2 := 0, 0, 0
	for _, l := range lines {
		if m := header.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[2])
			headers = append(headers, fmt.Sprintf("%s:%d", m[1], n))
		}
		if strings.Contains(l, "✓ [") {
			done++
			if strings.Contains(l, "✓ [set 1 · ") && strings.Contains(l, "/3]") {
				set1++
			}
			if strings.Contains(l, "✓ [set 2 · ") && strings.Contains(l, "/9]") {
				set2++
			}
		}
	}
	if strings.Join(headers, ",") != "set 1:3,set 2:9" || done != 12 || set1 != 3 || set2 != 9 {
		t.Fatalf("headers %v done %d set1 %d set2 %d lines %q", headers, done, set1, set2, lines)
	}
}

// First n code points of a result, the same prefix Python slicing of str(res) yields
func pyStrPrefix(s string, n int) string {
	r := []rune(s)
	return string(r[:min(n, len(r))])
}
