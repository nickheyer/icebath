package browser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const holdersLockName = ".holders.lock"

// This process's PID namespace inode and kernel boot id, false where neither is readable
var nsIdentity = sync.OnceValues(func() (string, bool) {
	ino, ok := pidNamespaceInode()
	if !ok {
		return "", false
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%s", ino, strings.TrimSpace(string(boot))), true
})

// Name suffix of this namespace's records, leases and throwaway dirs, empty without an identity
func nsTag() string {
	identity, ok := nsIdentity()
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(identity))
	return "-n" + hex.EncodeToString(sum[:])[:12]
}

// Integer value of a decoded JSON value that Python isinstance int accepts
func jsonInt(v any) (int64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

func objGet(data *pyjson.Object, key string) any {
	v, _ := data.Get(key)
	return v
}

// A record or lease written in this PID namespace and boot
func sameNamespace(data *pyjson.Object) bool {
	identity, ok := nsIdentity()
	if !ok {
		return objGet(data, "ns") == nil
	}
	return objGet(data, "ns") == identity
}

func heldHere(data *pyjson.Object, pidField string) bool {
	return pyrepr.Equal(objGet(data, pidField), int64(os.Getpid())) && sameNamespace(data)
}

// Windows lock error numbers that mean another process holds the lock
var ntLockContended = map[syscall.Errno]bool{syscall.EACCES: true, syscall.EDEADLK: true}

var ntLockTimeout = 30 * time.Second

// Lock wait that outlived its deadline, the TimeoutError of a leaked holder lock
type recordsLockTimeoutError struct{ msg string }

func (e *recordsLockTimeoutError) Error() string { return e.msg }

// Nil to retry a contended lock with time left, otherwise the error the failed attempt raises
func ntLockFailure(err error, records string, expired bool) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) || !ntLockContended[errno] {
		return err
	}
	if expired {
		return &recordsLockTimeoutError{fmt.Sprintf("browser launcher state %s stayed locked for %.0f s", records, ntLockTimeout.Seconds())}
	}
	return nil
}

// Exclusive cross process lock on a launcher state dir, held is always true when blocking
func recordsLock(records string, blocking bool) (held bool, unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(records, holdersLockName), os.O_RDWR|os.O_CREATE|oNoFollow, 0o600)
	if err != nil {
		return false, nil, err
	}
	held, err = lockFile(f, records, blocking)
	if err != nil {
		f.Close()
		return false, nil, err
	}
	return held, func() {
		if held {
			unlockFile(f)
		}
		f.Close()
	}, nil
}

func stem(path string) string {
	base := filepath.Base(path)
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		return base[:i]
	}
	return base
}

func leasePath(record string, holderPID int) string {
	return filepath.Join(filepath.Dir(record), fmt.Sprintf("%s@%d%s.lease", stem(record), holderPID, nsTag()))
}

// The JSON object a record or lease holds, removing a file that is unreadable or not an object
func loadStateFile(path string) *pyjson.Object {
	if raw, err := os.ReadFile(path); err == nil {
		if v, err := pyjson.LoadsBytes(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))); err == nil {
			if obj, ok := v.(*pyjson.Object); ok {
				return obj
			}
		}
	}
	os.Remove(path)
	return nil
}

// The pid is still the process that wrote a record or lease, alive and with the recorded start time
func processAlive(pid, startTime any) bool {
	n, ok := jsonInt(pid)
	if !ok || !launcherPidAlive(int(n)) {
		return false
	}
	return startTime == nil || pyrepr.Equal(optStartTime(launcherStartTime(int(n))), startTime)
}

func optStartTime(v int64, ok bool) any {
	if !ok {
		return nil
	}
	return v
}

func optInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nsValue() any {
	if identity, ok := nsIdentity(); ok {
		return identity
	}
	return nil
}

// Records that this process holds the browser, caller holds the records lock
func writeLease(record string, b *LaunchedBrowser) error {
	me := os.Getpid()
	lease := pyjson.NewObject()
	lease.Set("holder_pid", int64(me))
	lease.Set("holder_start_time", optStartTime(launcherStartTime(me)))
	lease.Set("pid", optInt(b.PID))
	lease.Set("start_time", optInt(b.StartTime))
	lease.Set("user_data_dir", b.UserDataDir)
	lease.Set("ns", nsValue())
	return config.AtomicJSONWriteWith(leasePath(record, me), lease)
}

func dropLease(record string) { os.Remove(leasePath(record, os.Getpid())) }

// Proven gone, written in this namespace and boot with a pid and start time that no longer match
func holderGone(lease *pyjson.Object) bool {
	return sameNamespace(lease) && !processAlive(objGet(lease, "holder_pid"), objGet(lease, "holder_start_time"))
}

// Leases of other live processes on the browser a record names, removing dead or stale ones
func otherLiveHolders(record string, browserStartTime any) []*pyjson.Object {
	prefix := stem(record) + "@"
	entries, _ := os.ReadDir(filepath.Dir(record))
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".lease") {
			names = append(names, filepath.Join(filepath.Dir(record), e.Name()))
		}
	}
	sort.Strings(names)
	var holders []*pyjson.Object
	for _, path := range names {
		lease := loadStateFile(path)
		if lease == nil || heldHere(lease, "holder_pid") {
			continue
		}
		recorded := objGet(lease, "start_time")
		otherLaunch := !pyrepr.Equal(recorded, browserStartTime) && (sameNamespace(lease) || recorded != nil)
		if otherLaunch || holderGone(lease) {
			os.Remove(path)
			continue
		}
		holders = append(holders, lease)
	}
	return holders
}

// Removes leases whose holder is gone, caller holds the records lock
func dropDeadLeases(records string) {
	entries, _ := os.ReadDir(records)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".lease") {
			continue
		}
		path := filepath.Join(records, e.Name())
		if lease := loadStateFile(path); lease != nil && holderGone(lease) {
			os.Remove(path)
		}
	}
}

// The live holder in this namespace a record can be handed to
func successor(holders []*pyjson.Object) *pyjson.Object {
	for _, h := range holders {
		if sameNamespace(h) {
			return h
		}
	}
	return nil
}

// Makes the holder the record's owner so the last holder out stops the browser
func handOver(record string, data, holder *pyjson.Object) error {
	out := pyjson.NewObject()
	for _, k := range data.Keys {
		out.Set(k, objGet(data, k))
	}
	out.Set("owner_pid", objGet(holder, "holder_pid"))
	out.Set("owner_start_time", objGet(holder, "holder_start_time"))
	if err := config.AtomicJSONWriteWith(record, out); err != nil {
		return err
	}
	launcherLogger().Info(fmt.Sprintf("browser: Chromium pid %v is still used by Hermes pid %v; handed it over instead of stopping it",
		pyrepr.Str(objGet(data, "pid")), pyrepr.Str(objGet(holder, "holder_pid"))))
	return nil
}
