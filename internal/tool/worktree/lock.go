package worktree

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/proc"
)

const (
	lockLive = "live"
	lockDead = "dead"
)

var icebathLock = regexp.MustCompile(`(?s)^icebath (?:pid=(\d+)|session=\S+)(?: db=(.+))?$`)

func (m *Manager) lockReason(ctx context.Context, repoRoot, worktreePath string, timeout time.Duration) (bool, *string, error) {
	common, ok, err := m.gitOut(ctx, repoRoot, timeout, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return false, nil, err
	}
	if !ok || common == "" {
		return false, nil, errors.New("not a git repository")
	}
	dotGit := filepath.Join(worktreePath, ".git")
	info, err := os.Stat(dotGit)
	if err != nil || !info.Mode().IsRegular() {
		return false, nil, nil
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return false, nil, err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if !utf8.Valid(data) {
		return false, nil, errors.New("unreadable .git file in " + worktreePath)
	}
	link := string(data)
	if !strings.HasPrefix(link, "gitdir:") {
		return false, nil, errors.New("unreadable .git file in " + worktreePath)
	}
	admin := resolvePath(joinPath(worktreePath, strings.TrimSpace(strings.TrimPrefix(link, "gitdir:"))))
	expected := resolvePath(filepath.Join(joinPath(repoRoot, common), "worktrees"))
	if normCase(filepath.Dir(admin)) != normCase(expected) {
		return false, nil, nil
	}
	locked, err := os.ReadFile(filepath.Join(admin, "locked"))
	if err != nil {
		if isNotExist(err) {
			return true, nil, nil
		}
		return false, nil, err
	}
	locked = bytes.TrimPrefix(locked, []byte{0xEF, 0xBB, 0xBF})
	reason := strings.Trim(strings.ToValidUTF8(string(locked), string(rune(0xFFFD))), " \t\n\v\f\r")
	return true, &reason, nil
}

func (m *Manager) lockState(ctx context.Context, repoRoot, worktreePath string, timeout time.Duration) (string, error) {
	found, reason, err := m.lockReason(ctx, repoRoot, worktreePath, timeout)
	if err != nil {
		if fatal(ctx, err) {
			return "", err
		}
		return lockLive, nil
	}
	if !found || reason == nil {
		return "", nil
	}
	if !strings.HasPrefix(*reason, "icebath ") {
		return lockDead, nil
	}
	match := icebathLock.FindStringSubmatch(*reason)
	if match == nil {
		return lockLive, nil
	}
	pid, db := match[1], match[2]
	if pid != "" {
		n, err := strconv.Atoi(pid)
		if err != nil || n == os.Getpid() || proc.Alive(n) {
			return lockLive, nil
		}
	}
	if db != "" {
		live, err := m.retainedTreeIsLive(ctx, db, worktreePath, false)
		if err != nil {
			return "", err
		}
		if live {
			return lockLive, nil
		}
		return lockDead, nil
	}
	if pid != "" {
		return lockDead, nil
	}
	return lockLive, nil
}

func treeMatchKey(value string) string {
	return strings.TrimRight(strings.ReplaceAll(normCase(value), `\`, "/"), "/")
}

func insideTree(worktreePath string) func(string) bool {
	clean := filepath.Clean(worktreePath)
	roots := []string{treeMatchKey(clean), treeMatchKey(resolvePath(clean))}
	return func(cwd string) bool {
		if cwd == "" {
			return false
		}
		k := treeMatchKey(cwd)
		for _, r := range roots {
			if k == r || strings.HasPrefix(k, r+"/") {
				return true
			}
		}
		return false
	}
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (m *Manager) retainedTreeIsLive(ctx context.Context, db, worktreePath string, pendingOnly bool) (bool, error) {
	if !isRegularFile(db) {
		return false, nil
	}
	if m.sessions == nil {
		return false, &MissingInterfaceError{Name: "SessionStore"}
	}
	rows, err := m.sessions.SessionsInside(ctx, db, insideTree(worktreePath))
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		slog.Debug("retained worktree session check failed; keeping tree", "error", err)
		return true, nil
	}
	for _, s := range rows {
		if s.PendingAdmission || (!pendingOnly && s.MessageCount > 0) {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) TreeLive(ctx context.Context, db, worktreePath string, pendingOnly bool) (bool, error) {
	return m.retainedTreeIsLive(ctx, db, worktreePath, pendingOnly)
}

func (m *Manager) TreeServed(ctx context.Context, db, worktreePath string) (string, error) {
	chats := map[string]bool{}
	if isRegularFile(db) {
		if m.sessions == nil {
			return "", &MissingInterfaceError{Name: "SessionStore"}
		}
		rows, err := m.sessions.SessionsInside(ctx, db, insideTree(worktreePath))
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			slog.Debug("retained worktree serve check failed; keeping tree", "error", err)
			return "unreadable", nil
		}
		for _, s := range rows {
			if !s.Ended || s.ActiveSchedule {
				return "session", nil
			}
		}
		for _, s := range rows {
			if s.HasChat {
				chats[s.ChatID] = true
			}
		}
	}
	bound, err := m.cronBoundToTree(ctx, worktreePath, chats)
	if err != nil {
		return "", err
	}
	if bound {
		return "cron", nil
	}
	return "", nil
}

func (m *Manager) cronBoundToTree(ctx context.Context, worktreePath string, chats map[string]bool) (bool, error) {
	if m.cron == nil {
		return false, &MissingInterfaceError{Name: "CronJobs"}
	}
	jobs, err := m.cron.RunnableJobs(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		slog.Debug("cron store unreadable; keeping tree", "error", err)
		return true, nil
	}
	inside := insideTree(worktreePath)
	for _, job := range jobs {
		if inside(job.Workdir) || (strings.ToLower(job.OriginPlatform) == "local" && chats[job.OriginChatID]) {
			return true, nil
		}
	}
	return false, nil
}
