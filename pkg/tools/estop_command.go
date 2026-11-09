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
