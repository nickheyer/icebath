//go:build !windows

package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Collector of adopted children for an init process or subreaper, leaving children it started to their own Wait
type Reaper struct {
	mu      sync.Mutex
	started map[int]bool
}

// Reaper with no started children
func NewReaper() *Reaper { return &Reaper{started: map[int]bool{}} }

// Starts cmd so no concurrent reap collects it ahead of its Wait, every child of this process starting here
func (r *Reaper) Start(cmd *exec.Cmd) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	r.started[cmd.Process.Pid] = true
	return nil
}

// Waits for a command started through Start and hands its pid back to reaping
func (r *Reaper) Wait(cmd *exec.Cmd) error {
	err := cmd.Wait()
	r.mu.Lock()
	delete(r.started, cmd.Process.Pid)
	r.mu.Unlock()
	return err
}

// Collects every exited child this process adopted, so none lingers as a zombie
func (r *Reaper) Reap() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	table, err := ReadTable()
	if err != nil {
		return err
	}
	self := os.Getpid()
	for _, e := range table {
		if e.PPID != self || e.Status != Zombie || r.started[e.PID] {
			continue
		}
		var ws unix.WaitStatus
		for {
			if _, err := unix.Wait4(e.PID, &ws, unix.WNOHANG, nil); !errors.Is(err, unix.EINTR) {
				break
			}
		}
	}
	return nil
}

// Reaps adopted children on every SIGCHLD until ctx ends
func (r *Reaper) Run(ctx context.Context) error {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	defer signal.Stop(sigs)
	for {
		if err := r.Reap(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sigs:
		}
	}
}
