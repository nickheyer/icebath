package gateway

import (
	"context"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Context whose home is the profile home given, the caller's own home when empty
func profileHomeContext(ctx context.Context, profileHome string) context.Context {
	if profileHome == "" {
		return ctx
	}
	return home.WithOverride(ctx, profileHome)
}

// False for an attestation whose timestamp is missing, unparsable or past the horizon
func attestationWithinHorizon(data any) bool {
	obj, ok := data.(*pyjson.Object)
	if !ok || obj == nil {
		return false
	}
	raw, present := obj.Get("ts")
	if !present {
		return false
	}
	stamp, aware, err := pydatetime.ParseISOFormat(pyrepr.Str(raw))
	if err != nil {
		return false
	}
	if !aware {
		stamp = time.Date(stamp.Year(), stamp.Month(), stamp.Day(), stamp.Hour(), stamp.Minute(), stamp.Second(), stamp.Nanosecond(), time.UTC)
	}
	age := float64(time.Now().UnixNano())/1e9 - float64(stamp.UnixNano())/1e9
	return -attestationClockSlackS <= age && age <= startAttestationMaxAgeS
}

// Marker instance identity of an attestation, false when it carries none
func attestationGeneration(data any) (string, bool) {
	obj, ok := data.(*pyjson.Object)
	if !ok || obj == nil {
		return "", false
	}
	value, _ := obj.Get("generation")
	if !pyrepr.Truthy(value) {
		return "", false
	}
	return pyrepr.Str(value), true
}

// Clears the start attestation of a profile home only while it is still the generation acted on
func ConsumeStartAttestation(ctx context.Context, generation, profileHome string) {
	scoped := profileHomeContext(ctx, profileHome)
	if current, ok := attestationGeneration(readStartAttestation(scoped)); ok && current == generation {
		clearStartAttestation(scoped)
	}
}

// Generation of an attestation vouching for gateway pids gone without a clean exit, false when undecidable
func AttestedDeathGeneration(ctx context.Context, currentPids []int, profileHome string) (string, bool) {
	scoped := profileHomeContext(ctx, profileHome)
	data := readStartAttestation(scoped)
	attested := attestedPidsFrom(data)
	if len(attested) == 0 || !attestationWithinHorizon(data) || !attestedDead(scoped, attested, currentPids, data) {
		return "", false
	}
	return attestationGeneration(data)
}

// Live gateway pids of one profile home's pid file, else of the process table, after an optional filter
func LiveGatewayPidsIn(ctx context.Context, allProfiles bool, profileHome string, pidFilter func([]int) ([]int, error)) ([]int, error) {
	var pids []int
	if profileHome != "" {
		pid, ok, err := GetRunningPid(filepath.Join(profileHome, "gateway.pid"), false)
		if err != nil {
			return nil, err
		}
		if ok && pid != 0 {
			pids = []int{pid}
		}
	} else {
		var err error
		if pids, err = FindGatewayPIDsFor(ctx, nil, allProfiles); err != nil {
			return nil, err
		}
	}
	if pidFilter != nil {
		return pidFilter(pids)
	}
	return pids, nil
}

// The last pids seen through the confirmation window, empty when the gateway vanished meanwhile
func confirmGatewayStableIn(ctx context.Context, initial []int, confirmS, intervalS float64, allProfiles bool, profileHome string, pidFilter func([]int) ([]int, error)) ([]int, error) {
	if confirmS <= 0 {
		return initial, nil
	}
	pids := initial
	deadline := pythread.Monotonic() + confirmS
	for pythread.Monotonic() < deadline {
		winSleep(time.Duration(intervalS * float64(time.Second)))
		var err error
		if pids, err = LiveGatewayPidsIn(ctx, allProfiles, profileHome, pidFilter); err != nil {
			return nil, err
		}
		if len(pids) == 0 {
			return nil, nil
		}
	}
	return pids, nil
}

// Polls for a gateway that stays visible through the confirmation window, empty at the timeout
func WaitForGatewayReadyIn(ctx context.Context, timeoutS float64, allProfiles bool, profileHome string, pidFilter func([]int) ([]int, error)) ([]int, error) {
	deadline := pythread.Monotonic() + timeoutS
	for pythread.Monotonic() < deadline {
		pids, err := LiveGatewayPidsIn(ctx, allProfiles, profileHome, pidFilter)
		if err != nil {
			return nil, err
		}
		if len(pids) > 0 {
			confirmed, err := confirmGatewayStableIn(ctx, pids, windowsReadyConfirmS, windowsReadyIntervalS, allProfiles, profileHome, pidFilter)
			if err != nil {
				return nil, err
			}
			if len(confirmed) > 0 {
				return confirmed, nil
			}
			continue
		}
		winSleep(time.Duration(windowsReadyIntervalS * float64(time.Second)))
	}
	return []int{}, nil
}

// Launches the gateway of a profile home fully detached, the caller's own gateway when empty
func SpawnDetachedIn(ctx context.Context, profileHome string) (int, error) {
	return spawnWindowsDetached(ctx, profileHome)
}
