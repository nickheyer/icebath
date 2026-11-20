//go:build windows

package manage

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Locks the first byte of each existing gateway.lock like gateway/status.py LK_NBLCK, a held one refusing maintenance
func flockMaintenance(dirs []string) (func(), error) {
	var files []*os.File
	release := func() {
		for _, f := range files {
			f.Close()
		}
	}
	for _, d := range dirs {
		f, err := os.OpenFile(filepath.Join(d, "gateway.lock"), os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			release()
			return nil, err
		}
		files = append(files, f)
		var overlapped windows.Overlapped
		if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
			release()
			return nil, &gatewayOwnershipConflict{Msg: "Gateway runtime already owns profile " + d}
		}
	}
	return release, nil
}
