package gateway

import "context"

// Step that schedules the FIFO drain of a session, the SessionAuthority._schedule method
type SessionSchedule func(ctx context.Context, a *SessionAuthority, ref SessionRef) error

// Installs the scheduling step every admission reaches and returns the one it replaced
func SwapSessionSchedule(schedule SessionSchedule) SessionSchedule {
	previous := scheduleSession
	scheduleSession = schedule
	return previous
}
