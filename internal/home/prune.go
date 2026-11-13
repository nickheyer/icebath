package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/proc"
)

// Prune audit rotation, the depth linked worktrees are looked for and the git release budget
const (
	pruneLogMaxBytes = 5 * 1024 * 1024
	pruneLogBackups  = 3
	gitFileMaxDepth  = 4
	gitPruneTimeout  = 15 * time.Second
)

// Activity state of a scratch entry against the idle cutoff
type touchState int

// A recent write was seen, the subtree could not be read, or nothing was written since the cutoff
const (
	touched touchState = iota
	scanFailed
	idle
)

// Audit sink for one prune, the rotating scratch-prune log echoed into the process log
type pruneAudit struct{ path string }

// Audit writer on logFile with its directory created, the process log alone when that directory cannot be made
func openPruneLog(logFile string) *pruneAudit {
	if err := makeDir(filepath.Dir(logFile), 0o777, true); err != nil {
		slog.Warn(fmt.Sprintf("scratch prune: cannot create %s: %v", filepath.Dir(logFile), err))
		return &pruneAudit{}
	}
	return &pruneAudit{path: logFile}
}

// Text with every byte that is not UTF-8 written as the surrogate escape it decodes to
func escapeInvalid(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			fmt.Fprintf(&b, `\udc%02x`, s[i])
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// Appends one timestamped record, rotating first when it would reach the size cap
func (a *pruneAudit) info(msg string) {
	msg = escapeInvalid(msg)
	slog.Info(msg)
	if a.path == "" {
		return
	}
	now := time.Now()
	line := now.Format("2006-01-02 15:04:05") + fmt.Sprintf(",%03d ", now.Nanosecond()/int(time.Millisecond)) + msg
	if fi, err := os.Stat(a.path); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 &&
		fi.Size()+int64(utf8.RuneCountInString(line)+1) >= pruneLogMaxBytes {
		if err := a.rollover(); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return
		}
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + lineEnding)
	_ = f.Close()
}

// Shifts the numbered backups up and moves the live log to the first, stopping where a concurrent prune rotated first
func (a *pruneAudit) rollover() error {
	for i := pruneLogBackups - 1; i > 0; i-- {
		src, dst := fmt.Sprintf("%s.%d", a.path, i), fmt.Sprintf("%s.%d", a.path, i+1)
		if !exists(src) {
			continue
		}
		if exists(dst) {
			if err := os.Remove(dst); err != nil {
				return err
			}
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	dst := a.path + ".1"
	if exists(dst) {
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	if exists(a.path) {
		return os.Rename(a.path, dst)
	}
	return nil
}

// Apparent size of path and everything under it, symlinks not followed
func treeBytes(path string) int64 {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	total := fi.Size()
	if !isRealDir(path) {
		return total
	}
	stack := []string{path}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			total += info.Size()
			if e.IsDir() {
				stack = append(stack, filepath.Join(dir, e.Name()))
			}
		}
	}
	return total
}

// Reports whether p is a directory that is not itself a symlink
func isRealDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&fs.ModeSymlink == 0 && isDir(p)
}

// Directory a failed listing names when permission refused it, empty for any other failure
func deniedDir(dir string, err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return dir
	}
	return ""
}

// Activity of the subtree at path against cutoff, with the directory that refused listing when that failed the scan
func scanSubtree(path string, cutoff time.Time) (touchState, string) {
	fi, err := os.Lstat(path)
	if err != nil {
		return scanFailed, ""
	}
	if !fi.ModTime().Before(cutoff) {
		return touched, ""
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		if _, err := os.Stat(path); err != nil && !ignorableStatError(err) {
			return scanFailed, ""
		}
		return idle, ""
	}
	if !fi.IsDir() {
		return idle, ""
	}
	stack := []string{path}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			return scanFailed, deniedDir(dir, err)
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				return scanFailed, deniedDir(dir, err)
			}
			if !info.ModTime().Before(cutoff) && !(linkCount(info) > 1 && info.Mode().IsRegular()) {
				return touched, ""
			}
			if e.IsDir() {
				stack = append(stack, filepath.Join(dir, e.Name()))
			}
		}
	}
	return idle, ""
}

// Reports whether p is root or lies below it, compared as written
func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimRight(root, string(filepath.Separator))+string(filepath.Separator))
}

// Removes the scratch entries idle since maxIdle, reaping what runs in them first, and returns the count removed
func pruneScratch(root string, maxIdle time.Duration, from lineage) int {
	root = filepath.Clean(root)
	audit := openPruneLog(ScratchPruneLog.In(filepath.Dir(filepath.Dir(root))))
	cutoff := time.Now().Add(-maxIdle)
	skip := loadStuckSkips(root, true)
	skip[scratchStamp], skip[stuckStateName], skip[stuckLockName] = true, true, true
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	var doomed []string
	for _, e := range entries {
		if skip[e.Name()] {
			continue
		}
		p := filepath.Join(root, e.Name())
		state, denied := scanSubtree(p, cutoff)
		if state == idle {
			doomed = append(doomed, p)
		} else if denied != "" && stuckTrackingSupported {
			recordStuck(root, p, []string{denied}, audit)
		}
	}
	doomed, err = reapRootedIn(root, doomed, from, audit)
	if err != nil {
		audit.info(fmt.Sprintf("scratch prune: process check failed (%v), nothing deleted this pass", err))
		return 0
	}
	if len(doomed) == 0 {
		return 0
	}
	repos := map[string]bool{}
	removed := 0
	for _, entry := range doomed {
		if isRealDir(entry) {
			for repo := range linkedWorktreeRepos(entry) {
				repos[repo] = true
			}
		}
		if removeIdleEntry(root, entry, cutoff, audit) {
			removed++
		}
	}
	releaseGitWorktrees(repos)
	return removed
}

// Deletes one doomed entry after a last activity check, recording a rescue, a vanish, a partial removal or the removal
func removeIdleEntry(root, entry string, cutoff time.Time, audit *pruneAudit) bool {
	size := treeBytes(entry)
	if state, _ := scanSubtree(entry, cutoff); state != idle {
		_, err := os.Lstat(entry)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			audit.info(fmt.Sprintf("scratch prune: entry %s vanished since selection", reprQuote(entry)))
		case state == touched:
			audit.info(fmt.Sprintf("scratch prune: rescued %s — touched since selection, kept", reprQuote(entry)))
		default:
			audit.info(fmt.Sprintf("scratch prune: kept %s — activity re-scan failed, left untouched", reprQuote(entry)))
		}
		return false
	}
	var denied []string
	if isRealDir(entry) {
		denied = removeCollectingDenied(entry)
	} else if err := os.Remove(entry); err != nil {
		audit.info(fmt.Sprintf("scratch prune: could not remove %s: %v", reprQuote(entry), err))
		return false
	}
	_, err := os.Lstat(entry)
	switch {
	case err == nil && len(denied) > 0 && stuckTrackingSupported:
		recordStuck(root, entry, denied, audit)
		return false
	case err == nil:
		audit.info(fmt.Sprintf("scratch prune: could not fully remove %s", reprQuote(entry)))
		return false
	case !errors.Is(err, fs.ErrNotExist):
		audit.info(fmt.Sprintf("scratch prune: could not confirm removal of %s", reprQuote(entry)))
		return false
	}
	audit.info(fmt.Sprintf("scratch prune: removed %s (%d bytes)", reprQuote(entry), size))
	return true
}

// Removes the tree at entry past every failure and returns the directories that refused access, in order found
func removeCollectingDenied(entry string) []string {
	var blockers []string
	var visit func(error)
	visit = func(err error) {
		if many, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range many.Unwrap() {
				visit(e)
			}
			return
		}
		var pe *fs.PathError
		if !errors.Is(err, fs.ErrPermission) || !errors.As(err, &pe) {
			return
		}
		b := pe.Path
		if pe.Op == "remove" {
			b = filepath.Dir(pe.Path)
		}
		if !slices.Contains(blockers, b) {
			blockers = append(blockers, b)
		}
	}
	if err := fileutil.RemoveTree(entry, false); err != nil {
		visit(err)
	}
	return blockers
}

// Repos whose linked worktrees live inside entry, found through the gitdir line of each worktree .git file
func linkedWorktreeRepos(entry string) map[string]bool {
	type frame struct {
		dir   string
		depth int
	}
	repos := map[string]bool{}
	stack := []frame{{entry, 0}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(cur.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(cur.dir, e.Name())
			switch {
			case e.Name() == ".git" && e.Type().IsRegular():
				if repo, ok := worktreeRepo(p); ok {
					repos[repo] = true
				}
			case e.IsDir() && cur.depth < gitFileMaxDepth && e.Name() != "node_modules" && e.Name() != ".venv" && e.Name() != "venv":
				stack = append(stack, frame{p, cur.depth + 1})
			}
		}
	}
	return repos
}

// Repo a linked worktree .git file points into, false when the file names no worktrees registration
func worktreeRepo(gitFile string) (string, bool) {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(strings.ToValidUTF8(strings.TrimPrefix(string(data), "\xef\xbb\xbf"), "�"))
	rest, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", false
	}
	parent := filepath.Dir(filepath.Clean(strings.TrimSpace(rest)))
	if filepath.Base(parent) != "worktrees" || filepath.Base(filepath.Dir(parent)) != ".git" {
		return "", false
	}
	return filepath.Dir(filepath.Dir(parent)), true
}

// Runs git worktree prune in each existing repo so registrations of deleted trees go away
func releaseGitWorktrees(repos map[string]bool) {
	for _, repo := range sortedKeys(repos) {
		if !isDir(repo) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), gitPruneTimeout)
		if _, err := proc.Run(ctx, exec.Command("git", "-C", repo, "worktree", "prune")); err != nil {
			slog.Debug(fmt.Sprintf("git worktree prune in %s failed: %v", repo, err))
		}
		cancel()
	}
}

// Keys of set in sorted order
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
