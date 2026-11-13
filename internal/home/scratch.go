package home

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Scratch retention and the throttle on how often a detached prune starts
const (
	// Time without a write anywhere in a scratch entry before a prune removes it
	ScratchMaxIdle = 24 * time.Hour
	scratchStamp   = ".last_prune"
	pruneInterval  = time.Hour
	// Longest temp root that leaves room for the AF_UNIX socket names browsers and the code kernel append
	socketTempDirMaxLen = 50
)

// Temp variables pointed at the scratch directory for every icebath process and child
func ScratchTempVars() []string { return []string{"TMPDIR", "TMP", "TEMP"} }

// Scratch directory of home, created and secured when the filesystem allows, without starting a prune
func PrepareScratchDir(home string) string {
	scratch := Scratch.In(home)
	if makeDir(scratch, 0o777, true) == nil && securesScratch {
		ApplySecureDirPolicy(scratch, home)
	}
	return scratch
}

// Scratch directory of home, created and secured, starting at most one detached idle prune per process and per hour
func (r *Resolver) ScratchDir(home string) string {
	scratch := PrepareScratchDir(home)
	r.pruneOnce(scratch)
	return scratch
}

// Starts the detached prune once per process and only when the stamp file is an hour old
func (r *Resolver) pruneOnce(scratch string) {
	if r.pruned.Swap(true) {
		return
	}
	stamp := filepath.Join(scratch, scratchStamp)
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < pruneInterval {
		return
	}
	if touch(stamp) != nil {
		return
	}
	spawnPrune(scratch)
}

// Sets the modification time of path to now, creating it when missing
func touch(path string) error {
	now := time.Now()
	if os.Chtimes(path, now, now) == nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	return f.Close()
}

// Points the temp variables of env at the scratch directory of its ICEBATH_HOME unless something else set them
func (r *Resolver) ApplyScratchTmpEnv(env map[string]string) bool {
	ours := env[EnvScratchDir]
	for _, key := range ScratchTempVars() {
		if v := strings.TrimSpace(env[key]); v != "" && v != ours {
			return false
		}
	}
	base := strings.TrimSpace(env[EnvHome])
	if base != "" {
		base = expandHome(base)
	} else {
		h, err := ProcessHome()
		if err != nil {
			return false
		}
		base = h
	}
	scratch := r.ScratchDir(base)
	for _, key := range ScratchTempVars() {
		env[key] = scratch
	}
	env[EnvScratchDir] = scratch
	return true
}

// Points this process temp variables at its scratch directory, rerun after anything re-homes the process
func (r *Resolver) ExportScratchTmpEnv() (bool, error) {
	env := map[string]string{}
	for _, key := range append(ScratchTempVars(), EnvScratchDir, EnvHome) {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	if !r.ApplyScratchTmpEnv(env) {
		return false, nil
	}
	for _, key := range append(ScratchTempVars(), EnvScratchDir) {
		if err := os.Setenv(key, env[key]); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Temp root short enough for AF_UNIX sockets, the usable temp root unless it is too long and /tmp exists
func SocketSafeTempDir() (string, error) {
	if forcedSocketRoot != "" {
		return forcedSocketRoot, nil
	}
	candidate, err := usableTempDir()
	if err != nil {
		return "", err
	}
	if utf8.RuneCountInString(candidate) <= socketTempDirMaxLen || !isDir("/tmp") {
		return candidate, nil
	}
	return "/tmp", nil
}

// Bytes of every non-directory listed under dir, symlinked directories never entered, zero when unreadable
func UsageBytes(dir string) int64 {
	var total int64
	stack := []string{dir}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(cur)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(cur, e.Name())
			if e.Type()&fs.ModeSymlink != 0 {
				if isDir(p) {
					continue
				}
			} else if e.IsDir() {
				stack = append(stack, p)
				continue
			}
			if fi, err := os.Lstat(p); err == nil {
				total += fi.Size()
			}
		}
	}
	return total
}

// Entries of the scratch directory a prune skips because this user cannot delete them
func StuckEntries(scratch string) []string {
	names := loadStuckSkips(scratch, false)
	out := make([]string, 0, len(names))
	for _, n := range sortedKeys(names) {
		out = append(out, filepath.Join(scratch, n))
	}
	return out
}
