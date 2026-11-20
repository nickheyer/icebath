//go:build !windows

package gateway

import (
	"fmt"
	"os"
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
