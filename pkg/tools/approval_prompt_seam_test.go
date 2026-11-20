package tools

import "time"

// Replaces the transport clock and returns the previous one, nil standing for the real clock
func swapTransportClock(fn func() time.Time) func() time.Time {
	var prev *func() time.Time
	if fn == nil {
		prev = transportClock.Swap(nil)
	} else {
		prev = transportClock.Swap(&fn)
	}
	if prev == nil {
		return nil
	}
	return *prev
}
