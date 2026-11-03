package shell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// One command line to run in a fresh shell with its own streams
type Cmd struct {
	Line string
	// Working directory, the process working directory when empty
	Dir string
	// KEY=VALUE pairs layered over the environment of the process
	Env []string
	// Standard input of the line, closed when nil
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Exit code of the line, with an error only when the shell cannot start or ctx ends it
func (c Cmd) Run(ctx context.Context) (int, error) {
	s, err := New(Options{Dir: c.Dir, Env: c.Env})
	if err != nil {
		return 0, err
	}
	res := s.run(ctx, c.Line, c.Stdin, c.Stdout, c.Stderr)
	if res.Killed {
		return res.ExitCode, ctx.Err()
	}
	return res.ExitCode, nil
}

// One program to start directly with its arguments, no shell reading the line
type Exec struct {
	Path string
	Args []string
	// Working directory, the process working directory when empty
	Dir string
	// KEY=VALUE pairs layered over the environment of the process
	Env []string
	// Standard input of the program, closed when nil
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Exit code of the program, with an error only when it cannot start or ctx ends it
func (e Exec) Run(ctx context.Context) (int, error) {
	path, err := exec.LookPath(e.Path)
	if err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, path)
	cmd.Args = append([]string{e.Path}, e.Args...)
	cmd.Env = append(os.Environ(), e.Env...)
	cmd.Dir = e.Dir
	cmd.Stdin = e.Stdin
	cmd.Stdout = e.Stdout
	cmd.Stderr = e.Stderr
	cmd.WaitDelay = killGrace
	proc.Group(cmd)
	cmd.Cancel = func() error { return proc.Kill(cmd) }
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if ctx.Err() != nil {
				return killedCode, ctx.Err()
			}
			return exit.ExitCode(), nil
		}
		return 0, err
	}
	return 0, nil
}
