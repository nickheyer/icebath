package env

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

const TerminalTempMaxIdle = 24 * time.Hour

var bgGroup = regexp.MustCompile(`^(icebath_bg_[A-Za-z0-9_-]+)\.(log|pid|exit)$`)

func subtreeTouchedSince(path string, cutoff time.Time) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return true
	}
	if !info.ModTime().Before(cutoff) {
		return true
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return false
	}
	stack := []string{path}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(current)
		if err != nil {
			return true
		}
		for _, e := range entries {
			child := filepath.Join(current, e.Name())
			st, err := os.Lstat(child)
			if err != nil {
				return true
			}
			if !st.ModTime().Before(cutoff) && !(st.Mode().IsRegular() && multiLinked(st)) {
				return true
			}
			if st.IsDir() {
				stack = append(stack, child)
			}
		}
	}
	return false
}

func (h *Host) terminalCacheDir(ctx context.Context) (string, error) {
	homeDir, err := h.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	return home.TerminalCache.In(homeDir), nil
}

func (h *Host) CleanupTerminalTemp(ctx context.Context, maxAge time.Duration) int {
	root, err := h.terminalCacheDir(ctx)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	mtimes := map[string]time.Time{}
	var order []string
	groupNewest := map[string]time.Time{}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		mtimes[p] = st.ModTime()
		order = append(order, p)
		if m := bgGroup.FindStringSubmatch(e.Name()); m != nil {
			if st.ModTime().After(groupNewest[m[1]]) {
				groupNewest[m[1]] = st.ModTime()
			}
		}
	}
	groupTouched := func(stem string) bool {
		for _, sfx := range []string{"log", "pid", "exit"} {
			st, err := os.Stat(filepath.Join(root, stem+"."+sfx))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil || !st.ModTime().Before(cutoff) {
				return true
			}
		}
		return false
	}
	removed := 0
	for _, p := range order {
		if m := bgGroup.FindStringSubmatch(filepath.Base(p)); m != nil {
			if !groupNewest[m[1]].Before(cutoff) || groupTouched(m[1]) {
				continue
			}
		} else if subtreeTouchedSince(p, cutoff) {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			if lst, err := os.Lstat(p); err == nil && lst.Mode()&fs.ModeSymlink != 0 {
				continue
			}
			_ = os.RemoveAll(p)
		} else if err := os.Remove(p); err != nil {
			continue
		}
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			removed++
		}
	}
	return removed
}

func (h *Host) pruneTerminalTempOnce(ctx context.Context) {
	h.pruneOnce.Do(func() {
		h.CleanupTerminalTemp(ctx, TerminalTempMaxIdle)
	})
}

func trimPosix(p string) string {
	if t := strings.TrimRight(p, "/"); t != "" {
		return t
	}
	return "/"
}

func (h *Host) localTempDir(ctx context.Context) (string, error) {
	if windowsHost {
		for _, key := range []string{"TERMINAL_TEMP_DIR", "TMPDIR"} {
			if c := os.Getenv(key); c != "" && filepath.IsAbs(c) && isDir(c) {
				return filepath.ToSlash(c), nil
			}
		}
		dir, err := h.terminalCacheDir(ctx)
		if err != nil {
			dir = filepath.Join(os.TempDir(), "icebath_terminal")
		}
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return "", err
		}
		h.pruneTerminalTempOnce(ctx)
		return strings.ReplaceAll(dir, `\`, "/"), nil
	}
	for _, key := range []string{"TERMINAL_TEMP_DIR", "TMPDIR", "TMP", "TEMP"} {
		c := os.Getenv(key)
		if c != "" && strings.HasPrefix(c, "/") && (key != "TERMINAL_TEMP_DIR" || isDir(c)) {
			return trimPosix(c), nil
		}
	}
	if dir, err := h.terminalCacheDir(ctx); err == nil {
		if err := os.MkdirAll(dir, 0o777); err == nil && strings.HasPrefix(dir, "/") &&
			fileutil.Accessible(dir, fileutil.AccessWrite|fileutil.AccessExec) {
			h.pruneTerminalTempOnce(ctx)
			return trimPosix(dir), nil
		}
	}
	fallback := os.TempDir()
	if !strings.HasPrefix(fallback, "/") {
		if abs, err := filepath.Abs(fallback); err == nil {
			fallback = abs
		}
	}
	return trimPosix(fallback), nil
}
