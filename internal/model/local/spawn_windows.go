package local

import (
	"context"
	"errors"
	"os/exec"
	"sync"

	"github.com/nickheyer/icebath/internal/proc"
)

type serverChild struct {
	cmd  *exec.Cmd
	id   proc.Identity
	tree *proc.Tree
	done chan struct{}
	exit error

	mu     sync.Mutex
	closed bool
}

func startServer(_ context.Context, _ *proc.Guard, _ *proc.Ledger, cmd *exec.Cmd) (*serverChild, error) {
	tree, err := proc.StartTree(cmd)
	if err != nil {
		return nil, err
	}
	c := &serverChild{cmd: cmd, tree: tree, done: make(chan struct{})}
	if id, err := proc.Identify(cmd.Process.Pid); err == nil {
		c.id = id
	} else {
		c.id = proc.Identity{PID: cmd.Process.Pid}
	}
	go func() {
		c.exit = tree.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *serverChild) contained() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func (c *serverChild) closeContainment() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if err := c.tree.Kill(); err != nil && !errors.Is(err, proc.ErrNotFound) {
		return err
	}
	return nil
}

func (c *serverChild) releaseOnceEmpty(context.Context) {}

func (c *serverChild) stopSurvivors(context.Context) {}
