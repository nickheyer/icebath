package manage

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestRenamePreservesProfileLocalCheckpointHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	tmp := manageEnv(t)
	captureOut(t)
	oldLive := liveDefaultMultiplexer
	liveDefaultMultiplexer = func() bool { return false }
	t.Cleanup(func() { liveDefaultMultiplexer = oldLive })
	ctx := context.Background()
	oldDir, err := CreateProfile(ctx, "oldname", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(oldDir, "project")
	outside := filepath.Join(tmp, "outside-project")
	writeFile(t, filepath.Join(outside, "keep.txt"), "v1\n")
	writeFile(t, filepath.Join(workdir, "pyproject.toml"), "[project]\nname = 'rename-checkpoint'\n")
	tracked := filepath.Join(workdir, "note.txt")
	writeFile(t, tracked, "before\n")
	m := tools.NewCheckpointManager(&tools.Env{Home: oldDir}, tools.CheckpointManagerOptions{Enabled: true, MaxSnapshots: 5})
	if !m.EnsureCheckpoint(workdir, "before profile rename") {
		t.Fatal("profile project checkpoint not taken")
	}
	checkpointHash := m.ListCheckpoints(workdir)[0].Hash
	if !m.EnsureCheckpoint(outside, "outside profile") {
		t.Fatal("outside project checkpoint not taken")
	}
	outsideHash := m.ListCheckpoints(outside)[0].Hash
	writeFile(t, tracked, "after\n")
	m.RecordAgentWrite(tracked)

	newDir, err := RenameProfile(ctx, "oldname", "newname")
	if err != nil {
		t.Fatal(err)
	}
	newWorkdir := filepath.Join(newDir, "project")
	m = tools.NewCheckpointManager(&tools.Env{Home: newDir}, tools.CheckpointManagerOptions{Enabled: true, MaxSnapshots: 5})
	if got := m.ListCheckpoints(newWorkdir); len(got) != 1 || got[0].Hash != checkpointHash {
		t.Fatalf("moved project checkpoints %+v", got)
	}
	if got := m.ListCheckpoints(outside); len(got) != 1 || got[0].Hash != outsideHash {
		t.Fatalf("outside checkpoints %+v", got)
	}
	all, err := m.ListAllCheckpoints()
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, e := range all {
		paths[e.Workdir] = true
	}
	resolvedNew, _ := filepath.EvalSymlinks(newWorkdir)
	resolvedOld := filepath.Join(filepath.Dir(filepath.Dir(resolvedNew)), "oldname", "project")
	if !paths[resolvedNew] || paths[resolvedOld] {
		t.Fatalf("project paths %v", paths)
	}
	restored := m.Restore(newWorkdir, checkpointHash, "", true)
	if restored["success"] != true {
		t.Fatalf("restore %v", restored)
	}
	if readText(t, filepath.Join(newWorkdir, "note.txt")) != "before\n" {
		t.Fatal("tracked file not restored")
	}
}

func TestRetryAfterPartialRekeyKeepsCheckpointsTakenUnderNewName(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	manageEnv(t)
	captureOut(t)
	oldLive := liveDefaultMultiplexer
	liveDefaultMultiplexer = func() bool { return false }
	t.Cleanup(func() { liveDefaultMultiplexer = oldLive })
	skipAliasCollision(t)
	ctx := context.Background()
	oldDir, err := CreateProfile(ctx, "oldname", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(oldDir, "project")
	writeFile(t, filepath.Join(workdir, "pyproject.toml"), "[project]\nname = 'retry'\n")
	writeFile(t, filepath.Join(workdir, "note.txt"), "v1\n")
	opts := tools.CheckpointManagerOptions{Enabled: true, MaxSnapshots: 5}
	if !tools.NewCheckpointManager(&tools.Env{Home: oldDir}, opts).EnsureCheckpoint(workdir, "c1") {
		t.Fatal("c1 not taken")
	}

	restore := tools.SetCheckpointDeleteRef(func(*tools.Env, string, string) bool { return false })
	newDir, err := RenameProfile(ctx, "oldname", "newname")
	restore()
	if err != nil {
		t.Fatal(err)
	}

	newWorkdir := filepath.Join(newDir, "project")
	newNote := filepath.Join(newWorkdir, "note.txt")
	newEnv := &tools.Env{Home: newDir}
	writeFile(t, newNote, "v2\n")
	m := tools.NewCheckpointManager(newEnv, opts)
	if !m.EnsureCheckpoint(newWorkdir, "c2") {
		t.Fatal("c2 not taken")
	}
	m.RecordAgentWrite(newNote)
	hashes := func(m *tools.CheckpointManager) []string {
		var out []string
		for _, e := range m.ListCheckpoints(newWorkdir) {
			out = append(out, e.Hash)
		}
		return out
	}
	before := hashes(m)
	if len(before) != 2 {
		t.Fatalf("history before retry %v", before)
	}
	store := tools.CheckpointStorePath(filepath.Join(newDir, "checkpoints"))
	newHash := tools.CheckpointProjectHash(newWorkdir)
	ledgerBefore := tools.LoadCheckpointLedger(store, newHash)

	ok, err := MigrateProfileIdentity(ctx, "oldname", "newname")
	if err != nil || !ok {
		t.Fatalf("retry %v %v", ok, err)
	}
	if after := hashes(tools.NewCheckpointManager(newEnv, opts)); !reflect.DeepEqual(after, before) {
		t.Fatalf("retry rewound history %v -> %v", before, after)
	}
	if !reflect.DeepEqual(tools.LoadCheckpointLedger(store, newHash), ledgerBefore) {
		t.Fatal("retry clobbered the newer ledger")
	}
	if refs := tools.ListCheckpointProjectRefs(nil, store, newWorkdir); !reflect.DeepEqual(refs, []string{tools.CheckpointRefName(newHash)}) {
		t.Fatalf("old identity survived the retry %v", refs)
	}
}
