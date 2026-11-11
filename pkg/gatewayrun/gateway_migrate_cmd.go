package gatewayrun

import (
	"context"
	"fmt"
	"io"
	"os"

	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Whether stdin is a terminal an operator can answer the apply prompt on
var migrateStdinIsTTY = func() bool { return xterm.IsTerminal(os.Stdin.Fd()) }

// Asks before applying, the default answer yes
var migratePromptYesNo = func(question string, def bool) bool { return provider.PromptYesNo(question, def, "") }

// Prints the plan and applies it, exiting 1 on a refusal or failed apply and 0 on success
func CmdMigrate(ctx context.Context, w io.Writer, ns map[string]any) error {
	if reason := migrateHostSupportsMigration(); reason != "" {
		fmt.Fprintf(w, "✗ %s\n", reason)
		return &gateway.SystemExitError{Code: 1}
	}
	plan, err := BuildMigrationPlan(ctx)
	if err != nil {
		return err
	}
	dryRun := pyrepr.Truthy(ns["dry_run"])
	printLines(w, FormatPlan(plan, dryRun))
	if dryRun || plan.AlreadyMultiplexed() {
		return nil
	}
	if plan.Manifest == nil && (plan.Blocked() || len(plan.Profiles) < 2) {
		if plan.Blocked() {
			return &gateway.SystemExitError{Code: 1}
		}
		return &gateway.SystemExitError{Code: 0}
	}
	if !pyrepr.Truthy(ns["yes"]) && migrateStdinIsTTY() {
		if !migratePromptYesNo("Apply this migration now?", true) {
			fmt.Fprintln(w, "Aborted; nothing changed.")
			return nil
		}
	}
	fmt.Fprintln(w)
	ok, err := ApplyMigration(ctx, w, plan, migrateServedWait)
	if err != nil {
		return err
	}
	if ok {
		return &gateway.SystemExitError{Code: 0}
	}
	return &gateway.SystemExitError{Code: 1}
}

// Update hook folding per profile gateways into one multiplexer when unblocked, else printing the blockers
func MaybeAutoMigrateAfterUpdate(ctx context.Context, w io.Writer) error {
	if migrateHostSupportsMigration() != "" {
		return nil
	}
	optedOut, err := AutoMigrationOptedOut(ctx, home.Root())
	if err != nil {
		return err
	}
	if optedOut {
		return nil
	}
	plan, err := BuildMigrationPlan(ctx)
	if err != nil {
		return err
	}
	if plan.AlreadyMultiplexed() || len(plan.Profiles) < 2 {
		return nil
	}
	resuming := plan.Manifest != nil
	if len(plan.StandaloneSecondaries()) == 0 && !resuming {
		return nil
	}
	masked := ""
	if !resuming {
		if masked, err = MaskedDefaultUnit(ctx, plan.DefaultHome); err != nil {
			return err
		}
	}
	if masked != "" {
		fmt.Fprintf(w, "\n  ℹ Per-profile gateways kept: the default gateway unit is masked (%s).\n"+
			"    Unmask it and run %s to fold them into one gateway.\n", masked, MigrateCommand)
		return nil
	}
	fmt.Fprintln(w)
	autoBlockers := AutoMigrationBlockers(plan)
	if !resuming && (plan.Blocked() || len(autoBlockers) > 0) {
		printLines(w, FormatUpdateWarning(plan, autoBlockers))
		return nil
	}
	fmt.Fprintln(w, "→ Migrating per-profile gateways onto one multiplexed default gateway...")
	printLines(w, FormatPlan(plan, false))
	_, err = ApplyMigration(ctx, w, plan, migrateServedWait)
	return err
}
