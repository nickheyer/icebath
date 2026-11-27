package worktrees

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Trees as profiles are renamed: the record a profile keeps of its trees moves with its folder, so the trees of repositories inside the
// folder are recorded, and linked by git, at where they lie now
type Profiles struct {
	Worktrees *Worktrees
}

// The record lies in the profile's folder, which goes with it
func (Profiles) Delete(string) error {
	return nil
}

// Path moved from the old folder into the new one, false for one outside the old folder
func moved(path, oldDir, newDir string) (string, bool) {
	rel, err := filepath.Rel(oldDir, path)
	if path == "" || err != nil || !filepath.IsLocal(rel) && rel != "." {
		return path, false
	}
	return filepath.Join(newDir, rel), true
}

// Records the trees that lay in the old folder at their places in the new one, and has git link each repository and its trees again there
func (p Profiles) Move(old, renamed string) ([]string, error) {
	cfg := p.Worktrees.config.In(renamed)
	oldDir, newDir := p.Worktrees.config.ProfileDir(old), p.Worktrees.config.ProfileDir(renamed)
	path := recordPath(cfg)
	release, err := fsx.Lock(context.Background(), path+".lock", 5*time.Second)
	if err != nil {
		return nil, &profile.StoreError{Store: path, Err: err}
	}
	defer release()
	trees := readRecord(cfg)
	repos := map[string][]string{}
	count := 0
	for i, tree := range trees {
		treePath, inPath := moved(tree.Path, oldDir, newDir)
		repo, inRepo := moved(tree.Repo, oldDir, newDir)
		if !inPath && !inRepo {
			continue
		}
		trees[i].Path, trees[i].Repo = treePath, repo
		repos[repo] = append(repos[repo], treePath)
		count++
	}
	if count == 0 {
		return nil, nil
	}
	data, err := json.MarshalIndent(struct {
		Version int    `json:"version"`
		Trees   []Tree `json:"trees"`
	}{1, trees}, "", "  ")
	if err == nil {
		err = fsx.Write(path, data, 0o600)
	}
	if err != nil {
		return nil, &profile.StoreError{Store: path, Err: err}
	}
	var errs []error
	for repo, paths := range repos {
		if !fsx.Exists(filepath.Join(repo, ".git")) {
			continue
		}
		result, err := run(repo, nil, time.Minute, append([]string{"worktree", "repair"}, paths...)...)
		if err == nil && result.Code != 0 {
			err = errors.New(strings.TrimSpace(result.Stderr))
		}
		if err != nil {
			errs = append(errs, &profile.StoreError{Store: repo, Err: fmt.Errorf("git worktree repair: %w", err)})
		}
	}
	return []string{fmt.Sprintf("✓ Worktree record updated: %d tree(s)", count)}, errors.Join(errs...)
}
