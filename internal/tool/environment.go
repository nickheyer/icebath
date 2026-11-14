package tool

import (
	"context"
	"io"
	"time"
)

type Environment interface {
	Kind() string
	Local() bool
	HostFilesystem() bool
	Persistent() bool
	Cwd() string
	TempDir() string
	Execute(ctx context.Context, command string, o ExecOptions) (ExecResult, error)
	Start(ctx context.Context, argv []string, o StartOptions) (Process, error)
	FetchFile(ctx context.Context, remote, local string, maxBytes int64) error
	Realpath(ctx context.Context, path string) (string, bool, error)
	Forward(ctx context.Context, remoteHost string, remotePort int) (Forward, error)
	Sync(ctx context.Context, force bool) error
	SyncBack(ctx context.Context) error
}

type CachePaths interface {
	ToAgentVisibleCachePath(ctx context.Context, hostPath, containerBase string) (string, error)
}

type ExecOptions struct {
	Cwd                       string
	Timeout                   time.Duration
	Stdin                     *string
	RewriteCompoundBackground bool
	BoundedCapture            bool
	Yield                     *Yield
	OnYield                   func(p Process, partial string) (ExecResult, bool)
	Activity                  func(label string)
}

type ExecResult struct {
	Output           string
	ExitCode         int
	TimedOut         bool
	Interrupted      bool
	Recreated        bool
	Cwd              string
	CwdObserved      bool
	PipelineStatus   []int
	SpillPath        string
	OutputTotalChars int
	YieldedSession   string
	YieldedPID       int
}

type StartOptions struct {
	Env  map[string]string
	Cwd  string
	User string
	PTY  bool
}

type Process interface {
	PID() int
	Stdin() io.WriteCloser
	Output() io.Reader
	Wait(ctx context.Context) (int, error)
	Exited() (int, bool)
	Kill() error
}

type Forward interface {
	Addr() string
	Failures() int
	Close() error
}

type EnvSource interface {
	Acquire(ctx context.Context, c *Call) (Environment, error)
	Active(ctx context.Context, taskID string) (Environment, bool, error)
	Kind(ctx context.Context, taskID string) string
}
