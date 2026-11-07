//go:build unix

package manage

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Flocks each existing gateway.lock like gateway/runtime_ownership.py, a held one refusing maintenance
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
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			release()
			return nil, &gatewayOwnershipConflict{Msg: "Gateway runtime already owns profile " + d}
		}
	}
	return release, nil
}
