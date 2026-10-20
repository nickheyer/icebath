// Shell tools that run command lines through a pluggable runner with timeouts and background jobs
package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tool"
)

const (
	BashName   = "Bash"
	OutputName = "BashOutput"
	KillName   = "KillShell"
)

// One command line with where and how long it may run
type Command struct {
	Line string
	Dir  string
	// KEY=VALUE pairs layered over the runner environment
	Env     []string
	Timeout time.Duration
	Stdout  io.Writer
	Stderr  io.Writer
}

// How a command ended, with the working directory it left behind
type Output struct {
	ExitCode int
	Dir      string
	TimedOut bool
	Duration time.Duration
}

// Executes command lines somewhere, locally by default and over any transport by substitution
type Runner interface {
	Run(ctx context.Context, cmd Command) (Output, error)
}

// Settings shared by the shell tools
type Options struct {
	// Local when nil
	Runner Runner
	// Starting directory, the process working directory when empty
	Dir string
	// KEY=VALUE pairs every command sees
	Env []string
	// Default per command, two minutes when zero
	Timeout time.Duration
	// Largest timeout the model may ask for, ten minutes when zero
	MaxTimeout time.Duration
	// Bytes of output kept per foreground command, 30000 when zero
	MaxOutput int
	// Bytes of output kept per background job, one MiB when zero
	MaxJobOutput int
}

func (o Options) timeout() time.Duration    { return orDur(o.Timeout, 2*time.Minute) }
func (o Options) maxTimeout() time.Duration { return orDur(o.MaxTimeout, 10*time.Minute) }
func (o Options) maxOutput() int            { return orInt(o.MaxOutput, 30000) }
func (o Options) maxJobOutput() int         { return orInt(o.MaxJobOutput, 1<<20) }

func orDur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func orInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// Shell state shared by Bash, BashOutput and KillShell
type Shell struct {
	opts   Options
	runner Runner
	mu     sync.Mutex
	dir    string
	jobs   *Jobs
}

// Builds a shell whose commands start in the configured directory
func New(o Options) (*Shell, error) {
	dir := o.Dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("shell: %s is not a directory", dir)
	}
	runner := o.Runner
	if runner == nil {
		runner = Local{}
	}
	return &Shell{opts: o, runner: runner, dir: dir, jobs: newJobs(o.maxJobOutput())}, nil
}

// Every shell tool sharing this state
func (s *Shell) Tools() []tool.Tool {
	return []tool.Tool{s.Bash(), s.Output(), s.Kill()}
}

// Working directory the next command starts in
func (s *Shell) Dir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dir
}

// Moves the working directory of the next command
func (s *Shell) SetDir(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("shell: %s is not a directory", dir)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dir = dir
	return nil
}

// Background jobs started so far, oldest first
func (s *Shell) Jobs() []JobInfo { return s.jobs.list() }

// Stops every running background job and forgets all of them
func (s *Shell) Close() { s.jobs.close() }

// Writer keeping the first and last halves of a byte budget with a count of what fell between
type capture struct {
	mu      sync.Mutex
	half    int
	head    []byte
	tail    []byte
	dropped int
}

func newCapture(max int) *capture { return &capture{half: max / 2} }

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if room := c.half - len(c.head); room > 0 {
		take := min(room, len(p))
		c.head = append(c.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	c.tail = append(c.tail, p...)
	if over := len(c.tail) - c.half; over > 0 {
		c.dropped += over
		c.tail = append([]byte(nil), c.tail[over:]...)
	}
	return n, nil
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == 0 {
		return string(c.head) + string(c.tail)
	}
	return fmt.Sprintf("%s\n[%d bytes omitted]\n%s", c.head, c.dropped, c.tail)
}
