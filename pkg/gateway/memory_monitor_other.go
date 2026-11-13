//go:build !unix && !windows

package gateway

// Reports no RSS where neither getrusage nor process memory counters exist
func readProcessRSSMB() (int, bool) { return 0, false }
