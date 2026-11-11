package app

import (
	"context"
	"os"
)

// TUI argv and cwd for a tui dir, progress and errors on the process stdout and stderr
func MakeTUIArgv(ctx context.Context, tuiDir string, dev bool) ([]string, string, error) {
	return makeTUIArgvTo(os.Stdout, os.Stderr)(ctx, tuiDir, dev)
}

// Seeds the source root, cwd and interpreter variables the TUI gateway bootstrap reads
func ApplyTUIPythonEnv(env map[string]string) error { return applyTUIPythonEnv(env) }
