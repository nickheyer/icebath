package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Profile home under a fake profiles root with its own checkpoint base resolved from the env home
func cpProfile(t *testing.T, root, name string) (string, *Env) {
	t.Helper()
	dir := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, &Env{Home: dir}
}

// Renames a profile directory the way the profile slice moves it
func cpRenameProfile(t *testing.T, oldDir, newDir string) {
	t.Helper()
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatal(err)
	}
}

// Reports whether a workdir appears in the cross project listing
func cpListedWorkdirs(t *testing.T, m *CheckpointManager) map[string]bool {
	t.Helper()
	all, err := m.ListAllCheckpoints()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range all {
		out[e.Workdir] = true
	}
	return out
}

func TestCheckpointRenamePreservesProfileLocalCheckpointHistory(t *testing.T) {
	root := cpTempDir(t)
	oldDir, oldEnv := cpProfile(t, root, "oldname")
	workdir := filepath.Join(oldDir, "project")
	outside := filepath.Join(root, "outside-project")
	for _, d := range []string{workdir, outside} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("v1\n"), 0o644)
	os.WriteFile(filepath.Join(workdir, "pyproject.toml"), []byte("[project]\nname = 'rename-checkpoint'\n"), 0o644)
	tracked := filepath.Join(workdir, "note.txt")
	os.WriteFile(tracked, []byte("before\n"), 0o644)

	m := NewCheckpointManager(oldEnv, CheckpointManagerOptions{Enabled: true, MaxSnapshots: 5})
	if !m.EnsureCheckpoint(workdir, "before profile rename") {
		t.Fatal("profile project not taken")
	}
	checkpointHash := m.ListCheckpoints(workdir)[0].Hash
	if !m.EnsureCheckpoint(outside, "outside profile") {
		t.Fatal("outside project not taken")
	}
	outsideHash := m.ListCheckpoints(outside)[0].Hash
	os.WriteFile(tracked, []byte("after\n"), 0o644)
	m.RecordAgentWrite(tracked)

	newDir := filepath.Join(root, "profiles", "newname")
	cpRenameProfile(t, oldDir, newDir)
	result := MigrateProfileCheckpointProjects(nil, oldDir, newDir)
	if result.Errors != 0 || result.Migrated != 1 || result.Scanned != 2 {
		t.Fatalf("migration counts wrong: %+v", result)
	}

	newWorkdir := filepath.Join(newDir, "project")
	newTracked := filepath.Join(newWorkdir, "note.txt")
	m = NewCheckpointManager(&Env{Home: newDir}, CheckpointManagerOptions{Enabled: true, MaxSnapshots: 5})
	if cps := m.ListCheckpoints(newWorkdir); len(cps) != 1 || cps[0].Hash != checkpointHash {
		t.Fatalf("moved project history lost: %v", cps)
	}
	if cps := m.ListCheckpoints(outside); len(cps) != 1 || cps[0].Hash != outsideHash {
		t.Fatalf("outside project history lost: %v", cps)
	}
	listed := cpListedWorkdirs(t, m)
	if !listed[checkpointResolve(newWorkdir)] || listed[workdir] {
		t.Fatalf("listing still carries the old identity: %v", listed)
	}
	plan, err := m.safeRestorePlan(newWorkdir, checkpointHash)
	if err != nil || plan.Error != "" || !reflect.DeepEqual(plan.Restore, []string{"note.txt"}) || len(plan.Skipped) != 0 {
		t.Fatalf("ledger not rebased: %+v %v", plan, err)
	}
	if r := m.Restore(newWorkdir, checkpointHash, "", true); !cpSuccess(r) {
		t.Fatalf("restore failed: %v", r)
	}
	if b, _ := os.ReadFile(newTracked); string(b) != "before\n" {
		t.Fatalf("restore wrong: %q", b)
	}
}

func TestCheckpointRetryAfterPartialRekeyKeepsCheckpointsTakenUnderNewName(t *testing.T) {
	root := cpTempDir(t)
	oldDir, oldEnv := cpProfile(t, root, "oldname")
	workdir := filepath.Join(oldDir, "project")
	os.MkdirAll(workdir, 0o755)
	os.WriteFile(filepath.Join(workdir, "pyproject.toml"), []byte("[project]\nname = 'retry'\n"), 0o644)
	os.WriteFile(filepath.Join(workdir, "note.txt"), []byte("v1\n"), 0o644)
	if !NewCheckpointManager(oldEnv, CheckpointManagerOptions{Enabled: true, MaxSnapshots: 5}).EnsureCheckpoint(workdir, "c1") {
		t.Fatal("c1 not taken")
	}

	newDir := filepath.Join(root, "profiles", "newname")
	cpRenameProfile(t, oldDir, newDir)
	realDelete := checkpointDeleteRef
	checkpointDeleteRef = func(*Env, string, string) bool { return false }
	partial := MigrateProfileCheckpointProjects(nil, oldDir, newDir)
	checkpointDeleteRef = realDelete
	if partial.Errors != 1 {
		t.Fatalf("partial rekey not reported: %+v", partial)
	}

	newWorkdir := filepath.Join(newDir, "project")
	newNote := filepath.Join(newWorkdir, "note.txt")
	newEnv := &Env{Home: newDir}
	os.WriteFile(newNote, []byte("v2\n"), 0o644)
	m := NewCheckpointManager(newEnv, CheckpointManagerOptions{Enabled: true, MaxSnapshots: 5})
	if !m.EnsureCheckpoint(newWorkdir, "c2") {
		t.Fatal("c2 not taken")
	}
	m.RecordAgentWrite(newNote)
	var before []string
	for _, e := range m.ListCheckpoints(newWorkdir) {
		before = append(before, e.Hash)
	}
	if len(before) != 2 {
		t.Fatalf("history before retry wrong: %v", before)
	}
	store := checkpointStorePath(filepath.Join(newDir, "checkpoints"))
	newHash := checkpointProjectHash(newWorkdir)
	ledgerBefore := loadCheckpointLedger(store, newHash)

	if retry := MigrateProfileCheckpointProjects(nil, oldDir, newDir); retry.Errors != 0 {
		t.Fatalf("retry failed: %+v", retry)
	}
	m = NewCheckpointManager(newEnv, CheckpointManagerOptions{Enabled: true, MaxSnapshots: 5})
	var after []string
	for _, e := range m.ListCheckpoints(newWorkdir) {
		after = append(after, e.Hash)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("retry rewound history: %v -> %v", before, after)
	}
	if !reflect.DeepEqual(loadCheckpointLedger(store, newHash), ledgerBefore) {
		t.Fatal("retry clobbered the newer ledger")
	}
	if refs := listCheckpointProjectRefs(nil, store, newWorkdir); !reflect.DeepEqual(refs, []string{checkpointRefName(newHash)}) {
		t.Fatalf("old identity survived the retry: %v", refs)
	}
}

func TestCheckpointRenameRefusesAForeignTargetHistory(t *testing.T) {
	root := cpTempDir(t)
	oldDir, oldEnv := cpProfile(t, root, "oldname")
	workdir := filepath.Join(oldDir, "project")
	os.MkdirAll(workdir, 0o755)
	os.WriteFile(filepath.Join(workdir, "a.txt"), []byte("old\n"), 0o644)
	if !NewCheckpointManager(oldEnv, CheckpointManagerOptions{Enabled: true, MaxSnapshots: 5}).EnsureCheckpoint(workdir, "old") {
		t.Fatal("old not taken")
	}
	newDir := filepath.Join(root, "profiles", "newname")
	cpRenameProfile(t, oldDir, newDir)
	newWorkdir := filepath.Join(newDir, "project")
	store := checkpointStorePath(filepath.Join(newDir, "checkpoints"))
	oldRef := checkpointRefName(checkpointProjectHash(workdir))
	newRef := checkpointRefName(checkpointProjectHash(newWorkdir))
	tree := checkpointGitOut(nil, []string{"rev-parse", oldRef + "^{tree}"}, store, newWorkdir, nil)
	foreign := checkpointGitOut(nil, []string{"commit-tree", tree, "-m", "foreign", "--no-gpg-sign"}, store, newWorkdir, nil)
	if ok, _, errText := runCheckpointGit(nil, []string{"update-ref", newRef, foreign}, store, newWorkdir, checkpointGitOpts{}); !ok {
		t.Fatal(errText)
	}
	if r := MigrateProfileCheckpointProjects(nil, oldDir, newDir); r.Errors != 1 || r.Migrated != 0 {
		t.Fatalf("foreign target overwritten: %+v", r)
	}
	if checkpointRefTip(nil, store, newWorkdir, newRef) != foreign || checkpointRefTip(nil, store, newWorkdir, oldRef) == "" {
		t.Fatal("either side of the conflict was rewritten")
	}
}

func TestCheckpointRebaseLedgerPathsMovesOnlyKeysUnderTheOldWorkdir(t *testing.T) {
	oldW, newW := filepath.Join(string(filepath.Separator), "old", "w"), filepath.Join(string(filepath.Separator), "new", "w")
	got := rebaseLedgerPaths(map[string]any{
		filepath.Join(oldW, "a.txt"):                            1,
		filepath.Join(oldW, "d", "b"):                           2,
		filepath.Join(string(filepath.Separator), "other", "c"): 3,
		"relative/key": 4,
	}, oldW, newW)
	want := map[string]any{
		filepath.Join(newW, "a.txt"):                            1,
		filepath.Join(newW, "d", "b"):                           2,
		filepath.Join(string(filepath.Separator), "other", "c"): 3,
		"relative/key": 4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebase wrong: %v", got)
	}
}
