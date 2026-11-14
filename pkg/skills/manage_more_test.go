package skills

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const recoveryContent = "---\nname: test-skill\ndescription: A test skill.\n---\n\n# Test Skill\n\nStep 1: Do the thing.\nStep 2: Do the other thing.\n"

// Swaps the post write scan for a fixed blocking verdict until the test ends
func forceScanBlocked(t *testing.T) {
	t.Helper()
	real := manageSteps.securityScan
	manageSteps.securityScan = func(*Scope, string) string { return "blocked" }
	t.Cleanup(func() { manageSteps.securityScan = real })
}

// Reports a true success key on a direct handler result
func objSuccess(o *pyjson.Object) bool {
	v, _ := o.Get("success")
	return v == true
}

// Scope sharing the home and cwd over a copied value map, the analogue of contextvars.copy_context
func copiedScope(s *Scope) *Scope {
	env := *s.Env
	env.Values = &sync.Map{}
	s.Env.Values.Range(func(k, v any) bool {
		env.Values.Store(k, v)
		return true
	})
	cp := *s
	cp.Env = &env
	return &cp
}

// SKILL.md whose frontmatter name matches the directory name
func namedSkillContent(name string) string {
	return "---\nname: " + name + "\ndescription: A test skill for unit testing.\n---\n\n# " + name + "\n\nStep 1: Do the thing.\n"
}

func TestCreateOccupiedDirectoryPreservedWhenScanBlocks(t *testing.T) {
	s, skills := manageScope(t)
	forceScanBlocked(t)
	target := filepath.Join(skills, "new-skill")
	os.Mkdir(target, 0o755)
	data := filepath.Join(target, "data.bin")
	os.WriteFile(data, []byte("keep me"), 0o644)
	result := mustObj(s.createSkill(context.Background(), "new-skill", validSkillContent, nil))
	empty := filepath.Join(skills, "empty-skill")
	os.Mkdir(empty, 0o755)
	emptyResult := mustObj(s.createSkill(context.Background(), "empty-skill", validSkillContent, nil))
	freshResult := mustObj(s.createSkill(context.Background(), "fresh-skill", validSkillContent, nil))
	var unreadableResult *pyjson.Object
	unreadable := filepath.Join(skills, "unreadable-skill")
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		os.Mkdir(unreadable, 0o755)
		os.Chmod(unreadable, 0)
		unreadableResult = mustObj(s.createSkill(context.Background(), "unreadable-skill", validSkillContent, nil))
		os.Chmod(unreadable, 0o700)
	}
	if objSuccess(result) || !strings.Contains(objStr(result, "error"), "Choose another name") {
		t.Fatalf("occupied create %v", result)
	}
	if got := readText(t, data); got != "keep me" || !isDir(target) || lexists(filepath.Join(target, "SKILL.md")) {
		t.Fatalf("occupied dir changed %q", got)
	}
	if objSuccess(emptyResult) || lexists(empty) {
		t.Fatalf("empty leftover kept %v", emptyResult)
	}
	if objSuccess(freshResult) || lexists(filepath.Join(skills, "fresh-skill")) {
		t.Fatalf("fresh dir kept %v", freshResult)
	}
	if unreadableResult != nil {
		if objSuccess(unreadableResult) || !strings.Contains(objStr(unreadableResult, "error"), "Choose another name") || lexists(filepath.Join(unreadable, "SKILL.md")) {
			t.Fatalf("unreadable dir %v", unreadableResult)
		}
	}
}

func TestCreateOccupiedDirectoryRefusedAndEmptyLeftoverReused(t *testing.T) {
	s, skills := manageScope(t)
	category := filepath.Join(skills, "category")
	os.Mkdir(category, 0o755)
	nested := filepath.Join(category, "nested-skill.md")
	os.WriteFile(nested, []byte("nested"), 0o644)
	result := mustObj(s.createSkill(context.Background(), "category", validSkillContent, nil))
	empty := filepath.Join(skills, "empty-skill")
	os.Mkdir(empty, 0o755)
	emptyResult := mustObj(s.createSkill(context.Background(), "empty-skill", validSkillContent, nil))
	if objSuccess(result) || !strings.Contains(objStr(result, "error"), "Choose another name") {
		t.Fatalf("occupied create %v", result)
	}
	if readText(t, nested) != "nested" || lexists(filepath.Join(category, "SKILL.md")) {
		t.Fatal("occupied category changed")
	}
	if !objSuccess(emptyResult) || !lexists(filepath.Join(empty, "SKILL.md")) {
		t.Fatalf("empty leftover not reused %v", emptyResult)
	}
}

func TestPatchRecoveryLoopReachesWorkingPatch(t *testing.T) {
	s, skills := manageScope(t)
	mustObj(s.createSkill(context.Background(), "my-skill", recoveryContent, nil))
	bad := manageCall(t, s, map[string]any{"action": "patch", "name": "my-skill", "new_string": "Step 1: Do it better."})
	if bad["success"] != false || !strings.Contains(strings.ToLower(bad["error"].(string)), "read") {
		t.Fatalf("bad call %v", bad)
	}
	skillMD := filepath.Join(skills, "my-skill", "SKILL.md")
	snippet := "Step 1: Do the thing."
	if !strings.Contains(readText(t, skillMD), snippet) {
		t.Fatal("snippet missing on disk")
	}
	good := manageCall(t, s, map[string]any{"action": "patch", "name": "my-skill", "old_string": snippet, "new_string": "Step 1: Do it better."})
	if good["success"] != true {
		t.Fatalf("guided retry must succeed, got %v", good)
	}
	after := readText(t, skillMD)
	if !strings.Contains(after, "Step 1: Do it better.") || !strings.Contains(after, "Step 2: Do the other thing.") || !strings.HasPrefix(after, "---") {
		t.Fatalf("patch not surgical %q", after)
	}
}

func TestRejectedPatchLeavesFileByteIdentical(t *testing.T) {
	for _, kwargs := range []map[string]any{
		{"old_string": nil, "new_string": "x"},
		{"old_string": "", "new_string": "x"},
		{"old_string": "Step 1: Do the thing.", "new_string": "Step 1: Do the thing."},
	} {
		s, skills := manageScope(t)
		mustObj(s.createSkill(context.Background(), "my-skill", recoveryContent, nil))
		skillMD := filepath.Join(skills, "my-skill", "SKILL.md")
		before := readText(t, skillMD)
		payload := map[string]any{"action": "patch", "name": "my-skill"}
		for k, v := range kwargs {
			payload[k] = v
		}
		result := manageCall(t, s, payload)
		if result["success"] != false || readText(t, skillMD) != before {
			t.Fatalf("rejected patch %v mutated or succeeded %v", kwargs, result)
		}
	}
}

func TestPatchEmptyNewStringDeletesAndMissingNewStringRejected(t *testing.T) {
	s, skills := manageScope(t)
	mustObj(s.createSkill(context.Background(), "my-skill", recoveryContent, nil))
	result := manageCall(t, s, map[string]any{"action": "patch", "name": "my-skill", "old_string": "Step 2: Do the other thing.\n", "new_string": ""})
	if result["success"] != true || strings.Contains(readText(t, filepath.Join(skills, "my-skill", "SKILL.md")), "Step 2:") {
		t.Fatalf("empty new_string wrongly rejected: %v", result)
	}
	missing := manageCall(t, s, map[string]any{"action": "patch", "name": "my-skill", "old_string": "Step 1: Do the thing."})
	if missing["success"] != false || !strings.Contains(missing["error"].(string), "new_string") {
		t.Fatalf("missing new_string %v", missing)
	}
}

func TestPatchExternalSkillWritesInPlace(t *testing.T) {
	s, skills := manageScope(t)
	external := filepath.Join(t.TempDir(), "vault")
	skillDir := filepath.Join(external, "ext-skill")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: ext-skill\ndescription: An external skill.\n---\n\n# External\n\nBody with OLD_MARKER here.\n"), 0o644)
	if err := s.Config.Set("skills.external_dirs", []any{external}); err != nil {
		t.Fatal(err)
	}
	result := mustObj(s.patchSkill(context.Background(), "ext-skill", strp("OLD_MARKER"), strp("NEW_MARKER"), nil, false))
	if !objSuccess(result) || !strings.Contains(readText(t, filepath.Join(skillDir, "SKILL.md")), "NEW_MARKER") {
		t.Fatalf("external patch %v", result.Vals)
	}
	if lexists(filepath.Join(skills, "ext-skill")) {
		t.Fatal("external skill duplicated into the local root")
	}
}

func TestBackgroundReviewImprovesAnySkillButDeletesOnlyManagedOnes(t *testing.T) {
	ctx := context.Background()
	s, skills := manageScope(t)
	mustObj(s.createSkill(ctx, "my-skill", validSkillContent, nil))
	s.SetPinned(ctx, "my-skill", true)
	os.WriteFile(filepath.Join(skills, ".bundled_manifest"), []byte("my-skill:abc\n"), 0o644)
	writeHubLock(t, skills, `{"installed": {"my-skill": {"source": "github"}}}`)
	SetCurrentWriteOrigin(s.Env, BackgroundReview)
	ResetBackgroundReviewReadMarks(s.Env)
	skillMD := filepath.Join(skills, "my-skill", "SKILL.md")
	markBackgroundReviewSkillRead(s.Env, skillMD)
	patched := manageCall(t, s, map[string]any{"action": "patch", "name": "my-skill", "old_string": "Do the thing.", "new_string": "Do the new thing."})
	written := manageCall(t, s, map[string]any{"action": "write_file", "name": "my-skill", "file_path": "references/lesson.md", "file_content": "# Lesson\n"})
	deleted := manageCall(t, s, map[string]any{"action": "delete", "name": "my-skill", "absorbed_into": "my-skill"})
	SetCurrentWriteOrigin(s.Env, "")
	if patched["success"] != true || written["success"] != true {
		t.Fatalf("review improvements refused %v %v", patched, written)
	}
	if !strings.Contains(readText(t, skillMD), "Do the new thing.") {
		t.Fatal("patch not on disk")
	}
	if deleted["success"] != false || !strings.Contains(deleted["error"].(string), "Refusing background curator delete") || !lexists(skillMD) {
		t.Fatalf("managed only delete %v", deleted)
	}
}

func TestBackgroundReviewDeleteFailsClosedWhenOwnershipLookupErrors(t *testing.T) {
	s, skills := manageScope(t)
	mustObj(s.createSkill(context.Background(), "manual-skill", validSkillContent, nil))
	os.WriteFile(filepath.Join(skills, ".bundled_manifest"), []byte{'x', 0x80, 0x81, '\n'}, 0o644)
	SetCurrentWriteOrigin(s.Env, BackgroundReview)
	result := manageCall(t, s, map[string]any{"action": "delete", "name": "manual-skill", "absorbed_into": "manual-skill"})
	SetCurrentWriteOrigin(s.Env, "")
	if result["success"] != false || !strings.Contains(strings.ToLower(result["error"].(string)), "ownership") {
		t.Fatalf("ownership error not fail closed %v", result)
	}
	if !strings.Contains(readText(t, filepath.Join(skills, "manual-skill", "SKILL.md")), "Do the thing.") {
		t.Fatal("skill changed")
	}
}

// Background review delete into a missing umbrella, the origin restored afterwards
func backgroundDelete(t *testing.T, s *Scope, name string) map[string]any {
	t.Helper()
	SetCurrentWriteOrigin(s.Env, BackgroundReview)
	defer SetCurrentWriteOrigin(s.Env, "")
	return manageCall(t, s, map[string]any{"action": "delete", "name": name, "absorbed_into": "umbrella"})
}

func TestBackgroundOwnershipPolicyConsistency(t *testing.T) {
	s, _ := manageScope(t)
	mustObj(s.createSkill(context.Background(), "flip-skill", validSkillContent, nil))
	first := backgroundDelete(t, s, "flip-skill")
	second := backgroundDelete(t, s, "flip-skill")
	if first["success"] != false || second["success"] != false || !strings.Contains(first["error"].(string), "not curator-managed") || !strings.Contains(second["error"].(string), "not curator-managed") {
		t.Fatalf("repeated delete verdicts differ %v %v", first, second)
	}

	fg, _ := manageScope(t)
	mustObj(fg.createSkill(context.Background(), "no-record", validSkillContent, nil))
	if res := manageCall(t, fg, map[string]any{"action": "patch", "name": "no-record", "old_string": "Do the thing.", "new_string": "Do the new thing."}); res["success"] != true {
		t.Fatalf("foreground write refused %v", res)
	}

	ad, _ := manageScope(t)
	mustObj(ad.createSkill(context.Background(), "adopt-me", validSkillContent, nil))
	before := backgroundDelete(t, ad, "adopt-me")
	ad.MarkAgentCreated(context.Background(), "adopt-me")
	after := backgroundDelete(t, ad, "adopt-me")
	if !strings.Contains(before["error"].(string), "not curator-managed") {
		t.Fatalf("unadopted delete %v", before)
	}
	if errText, _ := after["error"].(string); strings.Contains(errText, "curator-managed") {
		t.Fatalf("adoption did not lift the refusal %v", after)
	}
}

// Scope in a curator pass over one agent created skill named reviewed
func curatorPassScope(t *testing.T) (*Scope, string) {
	t.Helper()
	s, skills := manageScope(t)
	SetCurrentWriteOrigin(s.Env, BackgroundReview)
	ResetBackgroundReviewReadMarks(s.Env)
	if res := mustObj(s.createSkill(context.Background(), "reviewed", namedSkillContent("reviewed"), nil)); !objSuccess(res) {
		t.Fatalf("create %v", res.Vals)
	}
	s.MarkAgentCreated(context.Background(), "reviewed")
	return s, skills
}

// Background review patch of the reviewed skill step line
func reviewedPatch(s *Scope) map[string]any {
	out, _ := decodeObject(s.SkillManage(context.Background(), map[string]any{"action": "patch", "name": "reviewed", "old_string": "Step 1: Do the thing.", "new_string": "Step 1: Do the thing safely."}, "", ""))
	return out
}

func TestBackgroundReviewReadMarksAcrossCopiedToolContexts(t *testing.T) {
	ctx := context.Background()
	s, _ := curatorPassScope(t)
	viewed, _ := decodeObject(copiedScope(s).SkillView(ctx, "reviewed", "", "", true))
	if viewed["success"] != true {
		t.Fatalf("view %v", viewed)
	}
	if patched := reviewedPatch(copiedScope(s)); patched["success"] != true {
		t.Fatalf("read in one worker did not authorize the next %v", patched)
	}

	s2, _ := curatorPassScope(t)
	firstReview := copiedScope(s2)
	ResetBackgroundReviewReadMarks(s2.Env)
	secondReview := copiedScope(s2)
	if viewed, _ := decodeObject(firstReview.SkillView(ctx, "reviewed", "", "", true)); viewed["success"] != true {
		t.Fatalf("view %v", viewed)
	}
	blocked := reviewedPatch(secondReview)
	if blocked["success"] != false || blocked["_read_before_write_required"] != true {
		t.Fatalf("read marks leaked between reviews %v", blocked)
	}
}

func TestBackgroundReviewSupportFileOverwriteRequiresThatFileRead(t *testing.T) {
	ctx := context.Background()
	s, skills := curatorPassScope(t)
	ref := filepath.Join(skills, "reviewed", "references")
	os.Mkdir(ref, 0o755)
	os.WriteFile(filepath.Join(ref, "workflow.md"), []byte("old workflow\n"), 0o644)
	if viewed, _ := decodeObject(s.SkillView(ctx, "reviewed", "", "", true)); viewed["success"] != true {
		t.Fatalf("view %v", viewed)
	}
	write := map[string]any{"action": "write_file", "name": "reviewed", "file_path": "references/workflow.md", "file_content": "new workflow\n"}
	blocked := manageCall(t, s, write)
	if blocked["success"] != false || blocked["_read_before_write_required"] != true {
		t.Fatalf("unread support file overwritten %v", blocked)
	}
	if viewed, _ := decodeObject(s.SkillView(ctx, "reviewed", "references/workflow.md", "", true)); viewed["success"] != true {
		t.Fatalf("file view %v", viewed)
	}
	if allowed := manageCall(t, s, write); allowed["success"] != true {
		t.Fatalf("read support file refused %v", allowed)
	}
}

func TestFullCreateViaDispatcherIsNotAgentCreated(t *testing.T) {
	s, _ := manageScope(t)
	result := manageCall(t, s, map[string]any{"action": "create", "name": "test-skill", "content": validSkillContent})
	if result["success"] != true {
		t.Fatalf("create %v", result)
	}
	usage, err := s.LoadUsage()
	if err != nil {
		t.Fatal(err)
	}
	var createdBy any
	if rec, ok := usage.Get("test-skill"); ok {
		if obj, ok := rec.(*pyjson.Object); ok {
			createdBy, _ = obj.Get("created_by")
		}
	}
	switch createdBy {
	case "learn", nil, "", false:
	default:
		t.Fatalf("foreground create marked %v", createdBy)
	}
}

func TestBackgroundReviewDeleteRefusesBundledEvenWithAbsorbedInto(t *testing.T) {
	s, skills := manageScope(t)
	os.WriteFile(filepath.Join(skills, ".bundled_manifest"), []byte("bundled:abc\n"), 0o644)
	SetCurrentWriteOrigin(s.Env, BackgroundReview)
	manageCall(t, s, map[string]any{"action": "create", "name": "umbrella", "content": validSkillContent})
	manageCall(t, s, map[string]any{"action": "create", "name": "bundled", "content": validSkillContent})
	result := manageCall(t, s, map[string]any{"action": "delete", "name": "bundled", "absorbed_into": "umbrella"})
	SetCurrentWriteOrigin(s.Env, "")
	if result["success"] != false || !strings.Contains(strings.ToLower(result["error"].(string)), "bundled") {
		t.Fatalf("bundled delete %v", result)
	}
	if !lexists(filepath.Join(skills, "bundled", "SKILL.md")) {
		t.Fatal("bundled skill removed")
	}
}
