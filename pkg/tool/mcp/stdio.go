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
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
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
	// Time a server gets to exit after stdin closes before it is killed, two seconds when zero
	Grace time.Duration
}

type stdioConn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex
	in    chan []byte
	done  chan struct{}
	once  sync.Once
	grace time.Duration
	mu    sync.Mutex
	exit  error
	ended bool
}

func (s Stdio) Dial(ctx context.Context) (Transport, error) {
	if s.Command == "" {
		return nil, errors.New("mcp: stdio command is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	cmd.Stderr = s.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	proc.Group(cmd)
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
	grace := s.Grace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	c := &stdioConn{cmd: cmd, stdin: stdin, in: make(chan []byte, 16), done: make(chan struct{}), grace: grace}
	go c.pump(stdout)
	return c, nil
}

// Forwards each non empty line of server output until the stream ends, then records the exit
func (c *stdioConn) pump(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			c.in <- trimmed
		}
		if err != nil {
			break
		}
	}
	exit := c.cmd.Wait()
	c.mu.Lock()
	c.exit, c.ended = exit, true
	c.mu.Unlock()
	close(c.in)
}

// Exit error of the server process once it has ended
func (c *stdioConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ended {
		return nil
	}
	if c.exit == nil {
		return errors.New("server process exited")
	}
	return c.exit
}

func (c *stdioConn) Send(ctx context.Context, msg []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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

// Closes stdin, waits for a clean exit within the grace period and kills the process group otherwise
func (c *stdioConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.stdin.Close()
		exited := make(chan struct{})
		go func() {
			for range c.in {
			}
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(c.grace):
			proc.Kill(c.cmd)
			<-exited
		}
	})
	return nil
}
