package pydeadline

import "time"

// Calls pred every interval until it holds or the timeout passes, the last sleep cut to the time left
func PollUntil[T any](pred func() (T, bool), timeout, interval time.Duration) T {
	deadline := time.Now().Add(timeout)
	for {
		result, ok := pred()
		if ok {
			return result
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return result
		}
		time.Sleep(min(interval, remaining))
	}
}
