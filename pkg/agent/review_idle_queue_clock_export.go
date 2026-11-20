package agent

import (
	"math"
	"time"
)

// Quiet period clock of the process review idle queue, the QUEUE.quiet_period the idle memory trim reads
type ReviewQueueQuietClock struct{}

// Time since the last live turn ended with its stamp, false while a turn runs or before any turn
func (ReviewQueueQuietClock) QuietPeriod() (time.Duration, time.Time, bool) {
	quietFor, since := reviewQueue.quietPeriod()
	if since == nil {
		return 0, time.Time{}, false
	}
	sec, frac := math.Modf(*since)
	return time.Duration(quietFor * float64(time.Second)), time.Unix(int64(sec), int64(frac*1e9)), true
}
