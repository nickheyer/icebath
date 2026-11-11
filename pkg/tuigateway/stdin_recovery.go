package tuigateway

import (
	"fmt"
	"strings"
	"time"
)

// Spurious stdin EOF recoveries allowed per minute before the reader gives up
const MaxRecoveriesPerMinute = 10

// O_NONBLOCK and SO_RCVTIMEO state of fd 0 for crash log forensics
func DiagnoseStdinState() string {
	parts := stdinFlagParts()
	if tv, ok := stdinRcvTimeo(); ok {
		parts = append(parts, fmt.Sprintf("SO_RCVTIMEO=%d", tv))
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ", ")
}

// Recovers a spurious empty read of stdin, true when the reader should continue and false on a genuine close or rate limit
func HandleSpuriousEOF(recoveryTimes *[]float64, logFn func(string)) bool {
	if !stdinRecoverable() || !stdinNonblock() {
		logFn("stdin EOF (peer closed)")
		return false
	}
	now := float64(time.Now().UnixNano()) / 1e9
	kept := (*recoveryTimes)[:0]
	for _, t := range append(*recoveryTimes, now) {
		if t > now-60 {
			kept = append(kept, t)
		}
	}
	*recoveryTimes = kept
	if len(kept) > MaxRecoveriesPerMinute {
		logFn(fmt.Sprintf("stdin spurious-EOF recovery rate exceeded (%d/min, cap %d)", len(kept), MaxRecoveriesPerMinute))
		return false
	}
	logFn("stdin spurious EOF (subprocess O_NONBLOCK flip), recovering: " + DiagnoseStdinState())
	restoreStdinBlocking()
	return true
}
