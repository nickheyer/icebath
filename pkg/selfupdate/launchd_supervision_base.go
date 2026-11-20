package selfupdate

import (
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Seconds a launchd restart may take to show a supervised gateway, past the respawn throttle
const launchdSupervisionVerifyTimeout = 20.0

// Clock, sleep and fallback probe of the supervision wait, replaced by tests
var (
	supervisionMonotonic   = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	supervisionSleep       = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
	supervisionUnsupported = gateway.LaunchdUnsupportedMarkerExists
)
