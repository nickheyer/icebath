package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirtySubmoduleWorkIsNeverArchivedAway(t *testing.T) {
	repo := waRepo(t)
	tmp := filepath.Dir(repo)
	subOrigin, subWork := filepath.Join(tmp, "sub.git"), filepath.Join(tmp, "subwork")
	waGit(t, tmp, "init", "-q", "--bare", "-b", "main", subOrigin)
	waGit(t, tmp, "init", "-q", "-b", "main", subWork)
	waWrite(t, filepath.Join(subWork, "file.txt"), "base\n")
	waGit(t, subWork, "add", "-A")
	waGit(t, subWork, "commit", "-qm", "sub")
	waGit(t, subWork, "push", "-q", subOrigin, "main")
	waGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subOrigin, "sub")
	waGit(t, repo, "commit", "-qm", "add submodule")
	trees := map[string]string{}
	for _, name := range []string{"lane-subdirty", "lane-deinit", "lane-late"} {
		trees[name] = waTree(t, repo, name, true, false)
		waGit(t, trees[name], "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	}
	dirtyFile := filepath.Join(trees["lane-subdirty"], "sub", "file.txt")
	if err := os.WriteFile(dirtyFile, []byte("base\nUNCOMMITTED SUBMODULE EDIT\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deinitSub := filepath.Join(trees["lane-deinit"], "sub")
	moduleRepo := waGit(t, deinitSub, "rev-parse", "--absolute-git-dir")
	waGit(t, deinitSub, "switch", "-qc", "private-work")
	waWrite(t, filepath.Join(deinitSub, "file.txt"), "UNPUSHED\n")
	waGit(t, deinitSub, "commit", "-qam", "private")
	private := waGit(t, deinitSub, "rev-parse", "HEAD")
	waGit(t, deinitSub, "checkout", "-q", "--detach", "origin/main")
	waGit(t, trees["lane-deinit"], "submodule", "deinit", "-q", "sub")
	for _, tree := range trees {
		waIdle(t, tree, 100)
	}
	lateFile := filepath.Join(trees["lane-late"], "sub", "file.txt")
	lateBytes := [][]byte{[]byte("base\n")}
	realSnapshot := snapshotWorktreeFn
	t.Cleanup(func() { snapshotWorktreeFn = realSnapshot })
	snapshotWorktreeFn = func(repoRoot, wtPath, reason string, maxMB float64) (*Archive, error) {
		arc, err := realSnapshot(repoRoot, wtPath, reason, maxMB)
		if filepath.Base(wtPath) == "lane-late" {
			lateBytes = append(lateBytes, []byte("LATE SUBMODULE EDIT\n"))
			if writeErr := os.WriteFile(lateFile, lateBytes[len(lateBytes)-1], 0o644); writeErr != nil {
				t.Errorf("late edit: %v", writeErr)
			}
		}
		return arc, err
	}
	settings := SweepSettings()
	actions := map[string]bool{}
	for _, d := range PlanSweep(repo, 24, &settings, 0, nil) {
		actions[d.Action] = true
	}
	if len(actions) != 1 || !actions["keep-submodule-work"] {
		t.Fatalf("plan actions %v", actions)
	}
	if err := PruneStaleWorktrees(context.Background(), repo, 24); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(dirtyFile); string(body) != "base\nUNCOMMITTED SUBMODULE EDIT\r\n" {
		t.Fatalf("dirty submodule file %q", body)
	}
	if got := waGit(t, repo, "--git-dir", moduleRepo, "rev-parse", "private-work"); got != private {
		t.Fatalf("private commit %q want %q", got, private)
	}
	if body, _ := os.ReadFile(lateFile); string(body) != string(lateBytes[len(lateBytes)-1]) {
		t.Fatalf("late file %q", body)
	}
	if archives := waArchives(t, repo); len(archives) != 0 {
		t.Fatalf("archives %v", archives)
	}
	for _, tree := range trees {
		_, err := SnapshotWorktree(repo, tree, "session exit", 50)
		var archiveErr *ArchiveError
		if !errors.As(err, &archiveErr) || !strings.Contains(err.Error(), "submodule") {
			t.Fatalf("%s: snapshot error %v", filepath.Base(tree), err)
		}
	}
}
