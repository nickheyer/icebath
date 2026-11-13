package app

import (
	"context"
	"os"
)

// Launch arguments of the TUI the chat entry points pass through
type TUILaunch struct {
	Resume, Model, Provider, Toolsets, Query, Image                           string
	Native, AcceptHooks, Verbose, Quiet, Worktree, Checkpoints, PassSessionID bool
	Skills                                                                    []string
	MaxTurns                                                                  *int
}

// Runs the terminal UI over the real worktree setup, exit summary and relaunch
func LaunchTUI(ctx context.Context, l TUILaunch) int {
	return launchTUIWith(ctx, l, tuiLaunchDeps{
		setupWorktree: func(ctx context.Context) (map[string]any, error) {
			return setupTUIWorktree(ctx, os.Stderr)
		},
		run: runTerminalUI,
		printSummary: func(ctx context.Context, sessionID, activeSessionFile string) {
			printTUIExitSummary(ctx, os.Stdout, sessionID, activeSessionFile)
		},
		relaunch: relaunch,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	})
}
