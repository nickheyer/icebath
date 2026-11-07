//go:build !windows

package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pypwd"
)

// Owner name of a path for the log line, never failing
func dirOwner(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("unknown (%s)", deliveryOSErrorText(err))
	}
	uid := int(st.Sys().(*syscall.Stat_t).Uid)
	pw, err := pypwd.Getpwuid(uid)
	if err != nil {
		return fmt.Sprintf("uid %d", uid)
	}
	return pw.Name
}

// Whether a failed mailbox chmod means another uid owns the dir
func chmodRefusesMailbox(root string) bool {
	st, err := os.Stat(root)
	if err != nil {
		return true
	}
	return int(st.Sys().(*syscall.Stat_t).Uid) != os.Getuid()
}

// Exclusive whole file lock held until release, an open failure returned as is
func acquireFileLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, errors.New("file lock unavailable: " + path)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
