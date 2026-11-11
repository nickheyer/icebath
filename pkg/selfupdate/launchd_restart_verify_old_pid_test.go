package selfupdate

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestInvokingProfileIsVerifiedLikeItsSiblingsRestartThatLeavesTheOldPidSupervisedIsNotARestart(t *testing.T) {
	job := ownLaunchd(t, true, nil, true)
	vsSwap(t, &fleetWaitLaunchdSupervised, func(ctx context.Context, label string, oldPid int, haveOld bool) (bool, error) {
		return waitForLaunchdGatewaySupervision(ctx, label, oldPid, haveOld)
	})
	clock := &lsClock{now: 1000}
	vsSwap(t, &supervisionMonotonic, func() float64 { return clock.now })
	vsSwap(t, &supervisionSleep, func(s float64) { clock.slept = append(clock.slept, s); clock.now += s })
	vsSwap(t, &supervisionUnsupported, func(context.Context) bool { return false })
	var listings [][]string
	vsSwap(t, &fleetLaunchctlSupervised, func(_ context.Context, label string) (int, bool) {
		listings = append(listings, []string{"launchctl", "list", label})
		return gateway.ParseLaunchdPIDFromListOutput("\t\"PID\" = 4242;\n")
	})
	var out bytes.Buffer
	restarted, failed := runOwnFleetRestart(t, &out)
	if job.restarts != 1 || len(restarted) != 0 || !reflect.DeepEqual(failed, []string{"ai.icebath.gateway"}) ||
		!strings.Contains(out.String(), "ai.icebath.gateway") {
		t.Fatalf("restarts=%d restarted=%v failed=%v out=%q", job.restarts, restarted, failed, out.String())
	}
	if len(listings) <= 1 || lsSum(clock.slept) > launchdSupervisionVerifyTimeout {
		t.Fatalf("listings=%d slept=%v", len(listings), lsSum(clock.slept))
	}
}

func TestInvokingProfileIsVerifiedLikeItsSiblingsVerificationBudgetClearsTheRespawnThrottle(t *testing.T) {
	if launchdSupervisionVerifyTimeout < 15.0 {
		t.Fatalf("budget %v is under the launchd respawn throttle", launchdSupervisionVerifyTimeout)
	}
}
