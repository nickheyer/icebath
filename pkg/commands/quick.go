package commands

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Time a gateway exec quick command may run before its whole process tree is stopped
var execQuickCommandTimeout = 30 * time.Second

// Time subprocess.run gives a classic CLI exec quick command before its shell is killed
var cliQuickCommandTimeout = 30 * time.Second

const (
	stopExecTreeReap = 5 * time.Second
	sigintWaitSecs   = 250 * time.Millisecond
	streamPauseBytes = 2 * 65536
)

// CLIQuickStyle is how the classic CLI prints one quick command reply
type CLIQuickStyle int

const (
	// CLIQuickOutput is redacted command output printed through the ANSI to Rich text conversion
	CLIQuickOutput CLIQuickStyle = iota
	// CLIQuickDim is the no output notice printed dim
	CLIQuickDim
	// CLIQuickFailure is a timeout or error notice printed bold red
	CLIQuickFailure
)

// CLIQuickReply is the text one classic CLI exec quick command prints and its style
type CLIQuickReply struct {
	Text  string
	Style CLIQuickStyle
}

// Runs a gateway exec quick command with a sanitized env and redacted output, its tree stopped on timeout or cancellation
func RunGatewayExecQuickCommand(ctx context.Context, env *tools.Env, execCmd any, cwd string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	childEnv, err := quickChildEnv(ctx, env)
	if err != nil {
		return gatewayQuickError(ctx, err.Error()), nil
	}
	var command string
	switch c := execCmd.(type) {
	case string:
		command = c
	case []byte:
		command = string(c)
	default:
		return gatewayQuickError(ctx, "cmd must be a string"), nil
	}
	q, err := startQuickProc(func() (quickProgram, error) { return gatewayProgram(command, isBytes(execCmd)) }, cwd, childEnv)
	if err != nil {
		return gatewayQuickError(ctx, err.Error()), nil
	}
	if ctx.Err() != nil {
		q.stdout.close()
		q.stderr.close()
		if err := killShell(q); err != nil && !isPermissionOrLookup(err) {
			return gatewayQuickError(ctx, pyOSErrorText(err)), nil
		}
		<-q.exited
		return "", ctx.Err()
	}
	timer := time.NewTimer(execQuickCommandTimeout)
	defer timer.Stop()
	finished := q.finished()
	select {
	case <-finished:
	case <-timer.C:
		if !closed(finished) {
			stopExecTree(q)
			return i18n.T(ctx, "gateway.quick_command.timed_out", "", nil), nil
		}
	case <-ctx.Done():
		if !closed(finished) {
			stopExecTree(q)
			return "", ctx.Err()
		}
	}
	out := q.stdout.bytes()
	if len(out) == 0 {
		out = q.stderr.bytes()
	}
	text, err := pystr.DecodeUTF8(out)
	if err != nil {
		return gatewayQuickError(ctx, err.Error()), nil
	}
	output := pystr.Strip(text)
	if output != "" {
		output = tools.RedactSensitiveTextWith(env, output, tools.RedactOptions{})
	}
	if output == "" {
		return i18n.T(ctx, "gateway.quick_command.no_output", "", nil), nil
	}
	return output, nil
}

// Runs a classic CLI exec quick command like subprocess.run with shell, capture, utf-8 replace text and a 30 second timeout
func RunCLIExecQuickCommand(ctx context.Context, env *tools.Env, command any) (CLIQuickReply, error) {
	if err := ctx.Err(); err != nil {
		return CLIQuickReply{}, err
	}
	childEnv, err := quickChildEnv(ctx, env)
	if err != nil {
		return cliQuickError(ctx, err.Error()), nil
	}
	q, err := startQuickProc(func() (quickProgram, error) { return cliProgram(command) }, "", childEnv)
	if err != nil {
		return cliQuickError(ctx, err.Error()), nil
	}
	deadline := time.Now().Add(cliQuickCommandTimeout)
	timer := time.NewTimer(cliQuickCommandTimeout)
	defer timer.Stop()
	finished := q.finished()
	select {
	case <-finished:
	case <-timer.C:
		if !closed(finished) {
			if err := killShell(q); err != nil {
				q.stdout.close()
				q.stderr.close()
				<-q.exited
				return cliQuickError(ctx, pyOSErrorText(err)), nil
			}
			reapAfterTimeoutKill(q)
			return CLIQuickReply{Text: i18n.T(ctx, "cli.quick.timed_out", "", nil), Style: CLIQuickFailure}, nil
		}
	case <-ctx.Done():
		if !closed(finished) {
			wait := min(sigintWaitSecs, max(time.Until(deadline), 0))
			select {
			case <-q.exited:
			case <-time.After(wait):
			}
			err := killShell(q)
			q.stdout.close()
			q.stderr.close()
			if err != nil {
				<-q.exited
				return cliQuickError(ctx, pyOSErrorText(err)), nil
			}
			return CLIQuickReply{}, ctx.Err()
		}
	}
	output := pystr.Strip(cliText(q.stdout.bytes()))
	if output == "" {
		output = pystr.Strip(cliText(q.stderr.bytes()))
	}
	if output == "" {
		return CLIQuickReply{Text: i18n.T(ctx, "cli.quick.no_output", "", nil), Style: CLIQuickDim}, nil
	}
	return CLIQuickReply{Text: tools.RedactSensitiveTextWith(env, output, tools.RedactOptions{}), Style: CLIQuickOutput}, nil
}

// Child env of build_subprocess_env with its defaults, secrets scrubbed and the profile home bridged
func quickChildEnv(ctx context.Context, env *tools.Env) (*tools.ChildEnv, error) {
	return tools.BuildSubprocessEnvOrdered(ctx, env, tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
}

func gatewayQuickError(ctx context.Context, msg string) string {
	return i18n.T(ctx, "gateway.quick_command.error", "", map[string]any{"error": msg})
}

func cliQuickError(ctx context.Context, msg string) CLIQuickReply {
	return CLIQuickReply{Text: i18n.T(ctx, "cli.quick.error", "", map[string]any{"error": msg}), Style: CLIQuickFailure}
}

// Text mode stream, utf-8 with errors replace and universal newlines
func cliText(raw []byte) string {
	return pystr.TranslateNewlines(pystr.DecodeUTF8Replace(raw))
}

func isBytes(v any) bool {
	_, ok := v.([]byte)
	return ok
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Python str of the OSError a signal or terminate call raised
func pyOSErrorText(err error) string {
	if text, ok := pypath.ErrnoText(err); ok {
		return text
	}
	return pypath.OSErrorText(err)
}

// Whether the error is the ProcessLookupError or PermissionError asyncio suppresses when it closes a transport
func isPermissionOrLookup(err error) bool {
	switch pypath.OSErrorClass(err) {
	case "PermissionError", "ProcessLookupError":
		return true
	}
	return false
}

// Kills what the shell started, its group on POSIX, then reaps the shell for at most five seconds
func stopExecTree(q *quickProc) {
	q.stdout.detach()
	q.stderr.detach()
	if !closed(q.exited) {
		provider.KillProcessTree(q.proc.Pid, 0)
	}
	killQuickGroup(q.proc.Pid)
	select {
	case <-q.exited:
	case <-time.After(stopExecTreeReap):
	}
}

// Program, argv and process attributes one quick command shell starts with
type quickProgram struct {
	path string
	argv []string
	attr *syscall.SysProcAttr
}

// A started quick command shell with both captured streams
type quickProc struct {
	proc           *os.Process
	exited         chan struct{}
	stdout, stderr *quickStream
}

// Opens the pipes, converts the args, then spawns with the inherited stdin, every error text being the Python one
func startQuickProc(build func() (quickProgram, error), cwd string, env *tools.ChildEnv) (*quickProc, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, pyError(pyOSErrorText(err))
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, pyError(pyOSErrorText(err))
	}
	prog, err := build()
	envList := quickEnvList(env)
	if err == nil {
		prog, err = finishProgram(prog, cwd, envList)
	}
	if err != nil {
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		return nil, err
	}
	stdin, release := quickStdin()
	proc, err := os.StartProcess(prog.path, prog.argv, &os.ProcAttr{Dir: cwd, Env: envList, Files: []*os.File{stdin, outW, errW}, Sys: prog.attr})
	release()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, pyError(spawnErrorText(err, cwd))
	}
	q := &quickProc{proc: proc, exited: make(chan struct{}), stdout: newQuickStream(outR), stderr: newQuickStream(errR)}
	go func() {
		_, _ = proc.Wait()
		close(q.exited)
	}()
	return q, nil
}

// Closed once both streams reached EOF and the shell was reaped
func (q *quickProc) finished() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		<-q.stdout.done
		<-q.stderr.done
		<-q.exited
		close(ch)
	}()
	return ch
}

// One captured stream, read to EOF on its own goroutine
type quickStream struct {
	f        *os.File
	done     chan struct{}
	mu       sync.Mutex
	data     []byte
	detached bool
	unread   int
}

func newQuickStream(f *os.File) *quickStream {
	s := &quickStream{f: f, done: make(chan struct{})}
	go s.read()
	return s
}

// Reads to EOF, or after a detach until more than the asyncio pause threshold arrived unread, then closes
func (s *quickStream) read() {
	defer close(s.done)
	defer s.f.Close()
	buf := make([]byte, 32768)
	for {
		n, err := s.f.Read(buf)
		if n > 0 && s.keep(buf[:n]) {
			return
		}
		if err != nil {
			return
		}
	}
}

// Stores a chunk, or counts it once detached and reports whether reading pauses
func (s *quickStream) keep(chunk []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.detached {
		s.data = append(s.data, chunk...)
		return false
	}
	s.unread += len(chunk)
	return s.unread > streamPauseBytes
}

func (s *quickStream) detach() {
	s.mu.Lock()
	s.detached = true
	s.mu.Unlock()
}

func (s *quickStream) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

func (s *quickStream) close() { _ = s.f.Close() }

// Exception text standing for a Python error raised before the shell starts
type pyError string

func (e pyError) Error() string { return string(e) }

// Items list() yields for a config value that is neither str nor bytes, mapping keys in file order
func pyListItems(v any) ([]any, error) {
	if m := config.AsMap(v); m != nil {
		keys := config.MappingKeys(m)
		items := make([]any, len(keys))
		for i, k := range keys {
			items[i] = k
		}
		return items, nil
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice {
		items := make([]any, rv.Len())
		for i := range items {
			items[i] = rv.Index(i).Interface()
		}
		return items, nil
	}
	return nil, pyError(fmt.Sprintf("'%s' object is not iterable", config.PyTypeName(v)))
}

// TypeError os.fsencode and os.fsdecode raise for an argument that is neither str nor bytes
func notPathLike(v any) error {
	return pyError("expected str, bytes or os.PathLike object, not " + config.PyTypeName(v))
}
