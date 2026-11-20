package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func wcNs(repo, action string, kw map[string]any) map[string]any {
	ns := map[string]any{"repo": repo, "worktree_action": action, "json": true, "older_than": nil, "dry_run": false, "trees_only": false, "branches_only": false}
	for k, v := range kw {
		ns[k] = v
	}
	return ns
}

func wcJSON(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	return payload
}

func wcRun(t *testing.T, ns map[string]any) (int, string) {
	t.Helper()
	var out bytes.Buffer
	rc, err := cmdWorktree(context.Background(), ns, &out)
	if err != nil {
		t.Fatalf("cmd_worktree: %v\n%s", err, out.String())
	}
	return rc, out.String()
}

func TestCmdWorktreeJsonListJsonShape(t *testing.T) {
	repo := gcTestRepo(t)
	gcAddWorktree(t, repo, "icebath-json", "")
	rc, out := wcRun(t, wcNs(repo, "list", nil))
	if rc != 0 {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	payload := wcJSON(t, []byte(out))
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"archives", "branches", "external_trees", "repo", "trees"}; strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys %v", keys)
	}
	var tree map[string]any
	for _, item := range payload["trees"].([]any) {
		m := item.(map[string]any)
		if m["name"] == "icebath-json" {
			tree = m
		}
	}
	if tree == nil {
		t.Fatalf("icebath-json missing from %v", payload["trees"])
	}
	for _, k := range []string{"verdict", "reason", "age_days", "branch"} {
		if _, ok := tree[k]; !ok {
			t.Fatalf("tree lacks %s: %v", k, tree)
		}
	}
}

func TestCmdWorktreeJsonPruneDryRunJson(t *testing.T) {
	repo := gcTestRepo(t)
	gcAddWorktree(t, repo, "icebath-json-prune", "")
	rc, out := wcRun(t, wcNs(repo, "prune", map[string]any{"dry_run": true}))
	if rc != 0 {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	payload := wcJSON(t, []byte(out))
	if payload["dry_run"] != true {
		t.Fatalf("dry_run %v", payload["dry_run"])
	}
	found := false
	for _, a := range payload["actions"].([]any) {
		if strings.Contains(a.(string), "icebath-json-prune") {
			found = true
		}
	}
	if !found {
		t.Fatalf("actions %v", payload["actions"])
	}
	if !gcExists(filepath.Join(repo, ".worktrees", "icebath-json-prune")) {
		t.Fatal("dry run removed the tree")
	}
}

func TestCmdWorktreeJsonNegativeOlderThanRejected(t *testing.T) {
	repo := gcTestRepo(t)
	rc, out := wcRun(t, wcNs(repo, "prune", map[string]any{"older_than": -1.0}))
	if rc != 1 {
		t.Fatalf("rc %d\n%s", rc, out)
	}
}

func TestWorktreeAliasesNormalizeToList(t *testing.T) {
	prev := worktreeCmdFn
	t.Cleanup(func() { worktreeCmdFn = prev })
	seen := map[string]any{}
	worktreeCmdFn = func(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
		if _, ok := seen["action"]; !ok {
			seen["action"] = ns["worktree_action"]
		}
		return 0, nil
	}
	ns := map[string]any{"worktree_action": "audit", "command": "worktree"}
	if _, err := WorktreeCommand(context.Background(), nil, ns, io.Discard); err != nil {
		t.Fatal(err)
	}
	if seen["action"] != "list" {
		t.Fatalf("action %v", seen["action"])
	}
}

func TestPruneOlderThanBoundsWhatTheSweepRemoves(t *testing.T) {
	repo := waRepo(t)
	dirty := waTree(t, repo, "icebath-dirty", true, false)
	clean := waTree(t, repo, "icebath-clean", false, false)
	waIdle(t, dirty, 100)
	waIdle(t, clean, 100)
	rc, out := wcRun(t, wcNs(repo, "prune", map[string]any{"older_than": 30.0, "trees_only": true}))
	if rc != 0 {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	if !gcExists(dirty) || !gcExists(clean) {
		t.Fatal("a tree idle for less than --older-than days was removed")
	}
	if archives := waArchives(t, repo); len(archives) != 0 {
		t.Fatalf("archives %v", archives)
	}
}

func TestATreeResumedMidSweepIsNeverRemoved(t *testing.T) {
	repo := waRepo(t)
	first := waTree(t, repo, "lane-a", true, false)
	second := waTree(t, repo, "lane-b", true, false)
	staged := waTree(t, repo, "lane-staged", true, false)
	for _, tree := range []string{first, second, staged} {
		waIdle(t, tree, 100)
	}
	patch := "--- a/b.py\n+++ b/b.py\n@@ -1 +1 @@\n-x = 1\n+NEW STAGED CONTENT\n"
	realSnapshot := snapshotWorktreeFn
	t.Cleanup(func() { snapshotWorktreeFn = realSnapshot })
	var sleepers []*exec.Cmd
	t.Cleanup(func() {
		for _, proc := range sleepers {
			_ = proc.Process.Kill()
			_ = proc.Wait()
		}
	})
	snapshotWorktreeFn = func(repoRoot, wtPath, reason string, maxMB float64) (*Archive, error) {
		arc, err := realSnapshot(repoRoot, wtPath, reason, maxMB)
		if filepath.Base(wtPath) == "lane-staged" {
			apply := exec.Command("git", "apply", "--cached", "-")
			apply.Dir = wtPath
			apply.Stdin = strings.NewReader(patch)
			if out, runErr := apply.CombinedOutput(); runErr != nil {
				t.Errorf("git apply --cached: %v\n%s", runErr, out)
			}
			status := exec.Command("git", "status", "--porcelain")
			status.Dir = wtPath
			if _, runErr := status.Output(); runErr != nil {
				t.Errorf("git status: %v", runErr)
			}
			return arc, err
		}
		if writeErr := os.WriteFile(filepath.Join(wtPath, "notes.md"), []byte("edited during the snapshot\n"), 0o644); writeErr != nil {
			t.Errorf("write notes.md: %v", writeErr)
		}
		sleeper := exec.Command("sleep", "60")
		sleeper.Dir = second
		if startErr := sleeper.Start(); startErr != nil {
			t.Errorf("sleeper: %v", startErr)
		} else {
			sleepers = append(sleepers, sleeper)
		}
		return arc, err
	}
	plan, actions, err := worktreeSweep(context.Background(), repo, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	removals := []string{}
	for _, d := range plan {
		if d.Action == "archive-remove" {
			removals = append(removals, d.Name)
		}
	}
	sort.Strings(removals)
	if strings.Join(removals, ",") != "lane-a,lane-b,lane-staged" {
		t.Fatalf("archive-remove plan %v", removals)
	}
	if body, _ := os.ReadFile(filepath.Join(first, "notes.md")); string(body) != "edited during the snapshot\n" {
		t.Fatalf("lane-a notes.md %q", body)
	}
	if body, _ := os.ReadFile(filepath.Join(second, "notes.md")); !gcExists(second) || string(body) != "uncommitted\n" {
		t.Fatalf("lane-b exists %v notes.md %q", gcExists(second), body)
	}
	if !gcExists(staged) || waGit(t, staged, "show", ":b.py") != "NEW STAGED CONTENT" {
		t.Fatalf("lane-staged exists %v index %q", gcExists(staged), waGit(t, staged, "show", ":b.py"))
	}
	names := []string{}
	for _, ref := range waArchives(t, repo) {
		names = append(names, ref[strings.LastIndex(ref, "/")+1:])
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "lane-a,lane-staged" {
		t.Fatalf("archives %v", names)
	}
	sort.Strings(actions)
	want := []string{
		"archived, kept (changed 0s ago) → " + ArchivePrefix + "lane-a",
		"archived, kept (staged changes since the snapshot) → " + ArchivePrefix + "lane-staged",
	}
	if strings.Join(actions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("actions %q", actions)
	}
}
