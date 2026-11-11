package selfupdate

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Prints what the update will touch and how, the install, its profiles and each running service's restart
func printUpdatePlan(w io.Writer, plan *UpdatePlan) {
	fmt.Fprintln(w, "Update plan:")
	install := "  Install: " + plan.InstallMethod
	if pyrepr.Truthy(plan.ExpectedVersion) {
		install += " (v" + pyrepr.Str(plan.ExpectedVersion)
		if pyrepr.Truthy(plan.ExpectedSHA) {
			install += " @ " + clip(pyrepr.Str(plan.ExpectedSHA), 8)
		}
		install += ")"
	}
	fmt.Fprintln(w, install)
	if !plan.UpdatableInPlace {
		fmt.Fprintln(w, "  ⚠ This install is NOT updatable in place.")
		fmt.Fprintf(w, "    Update via: %s\n", plan.UpdateMechanism)
	}
	profiles := "(none found)"
	if len(plan.Profiles) > 0 {
		names := make([]string, len(plan.Profiles))
		for i, profile := range plan.Profiles {
			names[i] = pyrepr.Str(profile)
		}
		profiles = strings.Join(names, ", ")
	}
	fmt.Fprintf(w, "  Profiles: %s\n", profiles)
	if len(plan.Runtimes) == 0 {
		fmt.Fprintln(w, "  Running Icebath services: none detected — code swap only.")
		return
	}
	fmt.Fprintf(w, "  Running services to restart (%d):\n", len(plan.Runtimes))
	for _, runtime := range plan.Runtimes {
		sha := ""
		if runtime.CodeSHA != nil && *runtime.CodeSHA != "" {
			sha = " @ " + clip(*runtime.CodeSHA, 8)
		}
		pid := "None"
		if runtime.PID != nil {
			pid = fmt.Sprint(*runtime.PID)
		}
		fmt.Fprintf(w, "    • %s [%s] pid %s — %s%s\n", runtime.Kind, runtime.Profile, pid, runtime.Supervisor, sha)
		fmt.Fprintf(w, "      restart: %s\n", DescribeRestartMechanism(runtime.RestartVia, runtime.Profile))
	}
}

// Attaches the inventory to the open update receipt, never failing the update
func recordPlanInReceipt(ctx context.Context, plan *UpdatePlan) {
	defer func() {
		if r := recover(); r != nil {
			updateLog.Debug(fmt.Sprintf("Could not record plan in receipt: %v", r))
		}
	}()
	if current := updaterecord.CurrentReceipt(ctx); current != nil {
		current.Data.Set("plan", plan.ToDict())
	}
}
