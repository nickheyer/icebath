package tools

import (
	"runtime"
	"time"
)

// Gap under which a stopped process counts as clock sync or scheduling noise
const sleepThresholdSeconds = 5.0

var sleepClockStart = time.Now()

// Seconds the machine has spent awake, the unbiased interrupt time on Windows and the monotonic clock elsewhere
var AwakeSeconds = awakeClock()

// Clock that stops while the computer sleeps, the Windows unbiased interrupt time when kernel32 answers it
func awakeClock() func() float64 {
	if runtime.GOOS == "windows" {
		if clock := windowsUnbiasedClock(); clock != nil {
			return clock
		}
	}
	return func() float64 { return time.Since(sleepClockStart).Seconds() }
}

// Wall time a caller checking every interval seconds stood still past one interval
type SleepWatch struct {
	interval float64
	wall     float64
}

// Watch for a caller checking every interval seconds, its wall clock read now
func NewSleepWatch(interval float64) *SleepWatch {
	return &SleepWatch{interval: interval, wall: nowSeconds()}
}

// Seconds stopped since the previous call, zero for gaps under five seconds
func (w *SleepWatch) Slept() float64 {
	wall := nowSeconds()
	stopped := wall - w.wall - w.interval
	w.wall = wall
	if stopped >= sleepThresholdSeconds {
		return stopped
	}
	return 0
}
