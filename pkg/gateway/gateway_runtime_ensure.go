package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Swappable discovery and lock probes the ensure loop reads, the seams ensure tests replace
var (
	ensureDiscover           = discoverGatewayEndpoint
	ensureMissingOwnerState  = MissingOwnerState
	ensureCheckoutLockHeld   = func(ctx context.Context) (bool, error) { return updaterecord.CheckoutLockHeld(ctx, "") }
	ensureUpdateFenced       = updateFenced
	ensureSleep              = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
	ensureMultiplexerServes  = MultiplexerServesHome
	ensureImpliedHostRoot    = ImpliedHostRoot
	ensureStandaloneReasonAt = RecordedStandaloneReason
)

// Root whose pending reservation will serve home under its boot policy, empty otherwise
func startingMultiplexerRoot(ctx context.Context, h string) (string, error) {
	root, err := ensureImpliedHostRoot(ctx, h)
	if err != nil || root == "" {
		return "", err
	}
	if ensureMissingOwnerState(root) == "starting" {
		return root, nil
	}
	return "", nil
}

// Guard refusal a live standalone host published for its named profiles, bounded to 500 characters
func standaloneMultiplexerReason(root string) string {
	reason := ensureStandaloneReasonAt(filepath.Join(root, "gateway_state.json"))
	if reason == "" {
		return ""
	}
	return pystr.Slice(reason, 0, 500)
}

// Whether an update owns the install by a live or in flight marker, or any marker while the checkout lock is held
func updateFenced(ctx context.Context, homes []string) (bool, error) {
	for _, fenceHome := range homes {
		path := filepath.Join(fenceHome, updaterecord.MarkerName)
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return true, nil
		}
		if len(raw) == 0 {
			info, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return true, nil
			}
			if float64(time.Now().UnixNano())/1e9-float64(info.ModTime().UnixNano())/1e9 < updaterecord.EmptyMarkerGraceSeconds {
				return true, nil
			}
		}
		if verdict, _, _ := updaterecord.JudgeMarker(raw, nil); verdict == "live" || verdict == "ours" {
			return true, nil
		}
		held, err := ensureCheckoutLockHeld(ctx)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

// Verdict while an update owns the install, its own attachable drain or update paused
func fencedVerdict(ctx context.Context, h string, timeout float64) (ObservedEndpoint, error) {
	observed, err := ensureDiscover(ctx, h, ensureSeconds(timeout))
	if err != nil {
		return ObservedEndpoint{}, err
	}
	if observed.State == "draining" && Attachable(observed) {
		return observed, nil
	}
	return ObservedEndpoint{State: "draining", ReasonCode: "update_paused"}, nil
}

// Duration of a float second budget
func ensureSeconds(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

const replaceFailedDetail = "an older Icebath gateway still owns this profile after a stop; run: icebath gateway stop"

// Draining reasons after which the owner goes away by itself
var retiringReasons = map[string]bool{"idle_exit": true, "replaced": true}

// Discovery that stops an older owner of this home once and reports it retiring, refusals keep their remedy
func observeReplacingStale(ctx context.Context, h string, deadline float64, replaced map[string]bool) (ObservedEndpoint, error) {
	budget, err := runtimeRemaining(deadline)
	if err != nil {
		return ObservedEndpoint{}, err
	}
	observed, err := ensureDiscover(ctx, h, ensureSeconds(budget))
	if err != nil {
		return ObservedEndpoint{}, err
	}
	if observed.State != "incompatible" || observed.ReasonCode != "runtime_protocol" {
		return observed, nil
	}
	if replaced[h] {
		return ObservedEndpoint{State: "incompatible", ReasonCode: "runtime_protocol", Detail: replaceFailedDetail}, nil
	}
	replaced[h] = true
	if refusal := replaceIncompatibleOwner(h, deadline); refusal != "" {
		detail, ok := ReplaceRefusalDetail[refusal]
		if !ok {
			detail = replaceFailedDetail
		}
		return ObservedEndpoint{State: "incompatible", ReasonCode: "runtime_protocol", Detail: detail}, nil
	}
	return ObservedEndpoint{State: "draining", ReasonCode: "replaced"}, nil
}

// Ensures with the default startup deadline
func ensureGatewayRuntime(ctx context.Context, h string, idleExit bool) (ObservedEndpoint, error) {
	return EnsureGatewayRuntime(ctx, h, DefaultEnsureTimeout.Seconds(), idleExit)
}

// Starts or awaits the owner of profile home once, a pending owner staying pending at the deadline
func EnsureGatewayRuntime(ctx context.Context, profileHome string, timeout float64, idleExit bool) (ObservedEndpoint, error) {
	if math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout <= 0 {
		return ObservedEndpoint{}, fmt.Errorf("timeout must be finite and positive")
	}
	deadline := pythread.Monotonic() + timeout
	canonical, err := CanonicalHome(profileHome)
	if err != nil {
		return ObservedEndpoint{}, err
	}
	h := overlayOwnerHome(canonical)
	observed, err := ensureLoop(ctx, h, deadline, idleExit)
	if err == nil {
		return observed, nil
	}
	var start *RuntimeStartError
	switch {
	case errors.Is(err, ErrRuntimeDeadline) || discoveryErrorKind(err) == "timeout":
		return ObservedEndpoint{State: "starting", ReasonCode: "deadline"}, nil
	case errors.As(err, &start):
		return ObservedEndpoint{State: start.State, ReasonCode: start.Reason}, nil
	case errors.Is(err, os.ErrPermission):
		return refusedSeam(h), nil
	case runtimeOSError(err):
		return ObservedEndpoint{State: "inaccessible", ReasonCode: "runtime_start_failed"}, nil
	}
	return ObservedEndpoint{}, err
}

// Body of the ensure loop, its errors mapped to verdicts by the caller
func ensureLoop(ctx context.Context, h string, deadline float64, idleExit bool) (ObservedEndpoint, error) {
	requested, retiring := false, false
	var child SpawnedGateway
	childHome, childOffset := h, int64(0)
	replaced := map[string]bool{}
	delay := 0.025
	for {
		fenced, err := ensureUpdateFenced(ctx, []string{h, processHermesHome(), home.Root()})
		if err != nil {
			return ObservedEndpoint{}, err
		}
		if fenced {
			budget, err := runtimeRemaining(deadline)
			if err != nil {
				return ObservedEndpoint{}, err
			}
			return fencedVerdict(ctx, h, budget)
		}
		status, exited := 0, false
		if child != nil {
			status, exited = child.Poll()
		}
		observed, err := observeReplacingStale(ctx, h, deadline, replaced)
		if err != nil {
			return ObservedEndpoint{}, err
		}
		if observed.ReasonCode == "control_timeout" {
			return ObservedEndpoint{State: "starting", ReasonCode: "deadline"}, nil
		}
		if observed.State == "draining" && retiringReasons[observed.ReasonCode] {
			retiring, requested, child = true, false, nil
			budget, err := runtimeRemaining(deadline)
			if err != nil {
				return ObservedEndpoint{}, err
			}
			ensureSleep(min(0.1, budget))
			continue
		}
		if observed.State != "absent" && observed.State != "starting" {
			return observed, nil
		}
		if exited && observed.State == "absent" {
			pid := child.Pid()
			return ObservedEndpoint{State: "inaccessible", ReasonCode: "runtime_exited", Detail: StartupFailureReport(ctx, childHome, childOffset, status, &pid)}, nil
		}
		if observed.State == "starting" && !retiring {
			requested = true
		}
		if observed.State == "absent" {
			retiring = false
		}
		reserved := ""
		if observed.State == "absent" && !requested {
			if reserved, err = startingMultiplexerRoot(ctx, h); err != nil {
				return ObservedEndpoint{}, err
			}
		}
		if reserved != "" {
			if reason := standaloneMultiplexerReason(reserved); reason != "" {
				return ObservedEndpoint{State: "inaccessible", ReasonCode: "profile_parked", Detail: reason}, nil
			}
			requested = true
		}
		if observed.State == "absent" && !requested {
			target, err := ensureMultiplexerServes(ctx, h)
			if err != nil {
				return ObservedEndpoint{}, err
			}
			if target == "" {
				target = h
			}
			service, err := discoverExistingGatewayService(ctx, target, deadline)
			if err != nil {
				return ObservedEndpoint{}, err
			}
			if service == nil && target == h {
				host, err := ensureImpliedHostRoot(ctx, h)
				if err != nil {
					return ObservedEndpoint{}, err
				}
				if host != "" {
					target = host
					if service, err = discoverExistingGatewayService(ctx, target, deadline); err != nil {
						return ObservedEndpoint{}, err
					}
				}
			}
			budget, err := runtimeRemaining(deadline)
			if err != nil {
				return ObservedEndpoint{}, err
			}
			if observed, err = ensureDiscover(ctx, h, ensureSeconds(budget)); err != nil {
				return ObservedEndpoint{}, err
			}
			if observed.State != "absent" {
				continue
			}
			if service != nil {
				if err := startExistingGatewayService(ctx, service, deadline); err != nil {
					return ObservedEndpoint{}, err
				}
			} else {
				childHome, childOffset = target, StdioLogSize(target)
				if child, err = spawnUnmanagedGateway(ctx, target, deadline, idleExit, false); err != nil {
					return ObservedEndpoint{}, err
				}
			}
			requested = true
		}
		budget, err := runtimeRemaining(deadline)
		if err != nil {
			return ObservedEndpoint{}, err
		}
		ensureSleep(min(delay, budget))
		delay = min(delay*1.5, 0.05)
	}
}
