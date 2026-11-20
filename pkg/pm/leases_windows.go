//go:build windows

package pm

import (
	"os"

	"golang.org/x/sys/windows"
)

// Opens an existing lease sharing delete so the pruner can unlink it while holding its lock
func openLeaseProbe(lease string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(lease)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: lease, Err: err}
	}
	return os.NewFile(uintptr(h), lease), nil
}
