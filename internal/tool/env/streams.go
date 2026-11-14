package env

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/shell"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	runInDefaultTimeout = 30 * time.Second
	streamWaitDelay     = time.Second
	relayExitWait       = 2 * time.Second
	relayChunk          = 65536
)

const tcpRelayScript = `exec 3<>"/dev/tcp/$1/$2" || exit 1
cat <&0 >&3 &
cat <&3 &
wait -n
kill $(jobs -p) 2>/dev/null
exit 0`

type ExecPrefixer interface {
	ExecPrefix(user string, interactive bool) ([]string, error)
	ShellJoined() bool
}

func (s *Shell) execPrefix(user string, interactive bool) ([]string, bool) {
	ep, ok := s.b.(ExecPrefixer)
	if !ok {
		return nil, false
	}
	prefix, err := ep.ExecPrefix(user, interactive)
	if err != nil {
		slog.Warn(fmt.Sprintf("%s exec prefix failed; treating the backend as unable to stream", s.b.Kind()), "err", err)
		return nil, false
	}
	return prefix, len(prefix) > 0
}

func (s *Shell) SupportsStreams() bool {
	_, ok := s.execPrefix("", true)
	return ok
}

func remoteArgv(prefix, argv []string, env map[string]string, cwd string, shellJoined bool) []string {
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	exports := make([]string, 0, len(names))
	for _, k := range names {
		exports = append(exports, "export "+k+"="+shell.Quote(env[k])+";")
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shell.Quote(a)
	}
	script := strings.Join(exports, " ") + " exec " + strings.Join(quoted, " ")
	if cwd != "" {
		script = strings.Join(exports, " ") + " cd " + shell.Quote(cwd) + " && exec " + strings.Join(quoted, " ")
	}
	out := append([]string{}, prefix...)
	if shellJoined {
		return append(out, shell.Join([]string{"bash", "-c", script}))
	}
	return append(out, "bash", "-c", script)
}

func (s *Shell) RemoteCommand(argv []string, env map[string]string, cwd, user string, interactive bool) ([]string, bool) {
	prefix, ok := s.execPrefix(user, interactive)
	if !ok {
		return nil, false
	}
	ep := s.b.(ExecPrefixer)
	return remoteArgv(prefix, argv, env, cwd, ep.ShellJoined()), true
}

type streamProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *os.File
	done  chan struct{}
	code  int
}

func startStream(cmd *exec.Cmd) (*streamProcess, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, nil
	proc.Hide(cmd)
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
		return nil, err
	}
	_ = inR.Close()
	_ = outW.Close()
	p := &streamProcess{cmd: cmd, stdin: inW, out: outR, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		p.code = proc.ExitOf(cmd.ProcessState).Status()
		close(p.done)
	}()
	return p, nil
}

func (p *streamProcess) PID() int { return p.cmd.Process.Pid }

func (p *streamProcess) Stdin() io.WriteCloser { return p.stdin }

func (p *streamProcess) Output() io.Reader { return p.out }

func (p *streamProcess) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.code, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (p *streamProcess) Exited() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

func (p *streamProcess) Kill() error {
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (p *streamProcess) Close() error {
	_ = p.stdin.Close()
	return p.out.Close()
}

func (s *Shell) Start(ctx context.Context, argv []string, o tool.StartOptions) (tool.Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command to start")
	}
	if st, ok := s.b.(Starter); ok {
		return st.Start(ctx, argv, o)
	}
	if o.PTY {
		return nil, &UnsupportedError{Kind: s.b.Kind(), Op: "pseudo-terminal streams"}
	}
	command, ok := s.RemoteCommand(argv, o.Env, o.Cwd, o.User, true)
	if !ok {
		return nil, &UnsupportedError{Kind: s.b.Kind(), Op: "long-lived stdio streams"}
	}
	p, err := startStream(exec.Command(command[0], command[1:]...))
	if err != nil {
		return nil, err
	}
	return p, nil
}

type RunInOptions struct {
	Env     map[string]string
	User    string
	Timeout time.Duration
	Stdin   []byte
}

type RunInResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

type RunTimeoutError struct {
	Argv    []string
	Timeout time.Duration
}

func (e *RunTimeoutError) Error() string {
	return fmt.Sprintf("Command '%s' timed out after %s seconds", strings.Join(e.Argv, " "), formatSeconds(e.Timeout))
}

func (e *RunTimeoutError) Unwrap() error { return context.DeadlineExceeded }

type hostRunner interface {
	hostCommand(ctx context.Context, argv []string, env map[string]string, cwd string) (*exec.Cmd, error)
}

func (s *Shell) RunIn(ctx context.Context, argv []string, o RunInOptions) (RunInResult, error) {
	if len(argv) == 0 {
		return RunInResult{}, errors.New("no command to run")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = runInDefaultTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if hr, ok := s.b.(hostRunner); ok {
		c, err := hr.hostCommand(rctx, argv, o.Env, "")
		if err != nil {
			return RunInResult{}, err
		}
		cmd = c
	} else {
		command, ok := s.RemoteCommand(argv, o.Env, "", o.User, o.Stdin != nil)
		if !ok {
			return RunInResult{}, &UnsupportedError{Kind: s.b.Kind(), Op: "executing argv"}
		}
		cmd = exec.CommandContext(rctx, command[0], command[1:]...)
		proc.Hide(cmd)
	}
	if o.Stdin != nil {
		cmd.Stdin = bytes.NewReader(o.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = streamWaitDelay
	err := cmd.Run()
	if rctx.Err() != nil {
		if ctx.Err() != nil {
			return RunInResult{}, ctx.Err()
		}
		return RunInResult{}, &RunTimeoutError{Argv: argv, Timeout: timeout}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return RunInResult{}, err
		}
	}
	return RunInResult{ExitCode: proc.ExitOf(cmd.ProcessState).Status(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

type forwardKey struct {
	host string
	port int
}

type portForward struct {
	s        *Shell
	host     string
	port     int
	ln       net.Listener
	failures atomic.Int64
	closed   atomic.Bool
}

func (s *Shell) Forward(ctx context.Context, remoteHost string, remotePort int) (tool.Forward, error) {
	if remoteHost == "" {
		remoteHost = "127.0.0.1"
	}
	if fw, ok := s.b.(Forwarder); ok {
		return fw.Forward(ctx, remoteHost, remotePort)
	}
	_, local := s.b.(hostRunner)
	if _, starts := s.b.(Starter); !local && !starts && !s.SupportsStreams() {
		return nil, &UnsupportedError{Kind: s.b.Kind(), Op: "port forwarding"}
	}
	key := forwardKey{remoteHost, remotePort}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forwards == nil {
		s.forwards = map[forwardKey]*portForward{}
	}
	if f := s.forwards[key]; f != nil && !f.closed.Load() {
		return f, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &portForward{s: s, host: remoteHost, port: remotePort, ln: ln}
	s.forwards[key] = f
	go f.accept(local)
	return f, nil
}

func (f *portForward) Addr() string { return f.ln.Addr().String() }

func (f *portForward) Failures() int { return int(f.failures.Load()) }

func (f *portForward) Close() error {
	f.closed.Store(true)
	return f.ln.Close()
}

func (f *portForward) accept(local bool) {
	for !f.closed.Load() {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		if local {
			go f.serveDirect(conn)
		} else {
			go f.serveRelay(conn)
		}
	}
}

type firstChunk struct {
	mu   sync.Mutex
	data []byte
}

func (c *firstChunk) record(b []byte) {
	c.mu.Lock()
	if c.data == nil {
		c.data = append([]byte{}, b...)
	}
	c.mu.Unlock()
}

func (c *firstChunk) get() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data
}

func staleBrowserPath(request, reply []byte) bool {
	return bytes.HasPrefix(request, []byte("GET /devtools/browser/")) &&
		(bytes.HasPrefix(reply, []byte("HTTP/1.1 404 ")) || bytes.HasPrefix(reply, []byte("HTTP/1.0 404 ")))
}

func pump(src io.Reader, dst io.Writer, first *firstChunk) {
	buf := make([]byte, relayChunk)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			first.record(buf[:n])
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *portForward) serveRelay(conn net.Conn) {
	p, err := f.s.Start(context.Background(), []string{"bash", "-c", tcpRelayScript, "relay", f.host, strconv.Itoa(f.port)}, tool.StartOptions{})
	if err != nil {
		_ = conn.Close()
		f.failures.Add(1)
		return
	}
	var request, reply firstChunk
	go func() {
		pump(conn, p.Stdin(), &request)
		_ = p.Stdin().Close()
	}()
	pump(p.Output(), conn, &reply)
	_ = conn.Close()
	if (reply.get() == nil && exitedNonzero(p)) || staleBrowserPath(request.get(), reply.get()) {
		f.failures.Add(1)
	}
	_ = p.Kill()
	if c, ok := p.(io.Closer); ok {
		_ = c.Close()
	}
}

func exitedNonzero(p tool.Process) bool {
	ctx, cancel := context.WithTimeout(context.Background(), relayExitWait)
	defer cancel()
	code, err := p.Wait(ctx)
	return err == nil && code != 0
}

func (f *portForward) serveDirect(conn net.Conn) {
	target, err := net.Dial("tcp", net.JoinHostPort(f.host, strconv.Itoa(f.port)))
	if err != nil {
		_ = conn.Close()
		f.failures.Add(1)
		return
	}
	var request, reply firstChunk
	go func() {
		pump(conn, target, &request)
		if tc, ok := target.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	pump(target, conn, &reply)
	_ = conn.Close()
	_ = target.Close()
	if staleBrowserPath(request.get(), reply.get()) {
		f.failures.Add(1)
	}
}

func (s *Shell) closeForwards() {
	s.mu.Lock()
	forwards := s.forwards
	s.forwards = nil
	s.mu.Unlock()
	for _, f := range forwards {
		_ = f.Close()
	}
}

func (l *Local) hostCommand(ctx context.Context, argv []string, env map[string]string, cwd string) (*exec.Cmd, error) {
	childEnv, err := l.h.RunEnv(ctx, env)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = EnvList(childEnv)
	cmd.Dir = cwd
	proc.Hide(cmd)
	return cmd, nil
}

func (l *Local) Start(ctx context.Context, argv []string, o tool.StartOptions) (tool.Process, error) {
	if o.PTY {
		return nil, &UnsupportedError{Kind: l.Kind(), Op: "pseudo-terminal streams"}
	}
	childEnv, err := l.h.RunEnv(ctx, o.Env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = EnvList(childEnv)
	cmd.Dir = o.Cwd
	p, err := startStream(cmd)
	if err != nil {
		return nil, err
	}
	return p, nil
}
