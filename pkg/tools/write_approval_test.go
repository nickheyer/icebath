package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Asker that records every question and answers with a fixed choice
type waAsker struct {
	mu     sync.Mutex
	choice string
	calls  [][]Question
}

func (a *waAsker) Ask(_ context.Context, qs []Question) (FormResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, qs)
	return FormResult{Outcome: "answered", Answers: []Answer{{Status: "answered", Selected: []string{a.choice}}}}, nil
}

// Temp home env with an isolated session value map
func waEnv(t *testing.T) *Env {
	t.Helper()
	for _, k := range []string{"ICEBATH_CRON_SESSION", "ICEBATH_KANBAN_TASK", "ICEBATH_SESSION_PLATFORM", "ICEBATH_PLATFORM", "ICEBATH_SINGLE_QUERY_SESSION"} {
		t.Setenv(k, "")
	}
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	return &Env{Home: h, Values: &sync.Map{}}
}

// Writes subsystem.write_approval into the home config
func waSetApproval(t *testing.T, env *Env, subsystem string, enabled bool) {
	t.Helper()
	body := subsystem + ":\n  write_approval: false\n"
	if enabled {
		body = subsystem + ":\n  write_approval: true\n"
	}
	if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

func TestListPendingSkipsNonDictRecord(t *testing.T) {
	env := waEnv(t)
	StageWrite(env, "memory", map[string]any{"action": "add", "target": "user", "content": "ok"}, "ok", "foreground")
	if err := os.WriteFile(filepath.Join(PendingWriteDir(env, "memory"), "bad.json"), []byte(`"not a record"`), 0o644); err != nil {
		t.Fatal(err)
	}
	records := ListPending(env, "memory")
	if len(records) != 1 || records[0]["payload"].(map[string]any)["content"] != "ok" {
		t.Fatalf("got %v", records)
	}
	if GetPending(env, "memory", "bad") != nil {
		t.Fatal("non object record must read as absent")
	}
}

func TestNormalizeEnabledCoercesValues(t *testing.T) {
	for v, want := range map[any]bool{true: true, false: false, "on": true, "approve": true, "true": true, "off": false, "garbage": false} {
		if got := normalizeWriteApprovalEnabled(v); got != want {
			t.Fatalf("%v: got %v", v, got)
		}
	}
	if normalizeWriteApprovalEnabled(nil) {
		t.Fatal("nil must be off")
	}
}

func TestMemoryGateOffAllowsWrite(t *testing.T) {
	env := waEnv(t)
	if d := CheckWriteApproval(context.Background(), env, "memory", "s", "d"); !d.Allow || d.Stage || d.Blocked {
		t.Fatalf("got %+v", d)
	}
	if PendingCount(env, "memory") != 0 {
		t.Fatal("nothing should stage")
	}
}

func TestMemoryInlineApproveWrites(t *testing.T) {
	env := waEnv(t)
	waSetApproval(t, env, "memory", true)
	asker := &waAsker{choice: "once"}
	env.Ask = asker
	d := CheckWriteApproval(context.Background(), env, "memory", "add memory entry", "approved fact")
	if !d.Allow {
		t.Fatalf("got %+v", d)
	}
	if len(asker.calls) != 1 || !strings.Contains(asker.calls[0][0].Text, "approved fact") || asker.calls[0][0].Header != "Save to memory?" {
		t.Fatalf("calls %+v", asker.calls)
	}
	if PendingCount(env, "memory") != 0 {
		t.Fatal("approved write must not stage")
	}
}

func TestMemoryInlineDenyBlocks(t *testing.T) {
	env := waEnv(t)
	waSetApproval(t, env, "memory", true)
	env.Ask = &waAsker{choice: "deny"}
	d := CheckWriteApproval(context.Background(), env, "memory", "", "denied fact")
	if !d.Blocked || !strings.Contains(strings.ToLower(d.Message), "denied") {
		t.Fatalf("got %+v", d)
	}
}

func TestHeadlessMemoryWriteStagesWithoutInlinePrompt(t *testing.T) {
	for _, kv := range [][2]string{{"ICEBATH_SINGLE_QUERY_SESSION", "1"}, {"ICEBATH_CRON_SESSION", "1"}, {"ICEBATH_SESSION_PLATFORM", "webhook"}} {
		env := waEnv(t)
		waSetApproval(t, env, "memory", true)
		t.Setenv(kv[0], kv[1])
		asker := &waAsker{choice: "once"}
		env.Ask = asker
		d := CheckWriteApproval(context.Background(), env, "memory", "s", "headless fact")
		if !d.Stage || len(asker.calls) != 0 {
			t.Fatalf("%s: got %+v calls %d", kv[0], d, len(asker.calls))
		}
		t.Setenv(kv[0], "")
	}
}

func TestUnknownChoiceAndMissingAskerStage(t *testing.T) {
	env := waEnv(t)
	waSetApproval(t, env, "memory", true)
	if d := CheckWriteApproval(context.Background(), env, "memory", "s", "x"); !d.Stage {
		t.Fatalf("no asker must stage, got %+v", d)
	}
	env.Ask = &waAsker{choice: "timeout"}
	if d := CheckWriteApproval(context.Background(), env, "memory", "s", "x"); !d.Stage {
		t.Fatalf("unknown outcome must stage, got %+v", d)
	}
}

func TestSkillsAndBackgroundWritesAlwaysStage(t *testing.T) {
	env := waEnv(t)
	waSetApproval(t, env, "skills", true)
	asker := &waAsker{choice: "once"}
	env.Ask = asker
	if d := CheckWriteApproval(context.Background(), env, "skills", "s", "x"); !d.Stage || !strings.Contains(d.Message, "/skills pending") {
		t.Fatalf("got %+v", d)
	}
	waSetApproval(t, env, "memory", true)
	env.Values.Store(writeOriginValuesKey, backgroundReviewOrigin)
	if d := CheckWriteApproval(context.Background(), env, "memory", "s", "x"); !d.Stage || len(asker.calls) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestStagedHintNamesTheReviewCommandOnSlashSurfaces(t *testing.T) {
	env := waEnv(t)
	waSetApproval(t, env, "memory", true)
	d := CheckWriteApproval(context.Background(), env, "memory", "s", "surface hint fact")
	if !strings.Contains(d.Message, "/memory pending") {
		t.Fatal(d.Message)
	}
}

func TestStagedHintNamesThePendingDirOnHeadlessSurfaces(t *testing.T) {
	env := waEnv(t)
	waSetApproval(t, env, "memory", true)
	for _, kv := range [][2]string{{"ICEBATH_CRON_SESSION", "1"}, {"ICEBATH_KANBAN_TASK", "task-7"}, {"ICEBATH_SESSION_PLATFORM", "api_server"}} {
		t.Setenv(kv[0], kv[1])
		d := stagedDecision(env, "memory")
		if strings.Contains(d.Message, "review with /memory pending") || !strings.Contains(d.Message, "pending records live in") || !strings.Contains(d.Message, PendingWriteDir(env, "memory")) {
			t.Fatalf("%s: %s", kv[0], d.Message)
		}
		t.Setenv(kv[0], "")
	}
}

func TestStagedHintKeepsCommandForChatGatewayPlatform(t *testing.T) {
	env := waEnv(t)
	t.Setenv("ICEBATH_SESSION_PLATFORM", "telegram")
	if d := stagedDecision(env, "memory"); !strings.Contains(d.Message, "/memory pending") {
		t.Fatal(d.Message)
	}
}

func TestPendingStoreRoundTrip(t *testing.T) {
	env := waEnv(t)
	rec := StageWrite(env, "skills", map[string]any{"action": "create", "name": "x"}, "  gist  ", "")
	id := rec["id"].(string)
	if len(id) != 8 || rec["summary"] != "gist" || rec["origin"] != "foreground" {
		t.Fatalf("got %v", rec)
	}
	got := GetPending(env, "skills", id)
	if got == nil || got["action"] != "create" || PendingCount(env, "skills") != 1 {
		t.Fatalf("got %v", got)
	}
	if !DiscardPending(env, "skills", id) || DiscardPending(env, "skills", id) || PendingCount(env, "skills") != 0 {
		t.Fatal("discard must delete once")
	}
}

const waSkill = "---\nname: %s\ndescription: Probe skill for pending-diff tests.\n---\n# Probe\nStep 1.\n"

func waSkillText(n string) string { return strings.Replace(waSkill, "%s", n, 1) }

// Temp home with one installed probe skill and a directory name skill lookup
func waSkillEnv(t *testing.T) *Env {
	t.Helper()
	env := waEnv(t)
	dir := filepath.Join(env.Home, "skills", "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	wdWrite(t, filepath.Join(dir, "SKILL.md"), waSkillText("probe"))
	prev := FindSkillDir
	FindSkillDir = func(env *Env, name string) string {
		p := filepath.Join(env.Home, "skills", name)
		if isDir(p) {
			return p
		}
		return ""
	}
	t.Cleanup(func() { FindSkillDir = prev })
	return env
}

func waBatchRecord() map[string]any {
	return map[string]any{"id": "abc123", "payload": map[string]any{"action": "batch", "operations": []any{
		map[string]any{"action": "create", "name": "newprobe", "content": waSkillText("newprobe")},
		map[string]any{"action": "patch", "name": "probe", "old_string": "Step 1.", "new_string": "Step ONE."},
	}}}
}

func TestBatchDiffRendersPerOpContent(t *testing.T) {
	env := waSkillEnv(t)
	out := SkillPendingDiff(env, waBatchRecord(), nil)
	for _, want := range []string{"Step 1.", "-Step 1.", "+Step ONE."} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "(batch on") {
		t.Fatal(out)
	}
}

func TestSingleOpPathsUnchanged(t *testing.T) {
	env := waSkillEnv(t)
	create := SkillPendingDiff(env, map[string]any{"payload": map[string]any{"action": "create", "name": "newprobe", "content": waSkillText("newprobe")}}, nil)
	if create != waSkillText("newprobe") {
		t.Fatal(create)
	}
	patch := SkillPendingDiff(env, map[string]any{"payload": map[string]any{"action": "patch", "name": "probe", "old_string": "Step 1.", "new_string": "Step ONE."}}, nil)
	if !strings.Contains(patch, "-Step 1.") || !strings.Contains(patch, "+Step ONE.") {
		t.Fatal(patch)
	}
}

func TestBatchOpDiffsAgainstEarlierOpsNotDisk(t *testing.T) {
	env := waSkillEnv(t)
	rec := map[string]any{"id": "def456", "payload": map[string]any{"action": "batch", "operations": []any{
		map[string]any{"action": "create", "name": "newprobe", "content": waSkillText("newprobe")},
		map[string]any{"action": "patch", "name": "newprobe", "old_string": "Step 1.", "new_string": "Step ONE."},
		map[string]any{"action": "write_file", "name": "newprobe", "file_path": "ref.md", "file_content": "x\n"},
		map[string]any{"action": "patch", "name": "newprobe", "file_path": "ref.md", "old_string": "x", "new_string": "y"},
	}}}
	out := SkillPendingDiff(env, rec, nil)
	if strings.Contains(out, "(patch") {
		t.Fatal(out)
	}
	for _, want := range []string{"-Step 1.", "+Step ONE.", "a/ref.md", "-x", "+y"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestBatchNumberingIgnoresNonDictOps(t *testing.T) {
	env := waSkillEnv(t)
	rec := map[string]any{"id": "ghi789", "payload": map[string]any{"action": "batch", "operations": []any{
		"not-an-op",
		map[string]any{"action": "create", "name": "newprobe", "content": waSkillText("newprobe")},
	}}}
	out := SkillPendingDiff(env, rec, nil)
	if !strings.Contains(out, "## op 1/1:") || strings.Contains(out, "/2:") {
		t.Fatal(out)
	}
}

func TestPatchPreviewUsesTheApprovalMatcher(t *testing.T) {
	env := waSkillEnv(t)
	wdWrite(t, filepath.Join(env.Home, "skills", "probe", "SKILL.md"), waSkillText("probe")+"Step 1.\n")
	out := SkillPendingDiff(env, map[string]any{"payload": map[string]any{"action": "patch", "name": "probe", "old_string": "Step 1.", "new_string": "Step ONE.", "replace_all": false}}, nil)
	if !strings.Contains(out, "(patch would fail:") || !strings.Contains(out, "Found 2 matches") || strings.Contains(out, "+Step ONE.") {
		t.Fatal(out)
	}
}

func TestPatchPreviewReplaceAllFoldsEveryMatch(t *testing.T) {
	env := waSkillEnv(t)
	wdWrite(t, filepath.Join(env.Home, "skills", "probe", "SKILL.md"), waSkillText("probe")+"Step 1.\n")
	out := SkillPendingDiff(env, map[string]any{"payload": map[string]any{"action": "patch", "name": "probe", "old_string": "Step 1.", "new_string": "Step ONE.", "replace_all": true}}, nil)
	if strings.Contains(out, "(patch would fail") || strings.Count(out, "+Step ONE.") != 2 {
		t.Fatal(out)
	}
}

func TestBatchPreviewFlagsFailedPatchWithoutFolding(t *testing.T) {
	env := waSkillEnv(t)
	wdWrite(t, filepath.Join(env.Home, "skills", "probe", "SKILL.md"), waSkillText("probe")+"Step 1.\n")
	rec := map[string]any{"id": "pqr678", "payload": map[string]any{"action": "batch", "operations": []any{
		map[string]any{"action": "patch", "name": "probe", "old_string": "Step 1.", "new_string": "Step ONE."},
		map[string]any{"action": "write_file", "name": "probe", "file_path": "ref.md", "file_content": "z\n"},
	}}}
	out := SkillPendingDiff(env, rec, nil)
	if !strings.Contains(out, "(patch would fail:") || !strings.Contains(out, "Found 2 matches") {
		t.Fatal(out)
	}
}

func TestSkillGistShapes(t *testing.T) {
	if got := SkillGist("create", "probe", waSkillText("probe"), "", "", ""); got != "create 'probe' — Probe skill for pending-diff tests. (85 chars)" {
		t.Fatal(got)
	}
	if got := SkillGist("patch", "probe", "", "", "a\nb", "c"); got != "patch 'probe' SKILL.md (+1/-2 lines)" {
		t.Fatal(got)
	}
	if got := SkillGist("remove_file", "probe", "", "ref.md", "", ""); got != "remove ref.md from 'probe'" {
		t.Fatal(got)
	}
	if got := SkillGist("rename", "probe", "", "", "", ""); got != "rename 'probe'" {
		t.Fatal(got)
	}
}
