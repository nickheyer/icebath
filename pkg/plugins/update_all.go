package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	consoleutil "github.com/nickheyer/icebath/pkg/utils/console"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

var updateKind = map[string]string{"catalog": "catalog re-pin", "git": "git pull"}

var (
	updateAllChecks = func(ctx context.Context, dir string) []*CheckResult { return RunChecks(ctx, dir, RunChecksOptions{}) }
	updateAllTarget = userInstalledPluginDir
	updateAllOne    = updateOne
)

// Rows an update with no name moves, rows it skips with the reason, and the count already current
type UpdatePlan struct {
	Movable []*CheckResult
	Skipped []SkippedUpdate
	Current int
}

// One check row an update with no name cannot move and why
type SkippedUpdate struct {
	Row    *CheckResult
	Reason string
}

// Splits check rows into movable plugins and skipped ones with the reason each cannot move
func PlanUpdateAll(results []*CheckResult) UpdatePlan {
	var plan UpdatePlan
	for _, row := range results {
		available := row.UpdateAvailable != nil && *row.UpdateAvailable
		held := ""
		if available {
			held = heldForNewerIcebath(row)
		}
		switch {
		case row.NeedsFixing != "":
			plan.Skipped = append(plan.Skipped, SkippedUpdate{row, "needs fixing: " + row.NeedsFixing})
		case held != "":
			plan.Skipped = append(plan.Skipped, SkippedUpdate{row, fmt.Sprintf("update available after updating Icebath (%s)", held)})
		case available && updateKind[row.Class] != "":
			plan.Movable = append(plan.Movable, row)
		case available:
			plan.Skipped = append(plan.Skipped, SkippedUpdate{row, row.Class + " distribution; Icebath does not upgrade it"})
		case row.UpdateAvailable == nil:
			reason := row.Reason
			if reason == "" {
				reason = "update status unknown"
			}
			plan.Skipped = append(plan.Skipped, SkippedUpdate{row, reason})
		case row.Reason != "":
			plan.Skipped = append(plan.Skipped, SkippedUpdate{row, row.Reason})
		default:
			plan.Current++
		}
	}
	return plan
}

// First line of a reason capped at 200 characters
func shortReason(reason string) string {
	stripped := strings.TrimSpace(reason)
	if stripped == "" {
		return ""
	}
	first := []rune(textutil.Lines(stripped)[0])
	if len(first) <= 200 {
		return string(first)
	}
	return string(first[:197]) + "..."
}

// Text of an error raised inside one batch update, a package worker failure led by the exception class the worker reported
func updateFailureText(err error) string {
	var workerErr *pm.PyError
	if errors.As(err, &workerErr) {
		return workerErr.Class + ": " + workerErr.Message
	}
	return errchain.OSText(err)
}

// Runs one plan row through the single plugin update, returning updated, current or failed with detail
func updatePlanRow(ctx context.Context, row *CheckResult, console *Console, interactive bool) (string, string, error) {
	target, err := updateAllTarget(ctx, row.Name)
	if err != nil {
		return "", "", err
	}
	if target == "" {
		return "failed", "no longer installed", nil
	}
	moved, err := updateAllOne(ctx, row.Name, target, console, interactive, false)
	var consent *RepinConsentRequired
	var opErr *PluginOperationError
	var exit *CommandExit
	var interrupt *UserInterrupt
	switch {
	case err == nil:
	case errors.As(err, &consent):
		console.Print(fmt.Sprintf("[yellow]Update of %s not applied:[/yellow] %s", consoleutil.Escape(row.Name), consoleutil.Escape(consent.Error())))
		return "failed", shortReason("not applied: " + consent.Error()), nil
	case errors.As(err, &opErr):
		console.Print("[red]Error:[/red] " + consoleutil.Escape(opErr.Error()))
		return "failed", shortReason(opErr.Error()), nil
	case errors.As(err, &exit), errors.As(err, &interrupt), errors.Is(err, ErrUserInterrupt), errors.Is(err, context.Canceled):
		return "", "", err
	default:
		pkgLog().Warn(fmt.Sprintf("update of plugin %s failed", row.Name), excInfo(err)...)
		text := updateFailureText(err)
		console.Print("[red]Error:[/red] " + consoleutil.Escape(text))
		return "failed", shortReason(text), nil
	}
	if !moved {
		return "current", "", nil
	}
	current, latest := []rune(row.Current), []rune(row.Latest)
	return "updated", fmt.Sprintf("%s..%s", string(current[:min(8, len(current))]), string(latest[:min(8, len(latest))])), nil
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	return string(r[:min(n, len(r))])
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Prints the movable plugins table and one dim line per skipped plugin
func printUpdatePlan(console *Console, plan UpdatePlan) {
	table := newPluginTable("Plugin updates available", [][2]string{{"#", "dim"}, {"Plugin", "bold"}, {"Current", ""}, {"Latest", ""}, {"Update", ""}})
	for i, row := range plan.Movable {
		table.AddRow(fmt.Sprint(i+1), row.Name, firstRunes(dashIfEmpty(row.Current), 12), firstRunes(dashIfEmpty(row.Latest), 12), updateKind[row.Class])
	}
	console.Print("")
	console.printTable(table)
	printSkippedUpdates(console, plan)
}

func printSkippedUpdates(console *Console, plan UpdatePlan) {
	for _, s := range plan.Skipped {
		console.Print(fmt.Sprintf("  [dim]skipped %s (%s): %s[/dim]", consoleutil.Escape(s.Row.Name), s.Row.Class, consoleutil.Escape(s.Reason)))
	}
}

// Lists every plugin with an update and updates them all after one confirmation, exit 1 when any failed
func CmdUpdateAll(ctx context.Context, yes bool, out io.Writer) error {
	console := NewConsole(out)
	console.Print("[dim]Checking installed plugins for updates...[/dim]")
	dir, err := pluginsDir(home.Dir(ctx))
	if err != nil {
		return err
	}
	plan := PlanUpdateAll(updateAllChecks(ctx, dir))
	if len(plan.Movable) == 0 {
		printSkippedUpdates(console, plan)
		console.Print("[green]✓[/green] No plugin updates to apply.")
		return nil
	}
	printUpdatePlan(console, plan)
	count := len(plan.Movable)
	tty := isTTY()
	if !yes {
		if !tty {
			return fail(console, fmt.Sprintf("[yellow]Not updating: no terminal to confirm. Re-run with --yes to update these %d plugin(s).[/yellow]", count))
		}
		answer, err := inputLine(fmt.Sprintf("\nUpdate all %d? [Y/n]: ", count))
		if err != nil {
			answer = "n"
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "", "y", "yes":
		default:
			console.Print("Cancelled. Nothing was updated.")
			return nil
		}
	}
	type outcome struct{ name, status, detail string }
	var outcomes []outcome
	for _, row := range plan.Movable {
		console.Print(fmt.Sprintf("\n[bold]── %s[/bold]", row.Name))
		status, detail, err := updatePlanRow(ctx, row, console, tty)
		if err != nil {
			return err
		}
		outcomes = append(outcomes, outcome{row.Name, status, detail})
	}
	console.Print("\n[bold]Summary[/bold]")
	marks := map[string]string{"updated": "[green]✓ updated[/green]", "current": "[dim]= already current[/dim]", "failed": "[red]✗ failed[/red]"}
	failed := false
	for _, o := range outcomes {
		line := fmt.Sprintf("  %s  %s", marks[o.status], consoleutil.Escape(o.name))
		if o.detail != "" {
			line += fmt.Sprintf("  [dim]%s[/dim]", consoleutil.Escape(o.detail))
		}
		console.Print(line)
		failed = failed || o.status == "failed"
	}
	if failed {
		return &CommandExit{Code: 1}
	}
	return nil
}
