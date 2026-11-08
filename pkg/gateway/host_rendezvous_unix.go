//go:build !windows

package gateway

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// Whether this OS user wrote the record inside a directory this user owns
func hostRecordIsOwn(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false
	}
	uid := hostGetuid()
	fileUID := int(info.Sys().(*syscall.Stat_t).Uid)
	if fileUID != uid || int(parent.Sys().(*syscall.Stat_t).Uid) != uid {
		hostLog().Warn(fmt.Sprintf("ignoring host record %s: owned by uid %d (expected %d)", path, fileUID, uid))
		return false
	}
	return true
}

// Repairs a group or world accessible rendezvous directory to owner only
func tightenHostStateDir(directory string) {
	if info, err := os.Stat(directory); err == nil && info.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(directory, 0o700)
	}
}

// Creates or replaces path with owner only content through a 0600 temp file and an atomic rename
func writePrivateText(path, text string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	_ = f.Sync()
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

var hostSignalInstalled bool

// Clears every registered role on SIGTERM, then lets the default disposition or other listeners take the signal
func installHostSignalCleanup() {
	if hostSignalInstalled {
		return
	}
	hostSignalInstalled = true
	ignored := signal.Ignored(syscall.SIGTERM)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		<-ch
		HostRendezvousAtExit()
		signal.Stop(ch)
		if ignored {
			signal.Ignore(syscall.SIGTERM)
			return
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			os.Exit(128 + int(syscall.SIGTERM))
		}
	}()
}
