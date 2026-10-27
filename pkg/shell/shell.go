// Shell interpreting command lines inside the process, starting only the programs a line names
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// What prompts and tool descriptions call the shell
const Name = "icebath"

// Sentence telling a model what runs its command lines
const Dialect = "icebath's own shell, interpreting every command line in process with no other shell underneath"

const (
	// Time a killed process gets to release its output pipes before they are abandoned
	killGrace = 2 * time.Second
	// Bytes of output kept while no line is running, from processes left in the background
	maxIdle = 64 << 10
	// Exit code reported when the context ended a line early
	killedCode = 137
)

// Settings a shell starts with
type Options struct {
	// Starting directory, the process working directory when empty
	Dir string
	// KEY=VALUE pairs layered over the environment of the process
	Env []string
}

// How a command line ended and the working directory it left behind
type Result struct {
	ExitCode int
	// Exit code of every stage of the last pipeline the line ran, empty when it ran none
	Pipeline []int
	Dir      string
	// The line ended the shell with exit, so the next line starts a fresh one in the same directory
	Exited bool
	// The context ended before the line did and everything it started was killed
	Killed bool
}

// One persistent interpreter whose variables, functions, aliases and directory carry over between lines
type Shell struct {
	// Held for the length of every line so the interpreter sees one at a time
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	env    expand.Environ
	runner *interp.Runner
	out    *tap
	errs   *tap
	dirMu  sync.Mutex
	dir    string
}

// Starts a shell in the configured directory
func New(o Options) (*Shell, error) {
	dir, err := startDir(o.Dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Shell{ctx: ctx, cancel: cancel, env: expand.ListEnviron(append(os.Environ(), o.Env...)...), out: &tap{}, errs: &tap{}, dir: dir}
	r, err := s.newRunner(dir, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	s.runner = r
	return s, nil
}

// Absolute path of the directory, the process working directory when empty, refusing anything but a directory
func startDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("shell: %s is not a directory", abs)
	}
	return abs, nil
}

// Fresh interpreter in dir on the base environment, reading stdin when one is given
func (s *Shell) newRunner(dir string, stdin io.Reader) (*interp.Runner, error) {
	r, err := interp.New(interp.Dir(dir), interp.Env(s.env), interp.StdIO(stdin, s.out, s.errs), interp.Interactive(true), interp.ExecHandlers(handler), interp.OpenHandler(open))
	if err != nil {
		return nil, fmt.Errorf("shell: %w", err)
	}
	return r, nil
}

// Working directory the next line starts in
func (s *Shell) Dir() string {
	s.dirMu.Lock()
	defer s.dirMu.Unlock()
	return s.dir
}

func (s *Shell) setDir(dir string) {
	s.dirMu.Lock()
	defer s.dirMu.Unlock()
	s.dir = dir
}

// Copy of the shell as it stands, running lines beside it on its own state and dying with it
func (s *Shell) Fork() (*Shell, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithCancel(s.ctx)
	f := &Shell{ctx: ctx, cancel: cancel, env: s.env, out: &tap{}, errs: &tap{}, dir: s.Dir()}
	if s.runner == nil {
		r, err := f.newRunner(f.dir, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		f.runner = r
		return f, nil
	}
	f.runner = s.runner.Subshell()
	if err := interp.StdIO(nil, f.out, f.errs)(f.runner); err != nil {
		cancel()
		return nil, fmt.Errorf("shell: %w", err)
	}
	return f, nil
}

// Runs line to completion with stdin closed, killing everything it started when ctx ends first
func (s *Shell) Run(ctx context.Context, line string, stdout, stderr io.Writer) Result {
	return s.run(ctx, line, nil, stdout, stderr)
}

func (s *Shell) run(ctx context.Context, line string, stdin io.Reader, stdout, stderr io.Writer) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.attach(stdout)
	s.errs.attach(stderr)
	defer s.out.detach()
	defer s.errs.detach()
	prog, err := Parse(line)
	if err != nil {
		fmt.Fprintf(s.errs, "%s: %v\n", Name, err)
		return Result{ExitCode: 2, Dir: s.Dir()}
	}
	if s.runner == nil {
		r, err := s.newRunner(s.Dir(), stdin)
		if err != nil {
			fmt.Fprintf(s.errs, "%v\n", err)
			return Result{ExitCode: 1, Dir: s.Dir()}
		}
		s.runner = r
	} else if stdin != nil {
		if err := interp.StdIO(stdin, s.out, s.errs)(s.runner); err != nil {
			fmt.Fprintf(s.errs, "%s: %v\n", Name, err)
			return Result{ExitCode: 1, Dir: s.Dir()}
		}
	}
	st := rewrite(prog)
	runCtx, cancel := context.WithCancel(context.WithValue(context.WithValue(s.ctx, stagesKey{}, st), outKey{}, s.out))
	stop := context.AfterFunc(ctx, cancel)
	err = s.runner.Run(runCtx, prog)
	stop()
	res := Result{Dir: s.runner.Dir, Pipeline: st.last()}
	res.ExitCode, res.Killed = s.status(err, runCtx)
	if s.runner.Exited() && !res.Killed {
		res.Exited = true
		s.runner = nil
	}
	s.setDir(res.Dir)
	return res
}

// Exit code of a finished line from what the interpreter returned, and whether the context ended it
func (s *Shell) status(err error, ctx context.Context) (int, bool) {
	if code, ok := interp.IsExitStatus(err); ok {
		return int(code), false
	}
	if err == nil {
		return 0, false
	}
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return killedCode, true
	}
	fmt.Fprintf(s.errs, "%s: %v\n", Name, err)
	return 1, false
}

// Kills everything the shell started that is still running and releases it
func (s *Shell) Close() {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runner = nil
}

// Program parsed from a command line
func Parse(line string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(line), "")
}

// Writer feeding the line in progress and holding bytes that arrive between lines for the next one
type tap struct {
	mu   sync.Mutex
	w    io.Writer
	idle []byte
}

func (t *tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w != nil {
		return t.w.Write(p)
	}
	t.idle = append(t.idle, p...)
	if over := len(t.idle) - maxIdle; over > 0 {
		t.idle = append([]byte(nil), t.idle[over:]...)
	}
	return len(p), nil
}

// Routes the stream to w after handing it what arrived while idle
func (t *tap) attach(w io.Writer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if w == nil {
		w = io.Discard
	}
	if len(t.idle) > 0 {
		w.Write(t.idle)
		t.idle = nil
	}
	t.w = w
}

func (t *tap) detach() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.w = nil
}
