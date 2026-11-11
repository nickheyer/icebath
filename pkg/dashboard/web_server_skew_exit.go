package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	DefaultSkewPollS     = 30.0
	retireConfirmations  = 2
	unboundedRetirePolls = -1
)

// Boot and disk revisions when the checkout drifted since boot, nil otherwise
type Skew *[2]string

// Admission fence the retirement watchdogs exit through, proven idle before commit
type RetirementFence interface {
	Prepare() map[string]any
	Commit(token string) map[string]any
}

func skewExitLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.web_server_skew_exit")
}

// Retire only on a proven code change and never while the tree may be mid swap
func ShouldRetireForSkew(skew Skew, updateInProgress bool) bool {
	return skew != nil && !updateInProgress
}

// Whether an update owns the install, true when that cannot be read
func skewUpdateInProgress() bool {
	busy, err := updaterecord.UpdateInProgress(context.Background(), "")
	if err != nil {
		return true
	}
	return busy
}

// Goroutine exiting the server through the fence after two consecutive retirement reasons, closed when it stops
func runRetirementWatchdog(shouldExit *atomic.Bool, observe func() string, fence RetirementFence, pollS float64, maxPolls int) <-chan struct{} {
	if fence == nil {
		fence = provider.Retirement
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		seen, polls := 0, 0
		for !shouldExit.Load() && (maxPolls < 0 || polls < maxPolls) {
			polls++
			reason := observe()
			if reason != "" {
				seen++
			} else {
				seen = 0
			}
			if seen >= retireConfirmations {
				permit := fence.Prepare()
				token, _ := permit["token"].(string)
				if ok, _ := permit["ok"].(bool); ok {
					if committed, _ := fence.Commit(token)["ok"].(bool); committed {
						skewExitLog().Warn(reason)
						shouldExit.Store(true)
						return
					}
				}
			}
			time.Sleep(time.Duration(pollS * float64(time.Second)))
		}
	}()
	return done
}

// Goroutine setting the exit flag once the loaded code is provably stale and the backend provably idle
func StartCodeSkewWatchdog(shouldExit *atomic.Bool, skewFn func() Skew, updateProbe func() bool, fence RetirementFence, pollS float64, maxPolls int) <-chan struct{} {
	if skewFn == nil {
		skewFn = detectCodeSkew
	}
	if updateProbe == nil {
		updateProbe = skewUpdateInProgress
	}
	observe := func() string {
		skew := skewFn()
		if !(skew != nil && ShouldRetireForSkew(skew, updateProbe())) {
			return ""
		}
		return fmt.Sprintf("SSH-isolated backend loaded %s but the install is now at %s; "+
			"retiring so its client respawns it on the new code.", skew[0], skew[1])
	}
	return runRetirementWatchdog(shouldExit, observe, fence, pollS, maxPolls)
}

// Boot and disk revision labels of gateway code skew detection
func detectCodeSkew() Skew {
	boot, disk, ok := gateway.DetectCodeSkew()
	if !ok {
		return nil
	}
	return &[2]string{boot, disk}
}
