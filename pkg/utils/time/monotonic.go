package timeutil

import "time"

var (
	monotonicStart = time.Now()
	monotonicBase  = monotonicOrigin()
)

// Seconds on the system monotonic clock counted from boot, so a zero stamp always reads as long ago and the difference of two readings is the elapsed time
func Monotonic() float64 { return monotonicBase + float64(time.Since(monotonicStart))/1e9 }

// Boot clock reading at process start, else wall clock seconds since the epoch so a zero stamp still reads as long ago
func monotonicOrigin() float64 {
	if s, ok := bootSeconds(); ok {
		return s
	}
	return Epoch(monotonicStart)
}
