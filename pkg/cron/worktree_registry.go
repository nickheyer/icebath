package cron

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const worktreeRegistryName = "worktree_repos.json"

// Registry file of repos holding icebath made worktrees under the active profile home
func worktreeRegistryPath(h string) string { return filepath.Join(h, worktreeRegistryName) }

func worktreeRepoExists(repo string) bool {
	_, err := os.Stat(filepath.Join(repo, ".git"))
	return err == nil
}

// Non empty string entries of the registry repos list, empty when the file is missing or malformed
func readWorktreeRegistry(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil
	}
	repos, _ := obj.Get("repos")
	list, ok := repos.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, r := range list {
		if s, ok := r.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Registered repo roots that still exist, read only and never creating the file
func worktreeRegisteredRepos(h string) []string {
	var out []string
	for _, r := range readWorktreeRegistry(worktreeRegistryPath(h)) {
		if worktreeRepoExists(r) {
			out = append(out, r)
		}
	}
	return out
}

// Records a repo with icebath made worktrees, dropping vanished repos and logging any failure at debug
func RecordWorktreeRepo(h, repoRoot string) {
	if repoRoot == "" {
		return
	}
	if err := recordWorktreeRepo(h, repoRoot); err != nil {
		slog.Default().Debug("worktree registry update failed for "+repoRoot, "error", err)
	}
}

func recordWorktreeRepo(h, repoRoot string) error {
	repo, err := pypath.Realpath(repoRoot, pypath.Lax)
	if err != nil {
		return err
	}
	path := worktreeRegistryPath(h)
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	ok, err := acquireFlock(lock, 5*time.Second)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("Timed out waiting for the worktree registry lock")
	}
	current := readWorktreeRegistry(path)
	seen := map[string]bool{}
	kept := []string{}
	for _, r := range current {
		if !seen[r] && worktreeRepoExists(r) {
			kept = append(kept, r)
		}
		seen[r] = true
	}
	found := false
	for _, r := range kept {
		found = found || r == repo
	}
	if found && slices.Equal(kept, current) {
		return nil
	}
	if !found {
		kept = append(kept, repo)
	}
	repos := make([]any, len(kept))
	for i, r := range kept {
		repos[i] = r
	}
	doc := pyjson.NewObject()
	doc.Set("version", 1)
	doc.Set("repos", repos)
	return config.AtomicJSONWrite(path, doc)
}
