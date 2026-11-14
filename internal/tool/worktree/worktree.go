// Creates, classifies, archives, restores and reclaims the git worktrees icebath makes, and records their repositories
package worktree

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/toolchain"
)

type TreeSession struct {
	ChatID           string
	HasChat          bool
	MessageCount     int
	Ended            bool
	PendingAdmission bool
	ActiveSchedule   bool
}

type SessionStore interface {
	SessionsInside(ctx context.Context, db string, inside func(cwd string) bool) ([]TreeSession, error)
}

type CronJob struct {
	Workdir        string
	OriginPlatform string
	OriginChatID   string
}

type CronJobs interface {
	RunnableJobs(ctx context.Context) ([]CronJob, error)
}

type WorkspaceReleaser interface {
	ReleaseWorkspace(ctx context.Context, root string) error
}

type Options struct {
	Homes    *home.Resolver
	Tools    *toolchain.Store
	Config   *config.Store
	Sessions SessionStore
	Cron     CronJobs
	LSP      WorkspaceReleaser
	Guard    *proc.Guard
	Ledger   *proc.Ledger
}

type Manager struct {
	homes    *home.Resolver
	tools    *toolchain.Store
	config   *config.Store
	sessions SessionStore
	cron     CronJobs
	lsp      WorkspaceReleaser
	guard    *proc.Guard
	ledger   *proc.Ledger

	safeMu    sync.Mutex
	safeCache map[string][]string
}

func New(o Options) (*Manager, error) {
	if o.Homes == nil {
		return nil, &MissingInterfaceError{Name: "home.Resolver"}
	}
	if o.Config == nil {
		return nil, &MissingInterfaceError{Name: "config.Store"}
	}
	if err := checkTools(o); err != nil {
		return nil, err
	}
	return &Manager{
		homes:     o.Homes,
		tools:     o.Tools,
		config:    o.Config,
		sessions:  o.Sessions,
		cron:      o.Cron,
		lsp:       o.LSP,
		guard:     o.Guard,
		ledger:    o.Ledger,
		safeCache: map[string][]string{},
	}, nil
}

type MissingInterfaceError struct {
	Name string
}

func (e *MissingInterfaceError) Error() string {
	return "worktree: no " + e.Name + " was provided"
}

type ArchiveError struct {
	Msg string
	Err error
}

func (e *ArchiveError) Error() string { return e.Msg }

func (e *ArchiveError) Unwrap() error { return e.Err }

type CreateError struct {
	Msg  string
	Hint string
	Err  error
}

func (e *CreateError) Error() string { return e.Msg }

func (e *CreateError) Unwrap() error { return e.Err }

type DeadlineError struct {
	Command string
	Args    []string
	After   time.Duration
}

func (e *DeadlineError) Error() string {
	return fmt.Sprintf("%s %s timed out after %gs", e.Command, strings.Join(e.Args, " "), e.After.Seconds())
}

type RunError struct {
	Command string
	Err     error
}

func (e *RunError) Error() string { return e.Command + ": " + e.Err.Error() }

func (e *RunError) Unwrap() error { return e.Err }

type GitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *GitError) Error() string {
	if e.Stderr != "" {
		return e.Stderr
	}
	return fmt.Sprintf("git %s exited with status %d", strings.Join(e.Args, " "), e.Code)
}

func fatal(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil {
		return true
	}
	var missing *MissingInterfaceError
	return errors.As(err, &missing)
}

func (m *Manager) home(ctx context.Context) (string, error) {
	return m.homes.Home(ctx)
}

func (m *Manager) releaseLSP(ctx context.Context, root string) error {
	if m.lsp == nil {
		return &MissingInterfaceError{Name: "WorkspaceReleaser"}
	}
	if err := m.lsp.ReleaseWorkspace(ctx, root); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug("LSP release for worktree failed", "path", root, "error", err)
	}
	return nil
}
