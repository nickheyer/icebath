//go:build linux

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func prMkDiverged(t *testing.T, repo, name string, ageH float64) string {
	t.Helper()
	p := filepath.Join(repo, ".worktrees", name)
	_ = os.MkdirAll(filepath.Join(repo, ".worktrees"), 0o755)
	twGit(t, repo, "worktree", "add", p, "-b", "icebath/"+name, "HEAD")
	twWrite(t, filepath.Join(p, "salvaged.txt"), "diff that was reworked during salvage\n")
	twGit(t, p, "add", "salvaged.txt")
	twGit(t, p, "commit", "-m", "salvaged work")
	twAge(t, p, ageH)
	return p
}

func prFakeGh(t *testing.T, tmp, stdout string, exitCode int) {
	t.Helper()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' '%s'\nexit %d\n", stdout, exitCode)
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	if !strings.HasPrefix(path, bin+":") {
		t.Setenv("PATH", bin+":"+path)
	}
}

func TestPrMergedEscapeHatchMergedPrTreeIsReaped(t *testing.T) {
	repo := twRepo(t)
	wt := prMkDiverged(t, repo, "icebath-rebase-merged", 100)
	if worktreeCommitsAllMergedUpstream(wt, 30*time.Second, 20, nil) {
		t.Fatal("precondition: cherry must NOT consider this merged — the PR check is the only thing that can reap it")
	}
	prFakeGh(t, filepath.Dir(repo), `[{"number": 1}]`, 0)
	twPrune(t, repo)
	if gcExists(wt) {
		t.Fatal("clean tree whose branch has a MERGED PR is merged work — reap it")
	}
}

func TestPrMergedEscapeHatchNoMergedPrPreserved(t *testing.T) {
	repo := twRepo(t)
	wt := prMkDiverged(t, repo, "icebath-pr-open", 100)
	prFakeGh(t, filepath.Dir(repo), "[]", 0)
	twPrune(t, repo)
	if !gcExists(wt) {
		t.Fatal("no merged PR -> still unpushed work, preserve")
	}
}

func TestPrMergedEscapeHatchGhFailureFailsSafe(t *testing.T) {
	repo := twRepo(t)
	wt := prMkDiverged(t, repo, "icebath-gh-down", 100)
	prFakeGh(t, filepath.Dir(repo), "", 1)
	twPrune(t, repo)
	if !gcExists(wt) {
		t.Fatal("gh failure must preserve the tree (fail safe)")
	}
}

func TestPrMergedEscapeHatchDirtyTreeNeverReapedEvenWithMergedPr(t *testing.T) {
	repo := twRepo(t)
	wt := prMkDiverged(t, repo, "icebath-dirty-merged", 100)
	twWrite(t, filepath.Join(wt, "uncommitted.txt"), "in-flight\n")
	twAge(t, wt, 100)
	prFakeGh(t, filepath.Dir(repo), `[{"number": 1}]`, 0)
	twPrune(t, repo)
	if !gcExists(wt) {
		t.Fatal("dirty guard outranks the PR-merged verdict")
	}
}

func prMergedKeys(cache *mergeVerdicts) []string {
	var keys []string
	for _, k := range cache.keys {
		if strings.HasPrefix(k, "pr-merged:") {
			keys = append(keys, k)
		}
	}
	return keys
}

func TestPrMergedEscapeHatchMergedVerdictMemoizedByBranchAndHead(t *testing.T) {
	repo := twRepo(t)
	wt := prMkDiverged(t, repo, "icebath-memo", 100)
	prFakeGh(t, filepath.Dir(repo), `[{"number": 1}]`, 0)
	cache := newMergeVerdicts()
	if !worktreeBranchPRMerged(wt, 15*time.Second, cache) {
		t.Fatal("merged PR not reported")
	}
	keys := prMergedKeys(cache)
	if len(keys) != 1 || !cache.vals[keys[0]] {
		t.Fatalf("pr-merged keys %v", keys)
	}
	prFakeGh(t, filepath.Dir(repo), "", 1)
	if !worktreeBranchPRMerged(wt, 15*time.Second, cache) {
		t.Fatal("a cached True verdict must not re-consult gh")
	}
}

func TestPrMergedEscapeHatchNegativeVerdictNotCached(t *testing.T) {
	repo := twRepo(t)
	wt := prMkDiverged(t, repo, "icebath-nocache-neg", 100)
	prFakeGh(t, filepath.Dir(repo), "[]", 0)
	cache := newMergeVerdicts()
	if worktreeBranchPRMerged(wt, 15*time.Second, cache) {
		t.Fatal("no merged PR reported as merged")
	}
	if keys := prMergedKeys(cache); len(keys) != 0 {
		t.Fatalf("False must not be memoized — the PR can merge later with the same (branch, head) key: %v", keys)
	}
}
