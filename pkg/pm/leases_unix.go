//go:build !windows

package pm

import "os"

// Opens an existing lease for a lock probe
func openLeaseProbe(lease string) (*os.File, error) {
	return os.OpenFile(lease, os.O_RDWR, 0)
}
