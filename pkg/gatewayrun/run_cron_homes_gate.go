//go:build !icebath_core && !icebath_noextras

package gatewayrun

import (
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Whether the ticker visits home this cycle, false only when another gateway process owns it
func cronProfileGate(name, profileHome string) (tick bool) {
	defer func() {
		if rec := recover(); rec != nil {
			pkgLog().Debug(fmt.Sprintf("Cron profile gate probe failed for %s (ticking it): %v", name, rec))
			tick = true
		}
	}()
	liveness := cronGateLiveness(gateway.LivenessOptions{
		ProfileDir: profileHome, NoCache: true,
		PidProbe: func(path string) (int, bool, error) { return gateway.GetRunningPid(path, false) },
	})
	return !(liveness.Running && liveness.PID != nil && !gateway.PyValuesEqual(liveness.PID, int64(os.Getpid())))
}
