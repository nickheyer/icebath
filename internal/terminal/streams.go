package terminal

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
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

	"github.com/nickheyer/icebath/pkg/proc"
)

// Argv of this machine that runs the rest inside the environment, nil when the environment can't
func ExecPrefix(env Environment, user string, interactive bool) []string {
	streamer, ok := env.(Streamer)
	if !ok {
		return nil
	}
	prefix, err := streamer.ExecPrefix(user, interactive)
	if err != nil {
		slog.Warn(fmt.Sprintf("%s.exec_prefix() failed; treating the backend as unable to stream", env.Backend()), "error", err)
		return nil
	}
	if len(prefix) == 0 {
		return nil
	}
	return prefix
}

// The prefix and a bash -c that exports the variables and execs argv, one word for a client that joins its argv into a line
func RemoteArgv(prefix, argv []string, vars map[string]string, joined bool) []string {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	exports := make([]string, len(names))
	for i, name := range names {
		exports[i] = "export " + name + "=" + Quote(vars[name]) + ";"
	}
	script := strings.Join(exports, " ") + " exec " + Join(argv)
	out := append([]string{}, prefix...)
	if joined {
		return append(out, Join([]string{"bash", "-c", script}))
	}
	return append(out, "bash", "-c", script)
}

// Argv of this machine running argv inside the environment, nil when it has no exec prefix
func RemoteCommand(env Environment, argv []string, vars map[string]string, user string, interactive bool) []string {
	prefix := ExecPrefix(env, user, interactive)
	if prefix == nil {
		return nil
	}
	joiner, ok := env.(Joiner)
	return RemoteArgv(prefix, argv, vars, ok && joiner.ShellJoined())
}

// A long lived process inside an environment, its stdin and stdout pipes of this process
type Stream struct {
	Cmd    *exec.Cmd
	Stdin  *os.File
	Stdout *os.File
	done   chan struct{}
	code   int
}

// Starts argv inside the environment with byte pipes, stderr dropped when nil
func OpenStream(env Environment, argv []string, vars map[string]string, user string, stderr *os.File) (*Stream, error) {
	command := RemoteCommand(env, argv, vars, user, true)
	if command == nil {
		return nil, fmt.Errorf("%s cannot host a long-lived stdio stream", env.Backend())
	}
	cmd := exec.Command(command[0], command[1:]...)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout = stdinR, stdoutW
	if stderr != nil {
		cmd.Stderr = stderr
	}
	proc.HideWindow(cmd)
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{stdinR, stdinW, stdoutR, stdoutW} {
			f.Close()
		}
		return nil, err
	}
	stdinR.Close()
	stdoutW.Close()
	s := &Stream{Cmd: cmd, Stdin: stdinW, Stdout: stdoutR, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		s.code = exitCode(cmd, cmd.Wait())
	}()
	return s, nil
}

func (s *Stream) Kill() error {
	if s.Cmd.Process == nil {
		return nil
	}
	return s.Cmd.Process.Kill()
}

// Exit code once the stream ends within the timeout, waiting for ever when it is zero
func (s *Stream) Wait(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		<-s.done
		return s.code, nil
	}
	select {
	case <-s.done:
		return s.code, nil
	case <-time.After(timeout):
		return 0, context.DeadlineExceeded
	}
}

// Output of a short command run inside an environment
type RunResult struct {
	Stdout []byte
	Stderr []byte
	Code   int
}

// Runs a short argv inside the environment capturing its output, once more when the backend recovered a sandbox removed behind its back
func RunIn(ctx context.Context, env Environment, argv []string, vars map[string]string, user string, timeout time.Duration, stdin []byte) (*RunResult, error) {
	result, err := runIn(ctx, env, argv, vars, user, timeout, stdin)
	if err != nil || result.Code == 0 {
		return result, err
	}
	if recoverer, ok := env.(Recoverer); ok && recoverer.RecoverGone(ctx, string(result.Stderr)) {
		return runIn(ctx, env, argv, vars, user, timeout, stdin)
	}
	return result, nil
}

func runIn(ctx context.Context, env Environment, argv []string, vars map[string]string, user string, timeout time.Duration, stdin []byte) (*RunResult, error) {
	command := RemoteCommand(env, argv, vars, user, stdin != nil)
	if command == nil {
		return nil, fmt.Errorf("%s cannot exec argv", env.Backend())
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	proc.HideWindow(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("command timed out after %s", timeout)
	}
	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
		return nil, err
	}
	return &RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Code: exitCode(cmd, err)}, nil
}

// Python relay run inside the sandbox, bridging its stdio to a port there
const relayScript = "import os,socket,sys,threading\n" +
	"s=socket.create_connection((sys.argv[1],int(sys.argv[2])))\n" +
	"def up():\n" +
	"  while True:\n" +
	"    d=os.read(0,65536)\n" +
	"    if not d: break\n" +
	"    s.sendall(d)\n" +
	"  s.shutdown(socket.SHUT_WR)\n" +
	"threading.Thread(target=up,daemon=True).start()\n" +
	"while True:\n" +
	"  d=s.recv(65536)\n" +
	"  if not d: break\n" +
	"  os.write(1,d)\n"

// A loopback port of this machine whose connections land on a port inside an environment
type Forward struct {
	Env      Environment
	Host     string
	Port     int
	User     string
	Local    int
	listener net.Listener
	failures atomic.Int64
}

type forwardKey struct {
	env  Environment
	host string
	port int
}

// Open forwards of the host, one per environment, address and port
type forwards struct {
	mu sync.Mutex
	m  map[forwardKey]*Forward
}

// Connections that never reached the target
func (f *Forward) Failures() int { return int(f.failures.Load()) }

func newForward(env Environment, host string, port int, user string) (*Forward, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &Forward{Env: env, Host: host, Port: port, User: user, Local: listener.Addr().(*net.TCPAddr).Port, listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f, nil
}

// Relays one connection through a stream running the relay inside the sandbox
func (f *Forward) serve(conn net.Conn) {
	s, err := OpenStream(f.Env, []string{"python3", "-c", relayScript, f.Host, strconv.Itoa(f.Port)}, nil, f.User, nil)
	if err != nil {
		conn.Close()
		f.failures.Add(1)
		return
	}
	var mu sync.Mutex
	var request []byte
	go func() {
		defer s.Stdin.Close()
		buf := make([]byte, 65536)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				mu.Lock()
				if request == nil {
					request = append([]byte{}, buf[:n]...)
				}
				mu.Unlock()
				if _, err := s.Stdin.Write(buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var reply []byte
	buf := make([]byte, 65536)
	for {
		n, err := s.Stdout.Read(buf)
		if n > 0 {
			if reply == nil {
				reply = append([]byte{}, buf[:n]...)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	conn.Close()
	mu.Lock()
	sent := request
	mu.Unlock()
	if reply == nil && exitedBadly(s) || staleBrowser(sent, reply) {
		f.failures.Add(1)
	}
	s.Kill()
}

// A relaunched Chromium answering a cached devtools browser path with 404
func staleBrowser(request, reply []byte) bool {
	return bytes.HasPrefix(request, []byte("GET /devtools/browser/")) && (bytes.HasPrefix(reply, []byte("HTTP/1.1 404 ")) || bytes.HasPrefix(reply, []byte("HTTP/1.0 404 ")))
}

func exitedBadly(s *Stream) bool {
	code, err := s.Wait(2 * time.Second)
	return err == nil && code != 0
}

// Loopback port of this machine reaching the port inside the environment, one listener per environment, address and port
func (t *Terminal) ForwardPort(env Environment, port int, host, user string) (int, error) {
	host = cmp.Or(host, "127.0.0.1")
	key := forwardKey{env, host, port}
	t.ports.mu.Lock()
	defer t.ports.mu.Unlock()
	if t.ports.m == nil {
		t.ports.m = map[forwardKey]*Forward{}
	}
	if f := t.ports.m[key]; f != nil {
		return f.Local, nil
	}
	f, err := newForward(env, host, port, user)
	if err != nil {
		return 0, err
	}
	t.ports.m[key] = f
	return f.Local, nil
}

// Connections through the forward of the port that never reached it
func (t *Terminal) ForwardFailures(env Environment, port int, host string) int {
	t.ports.mu.Lock()
	defer t.ports.mu.Unlock()
	if f := t.ports.m[forwardKey{env, cmp.Or(host, "127.0.0.1"), port}]; f != nil {
		return f.Failures()
	}
	return 0
}

// Closes the forwards into the environment, as it goes
func (t *Terminal) closeForwards(env Environment) {
	t.ports.mu.Lock()
	defer t.ports.mu.Unlock()
	for key, f := range t.ports.m {
		if key.env == env {
			f.listener.Close()
			delete(t.ports.m, key)
		}
	}
}
