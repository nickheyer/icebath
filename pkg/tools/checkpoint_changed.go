package tools

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Pathspecs of the checkpoint base and its lock file inside a working directory, none when the base lies outside it or is the directory itself
func checkpointStorePathspecs(base, workingDir string) []string {
	rel, under := relativeTo(checkpointNormalizePath(base), checkpointNormalizePath(workingDir))
	if !under || rel == "." {
		return nil
	}
	lock := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+".lock")
	return []string{filepath.ToSlash(rel), filepath.ToSlash(lock)}
}

// Git arguments that keep the checkpoint base and its lock file out of a command over a working directory, none when the base lies outside it
func checkpointExcludeStoreArgs(base, workingDir string) []string {
	paths := checkpointStorePathspecs(base, workingDir)
	if len(paths) == 0 {
		return nil
	}
	args := []string{"--"}
	for _, path := range paths {
		args = append(args, ":(exclude)"+path)
	}
	return args
}

// Tree of the newest retained snapshot taken at or before a time, the oldest retained one when all are later, empty when no snapshot was taken from that time on
func (m *CheckpointManager) spanBaseTree(store, absDir string, since float64) string {
	ok, out, _ := runCheckpointGit(m.env, []string{"log", checkpointRefName(checkpointProjectHash(absDir)), "--format=%T|%at", "-n", strconv.Itoa(m.MaxSnapshots)},
		store, absDir, checkpointGitOpts{Allowed: map[int]bool{128: true, 129: true}})
	if !ok {
		return ""
	}
	type row struct {
		tree  string
		epoch float64
	}
	var rows []row
	for _, line := range strings.Split(out, "\n") {
		tree, stamp, _ := strings.Cut(line, "|")
		epoch, err := strconv.ParseUint(stamp, 10, 63)
		if tree == "" || err != nil {
			continue
		}
		rows = append(rows, row{tree, float64(epoch)})
	}
	inSpan := false
	for _, r := range rows {
		inSpan = inSpan || r.epoch >= math.Floor(since)
	}
	if !inSpan {
		return ""
	}
	for _, r := range rows {
		if r.epoch <= since {
			return r.tree
		}
	}
	return rows[len(rows)-1].tree
}

// ChangedFilesSince lists the files under a working directory that differ from the snapshot a span starting at an epoch second began from, tracked changes and deletions plus new files, read through a throwaway index so no object, ref or project index is written, none when checkpoints are off, no snapshot was taken in the span or git fails
func (m *CheckpointManager) ChangedFilesSince(workingDir string, since float64) []string {
	if m == nil || !m.Enabled || math.IsNaN(since) || math.IsInf(since, 0) || since <= 0 {
		return nil
	}
	absDir := checkpointNormalizePath(workingDir)
	store := m.store()
	if !checkpointStoreHasHead(store) {
		return nil
	}
	tree := m.spanBaseTree(store, absDir, since)
	if tree == "" {
		return nil
	}
	scratch, err := os.MkdirTemp(store, "undo-hint-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(scratch)
	index := checkpointGitOpts{IndexFile: filepath.Join(scratch, "index")}
	if ok, _, _ := runCheckpointGit(m.env, []string{"read-tree", tree}, store, absDir, index); !ok {
		return nil
	}
	excludes := checkpointExcludeStoreArgs(CheckpointBase(m.env), absDir)
	okChanged, changed, _ := runCheckpointGit(m.env, append([]string{"diff", "--name-only", "-z"}, excludes...), store, absDir, index)
	okNew, added, _ := runCheckpointGit(m.env, append([]string{"ls-files", "--others", "--exclude-standard", "-z"}, excludes...), store, absDir, index)
	if !okChanged || !okNew {
		return nil
	}
	seen := map[string]bool{}
	var paths []string
	for _, path := range strings.Split(changed+"\x00"+added, "\x00") {
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}
