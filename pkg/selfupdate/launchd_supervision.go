package selfupdate

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Seconds a launchd restart may take to show a supervised gateway, past the respawn throttle
const launchdSupervisionVerifyTimeout = 20.0

// Clock, sleep and fallback probe of the supervision wait, replaced by hermes tests
var (
	supervisionMonotonic   = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	supervisionSleep       = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
	supervisionUnsupported = gateway.LaunchdUnsupportedMarkerExists
)

// True once launchd supervises a gateway pid other than the old one, at once on the detached fallback
func waitForLaunchdGatewaySupervision(ctx context.Context, label string, oldPid int, haveOld bool) (bool, error) {
	return waitForLaunchdGatewaySupervisionWithin(ctx, label, oldPid, haveOld, launchdSupervisionVerifyTimeout, 0.5)
}

// The supervision wait with an explicit budget and poll interval, the first probe at once
func waitForLaunchdGatewaySupervisionWithin(ctx context.Context, label string, oldPid int, haveOld bool, timeout, interval float64) (bool, error) {
	if supervisionUnsupported(ctx) {
		return true, nil
	}
	if label == "" {
		var err error
		if label, err = fleetLaunchdLabel(ctx); err != nil {
			return false, err
		}
	}
	deadline := supervisionMonotonic() + max(timeout, 0.0)
	interval = max(interval, 0.01)
	for {
		pid, ok := fleetLaunchctlSupervised(ctx, label)
		if ok && (!haveOld || pid != oldPid) {
			return true, nil
		}
		remaining := deadline - supervisionMonotonic()
		if remaining <= 0 {
			return false, nil
		}
		supervisionSleep(min(interval, remaining))
	}
}
