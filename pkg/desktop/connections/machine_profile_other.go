//go:build !linux && !darwin && !freebsd && !windows

package connections

import "time"

// Platforms without a recorded birth time report the home directory age as unknown
func fileBirthtime(string) (time.Time, bool) { return time.Time{}, false }
