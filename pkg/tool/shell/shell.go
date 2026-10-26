// Shell tools that run command lines through a pluggable runner with timeouts and background jobs
package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const (
	BashName   = "Bash"
	OutputName = "BashOutput"
	KillName   = "KillShell"
)

// One command line with where and how long it may run
type Command struct {
	// Call or job identifier the output belongs to
	ID string
	// Plain words the model gave for what the command does
	Description string
	Line        string
	Dir         string
	// KEY=VALUE pairs layered over the runner environment
	Env     []string
	Timeout time.Duration
	Stdout  io.Writer
	Stderr  io.Writer
}

// How a command ended, with the working directory it left behind
type Output struct {
	ExitCode int
	// Exit code of every stage of the last pipeline, one entry for a plain command
	Pipeline []int
	Dir      string
	TimedOut bool
	Duration time.Duration
}

// Structured outcome of a foreground Bash call
type Run struct {
	Command     string
	Description string
	Dir         string
	ExitCode    int
	Pipeline    []int
	TimedOut    bool
	Duration    time.Duration
	Stdout      string
	Stderr      string
}

// Settings shared by the shell tools
type Options struct {
	// Starting directory, the process working directory when empty
	Dir string
	// KEY=VALUE pairs every command sees
	Env []string
	// Run allowed a command the model names no timeout for, unlimited when zero
	Timeout time.Duration
	// Largest timeout the model may ask for, any when zero
	MaxTimeout time.Duration
	// Receives every background job start, output chunk and finish
	OnJob func(JobEvent)
}

const (
	// Bytes of output kept per foreground command
	maxOutput = 30000
	// Bytes of output kept per background job
	maxJobOutput = 1 << 20
	// Finished background jobs remembered before the oldest is forgotten
	maxJobs = 32
)

// Sentence on the time a command may take, from the configured run and cap
func (o Options) limits() string {
	switch {
	case o.Timeout > 0 && o.MaxTimeout > 0:
		return fmt.Sprintf("Commands run for %s unless a timeout is given and for at most %s.", o.Timeout, o.MaxTimeout)
	case o.Timeout > 0:
		return fmt.Sprintf("Commands run for %s unless a timeout is given.", o.Timeout)
	case o.MaxTimeout > 0:
		return fmt.Sprintf("Commands run until they end unless a timeout is given and for at most %s.", o.MaxTimeout)
	}
	return "Commands run until they end unless a timeout is given."
}

// Timeout the model asked for, held under the largest allowed when one is set
func (o Options) cap(asked time.Duration) time.Duration {
	if o.MaxTimeout > 0 {
		return min(asked, o.MaxTimeout)
	}
	return asked
}

// Reports whether the local shell is bash, the one shell whose pipeline stage codes are read back
func pipelineCodes() bool {
	return filepath.Base(Local{}.shell()) == "bash"
}

// Shell state shared by Bash, BashOutput and KillShell
type Shell struct {
	opts Options
	mu   sync.Mutex
	dir  string
	jobs *Jobs
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
	return &Shell{opts: o, dir: dir, jobs: newJobs(o.OnJob)}, nil
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

// Snapshot of one background job
func (s *Shell) Job(id string) (JobInfo, bool) {
	jb, ok := s.jobs.get(id)
	if !ok {
		return JobInfo{}, false
	}
	return s.jobs.info(jb), true
}

// Output of a job from an absolute offset without moving the cursor the model reads with
func (s *Shell) Tail(id string, from int64) (data []byte, next int64, dropped int64, ok bool) {
	jb, ok := s.jobs.get(id)
	if !ok {
		return nil, 0, 0, false
	}
	data, next, dropped = jb.out.since(from)
	return data, next, dropped, true
}

// Channel closed when the job finishes
func (s *Shell) Wait(id string) (<-chan struct{}, bool) {
	jb, ok := s.jobs.get(id)
	if !ok {
		return nil, false
	}
	return jb.done, true
}

// Stops a running job and waits for it to end
func (s *Shell) KillJob(ctx context.Context, id string) error {
	jb, ok := s.jobs.get(id)
	if !ok {
		return fmt.Errorf("shell: no background job %q", id)
	}
	jb.cancel()
	select {
	case <-jb.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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

// Writer that forwards every chunk to a progress reporter under a stream name
type reporting struct {
	ctx    context.Context
	stream string
}

func (r reporting) Write(p []byte) (int, error) {
	tool.Report(r.ctx, tool.Progress{Stream: r.stream, Text: string(p)})
	return len(p), nil
}

const bashSchema = `{"type":"object","properties":{
"command":{"type":"string","description":"Command line to run"},
"timeout":{"type":"integer","minimum":1,"description":"Milliseconds the command may run before it is killed"},
"description":{"type":"string","description":"Short plain words saying what the command does, shown to the user"},
"run_in_background":{"type":"boolean","description":"Start the command as a background job and return its id at once"}},
"required":["command"],"additionalProperties":false}`

type bash struct{ *Shell }

// Tool that runs a command line in the shell, foreground or as a background job
func (s *Shell) Bash() tool.Tool { return bash{s} }

func (b bash) Definition() protocol.Tool {
	pipes := "A pipeline exits with the code of its last command."
	if pipelineCodes() {
		pipes = "A pipeline exits with the code of its last command, and the code of every stage is listed when an earlier stage failed."
	}
	return protocol.Tool{
		Name: BashName,
		Description: fmt.Sprintf("Runs a command line in a shell and returns its output, exit code and whether it timed out. The working directory persists between calls. Stdin is closed. %s %s Output is cut to %d bytes keeping the start and the end. With run_in_background the call returns a job id for BashOutput and KillShell.",
			pipes, b.opts.limits(), maxOutput),
		Parameters: json.RawMessage(bashSchema),
	}
}

func (bash) Annotations() tool.Annotations {
	return tool.Annotations{Destructive: true, OpenWorld: true}
}

func (b bash) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Command     string `json:"command"`
		Timeout     int    `json:"timeout"`
		Description string `json:"description"`
		Background  bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if strings.TrimSpace(in.Command) == "" {
		return tool.Error("command is required"), nil
	}
	timeout := b.opts.Timeout
	if in.Timeout > 0 {
		timeout = b.opts.cap(time.Duration(in.Timeout) * time.Millisecond)
	}
	id := ""
	if call, ok := tool.CallFrom(ctx); ok {
		id = call.ID
	}
	if in.Background {
		if in.Timeout == 0 {
			timeout = b.opts.MaxTimeout
		}
		job := b.jobs.start(Command{ID: id, Description: in.Description, Line: in.Command, Dir: b.Dir(), Env: b.opts.Env, Timeout: timeout})
		return tool.Textf("Started background job %s, read it with BashOutput and stop it with KillShell", job.ID).With(b.jobs.info(job)), nil
	}
	stdout, stderr := newCapture(maxOutput), newCapture(maxOutput)
	cmd := Command{
		ID:          id,
		Description: in.Description,
		Line:        in.Command,
		Dir:         b.Dir(),
		Env:         b.opts.Env,
		Timeout:     timeout,
		Stdout:      io.MultiWriter(stdout, reporting{ctx, "stdout"}),
		Stderr:      io.MultiWriter(stderr, reporting{ctx, "stderr"}),
	}
	out, err := Local{}.Run(ctx, cmd)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if out.Dir != "" {
		b.mu.Lock()
		b.dir = out.Dir
		b.mu.Unlock()
	}
	res := render(stdout.String(), stderr.String(), out, timeout)
	return res.With(Run{Command: in.Command, Description: in.Description, Dir: out.Dir, ExitCode: out.ExitCode, Pipeline: out.Pipeline, TimedOut: out.TimedOut, Duration: out.Duration, Stdout: stdout.String(), Stderr: stderr.String()}), nil
}

// Formats stdout, stderr and the outcome, marking failures as error results
func render(stdout, stderr string, out Output, timeout time.Duration) tool.Result {
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(stdout, "\n"))
	if stderr = strings.TrimRight(stderr, "\n"); stderr != "" {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("[stderr]\n")
		sb.WriteString(stderr)
	}
	if sb.Len() == 0 {
		sb.WriteString("(no output)")
	}
	if out.TimedOut {
		fmt.Fprintf(&sb, "\n[timed out after %s]", timeout)
	} else if out.ExitCode != 0 {
		fmt.Fprintf(&sb, "\n[exit code %d]", out.ExitCode)
	}
	if earlierStageFailed(out.Pipeline) {
		fmt.Fprintf(&sb, "\n[pipeline exit codes %s, an earlier command failed]", joinInts(out.Pipeline))
	}
	res := tool.Text(sb.String())
	res.IsError = out.TimedOut || out.ExitCode != 0
	return res
}

// Reports whether a stage before the last one exited nonzero
func earlierStageFailed(codes []int) bool {
	for i, c := range codes {
		if i < len(codes)-1 && c != 0 {
			return true
		}
	}
	return false
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, " ")
}

// Runs command lines through the local shell in its own process group with stdin closed, bash when found on PATH
type Local struct{}

func (l Local) Run(ctx context.Context, cmd Command) (Output, error) {
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}
	dirFile, err := os.CreateTemp("", "icebath-cwd-*")
	if err != nil {
		return Output{}, err
	}
	dirFile.Close()
	defer os.Remove(dirFile.Name())
	shell := l.shell()
	c := exec.CommandContext(ctx, shell, l.args(shell, cmd.Line, dirFile.Name())...)
	c.Dir = cmd.Dir
	c.Env = append(os.Environ(), cmd.Env...)
	c.Stdout = orDiscard(cmd.Stdout)
	c.Stderr = orDiscard(cmd.Stderr)
	proc.Group(c)
	c.Cancel = func() error { return proc.Kill(c) }
	c.WaitDelay = 2 * time.Second
	start := time.Now()
	if err := c.Start(); err != nil {
		if ctx.Err() != nil {
			return Output{Dir: cmd.Dir, ExitCode: 137, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}, nil
		}
		return Output{}, err
	}
	waitErr := c.Wait()
	out := Output{Dir: cmd.Dir, Duration: time.Since(start), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	if data, err := os.ReadFile(dirFile.Name()); err == nil {
		lines := strings.SplitN(strings.TrimRight(string(data), "\n"), "\n", 2)
		if dir := strings.TrimSpace(lines[0]); dir != "" && filepath.IsAbs(dir) {
			out.Dir = dir
		}
		if len(lines) > 1 {
			out.Pipeline = parseCodes(lines[1])
		}
	}
	out.ExitCode = exitCode(c, waitErr, ctx.Err() != nil)
	if waitErr != nil && !isExit(waitErr) {
		return out, waitErr
	}
	return out, nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

func isExit(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

// Exit codes written as space separated integers, nothing for an unreadable line
func parseCodes(line string) []int {
	var out []int
	for _, f := range strings.Fields(line) {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}
