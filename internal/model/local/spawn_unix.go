//go:build !windows

package local

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

const survivorGrace = 3 * time.Second

type serverChild struct {
	cmd   *exec.Cmd
	id    proc.Identity
	guard *proc.Guard
	done  chan struct{}
	exit  error
}

func startServer(ctx context.Context, guard *proc.Guard, ledger *proc.Ledger, cmd *exec.Cmd) (*serverChild, error) {
	if err := guard.StartOwned(ctx, cmd, ledger, "llama-server"); err != nil {
		if !errors.Is(err, proc.ErrUnguarded) {
			return nil, err
		}
		slog.Debug(fmt.Sprintf("llama-server runs without owner-death coverage: %v", err))
	}
	c := &serverChild{cmd: cmd, guard: guard, done: make(chan struct{})}
	if id, err := proc.Identify(cmd.Process.Pid); err == nil {
		c.id = id
	} else {
		c.id = proc.Identity{PID: cmd.Process.Pid}
	}
	go func() {
		c.exit = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *serverChild) contained() bool { return false }

func (c *serverChild) closeContainment() error { return nil }

func (c *serverChild) releaseOnceEmpty(ctx context.Context) {
	probe, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	c.guard.ReleaseWhenEmpty(probe, c.id.PID)
}

func (c *serverChild) stopSurvivors(ctx context.Context) {
	c.guard.StopSurvivors(ctx, c.id.PID, survivorGrace)
}
