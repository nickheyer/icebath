//go:build !icebath_core && !icebath_noextras

package gatewayrun

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
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

func TestQueuedWorkClientsAdaptersAndCronEachKeepItUp(t *testing.T) {
	r, h, db := idleExitRunner(t)
	ctx := context.Background()
	if got := busyReason(ctx, r, 0.2); got != "" {
		t.Fatalf("idle runner busy: %s", got)
	}
	epoch, admission := queueAdmission(t, db, "queued while idle")
	if got := busyReason(ctx, r, 0.2); !strings.Contains(got, "admissions pending") {
		t.Fatalf("queued admission: %q", got)
	}
	if _, err := db.CancelSessionInput(ctx, epoch, admission); err != nil {
		t.Fatal(err)
	}
	release := attachedClient(r)
	if got := busyReason(ctx, r, 0.2); !strings.Contains(got, "client(s) attached") {
		t.Fatalf("attached client: %q", got)
	}
	release()
	if got := busyReason(ctx, r, 60); !strings.Contains(got, "client activity") {
		t.Fatalf("recent activity: %q", got)
	}
	r.idleExitLastActivity.Store(math.Float64bits(timeutil.Monotonic() - 120))
	if _, err := r.sessionTicketStore.Mint(h, "u", "interactive", "", false); err != nil {
		t.Fatal(err)
	}
	if got := busyReason(ctx, r, 0.2); got != "client attaching" {
		t.Fatalf("outstanding ticket: %q", got)
	}
	r.sessionTicketStore.Revoke()
	r.config.Platforms[gateway.PlatformTelegram] = &gateway.PlatformConfig{Enabled: true}
	if got := busyReason(ctx, r, 0.2); !strings.Contains(got, "messaging") {
		t.Fatalf("configured adapter: %q", got)
	}
	clear(r.config.Platforms)
	prompt, jobName := "ping", "j"
	if _, err := cron.CreateJob(ctx, h, cron.CreateJobOptions{Prompt: &prompt, Schedule: "every 1h", Name: &jobName}); err != nil {
		t.Fatal(err)
	}
	if got := busyReason(ctx, r, 0.2); !strings.Contains(got, "cron jobs scheduled") {
		t.Fatalf("scheduled cron job: %q", got)
	}
	if err := os.WriteFile(filepath.Join(h, "cron", "jobs.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := busyReason(ctx, r, 0.2); !strings.Contains(got, "idle probe failed") {
		t.Fatalf("unreadable cron store: %q", got)
	}
}
