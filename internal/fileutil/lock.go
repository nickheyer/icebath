package fileutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Error for a file another holder already has claimed
var ErrLocked = errors.New("lock held by another holder")

// Bytes of a lock file read for the stamped holder, far more than a pid and newline take
const holderStampLimit = 4096

// Interval between attempts while a wait that ctx can end polls a held lock
const lockPollInterval = 50 * time.Millisecond

// Windows byte a lock covers unless Range says otherwise, past any content so other processes can read the file
const windowsLockOffset = 1 << 20

// Tunes how a file lock is taken
type LockOption func(*lockConfig)

// Settings the lock options collect
type lockConfig struct {
	shared   bool
	offset   int64
	length   int64
	perm     fs.FileMode
	permSet  bool
	noFollow bool
	holder   bool
	message  string
}

// Takes the lock shared, excluding exclusive holders but not other shared ones
func Shared() LockOption { return func(c *lockConfig) { c.shared = true } }

// Byte range a Windows lock covers, a POSIX flock always covering the whole file
func Range(offset, length int64) LockOption {
	return func(c *lockConfig) { c.offset, c.length = offset, length }
}

// Mode OpenLock creates its file with, an existing lock file chmodded down to it
func LockPerm(m fs.FileMode) LockOption {
	return func(c *lockConfig) { c.perm, c.permSet = m&fs.ModePerm, true }
}

// Makes OpenLock refuse a lock file that is a symlink or reparse point
func NoFollow() LockOption { return func(c *lockConfig) { c.noFollow = true } }

// Makes OpenLock stamp this process id into the file while held so a timed out waiter names a live holder
func RecordHolder() LockOption { return func(c *lockConfig) { c.holder = true } }

// Text a deadline error for the lock opens with
func TimeoutMessage(msg string) LockOption { return func(c *lockConfig) { c.message = msg } }

// Lock settings with one Windows byte at the default offset and a umask filtered lock file
func newLockConfig(opts []LockOption) lockConfig {
	c := lockConfig{offset: windowsLockOffset, length: 1, perm: 0o666}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// Wait deadline that passed while another holder still had the file
type LockTimeoutError struct {
	Path      string
	Message   string
	HolderPID int
}

// Caller message or the lock path, followed by the live holder when one is recorded
func (e *LockTimeoutError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "timed out waiting for lock " + e.Path
	}
	if e.HolderPID > 0 {
		msg += fmt.Sprintf("; another icebath process (pid %d) probably still holds it", e.HolderPID)
	}
	return msg
}

// Matches both ErrLocked and context.DeadlineExceeded
func (e *LockTimeoutError) Unwrap() []error {
	return []error{ErrLocked, context.DeadlineExceeded}
}

// Waits for the lock on f until ctx ends, a ctx that never ends taking the platform blocking wait
func waitLock(ctx context.Context, f *os.File, path string, cfg lockConfig) error {
	if ctx.Done() == nil {
		return blockingLock(f, cfg)
	}
	for {
		err := kernelLock(f, cfg)
		if !errors.Is(err, ErrLocked) {
			return err
		}
		t := time.NewTimer(lockPollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return waitEnded(ctx, path, cfg)
		case <-t.C:
		}
	}
}

// Error for a wait ctx ended, a LockTimeoutError naming any live holder when the deadline passed
func waitEnded(ctx context.Context, path string, cfg lockConfig) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("waiting for lock %s: %w", path, ctx.Err())
	}
	e := &LockTimeoutError{Path: path, Message: cfg.message}
	if cfg.holder {
		e.HolderPID = liveHolder(path)
	}
	return e
}

// Pid stamped at the head of a lock file when it names another process that is still alive, zero otherwise
func liveHolder(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	head := make([]byte, holderStampLimit)
	n, _ := io.ReadFull(f, head)
	_ = f.Close()
	fields := strings.Fields(strings.TrimPrefix(string(head[:n]), utf8BOM))
	if len(fields) == 0 {
		return 0
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 || pid == os.Getpid() || !processAlive(pid) {
		return 0
	}
	return pid
}

// Locks f once free or until ctx ends, endless waits blocking on POSIX and failing after nine seconds on Windows
func LockFile(ctx context.Context, f *os.File, opts ...LockOption) error {
	return waitLock(ctx, f, f.Name(), newLockConfig(opts))
}

// Takes the lock on f without waiting, an error matching ErrLocked when another holder has it
func TryLockFile(f *os.File, opts ...LockOption) error {
	return kernelLock(f, newLockConfig(opts))
}

// Releases the lock f holds over the range it was taken with
func UnlockFile(f *os.File, opts ...LockOption) error {
	return unlockFile(f, newLockConfig(opts))
}

// Held cross-process claim on a file, reentrant through the ctx it binds
type Lock struct {
	file  *os.File
	path  string
	cfg   lockConfig
	held  atomic.Bool
	outer *Lock
}

// Context key under which held locks are chained
type heldLocksKey struct{}

// One held lock in the chain a context carries
type heldNode struct {
	key    string
	lock   *Lock
	parent *heldNode
}

// Opens path as a lock file, creating it and its parents, and waits for its lock the way LockFile does
func OpenLock(ctx context.Context, path string, opts ...LockOption) (*Lock, error) {
	cfg := newLockConfig(opts)
	key, err := resolveLax(path)
	if err != nil {
		return nil, err
	}
	if outer := heldLock(ctx, key); outer != nil {
		return &Lock{path: key, cfg: cfg, outer: outer}, nil
	}
	return acquirePathLock(ctx, path, key, cfg, true)
}

// Opens path as a lock file and takes its lock without waiting, an error matching ErrLocked when held
func TryOpenLock(path string, opts ...LockOption) (*Lock, error) {
	key, err := resolveLax(path)
	if err != nil {
		return nil, err
	}
	return acquirePathLock(context.Background(), path, key, newLockConfig(opts), false)
}

// Opens, locks and verifies the lock file, reopening when a remover replaced the file while it was awaited
func acquirePathLock(ctx context.Context, path, key string, cfg lockConfig, wait bool) (*Lock, error) {
	for {
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return nil, err
		}
		f, err := openLockFile(path, cfg)
		if err != nil {
			return nil, err
		}
		if cfg.permSet {
			if err := f.Chmod(cfg.perm); err != nil {
				return nil, closeWith(err, f)
			}
		}
		if wait {
			err = waitLock(ctx, f, path, cfg)
		} else {
			err = kernelLock(f, cfg)
		}
		if err != nil {
			return nil, closeWith(err, f)
		}
		same, err := lockNamesPath(f, path)
		if err != nil {
			return nil, joinErr(err, unlockFile(f, cfg), f.Close())
		}
		if !same {
			if err := errors.Join(unlockFile(f, cfg), f.Close()); err != nil {
				return nil, err
			}
			if wait && ctx.Err() != nil {
				return nil, waitEnded(ctx, path, cfg)
			}
			continue
		}
		l := &Lock{file: f, path: key, cfg: cfg}
		l.held.Store(true)
		if cfg.holder {
			stampHolder(f)
		}
		return l, nil
	}
}

// Reports whether the locked file is still the one at path, false once it was unlinked or replaced
func lockNamesPath(f *os.File, path string) (bool, error) {
	held, err := f.Stat()
	if err != nil {
		return false, err
	}
	now, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(held, now), nil
}

// Replaces the lock file content with this process id, a refused write leaving the lock held without the name
func stampHolder(f *os.File) {
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
}

// Outer hold of key that ctx carries and that is still held
func heldLock(ctx context.Context, key string) *Lock {
	for n, _ := ctx.Value(heldLocksKey{}).(*heldNode); n != nil; n = n.parent {
		if samePathText(n.key, key) && n.lock.held.Load() {
			return n.lock
		}
	}
	return nil
}

// Derived ctx in which OpenLock of the same path returns a nested hold instead of waiting
func (l *Lock) Context(ctx context.Context) context.Context {
	root := l
	if l.outer != nil {
		root = l.outer
	}
	parent, _ := ctx.Value(heldLocksKey{}).(*heldNode)
	return context.WithValue(ctx, heldLocksKey{}, &heldNode{key: root.path, lock: root, parent: parent})
}

// Open handle of the held file, the outer hold handle for a nested hold
func (l *Lock) File() *os.File {
	if l.outer != nil {
		return l.outer.file
	}
	return l.file
}

// Clears a recorded holder, unlocks and closes the file, a nested or released hold doing nothing
func (l *Lock) Release() error {
	if l.outer != nil || !l.held.CompareAndSwap(true, false) {
		return nil
	}
	var errs []error
	if l.cfg.holder {
		errs = append(errs, l.file.Truncate(0))
	}
	errs = append(errs, unlockFile(l.file, l.cfg), l.file.Close())
	return errors.Join(errs...)
}

// Primary error unchanged when every other error is nil, else all of them joined
func joinErr(primary error, others ...error) error {
	if extra := errors.Join(others...); extra != nil {
		return errors.Join(primary, extra)
	}
	return primary
}

// Closes c and returns err, joined with the close failure when there is one
func closeWith(err error, c interface{ Close() error }) error {
	return joinErr(err, c.Close())
}
