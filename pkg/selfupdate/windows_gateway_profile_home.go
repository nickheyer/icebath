package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Seconds a Windows gateway start waits for a live gateway when the caller names no budget
const windowsGatewayReadyTimeoutS = 6.0

// Generation of a start attestation whose gateway died without a clean exit, empty when undecidable
func attestedDeathGeneration(ctx context.Context, currentPids []int, profileHome string) (string, error) {
	generation, _ := gateway.AttestedDeathGeneration(ctx, currentPids, profileHome)
	return generation, nil
}

// Clears the start attestation of exactly one generation in a profile home
func consumeStartAttestation(ctx context.Context, generation, profileHome string) error {
	gateway.ConsumeStartAttestation(ctx, generation, profileHome)
	return nil
}

// Live gateway pids of a profile home, else of the active profile, after an optional filter
func liveGatewayPids(ctx context.Context, profileHome string, pidFilter func([]int) ([]int, error)) ([]int, error) {
	return gateway.LiveGatewayPidsIn(ctx, false, profileHome, pidFilter)
}

// Pid of a detached gateway spawned for a profile home, the caller's own gateway when empty
func spawnDetached(ctx context.Context, profileHome string) (int, error) {
	return gateway.SpawnDetachedIn(ctx, profileHome)
}

// Gateway pids that stayed visible through the confirmation window, empty at the timeout
func waitForGatewayReady(ctx context.Context, timeoutS *float64, profileHome string, allProfiles bool, pidFilter func([]int) ([]int, error)) ([]int, error) {
	timeout := windowsGatewayReadyTimeoutS
	if timeoutS != nil {
		timeout = *timeoutS
	}
	return gateway.WaitForGatewayReadyIn(ctx, timeout, allProfiles, profileHome, pidFilter)
}
