package cron

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Usage audit jsonl under the profile's cron folder
func (s *Store) usageAuditPath() string {
	return filepath.Join(s.home, "cron", "usage_audit.jsonl")
}

// Current UTC instant with millisecond precision and a Z suffix
func utcnowISOms() string {
	now := time.Now().UTC()
	return now.Format("2006-01-02T15:04:05.") + fmt.Sprintf("%03dZ", now.Nanosecond()/1e6)
}

// Appends one usage audit record, logging rather than failing the run on a write error
func (s *Store) writeUsageAudit(record *pyjson.Object) {
	err := func() error {
		path := s.usageAuditPath()
		if err := ensureCronDir(filepath.Dir(path)); err != nil {
			return err
		}
		line, err := pyjson.Dumps(record, pyjson.EnsureASCII(false))
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		_, werr := f.WriteString(line + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		return werr
	}()
	if err != nil {
		s.log.Warn(fmt.Sprintf("usage_audit write failed: %v", err))
	}
}

// True when the process is tearing down and the error is a shutdown artefact
func interpreterShuttingDown(err error) bool {
	return tools.ProcessShuttingDown(err)
}

// Cron folder and tick lock file of the profile
func (s *Store) getLockPaths() (lockDir, lockFile string) {
	lockDir = filepath.Join(s.home, "cron")
	return lockDir, filepath.Join(lockDir, ".tick.lock")
}

// True when a lock error means another process holds the tick lock
func isLockContentionErrno(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	if runtime.GOOS == "windows" {
		return errno == syscall.EACCES || errno == syscall.EDEADLK
	}
	return errno == syscall.EWOULDBLOCK || errno == syscall.EAGAIN || errno == syscall.EACCES
}

// True when text carries the wording of a file descriptor exhaustion error
func isFDExhaustionText(text string) bool {
	lowered := strings.ToLower(text)
	return strings.Contains(lowered, "too many open files") || strings.Contains(lowered, "emfile")
}

// True for EMFILE or ENFILE anywhere in the chain or their wording in a wrapped error
func isFDExhaustion(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
		return true
	}
	return isFDExhaustionText(err.Error())
}

// Collects garbage and raises the open file soft limit so the next tick has descriptors to work with
func reclaimFDs() {
	func() {
		defer func() { _ = recover() }()
		runtime.GC()
	}()
	func() {
		defer func() { _ = recover() }()
		applyNofileSoftLimit(nil)
	}()
}

// Open file soft limit raiser a test replaces, the gateway resource limit helper by default
var applyNofileSoftLimit = gateway.ApplyNofileSoftLimit
