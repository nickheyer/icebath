package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/skills"
)

func curCLI() (*CuratorCLI, *bytes.Buffer) {
	var out bytes.Buffer
	return NewCuratorCLI(&out, &out, strings.NewReader("")), &out
}

func curRunCLI(t *testing.T, ctx context.Context, sub string, a CuratorArgs) (int, string) {
	t.Helper()
	cli, out := curCLI()
	code, err := cli.Run(ctx, sub, a)
	if err != nil {
		t.Fatalf("%s: %v\n%s", sub, err, out.String())
	}
	return code, out.String()
}

func curSidecar(t *testing.T, h string) map[string]map[string]any {
	t.Helper()
	data := map[string]map[string]any{}
	raw, err := os.ReadFile(filepath.Join(h, "skills", ".usage.json"))
	if os.IsNotExist(err) {
		return data
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func curWant(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Fatalf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestCuratorPinFailsLoudlyWhenWriteDoesNotLand(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	name := "sentinel-protected-skill"
	prev := skills.ProtectedBuiltinSkills
	skills.ProtectedBuiltinSkills = map[string]bool{name: true}
	t.Cleanup(func() { skills.ProtectedBuiltinSkills = prev })
	curWriteSkill(t, filepath.Join(h, "skills"), name, "")
	scope, _, _ := curatorScope(ctx)
	if ok, _ := scope.IsAgentCreated(name); !ok {
		t.Fatal("collision skill should read as agent created")
	}
	if ok, _ := scope.IsCurationEligible(name, ""); ok {
		t.Fatal("protected name should be ineligible")
	}
	code, out := curRunCLI(t, ctx, "pin", CuratorArgs{Skill: name})
	if code == 0 {
		t.Fatalf("pin reported success over a silent no-write: %q", out)
	}
	if curGet(curRecord(t, ctx, name), "pinned") == true {
		t.Fatal("usage record claims pinned despite failed eligibility")
	}
	if !strings.Contains(strings.ToLower(out), "pin") {
		t.Fatalf("refusal must mention the pin outcome: %q", out)
	}
}

func TestCuratorPinnedEligibleUnmanagedSkillVisibleInStatus(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteSkill(t, filepath.Join(h, "skills"), "legacy-skill", "")
	code, out := curRunCLI(t, ctx, "pin", CuratorArgs{Skill: "legacy-skill"})
	if code != 0 {
		t.Fatalf("eligible skill pin must succeed (rc=%d): %s", code, out)
	}
	if curGet(curRecord(t, ctx, "legacy-skill"), "pinned") != true {
		t.Fatal("eligible skill pin wrote nothing despite rc 0")
	}
	code, status := curRunCLI(t, ctx, "status", CuratorArgs{})
	if code != 0 || !strings.Contains(status, "legacy-skill") || !strings.Contains(strings.ToLower(status), "pinned") {
		t.Fatalf("pinned skill absent from status (rc=%d):\n%s", code, status)
	}
}

func TestCuratorPinManagedSkillEndToEnd(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteSkill(t, filepath.Join(h, "skills"), "managed-skill", "")
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "managed-skill")
	code, out := curRunCLI(t, ctx, "pin", CuratorArgs{Skill: "managed-skill"})
	if code != 0 || !strings.Contains(strings.ToLower(out), "pinned") {
		t.Fatalf("managed skill pin must succeed (rc=%d): %s", code, out)
	}
	if curGet(curRecord(t, ctx, "managed-skill"), "pinned") != true {
		t.Fatal("managed pin not recorded")
	}
	code, status := curRunCLI(t, ctx, "status", CuratorArgs{})
	if code != 0 {
		t.Fatalf("status rc %d", code)
	}
	curWant(t, strings.ToLower(status), "managed-skill", "pinned")
}

func curLockSkill(t *testing.T, dir, name string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: x\n---\n# "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCuratorLockUnlockRoundTripOnHandWrittenSkill(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curLockSkill(t, filepath.Join(h, "skills"), "coding-standards")
	code, out := curRunCLI(t, ctx, "lock", CuratorArgs{Skill: "coding-standards"})
	if code != 0 {
		t.Fatalf("lock rc %d: %s", code, out)
	}
	curWant(t, out, "locked 'coding-standards'", "icebath curator unlock coding-standards")
	scope, _, _ := curatorScope(ctx)
	if ok, _ := scope.IsLocked("coding-standards"); !ok {
		t.Fatal("skill not locked")
	}
	rec := curSidecar(t, h)["coding-standards"]
	if rec["locked"] != true || rec["pinned"] != false {
		t.Fatalf("sidecar record %v", rec)
	}
	code, out = curRunCLI(t, ctx, "unlock", CuratorArgs{Skill: "coding-standards"})
	if code != 0 {
		t.Fatalf("unlock rc %d: %s", code, out)
	}
	curWant(t, out, "unlocked 'coding-standards'")
	if ok, _ := scope.IsLocked("coding-standards"); ok {
		t.Fatal("skill still locked")
	}
}

func TestCuratorLockRefusesUnresolvedName(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	code, out := curRunCLI(t, ctx, "lock", CuratorArgs{Skill: "no-such-skill"})
	if code != 1 {
		t.Fatalf("rc %d", code)
	}
	curWant(t, out, "no skill named 'no-such-skill'")
	if _, ok := curSidecar(t, h)["no-such-skill"]; ok {
		t.Fatal("refused lock wrote a record")
	}
}

func TestCuratorLockResolvesCategorizedPath(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curLockSkill(t, filepath.Join(h, "skills", "ops"), "deploy-runbook")
	code, out := curRunCLI(t, ctx, "lock", CuratorArgs{Skill: "ops/deploy-runbook"})
	if code != 0 {
		t.Fatalf("rc %d: %s", code, out)
	}
	curWant(t, out, "locked 'deploy-runbook'")
	scope, _, _ := curatorScope(ctx)
	if ok, _ := scope.IsLocked("deploy-runbook"); !ok {
		t.Fatal("directory name not locked")
	}
}

func TestCuratorStatusListsLockedSkills(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curLockSkill(t, filepath.Join(h, "skills"), "coding-standards")
	if code, out := curRunCLI(t, ctx, "lock", CuratorArgs{Skill: "coding-standards"}); code != 0 {
		t.Fatalf("lock rc %d: %s", code, out)
	}
	code, out := curRunCLI(t, ctx, "status", CuratorArgs{})
	if code != 0 {
		t.Fatalf("status rc %d", code)
	}
	curWant(t, out, "locked (1): coding-standards")
}

func TestCuratorPinStillRefusesBundledSkills(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	dir := filepath.Join(h, "skills")
	curWriteSkill(t, dir, "bundled-skill", "")
	if err := os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte("bundled-skill:abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := curRunCLI(t, ctx, "pin", CuratorArgs{Skill: "bundled-skill"})
	if code != 1 {
		t.Fatalf("rc %d: %s", code, out)
	}
	if _, ok := curSidecar(t, h)["bundled-skill"]; ok && curSidecar(t, h)["bundled-skill"]["pinned"] == true {
		t.Fatal("bundled skill was pinned")
	}
	curWant(t, out, "cannot pin")
}

func TestCuratorUsageListsAllProvenances(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	dir := filepath.Join(h, "skills")
	for _, n := range []string{"agent-skill", "bundled-skill", "hub-skill"} {
		curWriteSkill(t, dir, n, "")
	}
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "agent-skill")
	if err := os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte("bundled-skill:abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".hub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hub", "lock.json"), []byte(`{"installed": {"hub-skill": {}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := curRunCLI(t, ctx, "usage", CuratorArgs{Sort: "activity"})
	if code != 0 {
		t.Fatalf("rc %d", code)
	}
	curWant(t, out, "agent=1", "bundled=1", "hub=1", "agent-skill", "bundled-skill", "hub-skill")
}

func TestCuratorUsageEmpty(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	code, out := curRunCLI(t, ctx, "usage", CuratorArgs{Sort: "activity"})
	if code != 0 {
		t.Fatalf("rc %d", code)
	}
	curWant(t, out, "no skills found")
}

func TestCuratorRunDefaultsToSynchronous(t *testing.T) {
	ctx, h := curTestHome(t, map[string]any{"enabled": true})
	curWriteSkill(t, filepath.Join(h, "skills"), "candidate", "")
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "candidate")
	calls := 0
	curatorLLMReview = func(context.Context, string) *pyjson.Object {
		calls++
		return curatorLLMMeta("llm-stub", nil)
	}
	code, out := curRunCLI(t, ctx, "run", CuratorArgs{Consolidate: true})
	if code != 0 {
		t.Fatalf("rc %d: %s", code, out)
	}
	if calls != 1 {
		t.Fatalf("review pass ran %d times before run returned", calls)
	}
	if strings.Contains(out, "background") {
		t.Fatalf("synchronous run printed the background notice:\n%s", out)
	}
}

func TestCuratorArchiveRefusesPinned(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteSkill(t, filepath.Join(h, "skills"), "pinned-skill", "")
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "pinned-skill")
	if !scope.SetPinned(ctx, "pinned-skill", true) {
		t.Fatal("pin failed")
	}
	code, out := curRunCLI(t, ctx, "archive", CuratorArgs{Skill: "pinned-skill"})
	if code != 1 {
		t.Fatalf("rc %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(h, "skills", "pinned-skill", "SKILL.md")); err != nil {
		t.Fatal("pinned skill was archived")
	}
	curWant(t, strings.ToLower(out), "pinned")
	curWant(t, out, "icebath curator unpin")
}

func curAge(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestCuratorPurgeAgesArchiveFromWhenItWasArchived(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	d := curWriteSkill(t, filepath.Join(h, "skills"), "old-helper", "")
	curAge(t, filepath.Join(d, "SKILL.md"))
	curAge(t, d)
	scope, env, _ := curatorScope(ctx)
	scope.RecordCreated(ctx, "old-helper", true, "", "")
	if ok, msg, err := ArchiveSkill(ctx, env, "old-helper"); !ok || err != nil {
		t.Fatalf("archive: %s %v", msg, err)
	}
	usage, err := scope.LoadUsage()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := curGet(usage, "old-helper").(*pyjson.Object)
	rec.Set("archived_at", "2000-01-01T00:00:00+00:00")
	if !scope.SaveUsage(ctx, usage) {
		t.Fatal("save usage failed")
	}
	days := 30
	if code, out := curRunCLI(t, ctx, "purge", CuratorArgs{Days: &days, Yes: true}); code != 0 {
		t.Fatalf("rc %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(h, "skills", ".archive", "old-helper", "SKILL.md")); err != nil {
		t.Fatal("fresh archive purged by its stale archived_at")
	}
}

func TestCuratorPurgePrefersRecordedArchivedAtOverStaleDirMtime(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteSkill(t, filepath.Join(h, "skills"), "legacy", "")
	scope, env, _ := curatorScope(ctx)
	scope.RecordCreated(ctx, "legacy", true, "", "")
	if ok, msg, err := ArchiveSkill(ctx, env, "legacy"); !ok || err != nil {
		t.Fatalf("archive: %s %v", msg, err)
	}
	archived := filepath.Join(h, "skills", ".archive", "legacy")
	curAge(t, archived)
	flattened := filepath.Join(h, "skills", ".archive", "accelerate")
	if err := os.MkdirAll(flattened, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flattened, "SKILL.md"), []byte("---\nname: huggingface-accelerate\ndescription: x\n---\n# body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	curAge(t, flattened)
	usage, err := scope.LoadUsage()
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := curGet(usage, "legacy").(*pyjson.Object)
	acc := pyjson.NewObject()
	acc.Set("state", "archived")
	acc.Set("archived_at", curGet(legacy, "archived_at"))
	usage.Set("huggingface-accelerate", acc)
	if !scope.SaveUsage(ctx, usage) {
		t.Fatal("save usage failed")
	}
	days := 30
	if code, out := curRunCLI(t, ctx, "purge", CuratorArgs{Days: &days, Yes: true}); code != 0 {
		t.Fatalf("rc %d: %s", code, out)
	}
	for _, p := range []string{archived, flattened} {
		if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err != nil {
			t.Fatalf("%s purged despite a recent archived_at", p)
		}
	}
}

func TestCuratorListUnmanagedItemizesAndExplains(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	dir := filepath.Join(h, "skills")
	for _, n := range []string{"legacy-one", "managed-one"} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + n + "\ndescription: test\nversion: 1.0.0\nmetadata:\n  hermes:\n    agent_created: true\n---\n# " + n + "\n"
		if err := os.WriteFile(filepath.Join(dir, n, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "managed-one")
	code, out := curRunCLI(t, ctx, "list-unmanaged", CuratorArgs{})
	if code != 0 || !strings.Contains(out, "legacy-one") || strings.Contains(out, "managed-one") {
		t.Fatalf("rc %d:\n%s", code, out)
	}
	if !strings.Contains(out, "no marker") && !strings.Contains(out, "created_by:null") {
		t.Fatalf("no explanation:\n%s", out)
	}
	curWant(t, out, "curator adopt")
}

func curGet(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}
