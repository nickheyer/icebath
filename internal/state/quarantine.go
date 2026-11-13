package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Wait for the cross-process quarantine lock before giving up
const quarantineLockWait = 5 * time.Second

// Takes the cross-process quarantine lock of path within five seconds, nil when it stayed held elsewhere
func quarantineLock(ctx context.Context, path string) (*fileutil.Lock, error) {
	wait, cancel := context.WithTimeout(ctx, quarantineLockWait)
	defer cancel()
	lock, err := fileutil.OpenLock(wait, path+".quarantine.lock")
	if errors.Is(err, fileutil.ErrLocked) && ctx.Err() == nil {
		return nil, nil
	}
	return lock, err
}

// Moves a non-SQLite database aside with its sidecars, returning the new path or empty when it stayed in place
func (e *Engine) quarantineInvalid(ctx context.Context, path string, locked bool) (string, error) {
	if !locked {
		lock, err := quarantineLock(ctx, path)
		if err != nil {
			return "", err
		}
		if lock == nil {
			slog.Error(fmt.Sprintf("quarantine lock for %s not acquired within 5s — refusing to "+
				"quarantine without the cross-process lock. The invalid file "+
				"is left in place. If sessions fail to load, run `icebath sessions recover "+
				"--source <state.db> --inspect-only`, or restore a snapshot with "+
				"`/snapshot list` / `/snapshot restore <id>` (terminal `icebath` chat only).", path))
			return "", nil
		}
		defer lock.Release()
	}
	if _, err := os.Stat(path); err != nil {
		slog.Info(fmt.Sprintf("quarantine of invalid %s skipped: already moved by another process", path))
		return "", nil
	}
	if !e.hasInvalidHeader(path, false) {
		slog.Info(fmt.Sprintf("quarantine of invalid %s skipped: it is no longer invalid (another "+
			"process quarantined it and a fresh DB was created)", path))
		return "", nil
	}
	kind := "notadb"
	if e.isZeroedFile(path, false) {
		kind = "zeroed"
	}
	stem := fmt.Sprintf("%s.%s-%s-%d", filepath.Base(path), kind, time.Now().Format("20060102-150405"), os.Getpid())
	dest := filepath.Join(filepath.Dir(path), stem+".bak")
	for n := 1; exists(dest); n++ {
		dest = filepath.Join(filepath.Dir(path), fmt.Sprintf("%s-%d.bak", stem, n))
	}
	if err := os.Rename(path, dest); err != nil {
		slog.Error(fmt.Sprintf("Failed to quarantine invalid %s: %v", path, err))
		return "", nil
	}
	for _, suffix := range []string{walSuffix, shmSuffix} {
		if exists(path + suffix) {
			_ = os.Rename(path+suffix, dest+suffix)
		}
	}
	return dest, nil
}

// Reports whether anything is at p, following symlinks
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Reports p readable and writable for this user or gone, a sidecar a sibling unlinked counting as gone
func usableOrGone(p string) bool {
	if fileutil.Accessible(p, fileutil.AccessRead|fileutil.AccessWrite) || !exists(p) {
		return true
	}
	return fileutil.Accessible(p, fileutil.AccessRead|fileutil.AccessWrite)
}

// Refuses or repairs a read-only database file, sidecar or directory before the first connection opens
func (e *Engine) PreflightWritable(ctx context.Context, path, label string) error {
	return e.preflightWritable(ctx, path, label, false)
}

// Preflight of the database directory alone when dirOnly is set, else of the directory and every database file present
func (e *Engine) preflightWritable(ctx context.Context, path, label string, dirOnly bool) error {
	home, err := e.home.Home(ctx)
	if err != nil {
		home = ""
	} else {
		home = resolvePath(home)
	}
	type target struct {
		path string
		dir  bool
	}
	targets := []target{{filepath.Dir(path), true}}
	if !dirOnly {
		for _, p := range sqliteFamily(path) {
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				targets = append(targets, target{p, false})
			}
		}
	}
	for _, t := range targets {
		if fi, err := os.Stat(t.path); (t.dir && (err != nil || !fi.IsDir())) || usableOrGone(t.path) {
			continue
		}
		x, what := "", "file"
		if t.dir {
			x, what = "x", "directory"
		}
		if home != "" && underDir(resolvePath(t.path), home) {
			if fi, err := os.Stat(t.path); err == nil {
				add := fs.FileMode(0o600)
				if t.dir {
					add = 0o700
				}
				_ = os.Chmod(t.path, fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)|add)
			}
			if fileutil.Accessible(t.path, fileutil.AccessRead|fileutil.AccessWrite) {
				slog.Info(fmt.Sprintf("%s preflight: repaired read-only %s (chmod u+rw%s)", label, t.path, x))
				continue
			}
		}
		walNote := ""
		if strings.HasSuffix(t.path, walSuffix) {
			walNote = " Do NOT delete the -wal file — it contains committed data that " +
				"will be merged into the database once it is writable."
		}
		return callErr(nil, "%s is not writable: %s %s is read-only for this user. icebath needs read-write access "+
			"to open the database. Fix with: chmod u+rw%s '%s' (files owned by another user may need sudo/chown).%s",
			label, what, t.path, x, t.path, walNote)
	}
	return nil
}

// Reports whether p is root or lies below it
func underDir(p, root string) bool {
	p, root = normCase(p), normCase(root)
	if p == root {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}
