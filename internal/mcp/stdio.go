package mcp

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Largest single line a stdio server may write
const maxLineBytes = 64 << 20

// Keeps the process groups of servers dying with icebath, however it ends
type Owner interface {
	Own(pgids ...int)
	Release(pgids ...int)
}

// Child process a stdio transport starts
type StdioParams struct {
	Command string
	Args    []string
	// Environment of the child over the names every child gets
	Env map[string]string
	Cwd string
	// Receives the child's stderr, discarded when nil
	Stderr io.Writer
	// Covers the child's group while it runs, nil for none
	Owner Owner
}

// Server run as a child process speaking newline framed JSON-RPC on its stdin and stdout
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	recv   chan *Message
	done   chan struct{}
	exited chan struct{}
	owner  Owner

	writeMu sync.Mutex
	errMu   sync.Mutex
	err     error
	closed  bool
}

func StartStdio(p StdioParams) (*StdioTransport, error) {
	if p.Command == "" {
		return nil, errors.New("A server on this computer needs a command that starts it.")
	}
	cmd := exec.Command(p.Command, p.Args...)
	cmd.Dir = p.Cwd
	env := baseEnvironment()
	for k, v := range p.Env {
		env[k] = v
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = cmp.Or(p.Stderr, io.Discard)
	terminal.NewSession(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &StdioTransport{cmd: cmd, stdin: stdin, recv: make(chan *Message, 64), done: make(chan struct{}), exited: make(chan struct{}), owner: p.Owner}
	if t.owner != nil {
		t.owner.Own(cmd.Process.Pid)
	}
	go t.pump(stdout)
	go func() {
		cmd.Wait()
		close(t.exited)
	}()
	return t, nil
}

// Values of the names every stdio child gets from this process, exported shell functions left out
func baseEnvironment() map[string]string {
	names := []string{"HOME", "LOGNAME", "PATH", "SHELL", "TERM", "USER"}
	if runtime.GOOS == "windows" {
		names = []string{"APPDATA", "HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA", "PATH", "PATHEXT", "PROCESSOR_ARCHITECTURE", "SYSTEMDRIVE",
			"SYSTEMROOT", "TEMP", "USERNAME", "USERPROFILE"}
	}
	env := map[string]string{}
	for _, name := range names {
		if v, ok := os.LookupEnv(name); ok && !strings.HasPrefix(v, "()") {
			env[name] = v
		}
	}
	return env
}

// Decodes stdout lines into messages until the child closes it
func (t *StdioTransport) pump(stdout io.Reader) {
	defer close(t.recv)
	reader := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := longLine(reader)
		if len(bytes.TrimSpace(line)) > 0 {
			msgs, derr := DecodeMessages(bytes.ToValidUTF8(line, []byte("�")))
			if derr != nil {
				t.setErr(fmt.Errorf("The server sent a line that is not MCP: %w", derr))
			}
			for _, m := range msgs {
				select {
				case t.recv <- m:
				case <-t.done:
					return
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				err = errors.New("The connection closed.")
			}
			t.setErr(err)
			return
		}
	}
}

func longLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err != bufio.ErrBufferFull {
			return buf, err
		}
		if len(buf) > maxLineBytes {
			return nil, fmt.Errorf("The server sent a line larger than %s.", strs.Size(float64(maxLineBytes)))
		}
	}
}

func (t *StdioTransport) setErr(err error) {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	if t.err == nil && !t.closed {
		t.err = err
	}
}

func (t *StdioTransport) Send(_ context.Context, msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.closed {
		return errors.New("The connection closed.")
	}
	if !t.Alive() {
		return errors.New("The connection closed.")
	}
	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("The connection closed, as its pipe broke: %w", err)
	}
	return nil
}

func (t *StdioTransport) Receive() <-chan *Message { return t.recv }

func (t *StdioTransport) Err() error {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return t.err
}

func (t *StdioTransport) Label() string { return "stdio" }

func (t *StdioTransport) PID() int { return t.cmd.Process.Pid }

func (t *StdioTransport) Alive() bool {
	select {
	case <-t.exited:
		return false
	default:
		return true
	}
}

// Ends stdin, gives the child two seconds to leave, then ends its whole group
func (t *StdioTransport) Close() error {
	t.writeMu.Lock()
	if t.closed {
		t.writeMu.Unlock()
		return nil
	}
	t.errMu.Lock()
	t.closed = true
	t.errMu.Unlock()
	close(t.done)
	t.stdin.Close()
	t.writeMu.Unlock()
	pid := t.PID()
	select {
	case <-t.exited:
	case <-time.After(2 * time.Second):
		terminal.SignalGroup(pid, false)
		select {
		case <-t.exited:
		case <-time.After(2 * time.Second):
			terminal.SignalGroup(pid, true)
			<-t.exited
		}
	}
	if !terminal.GroupGone(pid) {
		terminal.SignalGroup(pid, true)
	}
	if t.owner != nil {
		t.owner.Release(pid)
	}
	return nil
}
