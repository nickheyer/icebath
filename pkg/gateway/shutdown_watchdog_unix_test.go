//go:build !windows

package gateway

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSweepStaleTickSocketsUnlinksDeadAndUnparsablePids(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "gateway.loop-tick.1.sock")
	live := filepath.Join(dir, "gateway.loop-tick."+strconv.Itoa(os.Getpid())+".sock")
	dead := filepath.Join(dir, "gateway.loop-tick.4206649.sock")
	junk := filepath.Join(dir, "gateway.loop-tick.x.sock")
	for _, p := range []string{own, live, dead, junk} {
		writeBytes(t, p, "")
	}
	sweepStaleTickSockets(own)
	for p, want := range map[string]bool{own: true, live: true, dead: false, junk: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists=%v want %v", p, err == nil, want)
		}
	}
}
