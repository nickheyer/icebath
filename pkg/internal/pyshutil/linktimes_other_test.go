//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package pyshutil

import (
	"testing"
	"time"
)

// Skips where no call sets the times of a symlink itself
func setLinkTimes(t *testing.T, _ string, _ time.Time) {
	t.Helper()
	t.Skip("no call sets symlink times on this platform")
}
