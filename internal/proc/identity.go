package proc

import (
	"context"
	"errors"
	"syscall"
	"time"
)

// One incarnation of a pid, its start stamp telling it apart from a later process that reuses the number
type Identity struct {
	PID int
	// Start stamp from Birth, compared exactly
	Start float64
}

// Current incarnation of pid
func Identify(pid int) (Identity, error) {
	start, err := Birth(pid)
	if err != nil {
		return Identity{}, err
	}
	return Identity{PID: pid, Start: start}, nil
}

// Whether the pid still names this incarnation, an exited child its parent has not collected included
func (id Identity) Running() bool {
	start, err := Birth(id.PID)
	return err == nil && start == id.Start
}

// Whether this incarnation is running and has not exited into a zombie
func (id Identity) Alive() bool { return id.Running() && notExited(id.PID) }

// Delivers sig to this incarnation, ErrReused or ErrNotFound once the pid names another, on Windows terminating with code sig
func (id Identity) Signal(sig syscall.Signal) error { return signalIdentity(id, sig) }

// Blocks until this incarnation has exited or ctx ends, returning ctx's error in the second case
func (id Identity) Wait(ctx context.Context) error { return waitExit(ctx, id) }

// Polls until the incarnation is no longer alive, for kernels without an exit event source
func pollExit(ctx context.Context, id Identity) error {
	for attempt := 0; id.Alive(); attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-afterDelay(attempt):
		}
	}
	return nil
}

// Blocks until every incarnation has exited or ctx ends, returning those still alive
func Await(ctx context.Context, ids []Identity) []Identity {
	var alive []Identity
	for _, id := range ids {
		if id.Wait(ctx) != nil && id.Alive() {
			alive = append(alive, id)
		}
	}
	return alive
}

// Time the kernel gets to tear down and report a killed process before it counts as surviving
const killSettle = time.Second

// Sends SIGTERM to each live incarnation, SIGKILL to those alive after grace, and returns any that outlived both
func Stop(ctx context.Context, ids []Identity, grace time.Duration) []Identity {
	var asked, survivors []Identity
	for _, id := range ids {
		err := id.Signal(syscall.SIGTERM)
		switch {
		case err == nil:
			asked = append(asked, id)
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrReused):
		default:
			survivors = append(survivors, id)
		}
	}
	wait, cancel := context.WithTimeout(ctx, grace)
	late := Await(wait, asked)
	cancel()
	var killed []Identity
	for _, id := range late {
		err := id.Signal(syscall.SIGKILL)
		switch {
		case err == nil:
			killed = append(killed, id)
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrReused):
		default:
			survivors = append(survivors, id)
		}
	}
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), killSettle)
	defer cancel()
	return append(survivors, Await(settle, killed)...)
}
