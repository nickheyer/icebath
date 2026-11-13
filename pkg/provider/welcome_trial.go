package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Shared seam over the install state probe so tests can script the machine check
var (
	welcomeInstallProbe   = tools.InstallStateProbe
	welcomeProbeClear     = tools.InstallStateProbeClear
	welcomeTrialReserved  = tools.TrialReserved
	welcomeMarkUsed       = tools.MarkMachineUsed
	welcomeClaimLock      = tools.ClaimLock
	welcomeResolveSkipped = resolveProviderSkippingFreeTier
)

// Nous identity check of the claim blocker, replaced by a case as the hermes cases patch _has_nous_identity
var welcomeHasNousIdentity = hasNousIdentity

// Whether the profile or the shared store holds a Nous identity
func hasNousIdentity(env PoolEnv) bool {
	shared, _ := readSharedNousState(env)
	return currentNousState(env) != nil || shared != nil
}

// Something besides the welcome trial carries inference, asked of the resolver with the free tier hidden
func welcomeProviderConfigured(env PoolEnv) (bool, error) {
	provider, err := welcomeResolveSkipped(env, "auto")
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return false, nil
		}
		return false, err
	}
	return provider != "nous" || !hasGuest(env), nil
}

// A first chat can run here through the trial, claimed or claimable now, or a configured provider
func welcomeInferenceAvailable(env PoolEnv, assumeFresh bool) (bool, error) {
	if profileFreeInferenceActive(env) {
		return true, nil
	}
	configured, err := welcomeProviderConfigured(env)
	if err != nil || configured {
		return configured, err
	}
	reason, err := welcomeClaimBlocker(env, assumeFresh, &configured)
	return reason == "", err
}

func welcomeMachineIsUsed(ctx context.Context, assumeFresh bool) bool {
	return !assumeFresh && welcomeInstallProbe(ctx).Existing() && !welcomeTrialReserved()
}

// Why the trial cannot be claimed here, empty when it can, the machine check skipped when assumed fresh
func welcomeClaimBlocker(env PoolEnv, assumeFresh bool, configured *bool) (string, error) {
	if !onLocalDesktopBackend(env) {
		return "not the desktop's local backend", nil
	}
	if !guestAllowed(env) {
		return "guests are switched off", nil
	}
	if welcomeHasNousIdentity(env) {
		return "a Nous identity exists", nil
	}
	isConfigured := false
	if configured != nil {
		isConfigured = *configured
	} else {
		var err error
		if isConfigured, err = welcomeProviderConfigured(env); err != nil {
			return "", err
		}
	}
	if isConfigured {
		return "a provider is configured", nil
	}
	if welcomeMachineIsUsed(env.ctx(), assumeFresh) {
		return "Icebath was used on this machine", nil
	}
	return "", nil
}

// Welcome trial claim of every caller, replaced by a case as the hermes cases patch claim_desktop_free_tier
var ClaimDesktopFreeTier = claimDesktopFreeTier

// Mints the stamped guest and marks the machine under the claim lock, nil when the trial is unavailable
func claimDesktopFreeTier(env PoolEnv, assumeFresh, force bool) (map[string]any, error) {
	unlock, err := welcomeClaimLock()
	if err != nil {
		return nil, err
	}
	state, err := func() (map[string]any, error) {
		defer unlock()
		welcomeProbeClear()
		if _, err := SettleInterruptedClaim(env); err != nil {
			return nil, err
		}
		reason, err := welcomeClaimBlocker(env, assumeFresh, nil)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			env.log().Debug(fmt.Sprintf("welcome trial not claimed: %s", reason))
			return nil, nil
		}
		state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true, Force: force, WelcomeClaim: true})
		if err != nil {
			return nil, err
		}
		if state == nil {
			failure := lastMintFailure(env)
			message := AnonFailureCopy[AnonServerError]
			if pyTruthy(failure["error"]) {
				message = fmt.Sprint(failure["error"])
			}
			code := AnonServerError
			if pyTruthy(failure["error_code"]) {
				code = fmt.Sprint(failure["error_code"])
			}
			wait, has := anyFloat(failure["retry_after"])
			return nil, anonErr(message, code, time.Duration(wait*float64(time.Second)), has && failure["retry_after"] != nil)
		}
		if err := welcomeMarkUsed(env.ctx()); err != nil {
			return nil, err
		}
		return state, nil
	}()
	if err != nil || state == nil {
		return nil, err
	}
	env.log().Info("Nous free tier: welcome trial claimed on this machine")
	return state, nil
}

// Drops the reservation a claim cut off before its marker write left behind, true when it did
func SettleInterruptedClaim(env PoolEnv) (bool, error) {
	if !welcomeTrialReserved() {
		return false, nil
	}
	shared, _ := readSharedNousState(env)
	stamped := false
	for _, s := range []map[string]any{currentNousState(env), shared} {
		if isGuestState(s) && s[WelcomeClaimFlag] == true {
			stamped = true
		}
	}
	if !stamped {
		return false, nil
	}
	if err := welcomeMarkUsed(env.ctx()); err != nil {
		return false, err
	}
	env.log().Info("Nous free tier: an interrupted welcome-trial claim was settled")
	return true, nil
}
