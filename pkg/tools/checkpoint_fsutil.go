package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Deletes a tree, clearing the write bit on a failing path and its parent before retrying that one operation
func rmtreeReadonly(path string, ignoreErrors bool) error {
	err := rmtreeTop(path)
	if err != nil && !ignoreErrors {
		return err
	}
	return nil
}

// Refuses a symlink root the way shutil.rmtree does, then removes the tree
func rmtreeTop(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return &os.PathError{Op: "rmtree", Path: path, Err: errors.New("Cannot call rmtree on a symbolic link")}
	}
	return rmtreeDir(path)
}

// Removes a directory's entries depth first and then the directory itself
func rmtreeDir(dir string) error {
	var names []string
	err := rmtreeRetry(dir, func() error {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		names, err = f.Readdirnames(-1)
		return err
	})
	if err != nil {
		return err
	}
	for _, name := range names {
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		if err == nil && st.IsDir() {
			if err := rmtreeDir(p); err != nil {
				return err
			}
			continue
		}
		if err := rmtreeRetry(p, func() error { return rmtreeRemove(p) }); err != nil {
			return err
		}
	}
	return rmtreeRetry(dir, func() error { return rmtreeRemove(dir) })
}

// Single entry removal inside a tree delete, swapped by tests
var rmtreeRemove = os.Remove

// Retries an operation once after granting owner write and search on the path and its parent, permission errors only
func rmtreeRetry(p string, op func() error) error {
	err := op()
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	for _, candidate := range []string{filepath.Dir(p), p} {
		if st, serr := os.Stat(candidate); serr == nil {
			keep := st.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
			os.Chmod(candidate, keep|0o300)
		}
	}
	return op()
}

// Default age floor below which git debris is presumed live
const staleTmpPackMinAgeSeconds = 10 * 60

// Temp prefixes git writes in objects/pack and renames away on success
var tmpPackPrefixes = []string{"tmp_pack_", "tmp_idx_", "tmp_rev_", "tmp_mtimes_", ".tmp-"}

// Pack directory of a checkout or a bare repository
func gitPackDir(repoRoot string) string {
	gitDir := filepath.Join(repoRoot, ".git")
	if isDirFollow(gitDir) {
		return filepath.Join(gitDir, "objects", "pack")
	}
	return filepath.Join(repoRoot, "objects", "pack")
}

// Names git uses as a process, plain, dashed helpers and the exe spelling, empty for anything else
func gitProgramName(arg0 string) string {
	name := strings.TrimSpace(arg0)
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	if name == "git" || strings.HasPrefix(name, "git-") {
		return name
	}
	return ""
}

// Git process probe guarding the debris sweep, swapped by tests
var checkpointGitProcRunning = gitProcRunning

// Reports whether any git process may be running, answering true whenever the probe cannot tell
func gitProcRunning(env *Env) bool {
	if runtime.GOOS == "linux" {
		if ents, err := os.ReadDir("/proc"); err == nil {
			for _, e := range ents {
				if _, err := strconv.Atoi(e.Name()); err != nil {
					continue
				}
				comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					toolsLogger(env).Debug("git process scan failed; assuming git may be running")
					return true
				}
				if gitProgramName(strings.TrimRight(string(comm), "\n")) != "" {
					return true
				}
			}
			return false
		}
	}
	argv := []string{"pgrep", `^git(-.*|\.exe)?$`}
	if runtime.GOOS == "windows" {
		argv = []string{"tasklist", "/FO", "CSV", "/NH"}
	}
	res, err := runCapture(nil, argv, 10*time.Second, false, nil)
	if err != nil {
		toolsLogger(env).Debug("git process probe failed; assuming git may be running")
		return true
	}
	if runtime.GOOS == "windows" {
		if res.Code != 0 {
			return true
		}
		for _, line := range strings.Split(strings.ReplaceAll(res.Stdout, "\r\n", "\n"), "\n") {
			if line == "" {
				continue
			}
			first, _, _ := strings.Cut(line, ",")
			if gitProgramName(strings.Trim(first, `"`)) != "" {
				return true
			}
		}
		return false
	}
	return res.Code != 1
}

// Removes aged debris files listed by the candidate function, skipping entirely while a git process runs
func sweepStaleGitDebris(env *Env, directory string, candidates func() []string, minAge *int, defaultAge int, skipMsg string,
	logRemoved func(string, int64)) []string {
	if !isDirFollow(directory) {
		return nil
	}
	if checkpointGitProcRunning(env) {
		toolsLogger(env).Debug(skipMsg)
		return nil
	}
	age := defaultAge
	if minAge != nil {
		age = *minAge
	}
	cutoff := float64(time.Now().UnixNano())/1e9 - float64(age)
	var removed []string
	for _, entry := range candidates() {
		st, err := os.Stat(entry)
		if err != nil {
			if !checkpointPathMissing(err) {
				toolsLogger(env).Warn(fmt.Sprintf("Could not clear %s (skipping): %v", entry, err))
			}
			continue
		}
		if !st.Mode().IsRegular() || statMtime(st) >= cutoff {
			continue
		}
		if runtime.GOOS == "windows" {
			os.Chmod(entry, 0o666)
		}
		if err := os.Remove(entry); err != nil {
			toolsLogger(env).Warn(fmt.Sprintf("Could not clear %s (skipping): %v", entry, err))
			continue
		}
		removed = append(removed, entry)
		logRemoved(entry, st.Size())
	}
	return removed
}

// Removes aborted transfer temp packs and killed commit graph temps from a checkout or bare repository
func clearStaleTmpPacks(env *Env, repoRoot string, minAge *int) []string {
	packDir := gitPackDir(repoRoot)
	graphDir := filepath.Join(filepath.Dir(packDir), "info", "commit-graphs")
	candidates := func() []string {
		var found []string
		for _, spec := range []struct {
			dir      string
			prefixes []string
		}{{packDir, tmpPackPrefixes}, {graphDir, []string{"tmp_graph_"}}} {
			ents, err := os.ReadDir(spec.dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if slices.ContainsFunc(spec.prefixes, func(p string) bool { return strings.HasPrefix(e.Name(), p) }) {
					found = append(found, filepath.Join(spec.dir, e.Name()))
				}
			}
		}
		return found
	}
	return sweepStaleGitDebris(env, packDir, candidates, minAge, staleTmpPackMinAgeSeconds,
		"git process running; skipping tmp-pack sweep",
		func(p string, size int64) {
			toolsLogger(env).Info(fmt.Sprintf("Removed aborted-write git debris %s (%d bytes)", p, size))
		})
}

// Reads a text marker as UTF-8 with an optional byte order mark, an error for undecodable bytes
func readUTF8SigText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", &checkpointValueError{msg: "'utf-8' codec can't decode " + path}
	}
	return strings.TrimPrefix(string(raw), "\ufeff"), nil
}
