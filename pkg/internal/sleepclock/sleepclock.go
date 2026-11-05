package sleepclock

import "time"

var processStart = time.Now()

// Seconds on a clock that stops while the computer sleeps
var AwakeSeconds = awakeClock()

// Monotonic seconds since process start, the suspend excluding clock on Linux and macOS
func monotonicSeconds() float64 { return time.Since(processStart).Seconds() }
