package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// NousPortalSectionAccess reports the account entitled for the capability, asking through confirm before a Portal login when signed out
func NousPortalSectionAccess(env PoolEnv, out io.Writer, capability, coverageCategory string, confirm func(question string) (bool, error)) bool {
	if capability == "" {
		capability = "the Nous Tool Gateway"
	}
	entitled := func(info *NousPortalAccountInfo) bool {
		if info == nil {
			return false
		}
		if coverageCategory != "" {
			return info.ToolGatewayEntitledFor(coverageCategory)
		}
		return info.ToolGatewayEntitled()
	}
	info := nousSubAccountInfoFn(env, true)
	if !entitled(info) && (info == nil || !info.LoggedIn) {
		if !nousSectionPortalLogin(env, out, capability, confirm) {
			return false
		}
		info = nousSubAccountInfoFn(env, true)
	}
	if entitled(info) {
		return true
	}
	message := FormatNousPortalEntitlementMessageWith(info, NousEntitlementOptions{Capability: capability, CoverageCategory: coverageCategory})
	for _, line := range pySplitLines(message) {
		fmt.Fprintf(out, "  %s\n", line)
	}
	return false
}

// Seam over the Portal device login that persists credentials only
var nousSectionLoginPersist = nousLoginOnlyPersist

// Portal device login after a confirmation, leaving the active provider and model untouched
func nousSectionPortalLogin(env PoolEnv, out io.Writer, capability string, confirm func(question string) (bool, error)) bool {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s requires a Nous Portal login.\n", capability)
	proceed, err := confirm("Log in to Nous Portal now?")
	if err != nil {
		return false
	}
	if !proceed {
		fmt.Fprintln(out, "  Skipped Nous Portal login.")
		return false
	}
	handoff, err := nousSectionLoginPersist(env, out)
	if err != nil {
		var exit *NousLoginExitError
		var cancelled *DeviceFlowCancelledError
		var signInCancelled *SignInCancelledError
		switch {
		case errors.As(err, &exit):
		case errors.Is(err, context.Canceled) || errors.As(err, &cancelled) || errors.As(err, &signInCancelled):
			fmt.Fprintln(out, "\n  Login cancelled.")
		default:
			fmt.Fprintf(out, "  Nous Portal login failed: %v\n", err)
		}
		return false
	}
	fmt.Fprintln(out, "  Nous Portal login successful.")
	for _, view := range handoff {
		for _, line := range guestHandoffResultLines(view) {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	return true
}
