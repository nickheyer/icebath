package shell

import (
	"context"
	"io"
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
