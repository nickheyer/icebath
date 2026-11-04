package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Largest single stdio line accepted from a server
const maxStdioLineBytes = 64 * 1024 * 1024

// StdioParams describe the child process a stdio transport spawns
type StdioParams struct {
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string
	// Stderr receives the child's stderr, nil discards it
	Stderr io.Writer
}

// StdioTransport runs an MCP server as a child process speaking newline framed JSON-RPC
type StdioTransport struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	recv     chan *Message
	done     chan struct{}
	exited   chan struct{}
	writeMu  sync.Mutex
	errMu    sync.Mutex
	err      error
	closed   bool
	pid      int
	pgid     int
	start    time.Time
	waitErr  error
}

// StartStdio spawns the child and begins pumping its stdout into messages
func StartStdio(ctx context.Context, p StdioParams) (*StdioTransport, error) {
	if p.Command == "" {
		return nil, errors.New("stdio transport needs a command")
	}
	cmd := exec.Command(p.Command, p.Args...)
	cmd.Dir = p.Cwd
	cmd.Env = flattenEnv(p.Env)
	cmd.Stderr = p.Stderr
	if p.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	setProcessGroup(cmd)
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
	t := &StdioTransport{cmd: cmd, stdin: stdin, recv: make(chan *Message, 64), done: make(chan struct{}),
		exited: make(chan struct{}), pid: cmd.Process.Pid, start: time.Now()}
	t.pgid = processGroup(cmd.Process.Pid)
	go t.pump(stdout)
	go t.wait()
	return t, nil
}

// flattenEnv renders an env map as KEY=VALUE pairs
func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// pump reads stdout lines and decodes each into a message until EOF
func (t *StdioTransport) pump(stdout io.Reader) {
	defer close(t.recv)
	reader := bufio.NewReaderSize(stdout, 1024*1024)
	for {
		line, err := readLongLine(reader, maxStdioLineBytes)
		if len(bytes.TrimSpace(line)) > 0 {
			msgs, derr := DecodeMessages(line)
			if derr != nil {
				t.setErr(fmt.Errorf("stdio server sent a non JSON-RPC line: %w", derr))
			} else {
				for _, m := range msgs {
					select {
					case t.recv <- m:
					case <-t.done:
						return
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				t.setErr(err)
			} else {
				t.setErr(errors.New("Connection closed"))
			}
			return
		}
	}
}

// readLongLine reads one line up to limit bytes, failing on longer lines
func readLongLine(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			if len(buf) > limit {
				return nil, fmt.Errorf("stdio line exceeds %d bytes", limit)
			}
			continue
		}
		return buf, err
	}
}

// wait reaps the child and records its exit
func (t *StdioTransport) wait() {
	t.waitErr = t.cmd.Wait()
	close(t.exited)
}

// setErr records the first terminal error
func (t *StdioTransport) setErr(err error) {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	if t.err == nil && !t.closed {
		t.err = err
	}
}

// Send writes one message as a single line to the child's stdin
func (t *StdioTransport) Send(ctx context.Context, msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.closed {
		return errors.New("transport is closed")
	}
	if !t.Alive() {
		return errors.New("Connection closed")
	}
	_, err = t.stdin.Write(append(data, '\n'))
	if err != nil {
		return fmt.Errorf("broken pipe: %w", err)
	}
	return nil
}

// Receive returns the inbound message channel
func (t *StdioTransport) Receive() <-chan *Message { return t.recv }

// Err returns the terminal transport error
func (t *StdioTransport) Err() error {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return t.err
}

// Label names the transport
func (t *StdioTransport) Label() string { return "stdio" }

// PID returns the child's process id
func (t *StdioTransport) PID() int { return t.pid }

// PGID returns the child's process group id, zero when groups are unsupported
func (t *StdioTransport) PGID() int { return t.pgid }

// Alive reports whether the child process is still running
func (t *StdioTransport) Alive() bool {
	select {
	case <-t.exited:
		return false
	default:
		return true
	}
}

// Exited returns a channel closed once the child has been reaped
func (t *StdioTransport) Exited() <-chan struct{} { return t.exited }

// Close ends stdin, waits briefly for a clean exit, then terminates the whole process group
func (t *StdioTransport) Close() error {
	t.writeMu.Lock()
	if t.closed {
		t.writeMu.Unlock()
		return nil
	}
	t.closed = true
	close(t.done)
	_ = t.stdin.Close()
	t.writeMu.Unlock()
	select {
	case <-t.exited:
	case <-time.After(2 * time.Second):
		terminateGroup(t.pid, t.pgid, false)
		select {
		case <-t.exited:
		case <-time.After(2 * time.Second):
			terminateGroup(t.pid, t.pgid, true)
			<-t.exited
		}
	}
	if t.pgid > 0 && groupAlive(t.pgid) {
		terminateGroup(t.pid, t.pgid, true)
	}
	return nil
}

// pidExists reports whether a process with this pid is still present
func pidExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return signalZero(p)
}
