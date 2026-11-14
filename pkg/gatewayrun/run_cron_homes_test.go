package gatewayrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Pins the liveness answer the cron gate reads for one case
func pinCronGateLiveness(t *testing.T, answer func(gateway.LivenessOptions) gateway.GatewayLiveness) {
	t.Helper()
	previous := cronGateLiveness
	cronGateLiveness = answer
	t.Cleanup(func() { cronGateLiveness = previous })
}

func TestCronProfileGateStandsDownForAProfileWithItsOwnGateway(t *testing.T) {
	pinCronGateLiveness(t, func(gateway.LivenessOptions) gateway.GatewayLiveness {
		return gateway.GatewayLiveness{Running: true, PID: int64(os.Getpid() + 1), Source: "pid"}
	})
	if cronProfileGate("b", filepath.Join(t.TempDir(), "home-b")) {
		t.Fatal("another process owns this home's gateway, the ticker must not visit it")
	}
}

func TestCronProfileGateKeepsTickingAHomeThisProcessServes(t *testing.T) {
	for _, source := range []string{"pid", "multiplexer", "runtime_status"} {
		pinCronGateLiveness(t, func(gateway.LivenessOptions) gateway.GatewayLiveness {
			return gateway.GatewayLiveness{Running: true, PID: int64(os.Getpid()), Source: source}
		})
		if !cronProfileGate("b", filepath.Join(t.TempDir(), "home-b")) {
			t.Fatal(source)
		}
	}
}

func TestCronProfileGateKeepsTickingAHomeWithNoGateway(t *testing.T) {
	pinCronGateLiveness(t, func(gateway.LivenessOptions) gateway.GatewayLiveness {
		return gateway.GatewayLiveness{Running: false, PID: nil, Source: "none"}
	})
	if !cronProfileGate("b", filepath.Join(t.TempDir(), "home-b")) {
		t.Fatal("a home with no gateway must be ticked")
	}
}

func TestCronProfileGateProbeFailureTicksRatherThanSilentlyDroppingCron(t *testing.T) {
	pinCronGateLiveness(t, func(gateway.LivenessOptions) gateway.GatewayLiveness { panic("probe exploded") })
	if !cronProfileGate("b", filepath.Join(t.TempDir(), "home-b")) {
		t.Fatal("a raising probe must not be read as another gateway owning the home")
	}
}
