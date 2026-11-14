package selfupdate

import (
	"context"
	"math"
	"testing"
)

const lsLabel = "ai.icebath.gateway"

// Fake monotonic clock that advances only through the sleeps of the code under test
type lsClock struct {
	now   float64
	slept []float64
}

func lsInstall(t *testing.T, unsupported bool, results ...any) (*lsClock, *[]string) {
	t.Helper()
	clock := &lsClock{now: 1000}
	vsSwap(t, &supervisionMonotonic, func() float64 { return clock.now })
	vsSwap(t, &supervisionSleep, func(s float64) { clock.slept = append(clock.slept, s); clock.now += s })
	vsSwap(t, &supervisionUnsupported, func(context.Context) bool { return unsupported })
	calls := &[]string{}
	vsSwap(t, &fleetLaunchctlSupervised, func(_ context.Context, label string) (int, bool) {
		*calls = append(*calls, label)
		r := results[min(len(*calls)-1, len(results)-1)]
		switch v := r.(type) {
		case bool:
			return 4242, v
		case int:
			return v, true
		}
		return 0, false
	})
	return clock, calls
}

func lsSum(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

func TestWaitForLaunchdGatewaySupervisionReturnsTrueWhenAlreadySupervised(t *testing.T) {
	clock, _ := lsInstall(t, false, true)
	if ok, err := waitForLaunchdGatewaySupervision(context.Background(), lsLabel, 0, false); !ok || err != nil || len(clock.slept) != 0 {
		t.Fatal(ok, err, clock.slept)
	}
}

func TestWaitForLaunchdGatewaySupervisionWaitsOutTheLaunchdRespawnThrottle(t *testing.T) {
	results := make([]any, 0, 21)
	for range 20 {
		results = append(results, false)
	}
	clock, _ := lsInstall(t, false, append(results, true)...)
	if ok, err := waitForLaunchdGatewaySupervision(context.Background(), lsLabel, 0, false); !ok || err != nil || math.Abs(lsSum(clock.slept)-10.0) > 1e-6 {
		t.Fatal(ok, err, lsSum(clock.slept))
	}
}

func TestWaitForLaunchdGatewaySupervisionGivesUpAtTheDeadline(t *testing.T) {
	clock, _ := lsInstall(t, false, false)
	if ok, err := waitForLaunchdGatewaySupervisionWithin(context.Background(), lsLabel, 0, false, 20.0, 0.5); ok || err != nil || lsSum(clock.slept) > 20.0 {
		t.Fatal(ok, err, lsSum(clock.slept))
	}
}

func TestWaitForLaunchdGatewaySupervisionDetachedFallbackIsNotAFailure(t *testing.T) {
	clock, calls := lsInstall(t, true, false)
	if ok, err := waitForLaunchdGatewaySupervision(context.Background(), lsLabel, 0, false); !ok || err != nil || len(*calls) != 0 || len(clock.slept) != 0 {
		t.Fatal(ok, err, *calls)
	}
}
