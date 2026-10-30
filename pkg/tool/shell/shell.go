package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/shell"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Shell tools running command lines in the harness's own persistent shell with timeouts and background jobs

const (
	ShellName  = "Shell"
	OutputName = "ShellOutput"
	KillName   = "KillShell"
)

// Structured outcome of a foreground Shell call
type Run struct {
	Command     string
	Description string
	Dir         string
	ExitCode    int
	Pipeline    []int
	TimedOut    bool
	// The command ended the shell with exit and the next call starts a fresh one
	Exited   bool
	Duration time.Duration
	Stdout   string
	Stderr   string
}

// Settings shared by the shell tools
type Options struct {
	// Starting directory, the process working directory when empty
	Dir string
	// KEY=VALUE pairs layered over the environment the shell starts with
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
	maxOutput = 20000
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

// How a command ended, with the working directory it left behind
type Output struct {
	ExitCode int
	// Exit code of every stage of the last pipeline the command ran, empty when it ran none
	Pipeline []int
	// Command name of every stage of that pipeline
	Commands []string
	Dir      string
	TimedOut bool
	// The command ended the shell with exit
	Exited   bool
	Duration time.Duration
}

// Shell state shared by Shell, ShellOutput and KillShell
type Shell struct {
	opts Options
	sh   *shell.Shell
	jobs *Jobs
}

// Starts the shell in the configured directory
func New(o Options) (*Shell, error) {
	sh, err := shell.New(shell.Options{Dir: o.Dir, Env: o.Env})
	if err != nil {
		return nil, err
	}
	s := &Shell{opts: o, sh: sh}
	s.jobs = newJobs(s, o.OnJob)
	return s, nil
}

// Every shell tool sharing this state
func (s *Shell) Tools() []tool.Tool {
	return []tool.Tool{s.Command(), s.Output(), s.Kill()}
}

// Working directory the next command starts in
func (s *Shell) Dir() string { return s.sh.Dir() }

// Runs line in the shell, killing everything it started when the timeout or ctx ends first
func (s *Shell) run(ctx context.Context, line string, stdout, stderr io.Writer, timeout time.Duration) Output {
	return execute(ctx, s.sh, line, stdout, stderr, timeout)
}

// RunWith runs a command line with explicit writers and timeout on the shell itself,
// so it waits behind whatever the agent has running. Extensions that must not queue
// behind the agent use Fork instead
func (s *Shell) RunWith(ctx context.Context, line string, stdout, stderr io.Writer, timeout time.Duration) Output {
	return execute(ctx, s.sh, line, stdout, stderr, timeout)
}

// Fork runs a command line beside the agent's shell, on a copy of it as it stands with
// the same directory and environment, so a long scan never queues behind the agent's own
// Shell calls and the agent never waits behind it
func (s *Shell) Fork(ctx context.Context, line string, stdout, stderr io.Writer, timeout time.Duration) Output {
	sh, err := s.sh.Fork()
	if err != nil {
		return Output{ExitCode: 1}
	}
	defer sh.Close()
	return execute(ctx, sh, line, stdout, stderr, timeout)
}

// StartJob runs line as a background job on a copy of the shell as it stands, the way
// the Shell tool does, and reports its id so ShellOutput, KillShell and Wait take it
func (s *Shell) StartJob(callID, description, line string, timeout time.Duration) (string, error) {
	jb, err := s.jobs.start(callID, description, line, timeout)
	if err != nil {
		return "", err
	}
	return jb.ID, nil
}

// Runs line on sh under the timeout and reports how it ended
func execute(ctx context.Context, sh *shell.Shell, line string, stdout, stderr io.Writer, timeout time.Duration) Output {
	rctx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		rctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	start := time.Now()
	res := sh.Run(rctx, line, stdout, stderr)
	timedOut := res.Killed && errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	return Output{ExitCode: res.ExitCode, Pipeline: res.Pipeline, Commands: res.Commands, Dir: res.Dir, TimedOut: timedOut, Exited: res.Exited, Duration: time.Since(start)}
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

// Stops every background job, then the shell with everything it started
func (s *Shell) Close() {
	s.jobs.close()
	s.sh.Close()
}

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
	return fmt.Sprintf("%s\n[%d of %d bytes omitted from the middle, narrow the command to see them]\n%s", c.head, c.dropped, len(c.head)+c.dropped+len(c.tail), c.tail)
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

const commandSchema = `{"type":"object","properties":{
"command":{"type":"string","description":"Command line to run"},
"timeout":{"type":"integer","minimum":1,"description":"Milliseconds the command may run before it is killed"},
"description":{"type":"string","description":"Short plain words saying what the command does, shown to the user"},
"run_in_background":{"type":"boolean","description":"Start the command as a background job and return its id at once"}},
"required":["command"],"additionalProperties":false}`

type command struct{ *Shell }

// Tool that runs a command line in the shell, foreground or as a background job
func (s *Shell) Command() tool.Tool { return command{s} }

func (b command) Definition() protocol.Tool {
	return protocol.Tool{
		Name: ShellName,
		Description: fmt.Sprintf("Runs a command line in %s, one persistent shell shared by every call, so variables, functions, aliases and the working directory carry over between calls. The builtins %s run inside the harness and every other command starts the program of that name found on PATH. Stdin is closed. A pipeline exits with the code of its last command, and the code of every stage is listed when an earlier stage failed, exit 1 from grep, diff and their kin counting as no match rather than failure. A glob that matches nothing reaches the program as written. %s Output is cut to %d bytes keeping the start and the end. A timeout kills every process the command started. With run_in_background the call returns a job id for ShellOutput and KillShell, the job running on a copy of the shell state at that moment.",
			shell.Dialect, strings.Join(shell.Builtins, " "), b.opts.limits(), maxOutput),
		Parameters: json.RawMessage(commandSchema),
	}
}

func (command) Annotations() tool.Annotations {
	return tool.Annotations{Destructive: true, OpenWorld: true}
}

func (b command) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
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
		job, err := b.jobs.start(id, in.Description, in.Command, timeout)
		if err != nil {
			return tool.Error(err.Error()), nil
		}
		return tool.Textf("Started background job %s, read it with ShellOutput and stop it with KillShell", job.ID).With(b.jobs.info(job)), nil
	}
	stdout, stderr := newCapture(maxOutput), newCapture(maxOutput)
	out := b.run(ctx, in.Command, io.MultiWriter(stdout, reporting{ctx, "stdout"}), io.MultiWriter(stderr, reporting{ctx, "stderr"}), timeout)
	res := render(stdout.String(), stderr.String(), out, timeout)
	return res.With(Run{Command: in.Command, Description: in.Description, Dir: out.Dir, ExitCode: out.ExitCode, Pipeline: out.Pipeline, TimedOut: out.TimedOut, Exited: out.Exited, Duration: out.Duration, Stdout: stdout.String(), Stderr: stderr.String()}), nil
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
	switch {
	case out.TimedOut:
		fmt.Fprintf(&sb, "\n[timed out after %s, every process it started was killed]", timeout)
	case out.Exited:
		fmt.Fprintf(&sb, "\n[the shell exited with code %d, the next command starts a new one]", out.ExitCode)
	case out.ExitCode != 0:
		fmt.Fprintf(&sb, "\n[exit code %d]", out.ExitCode)
	}
	if earlierStageFailed(out.Pipeline, out.Commands) {
		fmt.Fprintf(&sb, "\n[pipeline exit codes %s, an earlier command failed]", joinInts(out.Pipeline))
	}
	res := tool.Text(sb.String())
	res.IsError = out.TimedOut || out.ExitCode != 0
	return res
}

// Reports whether a stage before the last one exited nonzero
// Programs whose exit code 1 answers no match or no difference rather than reporting a failure
var informational = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true, "diff": true, "cmp": true, "test": true, "[": true}

// Reports a stage before the last that failed, exit 1 from a search or compare being a plain answer
func earlierStageFailed(codes []int, names []string) bool {
	for i, c := range codes {
		if i == len(codes)-1 || c == 0 {
			continue
		}
		if c == 1 && i < len(names) && informational[filepath.Base(names[i])] {
			continue
		}
		return true
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
