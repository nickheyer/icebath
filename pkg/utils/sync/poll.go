package syncutil

import (
	"context"
	"time"
)

// Calls cond at once and then every interval until it reports done, timeout has passed or ctx ends, giving back the value of its last call; the last wait is cut to the time left
func PollUntil[T any](ctx context.Context, timeout, interval time.Duration, cond func() (T, bool)) T {
	deadline := time.Now().Add(timeout)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		v, done := cond()
		if done {
			return v
		}
		left := time.Until(deadline)
		if left <= 0 || ctx.Err() != nil {
			return v
		}
		wait := min(interval, left)
		if timer == nil {
			timer = time.NewTimer(wait)
		} else {
			timer.Reset(wait)
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			return v
		}
	}
}
