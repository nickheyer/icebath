package state

import (
	"log/slog"
	"math"
	"time"
)

// One turn lease acquire attempt with its write patience, the try_acquire_session_turn_lease of a store
type TurnLeaseTry func(sessionID, holder string, ttlSeconds float64, patienceS *float64) (bool, error)

// Polls a store's turn lease try until acquired, aborted or out of time, a nil sleep raising the missing attribute
func AcquireTurnLeasePolling(owner string, try TurnLeaseTry, sleepRetry func(deadline, patienceS float64) bool, sessionID, holder string, o TurnLeaseWait) (bool, error) {
	deadline := monotonicSeconds() + math.Max(0, o.WaitSeconds)
	patience := o.AcquirePatienceS
	var waitStarted, lastNotice *float64
	noticeEvery := math.Max(0, o.WaitNoticeIntervalSeconds)
	for {
		if o.ShouldAbort != nil {
			abort, err := o.ShouldAbort()
			if err != nil {
				slog.Debug("session turn lease should_abort callback failed", "error", err)
			} else if abort {
				return false, nil
			}
		}
		p := patience
		ok, err := try(sessionID, holder, o.TTLSeconds, &p)
		if err != nil {
			if ClassifyPersistenceError(err) != "locked" {
				return false, err
			}
			if o.OnContended != nil {
				if cerr := o.OnContended(); cerr != nil {
					slog.Debug("session turn lease on_contended callback failed", "error", cerr)
				}
			}
			patience = math.Min(o.AcquirePatienceS+math.Max(0, o.PollIntervalSeconds), math.Max(o.AcquirePatienceS, deadline-monotonicSeconds()))
			if sleepRetry == nil {
				return false, &stateTypedError{"AttributeError", "'" + owner + "' object has no attribute '_sleep_before_write_retry'"}
			}
			if !sleepRetry(deadline, 0) {
				return false, nil
			}
			continue
		}
		if ok {
			return true, nil
		}
		patience = o.AcquirePatienceS
		t := monotonicSeconds()
		remaining := deadline - t
		if remaining <= 0 {
			return false, nil
		}
		if waitStarted == nil {
			waitStarted = &t
		}
		if o.OnWait != nil && (lastNotice == nil || noticeEvery == 0 || t-*lastNotice >= noticeEvery) {
			if werr := o.OnWait(math.Max(0, t-*waitStarted)); werr != nil {
				slog.Debug("session turn lease on_wait callback failed", "error", werr)
			}
			lastNotice = &t
		}
		time.Sleep(time.Duration(math.Min(math.Max(0.01, o.PollIntervalSeconds), remaining) * float64(time.Second)))
	}
}
