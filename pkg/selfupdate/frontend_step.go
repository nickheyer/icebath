package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/pm"
)

// One source build step shown the way a contained build command is, raw when verbose and as one live tail otherwise
type frontendStep struct {
	label    string
	verbose  bool
	tail     *pm.LiveTail
	buffered strings.Builder
}

// Opens the display of a step whose npm warnings stay out of the live tail
func newFrontendStep(label string) *frontendStep {
	st := &frontendStep{label: label, verbose: pm.VerboseOutput(nil)}
	if st.verbose {
		fmt.Printf("  → %s…\n", label)
		return st
	}
	st.tail = pm.NewLiveTail(&st.label, nil, func(line string) bool { return strings.HasPrefix(strings.ToLower(line), "npm warn") }, "  ")
	return st
}

func (st *frontendStep) Write(p []byte) (int, error) {
	switch {
	case st.verbose:
		return os.Stdout.Write(p)
	case st.tail.Live:
		return st.tail.Write(p)
	}
	return st.buffered.Write(p)
}

// Prints a message of the step itself into its display
func (st *frontendStep) say(format string, args ...any) {
	fmt.Fprintf(st, format+"\n", args...)
}

// Closes the display, a buffered transcript shown only when the step failed
func (st *frontendStep) close(err error) error {
	if st.verbose {
		return err
	}
	if !st.tail.Live && err != nil {
		st.tail.WriteString(st.buffered.String())
	}
	st.tail.Close(err == nil)
	return err
}

// Runs a checkout writing command inside the update custody with its output in the step display
func (st *frontendStep) run(ctx context.Context, projectRoot string, command []string, dir string, env map[string]string) error {
	contained, err := ContainedCommand(ctx, command, true, projectRoot)
	if err != nil {
		return err
	}
	cmd := exec.Command(contained.Argv[0], contained.Argv[1:]...)
	cmd.Dir, cmd.Env = dir, pm.EnvList(env)
	cmd.ExtraFiles, cmd.SysProcAttr = contained.ExtraFiles, contained.Attr
	var out io.Writer = st
	if st.verbose {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	} else {
		cmd.Stdout, cmd.Stderr = out, out
	}
	runErr := cmd.Run()
	if closeErr := contained.Close(); closeErr != nil {
		return closeErr
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		code := exit.ExitCode()
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = -int(ws.Signal())
		}
		return &pm.CalledProcessError{Argv: append([]string(nil), command...), Code: code, Output: st.buffered.String()}
	}
	return runErr
}
