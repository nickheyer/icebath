package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Approval test environment with headless markers cleared and a fresh config cache
func waEnv(t *testing.T) *tools.Env {
	t.Helper()
	for _, k := range []string{"ICEBATH_CRON_SESSION", "ICEBATH_KANBAN_TASK", "ICEBATH_SESSION_PLATFORM", "ICEBATH_PLATFORM", "ICEBATH_SINGLE_QUERY_SESSION"} {
		t.Setenv(k, "")
	}
	config.ResetCache()
	return testEnv(t)
}

func setGate(t *testing.T, env *tools.Env, subsystem string, enabled bool) {
	t.Helper()
	if err := config.SetKey(env.Home, subsystem+".write_approval", enabled); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

// Store over the environment home, loaded like load_from_disk
func homeStore(t *testing.T, env *tools.Env) *Store {
	t.Helper()
	s := NewStore(filepath.Join(env.Home, "memories"), DefaultMemoryCharLimit, DefaultUserCharLimit, true, true)
	if err := s.LoadFromDisk(); err != nil {
		t.Fatal(err)
	}
	return s
}

// Asker answering every approval with one choice and recording the questions
type choiceAsker struct {
	mu     sync.Mutex
	choice string
	calls  [][]tools.Question
}

func (a *choiceAsker) Ask(_ context.Context, qs []tools.Question) (tools.FormResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, qs)
	return tools.FormResult{Outcome: "answered", Answers: []tools.Answer{{Status: "answered", Selected: []string{a.choice}}}}, nil
}

func pending(t *testing.T, env *tools.Env, args ...string) string {
	t.Helper()
	out, handled, err := HandlePendingSubcommand(env, tools.WriteApprovalMemory, args, PendingOptions{})
	if err != nil || !handled {
		t.Fatal(err, handled)
	}
	return out
}

func approve(t *testing.T, env *tools.Env, store *Store, id string) string {
	t.Helper()
	out, handled, err := HandlePendingSubcommand(env, tools.WriteApprovalMemory, []string{"approve", id}, PendingOptions{MemoryStore: store})
	if err != nil || !handled {
		t.Fatal(err, handled)
	}
	return out
}

func onDisk(t *testing.T, env *tools.Env) *Store {
	t.Helper()
	s, err := LoadOnDiskStore(env)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMemoryGateOffAllowsWriteThroughTheTool(t *testing.T) {
	env := waEnv(t)
	s := homeStore(t, env)
	r := callTool(t, env, s, "add", "user", "save me", nil, nil, nil)
	if !ok(r) || r["entry_count"] != float64(1) || tools.PendingCount(env, "memory") != 0 {
		t.Fatal(r)
	}
}

func TestCLIMemoryApproveWithoutLiveAgentUsesFreshStore(t *testing.T) {
	env := waEnv(t)
	setGate(t, env, "memory", true)
	staging := homeStore(t, env)
	r := callTool(t, env, staging, "add", "memory", "remember the launch date", nil, nil, nil)
	if r["pending_id"] == nil || tools.PendingCount(env, "memory") != 1 {
		t.Fatal(r)
	}
	out, err := MemoryCommand(env, []string{"approve", "all"}, nil)
	if err != nil || strings.Contains(out, "memory store unavailable") || !strings.Contains(out, "Approved 1") {
		t.Fatal(out, err)
	}
	if tools.PendingCount(env, "memory") != 0 {
		t.Fatal("record kept")
	}
	if !containsString(homeStore(t, env).Entries("memory"), "remember the launch date") {
		t.Fatal("write not applied")
	}
}

func TestLoadOnDiskStoreHonorsConfiguredLimitsAndPermissions(t *testing.T) {
	env := waEnv(t)
	for k, v := range map[string]any{"memory.memory_char_limit": 999, "memory.user_char_limit": 444, "memory.memory_enabled": false, "memory.user_profile_enabled": true} {
		if err := config.SetKey(env.Home, k, v); err != nil {
			t.Fatal(err)
		}
	}
	config.ResetCache()
	s := onDisk(t, env)
	if s.MemoryCharLimit != 999 || s.UserCharLimit != 444 || s.MemoryEnabled || !s.UserProfileEnabled {
		t.Fatalf("%+v", s)
	}
	os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte("memory: [unclosed"), 0o644)
	config.ResetCache()
	if kept := onDisk(t, env); kept.MemoryCharLimit != 999 || kept.UserCharLimit != 444 || kept.MemoryEnabled {
		t.Fatalf("broken edit dropped the last good config %+v", kept)
	}
	fresh := waEnv(t)
	os.WriteFile(filepath.Join(fresh.Home, "config.yaml"), []byte("memory: [unclosed"), 0o644)
	config.ResetCache()
	notInts := waEnv(t)
	if err := config.SetKey(notInts.Home, "memory.memory_char_limit", "lots"); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	for _, e := range []*tools.Env{fresh, notInts} {
		fallback := onDisk(t, e)
		if fallback.MemoryCharLimit != DefaultMemoryCharLimit || fallback.UserCharLimit != DefaultUserCharLimit || !fallback.MemoryEnabled || !fallback.UserProfileEnabled {
			t.Fatalf("%+v", fallback)
		}
	}
}

func TestHandleApproveAll(t *testing.T) {
	env := waEnv(t)
	s := homeStore(t, env)
	tools.StageWrite(env, "memory", map[string]any{"action": "add", "target": "user", "content": "a"}, "a", "foreground")
	tools.StageWrite(env, "memory", map[string]any{"action": "add", "target": "user", "content": "b"}, "b", "foreground")
	out := approve(t, env, s, "all")
	if !strings.Contains(out, "Approved 2") || tools.PendingCount(env, "memory") != 0 || len(s.Entries("user")) != 2 {
		t.Fatal(out)
	}
}

func TestHandleApproveSurfacesOverwrittenEntry(t *testing.T) {
	env := waEnv(t)
	s := homeStore(t, env)
	entry := "RULE A: gate merges. RULE B: ci per HEAD. RULE C: never squash."
	must(t)(s.Add("memory", entry))
	tools.StageWrite(env, "memory", map[string]any{"action": "batch", "target": "memory", "operations": []any{
		map[string]any{"action": "replace", "old_text": "RULE B: ci per HEAD.", "content": "RULE B: CI is per-head.", "matched_entry": entry}}},
		"batch", backgroundReviewOrigin)
	out := approve(t, env, s, "all")
	if !strings.Contains(out, "Approved 1") || !strings.Contains(out, entry) || !equalStrings(s.Entries("memory"), []string{"RULE B: CI is per-head."}) {
		t.Fatal(out, s.Entries("memory"))
	}
}

func TestHandleApproveListsAPatchAsTheEntryBeforeTheEdit(t *testing.T) {
	env := waEnv(t)
	s := homeStore(t, env)
	entry := "RULE A: gate merges. RULE B: ci per HEAD. RULE C: never squash."
	must(t)(s.Add("memory", entry))
	tools.StageWrite(env, "memory", map[string]any{"action": "batch", "target": "memory", "operations": []any{
		map[string]any{"action": "patch", "old_text": "ci per HEAD", "content": "CI is per-head", "matched_entry": entry}}},
		"batch", backgroundReviewOrigin)
	out := approve(t, env, s, "all")
	if !strings.Contains(out, "Patched entry (as it was before the edit):") || !strings.Contains(out, entry) || strings.Contains(out, "Overwrote") {
		t.Fatal(out)
	}
	if !equalStrings(s.Entries("memory"), []string{"RULE A: gate merges. RULE B: CI is per-head. RULE C: never squash."}) {
		t.Fatal(s.Entries("memory"))
	}
}

const (
	keptEntry     = "Repo lives in ~/src/app; tests via make test"
	reviewedEntry = "Staging DB: pg-staging-2 (old cluster, retiring)"
)

// Seeds memory and stages a remove the way the unattended background review does
func reviewStagesRemove(t *testing.T, env *tools.Env, shape string) (*Store, string) {
	t.Helper()
	s := homeStore(t, env)
	for _, e := range []string{keptEntry, reviewedEntry} {
		if r := must(t)(s.Add("memory", e)); !ok(r) {
			t.Fatal(r)
		}
	}
	env.Values.Store("write_origin", backgroundReviewOrigin)
	env.Values.Store(ReviewAttendedValuesKey, false)
	var staged map[string]any
	if shape == "single" {
		staged = callTool(t, env, s, "remove", "memory", nil, "Staging DB", nil, nil)
	} else {
		staged = callTool(t, env, s, nil, "memory", nil, nil, nil, ops(
			map[string]any{"action": "remove", "old_text": "Staging DB"},
			map[string]any{"action": "add", "content": "Deploys via make ship"}))
	}
	env.Values.Delete("write_origin")
	env.Values.Delete(ReviewAttendedValuesKey)
	if staged["staged"] != true {
		t.Fatal(staged)
	}
	return s, staged["pending_id"].(string)
}

func TestApproveRefusesStagedRemoveWhoseEntryChanged(t *testing.T) {
	for _, shape := range []string{"single", "batch"} {
		env := waEnv(t)
		s, pid := reviewStagesRemove(t, env, shape)
		newer := "Staging DB: pg-staging-3 (migrated 2026-09-20, creds in vault 'stg')"
		if r := callTool(t, env, s, "patch", "memory", newer, reviewedEntry, nil, nil); !ok(r) {
			t.Fatal(r)
		}
		out := approve(t, env, onDisk(t, env), pid)
		if !equalStrings(onDisk(t, env).Entries("memory"), []string{keptEntry, newer}) || !strings.Contains(out, "changed since it was staged") {
			t.Fatal(shape, out, onDisk(t, env).Entries("memory"))
		}
		if tools.GetPending(env, "memory", pid) == nil {
			t.Fatal("record discarded")
		}
		if !strings.Contains(pending(t, env, "pending"), reviewedEntry) {
			t.Fatal("pending list lacks the pinned entry")
		}
	}
}

func TestApproveNamesTheEntryARemoveDeleted(t *testing.T) {
	for _, shape := range []string{"single", "batch"} {
		env := waEnv(t)
		_, pid := reviewStagesRemove(t, env, shape)
		out := approve(t, env, onDisk(t, env), pid)
		if containsString(onDisk(t, env).Entries("memory"), reviewedEntry) || !strings.Contains(out, reviewedEntry) {
			t.Fatal(shape, out)
		}
	}
}

func TestApproveRefusesUnpinnedLegacyRemove(t *testing.T) {
	env := waEnv(t)
	_, pid := reviewStagesRemove(t, env, "single")
	path := filepath.Join(tools.PendingWriteDir(env, "memory"), pid+".json")
	raw, _ := os.ReadFile(path)
	var record map[string]any
	json.Unmarshal(raw, &record)
	delete(record["payload"].(map[string]any), "matched_entry")
	raw, _ = json.Marshal(record)
	os.WriteFile(path, raw, 0o644)
	if !strings.Contains(pending(t, env, "pending"), "unpinned legacy target") {
		t.Fatal("legacy record not flagged")
	}
	out := approve(t, env, onDisk(t, env), pid)
	if !strings.Contains(out, "Approved 0") || !strings.Contains(out, "predates entry pinning") {
		t.Fatal(out)
	}
	if !containsString(onDisk(t, env).Entries("memory"), reviewedEntry) || tools.GetPending(env, "memory", pid) == nil {
		t.Fatal("legacy remove applied or discarded")
	}
}

func TestHandleApprovalOnAndOff(t *testing.T) {
	env := waEnv(t)
	var captured []bool
	setMode := func(enabled bool) error { captured = append(captured, enabled); return nil }
	out, _, _ := HandlePendingSubcommand(env, tools.WriteApprovalMemory, []string{"approval", "on"}, PendingOptions{SetMode: setMode})
	if len(captured) != 1 || !captured[0] || !strings.Contains(out, "on") {
		t.Fatal(out, captured)
	}
	out, _, _ = HandlePendingSubcommand(env, tools.WriteApprovalSkills, []string{"approval", "off"}, PendingOptions{SetMode: setMode})
	if len(captured) != 2 || captured[1] || !strings.Contains(out, "off") {
		t.Fatal(out, captured)
	}
}

func TestConfigModeSetterPersistsTheGate(t *testing.T) {
	env := waEnv(t)
	out, _, _ := HandlePendingSubcommand(env, tools.WriteApprovalMemory, []string{"approval", "on"}, PendingOptions{SetMode: ConfigModeSetter(env, "memory")})
	config.ResetCache()
	if out != "memory.write_approval set to 'on'." || !tools.WriteApprovalEnabled(env, "memory") {
		t.Fatal(out)
	}
}

func TestMemoryInlineApproveWritesThroughTheTool(t *testing.T) {
	env := waEnv(t)
	setGate(t, env, "memory", true)
	asker := &choiceAsker{choice: "once"}
	env.Ask = asker
	s := homeStore(t, env)
	r := callTool(t, env, s, "add", "memory", "approved fact", nil, nil, nil)
	if !ok(r) || r["staged"] != nil || !equalStrings(s.Entries("memory"), []string{"approved fact"}) || tools.PendingCount(env, "memory") != 0 {
		t.Fatal(r)
	}
	if len(asker.calls) != 1 || !strings.Contains(asker.calls[0][0].Text, "approved fact") {
		t.Fatal(asker.calls)
	}
}

func TestMemoryInlineDenyBlocksThroughTheTool(t *testing.T) {
	env := waEnv(t)
	setGate(t, env, "memory", true)
	env.Ask = &choiceAsker{choice: "deny"}
	s := homeStore(t, env)
	r := callTool(t, env, s, "add", "memory", "denied fact", nil, nil, nil)
	if r["success"] != false || !strings.Contains(strings.ToLower(errText(r)), "denied") || len(s.Entries("memory")) != 0 || tools.PendingCount(env, "memory") != 0 {
		t.Fatal(r)
	}
}

func TestHeadlessMemoryWriteStagesWithoutInlinePromptThroughTheTool(t *testing.T) {
	for _, kv := range [][2]string{{"ICEBATH_SINGLE_QUERY_SESSION", "1"}, {"ICEBATH_CRON_SESSION", "1"}, {"ICEBATH_SESSION_PLATFORM", "webhook"}} {
		env := waEnv(t)
		setGate(t, env, "memory", true)
		t.Setenv(kv[0], kv[1])
		asker := &choiceAsker{choice: "once"}
		env.Ask = asker
		s := homeStore(t, env)
		r := callTool(t, env, s, "add", "memory", "headless fact", nil, nil, nil)
		if len(asker.calls) != 0 || !ok(r) || r["staged"] != true || len(s.Entries("memory")) != 0 || tools.PendingCount(env, "memory") != 1 {
			t.Fatal(kv, r)
		}
		t.Setenv(kv[0], "")
	}
}

func TestMemoryInvalidParamsRejectedBeforeStaging(t *testing.T) {
	env := waEnv(t)
	setGate(t, env, "memory", true)
	r := callTool(t, env, homeStore(t, env), "add", "memory", nil, nil, nil, nil)
	if r["success"] != false || tools.PendingCount(env, "memory") != 0 {
		t.Fatal(r)
	}
}

func stageOneMemoryWrite(t *testing.T, env *tools.Env) map[string]any {
	t.Helper()
	r := callTool(t, env, homeStore(t, env), "add", "memory", "surface hint fact", nil, nil, nil)
	if r["staged"] != true {
		t.Fatal(r)
	}
	return r
}

func TestStagedHintNamesTheReviewCommandOnSlashSurfacesThroughTheTool(t *testing.T) {
	env := waEnv(t)
	setGate(t, env, "memory", true)
	if r := stageOneMemoryWrite(t, env); !strings.Contains(r["message"].(string), "/memory pending") || tools.PendingCount(env, "memory") != 1 {
		t.Fatal(r)
	}
}

func TestStagedHintNamesThePendingDirOnHeadlessSurfacesThroughTheTool(t *testing.T) {
	env := waEnv(t)
	setGate(t, env, "memory", true)
	for _, kv := range [][2]string{{"ICEBATH_CRON_SESSION", "1"}, {"ICEBATH_KANBAN_TASK", "task-7"}, {"ICEBATH_SESSION_PLATFORM", "api_server"}} {
		t.Setenv(kv[0], kv[1])
		msg := stageOneMemoryWrite(t, env)["message"].(string)
		if strings.Contains(msg, "review with /memory pending") {
			t.Fatal(kv, msg)
		}
		if kv[0] != "ICEBATH_KANBAN_TASK" && (!strings.Contains(msg, "pending records live in") || !strings.Contains(msg, tools.PendingWriteDir(env, "memory"))) {
			t.Fatal(kv, msg)
		}
		t.Setenv(kv[0], "")
	}
	if tools.PendingCount(env, "memory") != 3 {
		t.Fatal(tools.PendingCount(env, "memory"))
	}
}

func TestStagedHintKeepsCommandForChatGatewayPlatformThroughTheTool(t *testing.T) {
	env := waEnv(t)
	setGate(t, env, "memory", true)
	t.Setenv("ICEBATH_SESSION_PLATFORM", "telegram")
	if r := stageOneMemoryWrite(t, env); !strings.Contains(r["message"].(string), "/memory pending") {
		t.Fatal(r)
	}
}

func TestEachReviewSubcommandTheCLIServesIsHandled(t *testing.T) {
	verbs := map[string][]string{
		"memory": {"pending", "approve", "apply", "reject", "deny", "drop", "approval", "mode"},
		"skills": {"pending", "diff", "approve", "apply", "reject", "deny", "drop", "approval", "mode"},
	}
	for subsystem, list := range verbs {
		for _, verb := range list {
			env := waEnv(t)
			if _, handled, err := HandlePendingSubcommand(env, subsystem, []string{verb, "x"}, PendingOptions{}); !handled || err != nil {
				t.Fatal(subsystem, verb, err)
			}
		}
	}
}

func TestMemoryHasNoDiffLikeTheCLI(t *testing.T) {
	if _, handled, _ := HandlePendingSubcommand(waEnv(t), "memory", []string{"diff", "x"}, PendingOptions{}); handled {
		t.Fatal("memory diff handled")
	}
	out, err := MemoryCommand(waEnv(t), []string{"diff", "x"}, nil)
	if err != nil || out != memoryUnknownSubcommand {
		t.Fatal(out, err)
	}
}

const probeSkill = "---\nname: %s\ndescription: Probe skill for pending-diff tests.\n---\n# Probe\nStep 1.\n"

func TestSkillsDiffSubcommandEndToEnd(t *testing.T) {
	env := waEnv(t)
	dir := filepath.Join(env.Home, "skills", "probe")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(strings.Replace(probeSkill, "%s", "probe", 1)), 0o644)
	prev := tools.FindSkillDir
	tools.FindSkillDir = func(env *tools.Env, name string) string {
		p := filepath.Join(env.Home, "skills", name)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
		return ""
	}
	defer func() { tools.FindSkillDir = prev }()
	rec := tools.StageWrite(env, "skills", map[string]any{"action": "batch", "operations": []any{
		map[string]any{"action": "create", "name": "newprobe", "content": strings.Replace(probeSkill, "%s", "newprobe", 1)},
		map[string]any{"action": "patch", "name": "probe", "old_string": "Step 1.", "new_string": "Step ONE."},
	}}, "batch(2 ops) on newprobe, probe", "foreground")
	out, handled, err := HandlePendingSubcommand(env, "skills", []string{"diff", rec["id"].(string)}, PendingOptions{})
	if err != nil || !handled || !strings.Contains(out, rec["id"].(string)) || strings.Contains(out, "(batch on") || !strings.Contains(out, "+Step ONE.") {
		t.Fatal(out, err)
	}
}

func TestSkillsApproveRunsTheBoundApplierAndReportsItsFailure(t *testing.T) {
	env := waEnv(t)
	good := tools.StageWrite(env, "skills", map[string]any{"action": "create", "name": "good"}, "good", "foreground")
	bad := tools.StageWrite(env, "skills", map[string]any{"action": "create", "name": "bad"}, "bad", "foreground")
	var applied []string
	opts := PendingOptions{ApplySkill: func(payload map[string]any) string {
		applied = append(applied, payload["name"].(string))
		if payload["name"] == "bad" {
			return `{"success": false, "error": "skill exists"}`
		}
		return `{"success": true}`
	}}
	out, _, err := HandlePendingSubcommand(env, "skills", []string{"approve", "all"}, opts)
	if err != nil || !strings.Contains(out, "Approved 1 skills write(s).") || !strings.Contains(out, bad["id"].(string)+": skill exists") {
		t.Fatal(out, err)
	}
	if len(applied) != 2 || tools.GetPending(env, "skills", good["id"].(string)) != nil || tools.GetPending(env, "skills", bad["id"].(string)) == nil {
		t.Fatal(applied)
	}
}

func TestPendingListTagsBackgroundReviewRecords(t *testing.T) {
	env := waEnv(t)
	auto := tools.StageWrite(env, "memory", map[string]any{"action": "add", "target": "memory", "content": "x"}, "auto one", backgroundReviewOrigin)
	tools.StageWrite(env, "memory", map[string]any{"action": "add", "target": "memory", "content": "y"}, "manual one", "foreground")
	out := pending(t, env)
	if !strings.HasPrefix(out, "memory.write_approval = off\n\nPending memory writes (2):") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "  "+auto["id"].(string)+" [auto]  auto one") || strings.Count(out, "[auto]") != 1 {
		t.Fatal(out)
	}
	if !strings.HasSuffix(out, "\nApply: /memory approve <id>   Reject: /memory reject <id>") {
		t.Fatal(out)
	}
	if got := pending(t, waEnv(t), "pending"); got != "No pending memory writes." {
		t.Fatal(got)
	}
}

func TestRejectSingleAllAndUnknown(t *testing.T) {
	env := waEnv(t)
	a := tools.StageWrite(env, "memory", map[string]any{"action": "add", "target": "memory", "content": "a"}, "a", "foreground")
	tools.StageWrite(env, "memory", map[string]any{"action": "add", "target": "memory", "content": "b"}, "b", "foreground")
	tools.StageWrite(env, "memory", map[string]any{"action": "add", "target": "memory", "content": "c"}, "c", "foreground")
	if out := pending(t, env, "reject", a["id"].(string)); out != "Rejected pending memory write '"+a["id"].(string)+"'." {
		t.Fatal(out)
	}
	if out := pending(t, env, "drop", "nope"); out != "No pending memory write with id 'nope'." {
		t.Fatal(out)
	}
	if out := pending(t, env, "deny", "ALL"); out != "Rejected 2 pending memory write(s)." {
		t.Fatal(out)
	}
	if out := pending(t, env, "reject"); out != "Usage: /memory approve|reject <id>  (or 'all')" {
		t.Fatal(out)
	}
}

func TestApprovalSwitchValidatesAndExplainsWithoutASetter(t *testing.T) {
	env := waEnv(t)
	if out := pending(t, env, "approval", "maybe"); out != "Invalid value 'maybe'. Use: on or off." {
		t.Fatal(out)
	}
	if out := pending(t, env, "mode", "enable"); out != "To change the memory approval gate, run:\n  icebath config set memory.write_approval true" {
		t.Fatal(out)
	}
	if out := pending(t, env, "approval"); out != "memory.write_approval = off\nSet with: /memory approval <on|off>" {
		t.Fatal(out)
	}
}

func TestApproveWithoutAStoreFailsEachRecord(t *testing.T) {
	env := waEnv(t)
	rec := tools.StageWrite(env, "memory", map[string]any{"action": "add", "target": "memory", "content": "a"}, "a", "foreground")
	out := approve(t, env, nil, "all")
	if out != "Approved 0 memory write(s).\nFailed:\n  "+rec["id"].(string)+": memory store unavailable" {
		t.Fatal(out)
	}
}
