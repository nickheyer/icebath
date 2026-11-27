package git

import (
	"context"
	"encoding/csv"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Age past which a lock or a temp file git writes is taken as left by a git that died: a healthy fetch is done in minutes
const StaleAge = 10 * time.Minute

// Index locks of the object store a killed git leaves, each failing every later write of its file
var objectLocks = []string{
	"objects/info/commit-graph.lock",
	"objects/info/commit-graphs/commit-graph-chain.lock",
	"objects/pack/multi-pack-index.lock",
	"objects/pack/multi-pack-index.d/multi-pack-index-chain.lock",
}

// Temp files git writes in objects/pack and renames away once a write succeeds
var tempPacks = []string{"tmp_pack_", "tmp_idx_", "tmp_rev_", "tmp_mtimes_", ".tmp-"}

// Temp a split commit graph write killed before its rename leaves in objects/info/commit-graphs
const tempGraph = "tmp_graph_"

// Whether a process name is git or one of its helpers, such as git-remote-https
func program(name string) bool {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	return name == "git" || strings.HasPrefix(name, "git-")
}

// Whether any git may be running on this machine, true whenever the look cannot tell
func Running() bool {
	switch runtime.GOOS {
	case "linux":
		entries, err := os.ReadDir("/proc")
		if err != nil {
			slog.Debug("git process scan failed; assuming git may be running", "error", err)
			return true
		}
		for _, entry := range entries {
			if strings.Trim(entry.Name(), "0123456789") != "" {
				continue
			}
			if name, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm")); err == nil && program(string(name)) {
				return true
			}
		}
		return false
	case "windows":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "tasklist", "/FO", "CSV", "/NH")
		group(cmd)
		out, err := cmd.Output()
		if err != nil {
			slog.Debug("git process probe failed; assuming git may be running", "error", err)
			return true
		}
		records, err := csv.NewReader(strings.NewReader(strings.ToValidUTF8(string(out), "�"))).ReadAll()
		if err != nil {
			return true
		}
		return slices.ContainsFunc(records, func(record []string) bool { return len(record) > 0 && program(record[0]) })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "pgrep", `^git(-.*|\.exe)?$`).Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
		return false
	}
	if err != nil {
		slog.Debug("git process probe failed; assuming git may be running", "error", err)
	}
	return true
}

// Removes the files the candidates name that are older than the age, skipping all of them while any git runs; the paths removed
func sweep(dir string, age time.Duration, skip string, candidates func() []string) []string {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil
	}
	if Running() {
		slog.Debug(skip)
		return nil
	}
	cutoff := time.Now().Add(-age)
	var removed []string
	for _, path := range candidates() {
		info, err := os.Stat(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("Could not clear "+path+" (skipping)", "error", err)
			}
			continue
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if runtime.GOOS == "windows" {
			os.Chmod(path, 0o666)
		}
		if err := os.Remove(path); err != nil {
			slog.Warn("Could not clear "+path+" (skipping)", "error", err)
			continue
		}
		removed = append(removed, path)
		slog.Info("Removed aborted-write git debris "+path, "bytes", info.Size())
	}
	return removed
}

// Pack folder of a checkout or of a bare repository
func packDir(repo string) string {
	if info, err := os.Stat(filepath.Join(repo, ".git")); err == nil && info.IsDir() {
		return filepath.Join(repo, ".git", "objects", "pack")
	}
	return filepath.Join(repo, "objects", "pack")
}

// Removes the temp packs of aborted transfers and the temps of killed commit graph writes, older than the age, of a checkout or a bare
// repository while no git runs; the paths removed
func SweepTemp(repo string, age time.Duration) []string {
	packs := packDir(repo)
	graphs := filepath.Join(filepath.Dir(packs), "info", "commit-graphs")
	return sweep(packs, age, "git process running; skipping tmp-pack sweep", func() []string {
		var found []string
		for _, spec := range []struct {
			dir      string
			prefixes []string
		}{{packs, tempPacks}, {graphs, []string{tempGraph}}} {
			entries, _ := os.ReadDir(spec.dir)
			for _, entry := range entries {
				if slices.ContainsFunc(spec.prefixes, func(prefix string) bool { return strings.HasPrefix(entry.Name(), prefix) }) {
					found = append(found, filepath.Join(spec.dir, entry.Name()))
				}
			}
		}
		return found
	})
}

// Removes the stale index locks of the object store under a git folder while no git runs; the paths removed
func SweepObjectLocks(gitDir string) []string {
	return sweep(gitDir, StaleAge, "git process running; skipping stale-lock sweep", func() []string {
		paths := make([]string, len(objectLocks))
		for i, name := range objectLocks {
			paths[i] = filepath.Join(gitDir, filepath.FromSlash(name))
		}
		return paths
	})
}

// Gives every pack of a partial clone without a promisor marker one, so git fetches the objects it lacks rather than failing; how many
// were marked
func MarkPromisor(repo string) int {
	packs, _ := filepath.Glob(filepath.Join(packDir(repo), "pack-*.pack"))
	marked := 0
	for _, pack := range packs {
		marker := strings.TrimSuffix(pack, filepath.Ext(pack)) + ".promisor"
		f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			if !errors.Is(err, fs.ErrExist) {
				slog.Warn("Could not mark "+pack+" as a partial-clone pack", "error", err)
			}
			continue
		}
		f.Close()
		marked++
	}
	if marked > 0 {
		slog.Info("Marked partial-clone packs", "count", marked, "repo", repo)
	}
	return marked
}
