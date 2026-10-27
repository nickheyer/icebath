package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Longest pause between attempts to start a program whose file is still being written
const busyRetry = 300 * time.Millisecond

// Middleware installing the program runner in every interpreter the shell creates
func handler(interp.ExecHandlerFunc) interp.ExecHandlerFunc { return run }

type outKey struct{}

// Opens the shell's stdout path on the stream of the line in progress and every other path as a file
func open(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if path != stdoutPath {
		return interp.DefaultOpenHandler()(ctx, path, flag, perm)
	}
	w, ok := ctx.Value(outKey{}).(io.Writer)
	if !ok {
		return nil, fmt.Errorf("%s: no line in progress", path)
	}
	return sink{w}, nil
}

// Write only file over the stream of a line
type sink struct{ io.Writer }

func (sink) Read([]byte) (int, error) { return 0, io.EOF }
func (sink) Close() error             { return nil }

// Runs the named program in its own process group, or a shebang-less script in the interpreter itself
func run(ctx context.Context, args []string) error {
	if args[0] == stageCmd {
		return recordStage(ctx, args)
	}
	hc := interp.HandlerCtx(ctx)
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.ExitStatus(127)
	}
	cmd, err := start(ctx, hc, path, args)
	if errors.Is(err, syscall.ENOEXEC) {
		return script(ctx, hc, path, args)
	}
	if err == nil {
		err = cmd.Wait()
	}
	var exit *exec.ExitError
	var failed *exec.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit):
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return interp.ExitStatus(uint8(128 + int(ws.Signal())))
		}
		return interp.ExitStatus(uint8(exit.ExitCode()))
	case errors.Is(err, exec.ErrWaitDelay):
		return nil
	case errors.As(err, &failed):
		fmt.Fprintln(hc.Stderr, err)
		return interp.ExitStatus(127)
	}
	return err
}

// Starts path with the command's streams, retrying while its file is still busy being written
func start(ctx context.Context, hc interp.HandlerContext, path string, args []string) (*exec.Cmd, error) {
	for delay := time.Millisecond; ; delay *= 2 {
		cmd := exec.CommandContext(ctx, path)
		cmd.Args = args
		cmd.Env = environ(hc.Env)
		cmd.Dir = hc.Dir
		cmd.Stdin = hc.Stdin
		cmd.Stdout = hc.Stdout
		cmd.Stderr = hc.Stderr
		cmd.WaitDelay = killGrace
		proc.Group(cmd)
		cmd.Cancel = func() error { return proc.Kill(cmd) }
		err := cmd.Start()
		if !errors.Is(err, syscall.ETXTBSY) || delay >= busyRetry {
			return cmd, err
		}
		time.Sleep(delay)
	}
}

// Variables the shell exports, as a program receives them
func environ(env expand.Environ) []string {
	vals := map[string]string{}
	env.Each(func(name string, vr expand.Variable) bool {
		switch {
		case !vr.IsSet():
			delete(vals, name)
		case vr.Exported && vr.Kind == expand.String:
			vals[name] = vr.String()
		}
		return true
	})
	list := make([]string, 0, len(vals))
	for name, val := range vals {
		list = append(list, name+"="+val)
	}
	return list
}

// Runs a script naming no interpreter in a fresh interpreter of its own with the script's arguments
func script(ctx context.Context, hc interp.HandlerContext, path string, args []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.ExitStatus(126)
	}
	head, _, _ := bytes.Cut(data, []byte("\n"))
	if bytes.IndexByte(head, 0) >= 0 {
		fmt.Fprintf(hc.Stderr, "%s: cannot execute binary file\n", args[0])
		return interp.ExitStatus(126)
	}
	prog, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(bytes.NewReader(data), args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.ExitStatus(2)
	}
	r, err := interp.New(interp.Dir(hc.Dir), interp.Env(expand.ListEnviron(environ(hc.Env)...)), interp.StdIO(hc.Stdin, hc.Stdout, hc.Stderr), interp.ExecHandlers(handler), interp.OpenHandler(open))
	if err != nil {
		return err
	}
	r.Params = args[1:]
	return r.Run(ctx, prog)
}
