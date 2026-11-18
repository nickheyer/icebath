//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Serializes provisioning of one runtime store, remembering the run that left it unfinished
type provisionGate struct{ store string }

// Gate of the store beside venv
func newProvisionGate(venv string) provisionGate { return provisionGate{store: RuntimeSetStore(venv)} }

func (g provisionGate) unfinishedPath() string { return filepath.Join(g.store, "provision-unfinished") }
func (g provisionGate) lockPath() string       { return filepath.Join(g.store, ".provision.lock") }

// Takes the store's provisioning lock once every other provisioning released it, giving up when ctx ends
func (g provisionGate) enter(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(g.store, 0o777); err != nil {
		return nil, err
	}
	for {
		f, err := os.OpenFile(g.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		held, err := LockFD(f, false, 0)
		if err != nil {
			f.Close()
			return nil, err
		}
		if held {
			return func() { f.Close() }, nil
		}
		f.Close()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// Whether a provisioning changes the environment now or died before it finished, any unreadable marker counting
func (g provisionGate) unfinished() bool {
	_, err := os.Lstat(g.unfinishedPath())
	return !errors.Is(err, fs.ErrNotExist)
}

// Marks the environment as being changed by this process until finish runs
func (g provisionGate) begin() error {
	path := g.unfinishedPath()
	staging := fmt.Sprintf("%s.stage.%d", path, os.Getpid())
	err := os.WriteFile(staging, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o666)
	if err == nil {
		err = os.Rename(staging, path)
	}
	if err != nil {
		os.Remove(staging)
	}
	return err
}

// Removes the unfinished marker of a provisioning that installed the set
func (g provisionGate) finish() error {
	if err := os.Remove(g.unfinishedPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
