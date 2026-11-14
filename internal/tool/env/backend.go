// Execution environments every tool runs commands through: the shared shell core, the local backend, child environment policy and passthrough registries
package env

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/internal/tool"
)

type StdinMode int

const (
	StdinPipe StdinMode = iota
	StdinPayload
	StdinHeredoc
)

type RunOptions struct {
	Login   bool
	Timeout time.Duration
	Stdin   *string
	Cwd     string
}

type Traits struct {
	Local                    bool
	HostFilesystem           bool
	Persistent               bool
	StdinMode                StdinMode
	SnapshotTimeout          time.Duration
	PersistEnv               bool
	PasswordlessSudoProbe    bool
	ProfileScopedPassthrough bool
	TempDir                  string
}

type Backend interface {
	Kind() string
	RunBash(ctx context.Context, script string, o RunOptions) (tool.Process, error)
	Close(ctx context.Context, remove bool) error
	Traits() Traits
}

type Syncer interface {
	Sync(ctx context.Context, force bool) error
	SyncBack(ctx context.Context) error
}

type Starter interface {
	Start(ctx context.Context, argv []string, o tool.StartOptions) (tool.Process, error)
}

type Forwarder interface {
	Forward(ctx context.Context, remoteHost string, remotePort int) (tool.Forward, error)
}

type Recreator interface {
	TakeRecreated() bool
}

type PathQuoter interface {
	QuoteShellPath(path string) string
	QuoteCwd(cwd string) string
}

type CwdKeeper interface {
	RecoverCwd(cwd string) string
	ObservedCwd(previous, observed string) (string, bool)
}

type ScopedNames interface {
	ScopedPassthroughNames(ctx context.Context) []string
}

type ForceKiller interface {
	ForceKill() error
}

type Releaser interface {
	Release()
}

type CommandPreparer interface {
	Prepare(ctx context.Context, command string, nopasswd func(ctx context.Context) bool) (string, *string, error)
	RewriteCompoundBackground(command string) string
}

const defaultRetryHint = "This is an infrastructure failure, not a command failure. " +
	"Verify the backend is reachable (network, service running, " +
	"credentials), then retry the same command — recovery is " +
	"automatic once the backend is back."

type ConnectionError struct {
	Reason    string
	RetryHint string
	Err       error
}

func NewConnectionError(reason, retryHint string, err error) *ConnectionError {
	if retryHint == "" {
		retryHint = defaultRetryHint
	}
	return &ConnectionError{Reason: reason, RetryHint: retryHint, Err: err}
}

func (e *ConnectionError) Error() string { return e.Reason }

func (e *ConnectionError) Unwrap() error { return e.Err }

type FetchError struct {
	Msg string
	Err error
}

func (e *FetchError) Error() string { return e.Msg }

func (e *FetchError) Unwrap() error { return e.Err }

func quotedPath(p string) string {
	return fmt.Sprintf("'%s'", p)
}
