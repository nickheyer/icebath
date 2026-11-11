package tools

import (
	"context"
	"fmt"
	"io"
)

// ResumeCommand lifts the emergency stop icebath pause engaged, hermes_cli/subcommands/pause.py cmd_resume
func ResumeCommand(ctx context.Context, h string, args []string, w io.Writer) (int, error) {
	if Disengage(ctx) {
		fmt.Fprintln(w, "▶️  Icebath resumed — dispatch picks up on the next tick.")
	} else {
		fmt.Fprintf(w, "Icebath is not paused (no sentinel at %s).\n", SentinelPath(ctx))
	}
	return 0, nil
}

// PauseCommand engages the emergency stop with an optional reason, hermes_cli/subcommands/pause.py cmd_pause
func PauseCommand(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	reason, _ := ns["reason"].(string)
	already := IsEngaged(ctx)
	path := Engage(ctx, reason)
	verb := "Icebath paused"
	if already {
		verb = "Still paused"
	}
	detail := ""
	if state := GetState(ctx); state != nil && state.Reason != "" {
		detail = " — reason: " + state.Reason
	}
	fmt.Fprintf(w, "⏸️  %s%s\n", verb, detail)
	fmt.Fprintf(w, "    sentinel: %s\n", path)
	fmt.Fprintln(w, "    Cron dispatch, kanban dispatch, and new gateway turns (messaging and interactive\n"+
		"    CLI, TUI and Desktop turns alike) are on hold. In-flight work keeps running.\n"+
		"    Run `icebath resume` to lift the pause.")
	return 0, nil
}
