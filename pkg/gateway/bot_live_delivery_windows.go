package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

// Owner account of a path for the log line, never failing
func dirOwner(path string) string {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Sprintf("unknown (%s)", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Sprintf("unknown (%s)", err)
	}
	name, domain, _, err := owner.LookupAccount("")
	if err != nil {
		return fmt.Sprintf("unknown (%s)", err)
	}
	if domain != "" {
		return domain + `\` + name
	}
	return name
}

// A foreign Windows ACL denying attribute writes never refuses a mailbox this process can still use
func chmodRefusesMailbox(root string) bool { return false }

// Exclusive lock on the first byte retried ten times a second apart like msvcrt LK_LOCK
func acquireFileLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	var locked bool
	for attempt := 0; attempt < 10; attempt++ {
		ol := new(windows.Overlapped)
		if windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol) == nil {
			locked = true
			break
		}
		time.Sleep(time.Second)
	}
	if !locked {
		f.Close()
		return nil, errors.New("file lock unavailable: " + path)
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
		f.Close()
	}, nil
}
