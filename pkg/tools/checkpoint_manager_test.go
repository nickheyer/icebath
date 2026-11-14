package tools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCheckpointProjectHashIdentifiesDirAndExpandsTilde(t *testing.T) {
	home := cpFakeHome(t, cpTempDir(t))
	project := filepath.Join(home, "project")
	os.MkdirAll(project, 0o755)
	if checkpointProjectHash(project) != checkpointProjectHash(project) {
		t.Fatal("hash is not deterministic")
	}
	if checkpointProjectHash(project) == checkpointProjectHash(filepath.Join(home, "other")) {
		t.Fatal("different dirs share a hash")
	}
	if checkpointProjectHash("~/project") != checkpointProjectHash(project) {
		t.Fatal("tilde path hashed differently from its expansion")
	}
}

func TestCheckpointStoreInitCreatesGitStore(t *testing.T) {
	f := newCPFixture(t)
	store := f.store()
	if msg, err := initCheckpointStore(f.env, store, f.work); msg != "" || err != nil {
		t.Fatalf("init failed: %q %v", msg, err)
	}
	for _, p := range []string{"HEAD", "objects", filepath.Join("info", "exclude")} {
		if !f.exists(filepath.Join(store, p)) {
			t.Fatalf("missing %s", p)
		}
	}
	if !strings.Contains(f.read(filepath.Join(store, "info", "exclude")), "node_modules/") {
		t.Fatal("exclude file lacks the default patterns")
	}
	if f.exists(filepath.Join(f.work, ".git")) {
		t.Fatal("project became a git repo")
	}
	if msg, err := initCheckpointStore(f.env, store, f.work); msg != "" || err != nil {
		t.Fatal("init is not idempotent")
	}
}

func TestCheckpointLegacyMigrationArchivesPreV2Repos(t *testing.T) {
	f := newCPFixture(t)
	fake := filepath.Join(f.base, "deadbeefcafebabe")
	f.write(filepath.Join(fake, "HEAD"), "ref: refs/heads/main\n")
	f.write(filepath.Join(fake, checkpointWorkdirMarker), f.work+"\n")
	f.mkdir(filepath.Join(fake, "objects"))
	if msg, err := initCheckpointStore(f.env, f.store(), f.work); msg != "" || err != nil {
		t.Fatalf("init failed: %q %v", msg, err)
	}
	if f.exists(fake) {
		t.Fatal("pre-v2 repo was not moved")
	}
	ents, _ := os.ReadDir(f.base)
	var legacies []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "legacy-") {
			legacies = append(legacies, e.Name())
		}
	}
	if len(legacies) != 1 || !f.exists(filepath.Join(f.base, legacies[0], "deadbeefcafebabe", "HEAD")) {
		t.Fatalf("legacy archive layout wrong: %v", legacies)
	}
}

func TestCheckpointDisabledManagerIsInert(t *testing.T) {
	f := newCPFixture(t)
	m := f.mgr(false, 20, 500, 10)
	m.NewTurn()
	if m.EnsureCheckpoint(f.work, "auto") {
		t.Fatal("disabled manager took a checkpoint")
	}
}

func TestCheckpointFirstDedupsAndSkipsUnsafeDirs(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	if !m.EnsureCheckpoint(f.work, "first") {
		t.Fatal("first checkpoint not taken")
	}
	if m.EnsureCheckpoint(f.work, "second") {
		t.Fatal("second checkpoint in the same turn was not deduplicated")
	}
	if m.EnsureCheckpoint("/", "root") {
		t.Fatal("root was snapshotted")
	}
	if m.EnsureCheckpoint(checkpointUserHome(), "home") {
		t.Fatal("home was snapshotted")
	}
}

func TestCheckpointNewTurnResetsDedupButNeedsChanges(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	if !m.EnsureCheckpoint(f.work, "turn 1") {
		t.Fatal("turn 1 not taken")
	}
	m.NewTurn()
	if m.EnsureCheckpoint(f.work, "no changes") {
		t.Fatal("unchanged tree produced a checkpoint")
	}
	m.NewTurn()
	f.write(filepath.Join(f.work, "main.py"), "print('modified')\n")
	if !m.EnsureCheckpoint(f.work, "turn 2") {
		t.Fatal("changed tree not checkpointed")
	}
}

func TestCheckpointListReportsNewestFirst(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	if len(m.ListCheckpoints(f.work)) != 0 {
		t.Fatal("listing before any checkpoint is not empty")
	}
	m.EnsureCheckpoint(f.work, "first")
	got := m.ListCheckpoints(f.work)
	if len(got) != 1 || got[0].Reason != "first" || got[0].Hash == "" || got[0].ShortHash == "" || got[0].Timestamp == "" {
		t.Fatalf("first listing wrong: %+v", got)
	}
	for i, r := range []string{"second", "third"} {
		m.NewTurn()
		f.write(filepath.Join(f.work, "main.py"), "v"+string(rune('2'+i))+"\n")
		m.EnsureCheckpoint(f.work, r)
	}
	reasons := cpReasons(m, f.work)
	if len(reasons) != 3 || reasons[0] != "third" || reasons[2] != "first" {
		t.Fatalf("order wrong: %v", reasons)
	}
}

func TestCheckpointProjectsShareOneStoreButListSeparately(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	a, b := filepath.Join(f.tmp, "proj-a"), filepath.Join(f.tmp, "proj-b")
	f.write(filepath.Join(a, "f.py"), "a\n")
	f.write(filepath.Join(b, "g.py"), "b\n")
	if !m.EnsureCheckpoint(a, "A-1") {
		t.Fatal("A not taken")
	}
	m.NewTurn()
	if !m.EnsureCheckpoint(b, "B-1") {
		t.Fatal("B not taken")
	}
	if !f.exists(filepath.Join(f.base, "store", "HEAD")) {
		t.Fatal("shared store missing")
	}
	for _, p := range []string{a, b} {
		if !f.exists(filepath.Join(f.base, "store", "projects", checkpointProjectHash(p)+".json")) {
			t.Fatalf("metadata missing for %s", p)
		}
	}
	if r := cpReasons(m, a); len(r) != 1 || r[0] != "A-1" {
		t.Fatalf("A listing leaked: %v", r)
	}
	if r := cpReasons(m, b); len(r) != 1 || r[0] != "B-1" {
		t.Fatalf("B listing leaked: %v", r)
	}
}

func TestCheckpointMaxSnapshotsTrimsHistory(t *testing.T) {
	f := newCPFixture(t)
	m := f.mgr(true, 3, 500, 10)
	for i := 0; i < 6; i++ {
		f.write(filepath.Join(f.work, "main.py"), "v"+string(rune('0'+i))+"\n")
		m.NewTurn()
		m.EnsureCheckpoint(f.work, "step-"+string(rune('0'+i)))
	}
	reasons := cpReasons(m, f.work)
	if len(reasons) != 3 || reasons[0] != "step-5" || reasons[2] != "step-3" {
		t.Fatalf("trim wrong: %v", reasons)
	}
}

func TestCheckpointMaxFileSizeSkipsLargeFiles(t *testing.T) {
	f := newCPFixture(t)
	wd := filepath.Join(f.tmp, "proj")
	f.write(filepath.Join(wd, "small.py"), "tiny\n")
	f.write(filepath.Join(wd, "weights.bin"), strings.Repeat("\x00", 2*1024*1024))
	m := f.mgr(true, 5, 500, 1)
	if !m.EnsureCheckpoint(wd, "initial") {
		t.Fatal("not taken")
	}
	ok, files, _ := runCheckpointGit(f.env, []string{"ls-tree", "-r", "--name-only", checkpointRefName(checkpointProjectHash(wd))}, f.store(), wd, checkpointGitOpts{})
	names := pySplitLines(files)
	if !ok || !cpHas(names, "small.py") || cpHas(names, "weights.bin") {
		t.Fatalf("size cap not applied: %v", names)
	}
}

func TestCheckpointRestoreRefusesUncapturedNestedRepository(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	project := filepath.Join(f.tmp, "nestproj")
	nested := filepath.Join(project, "nested")
	cpNestedRepo(t, nested, "main.py", "committed\n")
	if !m.EnsureCheckpoint(project, "initial") {
		t.Fatal("not taken")
	}
	cp := f.newest(m, project)
	f.write(filepath.Join(nested, "main.py"), "agent overwrite\n")
	r := m.Restore(project, cp, "", false)
	if cpSuccess(r) || strings.Join(cpList(r, "nested_repositories"), ",") != "nested" || !strings.Contains(r["error"].(string), "rollback was not performed") {
		t.Fatalf("nested refusal wrong: %v", r)
	}
	if f.read(filepath.Join(nested, "main.py")) != "agent overwrite\n" {
		t.Fatal("nested repo files touched")
	}
}

func TestCheckpointRestoreSelectiveScopeIgnoresUnrelatedNestedRepository(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	project := filepath.Join(f.tmp, "nestproj")
	nested := filepath.Join(project, "nested")
	cpNestedRepo(t, nested, "main.py", "committed\n")
	notes := filepath.Join(project, "notes.txt")
	f.write(notes, "before\n")
	if !m.EnsureCheckpoint(project, "initial") {
		t.Fatal("not taken")
	}
	cp := f.newest(m, project)
	f.write(notes, "after\n")
	f.write(filepath.Join(nested, "main.py"), "agent overwrite\n")
	if r := m.Restore(project, cp, "notes.txt", false); !cpSuccess(r) {
		t.Fatalf("unrelated restore failed: %v", r)
	}
	if f.read(notes) != "before\n" || f.read(filepath.Join(nested, "main.py")) != "agent overwrite\n" {
		t.Fatal("selective restore touched the wrong files")
	}
	for _, requested := range []string{"nested", "nested/main.py"} {
		f.write(notes, "changed again\n")
		r := m.Restore(project, cp, requested, false)
		if cpSuccess(r) || strings.Join(cpList(r, "nested_repositories"), ",") != "nested" {
			t.Fatalf("%s not refused: %v", requested, r)
		}
		if f.read(filepath.Join(nested, "main.py")) != "agent overwrite\n" {
			t.Fatal("nested file restored")
		}
	}
}

func TestCheckpointRestoreUnknownHashFails(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	if cpSuccess(m.Restore(f.work, "abc123", "", false)) {
		t.Fatal("restore without checkpoints succeeded")
	}
	m.EnsureCheckpoint(f.work, "initial")
	if cpSuccess(m.Restore(f.work, "deadbeef1234", "", false)) {
		t.Fatal("restore of unknown hash succeeded")
	}
}

func TestCheckpointTildePathSupportsDiffAndRestoreFlow(t *testing.T) {
	f := newCPFixture(t)
	home := cpFakeHome(t, f.tmp)
	m := f.enabled()
	project := filepath.Join(home, "project")
	file := filepath.Join(project, "main.py")
	f.write(file, "original\n")
	if !m.EnsureCheckpoint("~/project", "initial") {
		t.Fatal("tilde checkpoint not taken")
	}
	m.NewTurn()
	f.write(file, "changed\n")
	cps := m.ListCheckpoints(project)
	if len(cps) != 1 {
		t.Fatalf("listing wrong: %v", cps)
	}
	d := m.Diff("~/project", cps[0].Hash)
	if !cpSuccess(d) || !strings.Contains(d["diff"].(string), "main.py") {
		t.Fatalf("diff wrong: %v", d)
	}
	if r := m.Restore("~/project", cps[0].Hash, "", false); !cpSuccess(r) || f.read(file) != "original\n" {
		t.Fatalf("restore wrong: %v", r)
	}
}

// Baseline checkpoint of the fixture project for the safe restore tests
func cpSafeBase(t *testing.T, f *cpFixture, m *CheckpointManager) string {
	t.Helper()
	if !m.EnsureCheckpoint(f.work, "initial") {
		t.Fatal("baseline not taken")
	}
	m.NewTurn()
	return f.newest(m, f.work)
}

func TestCheckpointSafeRestoreSkipsUserEditedFile(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	base := cpSafeBase(t, f, m)
	f.write(filepath.Join(f.work, "main.py"), "agent version\n")
	m.RecordAgentWrite(filepath.Join(f.work, "main.py"))
	f.write(filepath.Join(f.work, "README.md"), "user hand edit\n")
	r := m.Restore(f.work, base, "", true)
	if !cpSuccess(r) || f.read(filepath.Join(f.work, "main.py")) != "print('hello')\n" || f.read(filepath.Join(f.work, "README.md")) != "user hand edit\n" {
		t.Fatalf("safe restore wrong: %v", r)
	}
	if !cpHas(cpList(r, "skipped_user_edits"), "README.md") || !cpHas(cpList(r, "restored_files"), "main.py") {
		t.Fatalf("classification wrong: %v", r)
	}
}

func TestCheckpointSafeRestoreFindsLedgerUnderWalkedKey(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	base := cpSafeBase(t, f, m)
	f.write(filepath.Join(f.tmp, "package.json"), "{}\n")
	f.write(filepath.Join(f.work, "main.py"), "agent version\n")
	f.write(filepath.Join(f.work, "README.md"), "user hand edit\n")
	m.RecordAgentWrite(filepath.Join(f.work, "main.py"))
	r := m.Restore(f.work, base, "", true)
	if !cpSuccess(r) || f.read(filepath.Join(f.work, "main.py")) != "print('hello')\n" || f.read(filepath.Join(f.work, "README.md")) != "user hand edit\n" {
		t.Fatalf("restore wrong: %v", r)
	}
	if got := cpList(r, "restored_files"); len(got) != 1 || got[0] != "main.py" || !cpHas(cpList(r, "skipped_user_edits"), "README.md") {
		t.Fatalf("classification wrong: %v", r)
	}
}

func TestCheckpointSafeRestoreSkipsFileUserEditedAfterAgent(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	base := cpSafeBase(t, f, m)
	p := filepath.Join(f.work, "main.py")
	f.write(p, "agent version\n")
	m.RecordAgentWrite(p)
	f.write(p, "user tweaked the agent's file\n")
	r := m.Restore(f.work, base, "", true)
	if !cpSuccess(r) || f.read(p) != "user tweaked the agent's file\n" || !cpHas(cpList(r, "skipped_user_edits"), "main.py") {
		t.Fatalf("user edit not preserved: %v", r)
	}
}

func TestCheckpointSafeRestoreRestoresAgentDeletedFile(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	base := cpSafeBase(t, f, m)
	p := filepath.Join(f.work, "main.py")
	f.write(p, "agent version\n")
	m.RecordAgentWrite(p)
	os.Remove(p)
	if r := m.Restore(f.work, base, "", true); !cpSuccess(r) || f.read(p) != "print('hello')\n" {
		t.Fatalf("deleted agent file not restored: %v", r)
	}
}

func TestCheckpointSafeRestoreFallsBackToFullWhenNoLedger(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	base := cpSafeBase(t, f, m)
	p := filepath.Join(f.work, "main.py")
	f.write(p, "changed without ledger\n")
	r := m.Restore(f.work, base, "", true)
	if !cpSuccess(r) || f.read(p) != "print('hello')\n" {
		t.Fatalf("fallback restore wrong: %v", r)
	}
	if _, has := r["skipped_user_edits"]; has {
		t.Fatal("fallback carried a per file classification")
	}
}

func TestCheckpointSafeRestoreRemovesAgentCreatedFileKeepsUserEdit(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	base := cpSafeBase(t, f, m)
	agent := filepath.Join(f.work, "agent.txt")
	f.write(agent, "agent file\n")
	m.RecordAgentWrite(agent)
	f.write(filepath.Join(f.work, "README.md"), "user edit\n")
	r := m.Restore(f.work, base, "", true)
	if !cpSuccess(r) || f.read(filepath.Join(f.work, "README.md")) != "user edit\n" || f.exists(agent) {
		t.Fatalf("restore wrong: %v", r)
	}
	if !cpHas(cpList(r, "skipped_user_edits"), "README.md") || !cpHas(cpList(r, "restored_files"), "agent.txt") {
		t.Fatalf("classification wrong: %v", r)
	}
}

func TestCheckpointSafeRestoreKeepsAnOversizeFileTheCapExcluded(t *testing.T) {
	f := newCPFixture(t)
	m := f.mgr(true, 50, 500, 1)
	corpus := filepath.Join(f.work, "corpus.jsonl")
	f.write(corpus, "original\n"+strings.Repeat("x", 2*1024*1024))
	base := cpSafeBase(t, f, m)
	f.write(corpus, "agent appended\n"+strings.Repeat("y", 2*1024*1024))
	m.RecordAgentWrite(corpus)
	r := m.Restore(f.work, base, "", true)
	if !cpSuccess(r) || !f.exists(corpus) || !strings.HasPrefix(f.read(corpus), "agent appended") || !cpHas(cpList(r, "skipped_oversize"), "corpus.jsonl") {
		t.Fatalf("oversize file not kept: %v", r)
	}
}

func TestCheckpointSafeRestoreDoesNotReportAKeptFileAsRestored(t *testing.T) {
	f := newCPFixture(t)
	m := f.mgr(true, 50, 500, 1)
	corpus := filepath.Join(f.work, "corpus.jsonl")
	f.write(corpus, strings.Repeat("o", 2*1024*1024))
	base := cpSafeBase(t, f, m)
	f.write(corpus, strings.Repeat("a", 2*1024*1024))
	m.RecordAgentWrite(corpus)
	f.write(filepath.Join(f.work, "main.py"), "agent version\n")
	m.RecordAgentWrite(filepath.Join(f.work, "main.py"))
	r := m.Restore(f.work, base, "", true)
	if got := cpList(r, "restored_files"); len(got) != 1 || got[0] != "main.py" || f.read(filepath.Join(f.work, "main.py")) != "print('hello')\n" {
		t.Fatalf("restored_files wrong: %v", r)
	}
}

func TestCheckpointSafeRestoreStillRevertsAFileThatGrewPastTheCap(t *testing.T) {
	f := newCPFixture(t)
	m := f.mgr(true, 50, 500, 1)
	grew := filepath.Join(f.work, "grew.txt")
	f.write(grew, "precious original\n")
	base := cpSafeBase(t, f, m)
	f.write(grew, "agent bloated it\n"+strings.Repeat("b", 2*1024*1024))
	m.RecordAgentWrite(grew)
	r := m.Restore(f.work, base, "", true)
	if !cpSuccess(r) || f.read(grew) != "precious original\n" || !cpHas(cpList(r, "restored_files"), "grew.txt") || cpHas(cpList(r, "skipped_oversize"), "grew.txt") {
		t.Fatalf("grown file not reverted: %v", r)
	}
}

func TestCheckpointTheCapBoundaryIsTheSameOnBothSidesOfTheRoundTrip(t *testing.T) {
	f := newCPFixture(t)
	capBytes := 1024 * 1024
	m := f.mgr(true, 50, 500, 1)
	atCap, overCap := filepath.Join(f.work, "at_cap.bin"), filepath.Join(f.work, "over_cap.bin")
	f.write(atCap, strings.Repeat("o", capBytes))
	f.write(overCap, strings.Repeat("o", capBytes+1))
	base := cpSafeBase(t, f, m)
	f.write(atCap, strings.Repeat("a", capBytes))
	f.write(overCap, strings.Repeat("a", capBytes+1))
	m.RecordAgentWrite(atCap)
	m.RecordAgentWrite(overCap)
	r := m.Restore(f.work, base, "", true)
	if !cpSuccess(r) {
		t.Fatalf("restore failed: %v", r)
	}
	if f.read(atCap) != strings.Repeat("o", capBytes) || !cpHas(cpList(r, "restored_files"), "at_cap.bin") || cpHas(cpList(r, "skipped_oversize"), "at_cap.bin") {
		t.Fatalf("at cap file wrong: %v", r)
	}
	if f.read(overCap) != strings.Repeat("a", capBytes+1) || !cpHas(cpList(r, "skipped_oversize"), "over_cap.bin") || cpHas(cpList(r, "restored_files"), "over_cap.bin") {
		t.Fatalf("over cap file wrong: %v", r)
	}
}

func TestCheckpointSafeRestoreStillDeletesASmallAgentCreatedFile(t *testing.T) {
	f := newCPFixture(t)
	m := f.mgr(true, 50, 500, 1)
	base := cpSafeBase(t, f, m)
	scratch := filepath.Join(f.work, "scratch.txt")
	f.write(scratch, "agent scratch\n")
	m.RecordAgentWrite(scratch)
	r := m.Restore(f.work, base, "", true)
	if f.exists(scratch) || !cpHas(cpList(r, "restored_files"), "scratch.txt") || len(cpList(r, "skipped_oversize")) != 0 {
		t.Fatalf("small agent file not deleted: %v", r)
	}
	if _, has := r["failed_deletes"]; has {
		t.Fatal("failed_deletes present without a failure")
	}
}

func TestCheckpointSafeRestoreDoesNotReportAFailedDeleteAsRestored(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	base := cpSafeBase(t, f, m)
	stubborn := filepath.Join(f.work, "stubborn.txt")
	f.write(stubborn, "agent scratch\n")
	m.RecordAgentWrite(stubborn)
	cpSwap(t, &checkpointRemove, func(p string) error {
		if filepath.Base(p) == "stubborn.txt" {
			return &os.PathError{Op: "unlink", Path: p, Err: syscall.EACCES}
		}
		return os.Remove(p)
	})
	r := m.Restore(f.work, base, "", true)
	if !cpSuccess(r) || !f.exists(stubborn) || cpHas(cpList(r, "restored_files"), "stubborn.txt") {
		t.Fatalf("failed delete misreported: %v", r)
	}
	if got := cpList(r, "failed_deletes"); len(got) != 1 || got[0] != "stubborn.txt" {
		t.Fatalf("failed_deletes wrong: %v", r)
	}
}

func TestCheckpointUnsafeRestoreOverwritesEverything(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	base := cpSafeBase(t, f, m)
	f.write(filepath.Join(f.work, "main.py"), "agent version\n")
	m.RecordAgentWrite(filepath.Join(f.work, "main.py"))
	f.write(filepath.Join(f.work, "README.md"), "user hand edit\n")
	r := m.Restore(f.work, base, "", false)
	if !cpSuccess(r) || f.read(filepath.Join(f.work, "main.py")) != "print('hello')\n" || f.read(filepath.Join(f.work, "README.md")) != "# Project\n" {
		t.Fatalf("full restore wrong: %v", r)
	}
}

func TestCheckpointRecordAgentWriteDisabledManagerNoop(t *testing.T) {
	f := newCPFixture(t)
	f.mgr(false, 20, 500, 10).RecordAgentWrite(filepath.Join(f.work, "main.py"))
	if f.exists(filepath.Join(f.base, "store")) {
		t.Fatal("disabled manager created a store")
	}
}

func TestCheckpointRegisteredProjectOwnsWritesEvenBelowAnAncestorMarker(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	f.write(filepath.Join(filepath.Dir(f.work), "pyproject.toml"), "[project]\n")
	if !m.EnsureCheckpoint(f.work, "original") {
		t.Fatal("not taken")
	}
	cp := f.newest(m, f.work)
	target := filepath.Join(f.work, "main.py")
	f.write(target, "agent\n")
	m.RecordAgentWrite(target)
	f.write(target, "user\n")
	if r := m.Restore(f.work, cp, "", true); !cpSuccess(r) || f.read(target) != "user\n" {
		t.Fatalf("user edit lost: %v", r)
	}
	if wd, err := m.WorkingDirForPath(target); err != nil || wd != f.work {
		t.Fatalf("owner wrong: %q %v", wd, err)
	}
}

func TestCheckpointResolvesProjectRootMarkers(t *testing.T) {
	tmp := cpTempDir(t)
	home := cpFakeHome(t, tmp)
	m := NewCheckpointManager(nil, CheckpointManagerOptions{Enabled: true, MaxSnapshots: 20})
	gitProj := filepath.Join(tmp, "myproject")
	os.MkdirAll(filepath.Join(gitProj, "src"), 0o755)
	os.MkdirAll(filepath.Join(gitProj, ".git"), 0o755)
	os.WriteFile(filepath.Join(gitProj, "src", "main.py"), []byte("x\n"), 0o644)
	if wd, _ := m.WorkingDirForPath(filepath.Join(gitProj, "src", "main.py")); wd != gitProj {
		t.Fatalf(".git marker not found: %s", wd)
	}
	pyProj := filepath.Join(home, "pyproj")
	os.MkdirAll(filepath.Join(pyProj, "src"), 0o755)
	os.WriteFile(filepath.Join(pyProj, "pyproject.toml"), []byte("[project]\n"), 0o644)
	os.WriteFile(filepath.Join(pyProj, "src", "file.py"), []byte("x\n"), 0o644)
	if wd, _ := m.WorkingDirForPath("~/pyproj/src/file.py"); wd != pyProj {
		t.Fatalf("pyproject marker through tilde not found: %s", wd)
	}
}

func TestCheckpointWorkingDirFallsBackToParent(t *testing.T) {
	home := cpFakeHome(t, cpTempDir(t))
	m := NewCheckpointManager(nil, CheckpointManagerOptions{Enabled: true, MaxSnapshots: 20})
	file := filepath.Join(home, "random", "file.py")
	os.MkdirAll(filepath.Dir(file), 0o755)
	os.WriteFile(file, []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(home, "pyproject.toml"), []byte("[project]\n"), 0o644)
	if wd, _ := m.WorkingDirForPath(file); wd != filepath.Dir(file) {
		t.Fatalf("fallback wrong: %s", wd)
	}
}

func TestCheckpointGitEnvPinsStoreWorktreeAndIgnoresAmbientGitState(t *testing.T) {
	tmp := cpTempDir(t)
	home := cpFakeHome(t, tmp)
	store, work := filepath.Join(tmp, "store"), filepath.Join(tmp, "work")
	t.Setenv("GIT_INDEX_FILE", "/some/index")
	g, err := checkpointGitEnv(nil, store, work, "")
	if err != nil {
		t.Fatal(err)
	}
	if g["GIT_DIR"] != store || g["GIT_WORK_TREE"] != checkpointResolve(work) {
		t.Fatalf("store or worktree not pinned: %v %v", g["GIT_DIR"], g["GIT_WORK_TREE"])
	}
	if _, leaked := g["GIT_INDEX_FILE"]; leaked {
		t.Fatal("ambient index leaked")
	}
	if g["GIT_CONFIG_GLOBAL"] != os.DevNull || g["GIT_CONFIG_SYSTEM"] != os.DevNull || g["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Fatal("user config not neutralised")
	}
	g, _ = checkpointGitEnv(nil, store, work, filepath.Join(store, "indexes", "abc"))
	if !strings.HasSuffix(g["GIT_INDEX_FILE"], filepath.Join("indexes", "abc")) {
		t.Fatalf("index not set: %s", g["GIT_INDEX_FILE"])
	}
	tilde := filepath.Join(home, "work")
	os.MkdirAll(tilde, 0o755)
	g, _ = checkpointGitEnv(nil, store, "~/work", "")
	if g["GIT_WORK_TREE"] != checkpointResolve(tilde) {
		t.Fatalf("tilde not expanded: %s", g["GIT_WORK_TREE"])
	}
}

func TestCheckpointFailuresNeverRaise(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	cpSwap(t, &runCheckpointGit, func(*Env, []string, string, string, checkpointGitOpts) (bool, string, string) {
		panic(errors.New("git exploded"))
	})
	if m.EnsureCheckpoint(f.work, "test") {
		t.Fatal("exploding git produced a checkpoint")
	}
	cpSwap(t, &checkpointWhich, func(string, string) string { return "" })
	m.NewTurn()
	if m.EnsureCheckpoint(f.work, "test") {
		t.Fatal("missing git produced a checkpoint")
	}
}

func TestCheckpointTouchProjectNonDictMetaDoesNotFail(t *testing.T) {
	tmp := cpTempDir(t)
	store := filepath.Join(tmp, "store")
	workdir := filepath.Join(tmp, "project")
	initCheckpointStore(nil, store, workdir)
	metaPath := checkpointProjectMetaPath(store, checkpointProjectHash(workdir))
	os.MkdirAll(filepath.Dir(metaPath), 0o755)
	os.WriteFile(metaPath, []byte("[]"), 0o644)
	touchCheckpointProject(nil, store, workdir)
	data := cpReadJSON(t, metaPath)
	if _, ok := data["last_touch"]; !ok {
		t.Fatal("last_touch missing")
	}
	if _, ok := data["workdir"]; !ok {
		t.Fatal("workdir missing")
	}
}

func TestCheckpointRestoreRejectsArgumentInjection(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	m.EnsureCheckpoint(f.work, "initial")
	r := m.Restore(f.work, "--patch", "", false)
	if cpSuccess(r) || !strings.Contains(r["error"].(string), "Invalid commit hash") || !strings.Contains(r["error"].(string), "must not start with '-'") {
		t.Fatalf("flag hash accepted: %v", r)
	}
	if r := m.Restore(f.work, "-p", "", false); cpSuccess(r) || !strings.Contains(r["error"].(string), "Invalid commit hash") {
		t.Fatalf("-p accepted: %v", r)
	}
}

func TestCheckpointRestoreRejectsInvalidHexChars(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	m.EnsureCheckpoint(f.work, "initial")
	if r := m.Restore(f.work, "abc; rm -rf /", "", false); cpSuccess(r) || !strings.Contains(r["error"].(string), "expected 4-64 hex characters") {
		t.Fatalf("restore accepted junk: %v", r)
	}
	if r := m.Diff(f.work, "abc&def"); cpSuccess(r) || !strings.Contains(r["error"].(string), "expected 4-64 hex characters") {
		t.Fatalf("diff accepted junk: %v", r)
	}
}

func TestCheckpointRestoreFilePathConfinedToWorkingDir(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	m.EnsureCheckpoint(f.work, "initial")
	target := f.newest(m, f.work)
	if r := m.Restore(f.work, target, filepath.Join(f.tmp, "outside_file.txt"), false); cpSuccess(r) || !strings.Contains(r["error"].(string), "got absolute path") {
		t.Fatalf("absolute path accepted: %v", r)
	}
	if r := m.Restore(f.work, target, "../outside_file.txt", false); cpSuccess(r) || !strings.Contains(r["error"].(string), "escapes the working directory") {
		t.Fatalf("traversal accepted: %v", r)
	}
	if r := m.Restore(f.work, target, "main.py", false); !cpSuccess(r) {
		t.Fatalf("inside path refused: %v", r)
	}
	f.write(filepath.Join(f.work, "subdir", "test.txt"), "hello")
	m.NewTurn()
	m.EnsureCheckpoint(f.work, "second")
	if r := m.Restore(f.work, f.newest(m, f.work), "subdir/test.txt", false); !cpSuccess(r) {
		t.Fatalf("nested inside path refused: %v", r)
	}
}

func TestCheckpointRestoreFilePathSymlinkEscapeRefused(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	m.EnsureCheckpoint(f.work, "initial")
	target := f.newest(m, f.work)
	outside := filepath.Join(f.tmp, "outside", "deep")
	f.mkdir(outside)
	if err := os.Symlink(outside, filepath.Join(f.work, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if r := m.Restore(f.work, target, "link/../x", false); cpSuccess(r) || !strings.Contains(r["error"].(string), "escapes the working directory") {
		t.Fatalf("symlink parent traversal accepted: %v", r)
	}
}

func TestCheckpointInitDisablesCommitAndTagSigning(t *testing.T) {
	f := newCPFixture(t)
	store := f.store()
	initCheckpointStore(f.env, store, f.work)
	for _, key := range []string{"commit.gpgsign", "tag.gpgSign"} {
		out, _ := exec.Command("git", "config", "--file", filepath.Join(store, "config"), "--get", key).Output()
		if strings.TrimSpace(string(out)) != "false" {
			t.Fatalf("%s not disabled: %q", key, out)
		}
	}
}

func TestCheckpointWorksWithGlobalGpgsignAndBrokenGpg(t *testing.T) {
	f := newCPFixture(t)
	fakeHome := filepath.Join(f.tmp, "fake_home")
	f.write(filepath.Join(fakeHome, ".gitconfig"), "[user]\n    email = real@user.com\n    name = Real User\n"+
		"[commit]\n    gpgsign = true\n[tag]\n    gpgSign = true\n[gpg]\n    program = /nonexistent/fake-gpg-binary\n")
	t.Setenv("HOME", fakeHome)
	t.Setenv("GPG_TTY", "")
	t.Setenv("DISPLAY", "")
	m := f.mgr(true, 20, 500, 10)
	if !m.EnsureCheckpoint(f.work, "with-global-gpgsign") || len(m.ListCheckpoints(f.work)) != 1 {
		t.Fatal("global gpgsign broke checkpoints")
	}
}

func TestCheckpointPruneDeletesLegacyOrphanWhenWorkdirMissing(t *testing.T) {
	tmp := cpTempDir(t)
	base := filepath.Join(tmp, "checkpoints")
	alive := filepath.Join(tmp, "alive")
	os.MkdirAll(alive, 0o755)
	aliveRepo := cpSeedLegacyRepo(t, base, strings.Repeat("aaaa", 4), alive, nil)
	orphanRepo := cpSeedLegacyRepo(t, base, strings.Repeat("bbbb", 4), filepath.Join(tmp, "was-deleted"), nil)
	r, err := PruneCheckpoints(nil, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: true, Base: base})
	if err != nil || r.Scanned != 2 || r.DeletedOrphan != 1 || r.DeletedStale != 0 {
		t.Fatalf("counts wrong: %+v %v", r, err)
	}
	if _, err := os.Stat(aliveRepo); err != nil {
		t.Fatal("alive repo deleted")
	}
	if _, err := os.Stat(orphanRepo); err == nil {
		t.Fatal("orphan repo kept")
	}
}

func TestCheckpointPruneNoopCasesDeleteNothing(t *testing.T) {
	tmp := cpTempDir(t)
	r, _ := PruneCheckpoints(nil, CheckpointPruneOptions{RetentionDays: 7, DeleteOrphans: true, Base: filepath.Join(tmp, "does-not-exist")})
	if r.Scanned != 0 || r.DeletedOrphan != 0 {
		t.Fatalf("missing base not a noop: %+v", r)
	}
	base := filepath.Join(tmp, "checkpoints")
	orphan := cpSeedLegacyRepo(t, base, strings.Repeat("ffff", 4), filepath.Join(tmp, "gone"), nil)
	os.MkdirAll(filepath.Join(base, "garbage-dir"), 0o755)
	os.WriteFile(filepath.Join(base, "garbage-dir", "random.txt"), []byte("hi"), 0o644)
	r, _ = PruneCheckpoints(nil, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: false, Base: base})
	if r.Scanned != 1 || r.DeletedOrphan != 0 {
		t.Fatalf("keep orphans wrong: %+v", r)
	}
	for _, p := range []string{orphan, filepath.Join(base, "garbage-dir")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s deleted", p)
		}
	}
}

func TestCheckpointPruneDeletesOrphanProjectEntry(t *testing.T) {
	f := newCPFixture(t)
	alive, gone := filepath.Join(f.tmp, "alive"), filepath.Join(f.tmp, "was-gone")
	f.write(filepath.Join(alive, "f"), "a")
	f.write(filepath.Join(gone, "g"), "b")
	m := f.mgr(true, 20, 500, 10)
	if !m.EnsureCheckpoint(alive, "alive") {
		t.Fatal("alive not taken")
	}
	m.NewTurn()
	if !m.EnsureCheckpoint(gone, "gone") {
		t.Fatal("gone not taken")
	}
	os.RemoveAll(gone)
	r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: true, Base: f.base})
	if r.DeletedOrphan < 1 {
		t.Fatalf("orphan not deleted: %+v", r)
	}
	if !f.exists(filepath.Join(f.base, "store", "projects", checkpointProjectHash(alive)+".json")) {
		t.Fatal("alive project wiped")
	}
	if f.exists(filepath.Join(f.base, "store", "projects", checkpointProjectHash(gone)+".json")) {
		t.Fatal("gone project kept")
	}
}

func TestCheckpointRetentionPrunesStaleProjectsAndLegacyArchives(t *testing.T) {
	f := newCPFixture(t)
	fresh, stale := filepath.Join(f.tmp, "fresh"), filepath.Join(f.tmp, "stale")
	f.write(filepath.Join(fresh, "f"), "f")
	f.write(filepath.Join(stale, "s"), "s")
	m := f.mgr(true, 20, 500, 10)
	m.EnsureCheckpoint(fresh, "fresh")
	m.NewTurn()
	m.EnsureCheckpoint(stale, "stale")
	metaPath := filepath.Join(f.base, "store", "projects", checkpointProjectHash(stale)+".json")
	meta := cpReadJSON(t, metaPath)
	meta["last_touch"] = float64(time.Now().Unix()) - 60*86400
	cpWriteJSON(t, metaPath, meta)
	oldLegacy := filepath.Join(f.base, "legacy-20200101-000000")
	f.write(filepath.Join(oldLegacy, "junk"), strings.Repeat("x", 1000))
	old := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(filepath.Join(oldLegacy, "junk"), old, old)
	os.Chtimes(oldLegacy, old, old)
	r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 30, DeleteOrphans: false, Base: f.base})
	if r.DeletedStale < 2 {
		t.Fatalf("stale count wrong: %+v", r)
	}
	if !f.exists(filepath.Join(f.base, "store", "projects", checkpointProjectHash(fresh)+".json")) || f.exists(metaPath) || f.exists(oldLegacy) {
		t.Fatal("retention deleted the wrong entries")
	}
}

func TestCheckpointV2AllowlistRestrictsDeletionToApprovedHash(t *testing.T) {
	f := newCPFixture(t)
	previewed, newlyGone := filepath.Join(f.tmp, "previewed-gone"), filepath.Join(f.tmp, "newly-gone")
	f.write(filepath.Join(previewed, "f"), "a")
	f.write(filepath.Join(newlyGone, "f"), "b")
	m := f.mgr(true, 20, 500, 10)
	m.EnsureCheckpoint(previewed, "previewed")
	m.NewTurn()
	m.EnsureCheckpoint(newlyGone, "newly-gone")
	ph, nh := checkpointProjectHash(previewed), checkpointProjectHash(newlyGone)
	os.RemoveAll(previewed)
	os.RemoveAll(newlyGone)
	r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: true, Base: f.base, OrphanAllowlist: map[string]bool{ph: true}})
	if r.DeletedOrphan != 1 || f.exists(filepath.Join(f.base, "store", "projects", ph+".json")) || !f.exists(filepath.Join(f.base, "store", "projects", nh+".json")) {
		t.Fatalf("allowlist not honoured: %+v", r)
	}
}

func TestCheckpointPreV2AllowlistRestrictsDeletionToApprovedPath(t *testing.T) {
	tmp := cpTempDir(t)
	base := filepath.Join(tmp, "checkpoints")
	previewedRepo := cpSeedLegacyRepo(t, base, strings.Repeat("aaaa", 4), filepath.Join(tmp, "previewed-gone"), nil)
	newlyGoneRepo := cpSeedLegacyRepo(t, base, strings.Repeat("bbbb", 4), filepath.Join(tmp, "newly-gone"), nil)
	r, _ := PruneCheckpoints(nil, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: true, Base: base, OrphanAllowlist: map[string]bool{previewedRepo: true}})
	if r.DeletedOrphan != 1 {
		t.Fatalf("count wrong: %+v", r)
	}
	if _, err := os.Stat(previewedRepo); err == nil {
		t.Fatal("previewed repo kept")
	}
	if _, err := os.Stat(newlyGoneRepo); err != nil {
		t.Fatal("unapproved repo deleted")
	}
}

func TestCheckpointPreviewBoundAllowlistSurvivesOrphanDuringConfirmation(t *testing.T) {
	f := newCPFixture(t)
	previewedWork := filepath.Join(f.tmp, "was-deleted-before-preview")
	stillAlive := filepath.Join(f.tmp, "still-alive-during-preview")
	f.mkdir(stillAlive)
	first := cpSeedLegacyRepo(t, f.base, strings.Repeat("eeee", 4), previewedWork, nil)
	second := cpSeedLegacyRepo(t, f.base, strings.Repeat("ffff", 4), stillAlive, nil)
	status, err := CheckpointStoreStatus(f.env, "")
	if err != nil {
		t.Fatal(err)
	}
	allow := map[string]bool{}
	for _, p := range status.Projects {
		if !p.Exists {
			allow[p.Hash] = true
		}
	}
	for _, p := range status.PreV2Projects {
		if !p.Exists {
			allow[p.Path] = true
		}
	}
	os.RemoveAll(stillAlive)
	if _, err := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: true, OrphanAllowlist: allow}); err != nil {
		t.Fatal(err)
	}
	if f.exists(first) || !f.exists(second) {
		t.Fatal("deletion not bound to the previewed set")
	}
	empty, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: true, OrphanAllowlist: map[string]bool{}})
	if empty.DeletedOrphan != 0 || !f.exists(second) {
		t.Fatal("empty allowlist deleted an unpreviewed orphan")
	}
}

func TestCheckpointPruneSweepsTmpPackDebrisEvenWhenNoRefMoved(t *testing.T) {
	f := newCPFixture(t)
	cpSwap(t, &checkpointGitProcRunning, func(*Env) bool { return false })
	work := filepath.Join(f.tmp, "proj")
	f.write(filepath.Join(work, "f"), "f")
	f.mgr(true, 20, 500, 10).EnsureCheckpoint(work, "seed")
	pack := filepath.Join(f.base, "store", "objects", "pack")
	f.mkdir(pack)
	debris := filepath.Join(pack, "tmp_pack_killedGc")
	f.write(debris, strings.Repeat("x", 512))
	stamp := time.Now().Add(-11 * time.Minute)
	os.Chtimes(debris, stamp, stamp)
	fresh := filepath.Join(pack, "tmp_pack_inFlight")
	f.write(fresh, "y")
	r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 30, DeleteOrphans: false, Base: f.base})
	if r.DeletedStale != 0 || f.exists(debris) || !f.exists(fresh) || r.BytesFreed < 512 {
		t.Fatalf("debris sweep wrong: %+v", r)
	}
}

func TestCheckpointMaybeAutoPrunePrunesOnceThenSkipsWithinInterval(t *testing.T) {
	tmp := cpTempDir(t)
	base := filepath.Join(tmp, "checkpoints")
	os.MkdirAll(base, 0o755)
	os.WriteFile(filepath.Join(base, ".last_prune"), []byte("not-a-timestamp"), 0o644)
	cpSeedLegacyRepo(t, base, strings.Repeat("0000", 4), filepath.Join(tmp, "gone"), nil)
	o := DefaultCheckpointAutoPruneOptions()
	o.Base = base
	out := MaybeAutoPruneCheckpoints(nil, o)
	if out.Skipped || out.Result == nil || out.Result.DeletedOrphan != 1 {
		t.Fatalf("first run wrong: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(base, ".last_prune")); err != nil {
		t.Fatal("marker missing")
	}
	also := cpSeedLegacyRepo(t, base, strings.Repeat("2222", 4), filepath.Join(tmp, "also-gone"), nil)
	if second := MaybeAutoPruneCheckpoints(nil, o); !second.Skipped {
		t.Fatalf("second run not skipped: %+v", second)
	}
	if _, err := os.Stat(also); err != nil {
		t.Fatal("skipped run deleted an orphan")
	}
}

func TestCheckpointStoreStatusReportsProjectsAndLegacy(t *testing.T) {
	f := newCPFixture(t)
	f.mgr(true, 20, 500, 10).EnsureCheckpoint(f.work, "initial")
	f.write(filepath.Join(f.base, "legacy-20200101-000000", "junk"), strings.Repeat("x", 100))
	info, err := CheckpointStoreStatus(f.env, "")
	if err != nil {
		t.Fatal(err)
	}
	if info.ProjectCount != 1 || info.Projects[0].Workdir != checkpointResolve(f.work) || info.Projects[0].Commits < 1 || !info.Projects[0].Exists {
		t.Fatalf("projects wrong: %+v", info.Projects)
	}
	if len(info.LegacyArchives) != 1 || info.LegacyArchives[0].SizeBytes < 100 {
		t.Fatalf("legacy wrong: %+v", info.LegacyArchives)
	}
}

func TestCheckpointClearAllWipesBaseThenIsANoop(t *testing.T) {
	f := newCPFixture(t)
	f.mgr(true, 20, 500, 10).EnsureCheckpoint(f.work, "initial")
	if !f.exists(f.base) {
		t.Fatal("base missing")
	}
	r := ClearAllCheckpoints(f.env, "")
	if !r.Deleted || r.BytesFreed <= 0 || f.exists(f.base) {
		t.Fatalf("clear wrong: %+v", r)
	}
	if r := ClearAllCheckpoints(f.env, ""); r.Deleted || r.BytesFreed != 0 {
		t.Fatalf("second clear not a noop: %+v", r)
	}
}

func TestCheckpointClearLegacyOnlyRemovesLegacyDirs(t *testing.T) {
	f := newCPFixture(t)
	f.mgr(true, 20, 500, 10).EnsureCheckpoint(f.work, "initial")
	legacy := filepath.Join(f.base, "legacy-20200101-000000")
	f.write(filepath.Join(legacy, "junk"), strings.Repeat("x", 1000))
	r := ClearLegacyCheckpoints(f.env, "")
	if r.Deleted != 1 || r.BytesFreed < 1000 || f.exists(legacy) || !f.exists(filepath.Join(f.base, "store", "HEAD")) {
		t.Fatalf("clear legacy wrong: %+v", r)
	}
}

func TestCheckpointClearLegacyCountsAnUndeletableArchive(t *testing.T) {
	f := newCPFixture(t)
	stuck := filepath.Join(f.base, "legacy-20200101-000000")
	other := filepath.Join(f.base, "legacy-20200102-000000")
	f.write(filepath.Join(stuck, "blob"), strings.Repeat("x", 1024))
	f.write(filepath.Join(other, "blob"), strings.Repeat("x", 1024))
	cpSwap(t, &rmtreeRemove, func(p string) error {
		if p == stuck {
			return &os.PathError{Op: "rmdir", Path: p, Err: syscall.EIO}
		}
		return os.Remove(p)
	})
	r := ClearLegacyCheckpoints(f.env, "")
	if r.Deleted != 1 || r.Errors != 1 || !f.exists(stuck) || f.exists(other) {
		t.Fatalf("failure not reported: %+v", r)
	}
}

// Project with one checkpoint under the fixture base for the orphan evidence tests
func cpProjectWithHistory(t *testing.T, f *cpFixture, work string) {
	t.Helper()
	f.write(filepath.Join(work, "main.py"), "print('x')\n")
	if !f.mgr(true, 10, 500, 10).EnsureCheckpoint(work, "initial") {
		t.Fatal("history not taken")
	}
}

// Reports whether the project's metadata survived
func cpHistorySurvives(f *cpFixture, work string) bool {
	return f.exists(checkpointProjectMetaPath(f.store(), checkpointProjectHash(work)))
}

func TestCheckpointAbsentWorkdirWithoutDeletionEvidenceKeepsHistory(t *testing.T) {
	f := newCPFixture(t)
	vanishedMount := filepath.Join(f.tmp, "mnt-a")
	vanished := filepath.Join(vanishedMount, "work", "proj")
	cpProjectWithHistory(t, f, vanished)
	mountPoint := filepath.Join(f.tmp, "mnt-b", "volume")
	unmounted := filepath.Join(mountPoint, "proj")
	cpProjectWithHistory(t, f, unmounted)
	vanishedHash, unmountedHash := checkpointProjectHash(vanished), checkpointProjectHash(unmounted)
	os.RemoveAll(vanishedMount)
	os.RemoveAll(unmounted)
	if ents, _ := os.ReadDir(mountPoint); len(ents) != 0 {
		t.Fatal("mount point not empty")
	}
	PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: true, Base: f.base})
	for _, h := range []string{vanishedHash, unmountedHash} {
		if !f.exists(checkpointProjectMetaPath(f.store(), h)) {
			t.Fatal("history deleted for an unmounted project")
		}
	}
}

func TestCheckpointPopulatedUnderlayMountpointKeepsItsCheckpoints(t *testing.T) {
	f := newCPFixture(t)
	mountPoint := filepath.Join(f.tmp, "mnt", "volume")
	work := filepath.Join(mountPoint, "project")
	cpProjectWithHistory(t, f, work)
	hash := checkpointProjectHash(work)
	if err := os.Rename(mountPoint, filepath.Join(f.tmp, "detached-volume")); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(mountPoint, ".keep"), "")
	r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: true, Base: f.base})
	if r.DeletedOrphan != 0 || !f.exists(checkpointProjectMetaPath(f.store(), hash)) {
		t.Fatalf("underlay entries mistaken for attachment: %+v", r)
	}
}

func TestCheckpointRecordedParentIdentitySurvivesProbeFailureOnTouch(t *testing.T) {
	f := newCPFixture(t)
	work := filepath.Join(f.tmp, "projects", "proj")
	cpProjectWithHistory(t, f, work)
	metaPath := checkpointProjectMetaPath(f.store(), checkpointProjectHash(work))
	before := cpReadJSON(t, metaPath)
	if before["workdir_parent_dev"] == nil || before["workdir_parent_ino"] == nil {
		t.Fatalf("identity not recorded: %v", before)
	}
	cpSwap(t, &checkpointParentIdentity, func(string) (uint64, uint64, error) { return 0, 0, errors.New("transient I/O error") })
	registerCheckpointProject(f.env, f.store(), work)
	after := cpReadJSON(t, metaPath)
	if after["workdir_parent_dev"] != before["workdir_parent_dev"] || after["workdir_parent_ino"] != before["workdir_parent_ino"] {
		t.Fatalf("identity lost: %v -> %v", before, after)
	}
}

func TestCheckpointSessionDiffEmptyWhenNothingChanged(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	r := m.SessionDiff(f.work)
	if !cpSuccess(r) || r["empty"] != true || r["diff"] != "" {
		t.Fatalf("no checkpoint not empty: %v", r)
	}
	m.EnsureCheckpoint(f.work, "baseline")
	r = m.SessionDiff(f.work)
	if !cpSuccess(r) || r["empty"] != true || r["diff"] != "" {
		t.Fatalf("unchanged tree not empty: %v", r)
	}
}

func TestCheckpointSessionDiffIncludesNewlyAddedFiles(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	m.EnsureCheckpoint(f.work, "baseline")
	f.write(filepath.Join(f.work, "feature.py"), "x = 1\n")
	r := m.SessionDiff(f.work)
	diff, _ := r["diff"].(string)
	if !cpSuccess(r) || !strings.Contains(diff, "feature.py") || !strings.Contains(diff, "+x = 1") {
		t.Fatalf("new file missing from session diff: %v", r)
	}
}
