package env

import (
	"fmt"
	"sync"
	"time"
)

type ActivityThrottle struct {
	cb       func(label string)
	interval time.Duration

	mu    sync.Mutex
	start time.Time
	last  time.Time
}

func NewActivityThrottle(cb func(label string), interval time.Duration) *ActivityThrottle {
	if interval <= 0 {
		interval = activityInterval
	}
	now := time.Now()
	return &ActivityThrottle{cb: cb, interval: interval, start: now, last: now}
}

func (a *ActivityThrottle) Touch(label string) {
	now := time.Now()
	a.mu.Lock()
	if now.Sub(a.last) < a.interval {
		a.mu.Unlock()
		return
	}
	a.last = now
	elapsed := int(now.Sub(a.start).Seconds())
	a.mu.Unlock()
	if a.cb != nil {
		a.cb(fmt.Sprintf("%s (%ds elapsed)", label, elapsed))
	}
}
