package gateway

// Loop liveness watchdog defaults, a probe every 30 seconds, 10 seconds to answer, 3 strikes
const (
	DefaultLoopWatchdogIntervalS  = 30.0
	DefaultLoopWatchdogTimeoutS   = 10.0
	DefaultLoopWatchdogMaxStrikes = 3
)
