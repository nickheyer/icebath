package cron

// Seconds since the ticker of a home last iterated, false when never or unknown
func TickerHeartbeatAge(h string) (float64, bool) { return openCronStore(h, nil).TickerHeartbeatAge() }

// Terminal attempt states of the executions ledger
var TerminalExecutionStates = map[string]bool{"completed": true, "failed": true, "unknown": true, "cancelled": true, "missed": true}
