package proc

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/windows"
)

// Opens the incarnation id with access, ErrReused when the pid now names another process
func openIdentity(id Identity, access uint32) (windows.Handle, error) {
	h, err := openProcess(id.PID, access|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return 0, err
	}
	start, err := handleStart(h, id.PID)
	if err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	if start != id.Start {
		windows.CloseHandle(h)
		return 0, fmt.Errorf("pid %d: %w", id.PID, ErrReused)
	}
	return h, nil
}

// Ends the incarnation id with exit code, refusing a pid that now names another process
func terminateIdentity(id Identity, code uint32) error {
	h, err := openIdentity(id, windows.PROCESS_TERMINATE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, code); err != nil {
		if handleExited(h) {
			return fmt.Errorf("pid %d: %w", id.PID, ErrNotFound)
		}
		return fmt.Errorf("pid %d: %w", id.PID, err)
	}
	return nil
}

// Signal zero checks the incarnation through a handle, any other signal terminates it with that number as exit code
func signalIdentity(id Identity, sig syscall.Signal) error {
	if sig != 0 {
		return terminateIdentity(id, uint32(sig))
	}
	h, err := openIdentity(id, 0)
	if err != nil {
		return err
	}
	windows.CloseHandle(h)
	return nil
}

// Waits on the process handle and a cancel event together, polling only when the handle is refused
func waitExit(ctx context.Context, id Identity) error {
	h, err := openIdentity(id, 0)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrReused):
		return nil
	case err != nil:
		return pollExit(ctx, id)
	}
	defer windows.CloseHandle(h)
	if err = waitHandle(ctx, h); err != nil && ctx.Err() == nil {
		return pollExit(ctx, id)
	}
	return err
}

// Blocks until the process behind h exits or ctx ends
func waitHandle(ctx context.Context, h windows.Handle) error {
	cancel, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("cancel event: %w", err)
	}
	defer windows.CloseHandle(cancel)
	stop := context.AfterFunc(ctx, func() { windows.SetEvent(cancel) })
	defer stop()
	ev, err := windows.WaitForMultipleObjects([]windows.Handle{h, cancel}, false, windows.INFINITE)
	switch {
	case err != nil:
		return fmt.Errorf("wait for process: %w", err)
	case ev == windows.WAIT_OBJECT_0, handleExited(h):
		return nil
	}
	return ctx.Err()
}
