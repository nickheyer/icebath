package tools

import (
	"context"
	"math"
	"os"
	"strings"
	"time"
)

// Child environment marker naming the host that owns the pseudo terminal
const (
	PtyHostEnv       = "ICEBATH_PTY_HOST"
	PtyHostDashboard = "dashboard"
)

const (
	ptyMinDimension = 1
	ptyMaxCols      = 2000
	ptyMaxRows      = 1000
)

// Default bound on how long a write waits for a full child input buffer to drain
const PtyWriteTimeout = 10 * time.Second

// Write timeout meaning wait for as long as the child leaves its input unread
const PtyNoTimeout = time.Duration(-1)

const (
	ptyHelperShutdownGraceS    = 1.5
	ptyMaxHelperShutdownGraceS = 2.0
)

// PtyUnavailableError reports that no pseudo terminal can be created on this host
type PtyUnavailableError struct{ Msg string }

func (e *PtyUnavailableError) Error() string { return e.Msg }

// One non blocking write attempt, bytes accepted and false once the bridge closed
type PtyWriteAttempt func() (int, bool, error)

// Wraps every write attempt so a caller can hold a lock around exactly the bytes that reach the child
type PtyWriteFence func(attempt PtyWriteAttempt) (int, bool, error)

// Options of a bridge spawn, a nil Env building the agent subprocess environment
type PtySpawnOptions struct {
	Cwd  string
	Env  map[string]string
	Cols int
	Rows int
}

// TerminalBridge is the byte streaming surface the dashboard terminal drives on every platform
type TerminalBridge interface {
	Pid() int
	IsAlive() bool
	Read(timeout time.Duration) ([]byte, error)
	Write(ctx context.Context, data []byte, timeout time.Duration, fence PtyWriteFence) (bool, error)
	Resize(cols, rows int)
	Close() error
}

// Clamps into the valid dimension range, non numeric or non finite values falling back to the minimum
func clampDimension(value any, maximum int) int {
	var n int
	switch v := value.(type) {
	case int:
		n = v
	case int64:
		n = int(v)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return ptyMinDimension
		}
		n = int(v)
	case bool:
		if v {
			n = 1
		}
	case string:
		b, err := pyIntFromString(v, 10)
		if err != nil {
			return ptyMinDimension
		}
		if !b.IsInt64() {
			if b.Sign() < 0 {
				return ptyMinDimension
			}
			return maximum
		}
		n = int(b.Int64())
	default:
		return ptyMinDimension
	}
	return max(ptyMinDimension, min(n, maximum))
}

// Grace a dashboard helper gets for its own SIGHUP shutdown, half a second past the gateway grace
func ptyHelperShutdownGrace() float64 {
	gateway := 1.0
	if raw := os.Getenv("ICEBATH_TUI_GATEWAY_SHUTDOWN_GRACE_S"); strings.TrimFunc(raw, pyIsSpace) != "" {
		if f, err := pyFloatFromString(raw); err == nil {
			gateway = f
		}
	}
	if math.IsInf(gateway, 0) || math.IsNaN(gateway) || gateway <= 0 {
		gateway = 1.0
	}
	return min(max(ptyHelperShutdownGraceS, gateway+0.5), ptyMaxHelperShutdownGraceS)
}
