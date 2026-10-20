package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Server started as a child process speaking newline delimited JSON on its standard streams
type Stdio struct {
	Command string
	Args    []string
	// KEY=VALUE pairs layered over the process environment
	Env []string
	Dir string
	// Receives the server standard error, discarded when nil
	Stderr io.Writer
}

type stdioConn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex
	in    chan []byte
	done  chan struct{}
	once  sync.Once
}

func (s Stdio) Dial(ctx context.Context) (Transport, error) {
	if s.Command == "" {
		return nil, errors.New("mcp: stdio command is required")
	}
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	cmd.Stderr = s.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	c := &stdioConn{cmd: cmd, stdin: stdin, in: make(chan []byte, 16), done: make(chan struct{})}
	go c.pump(stdout)
	return c, nil
}

// Forwards each non empty line of server output until the stream ends
func (c *stdioConn) pump(r io.Reader) {
	defer close(c.in)
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			c.in <- trimmed
		}
		if err != nil {
			return
		}
	}
}

func (c *stdioConn) Send(ctx context.Context, msg []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.done:
		return errors.New("mcp: stdio transport closed")
	default:
	}
	_, err := c.stdin.Write(append(msg, '\n'))
	return err
}

func (c *stdioConn) Receive() <-chan []byte { return c.in }

// Closes stdin, waits briefly for a clean exit and kills the process group otherwise
func (c *stdioConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.stdin.Close()
		exited := make(chan struct{})
		go func() {
			c.cmd.Wait()
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
	})
	return nil
}
