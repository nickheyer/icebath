//go:build !windows

package gateway

import (
	"os"
	"strconv"
)

// Subject a managed worker's adoption ticket carries, the owner's POSIX uid
func ownerWorkerSubject() string {
	return "uid:" + strconv.Itoa(os.Getuid())
}
