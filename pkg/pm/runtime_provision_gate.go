package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Shortest and longest wait between first use provisioning attempts of a store that keeps failing
const (
	provisionRetryBase = 30 * time.Second
	provisionRetryCap  = 30 * time.Minute
)

// Clock the provisioning gate reads
var provisionNow = time.Now

// Last failed first use provisioning of a runtime store, shared by every process of the install
type provisionFailure struct {
	Set      string `json:"set"`
	AtMillis int64  `json:"at_ms"`
	Count    int    `json:"count"`
	Error    string `json:"error"`
}

// First use provisioning that did not run because the same set failed inside its retry wait
type ProvisionDeferred struct {
	Cause string
	Retry time.Duration
}

func (e *ProvisionDeferred) Error() string {
	return fmt.Sprintf("%s (provisioning is tried again in %s, run `icebath pm install` to provision at once)",
		e.Cause, e.Retry.Round(time.Second))
}

// Wait after count failures in a row, doubling from the base up to the cap
func provisionRetryAfter(count int) time.Duration {
	wait := provisionRetryBase
	for i := 1; i < count && wait < provisionRetryCap; i++ {
		wait *= 2
	}
	return min(wait, provisionRetryCap)
}

// Serializes provisioning of one runtime store, remembering its last failure and the run that left it unfinished
type provisionGate struct {
	store string
	set   string
}

// Gate of the store beside venv for the requirement set with these pinned bytes
func newProvisionGate(venv string, requirements []byte) provisionGate {
	sum := sha256.Sum256(requirements)
	return provisionGate{store: RuntimeSetStore(venv), set: hex.EncodeToString(sum[:])}
}

func (g provisionGate) recordPath() string     { return filepath.Join(g.store, "provision-failure.json") }
func (g provisionGate) unfinishedPath() string { return filepath.Join(g.store, "provision-unfinished") }
func (g provisionGate) lockPath() string       { return filepath.Join(g.store, ".provision.lock") }

// Recorded failure of this requirement set, false when none is recorded or the record names another set
func (g provisionGate) read() (provisionFailure, bool) {
	var rec provisionFailure
	data, err := os.ReadFile(g.recordPath())
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.Set != g.set || rec.Count < 1 {
		return provisionFailure{}, false
	}
	return rec, true
}

// Recorded failure still inside its retry wait, nil once the wait ended or nothing is recorded
func (g provisionGate) deferral() *ProvisionDeferred {
	rec, ok := g.read()
	if !ok {
		return nil
	}
	elapsed := provisionNow().Sub(time.UnixMilli(rec.AtMillis))
	if wait := provisionRetryAfter(rec.Count); elapsed >= 0 && elapsed < wait {
		return &ProvisionDeferred{Cause: rec.Error, Retry: wait - elapsed}
	}
	return nil
}

// Opens the store's lock file, creating the store
func (g provisionGate) openLock() (*os.File, error) {
	if err := os.MkdirAll(g.store, 0o777); err != nil {
		return nil, err
	}
	return os.OpenFile(g.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
}

// Takes the store's provisioning lock without waiting, false when another provisioning holds it
func (g provisionGate) tryEnter() (func(), bool, error) {
	f, err := g.openLock()
	if err != nil {
		return nil, false, err
	}
	held, err := LockFD(f, false, 0)
	if err != nil || !held {
		f.Close()
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}

// Takes the store's provisioning lock once every other provisioning released it, giving up when ctx ends
func (g provisionGate) enter(ctx context.Context) (func(), error) {
	for {
		release, held, err := g.tryEnter()
		if err != nil || held {
			return release, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// Waits until no provisioning holds the store's lock, giving up when ctx ends
func (g provisionGate) awaitRelease(ctx context.Context) error {
	release, err := g.enter(ctx)
	if err != nil {
		return err
	}
	release()
	return nil
}

// Records cause as the newest failure of the set and returns it, joined with a failure to record it
func (g provisionGate) failed(cause error) error {
	count := 1
	if rec, ok := g.read(); ok {
		count = rec.Count + 1
	}
	data, err := json.Marshal(provisionFailure{Set: g.set, AtMillis: provisionNow().UnixMilli(), Count: count, Error: cause.Error()})
	if err != nil {
		return errors.Join(cause, err)
	}
	if err := g.publish(g.recordPath(), data); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// Writes data to path through a staging file renamed over it
func (g provisionGate) publish(path string, data []byte) error {
	staging := fmt.Sprintf("%s.stage.%d", path, os.Getpid())
	err := os.WriteFile(staging, data, 0o666)
	if err == nil {
		err = os.Rename(staging, path)
	}
	if err != nil {
		os.Remove(staging)
	}
	return err
}

// Forgets the recorded failure once the set is installed
func (g provisionGate) clear() error {
	if err := os.Remove(g.recordPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Whether a provisioning changes the environment now or died before it finished, any unreadable marker counting
func (g provisionGate) unfinished() bool {
	_, err := os.Lstat(g.unfinishedPath())
	return !errors.Is(err, fs.ErrNotExist)
}

// Whether another provisioning holds the store lock now, the one a start must not duplicate
func (g provisionGate) running() (bool, error) {
	release, held, err := g.tryEnter()
	if err != nil || !held {
		return err == nil, err
	}
	release()
	return false, nil
}

// Marks the environment as being changed by this process until finish runs
func (g provisionGate) begin() error {
	if err := os.MkdirAll(g.store, 0o777); err != nil {
		return err
	}
	return g.publish(g.unfinishedPath(), []byte(strconv.Itoa(os.Getpid())+"\n"))
}

// Removes the unfinished marker and the recorded failure of a provisioning that installed the set
func (g provisionGate) finish() error {
	if err := os.Remove(g.unfinishedPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return g.clear()
}
