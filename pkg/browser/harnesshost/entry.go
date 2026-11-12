package harnesshost

import (
	"fmt"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

// Hidden verb under which a re-executed binary hosts the harness interpreter with the shot helpers installed
const Entry = "browser-harness-host"

// Exit statuses of a refused command line, an interpreter that could not start, and a failed Py_FinalizeEx
const (
	exitUsage          = 2
	exitStartFailed    = 3
	exitFinalizeFailed = 120
	finalizeDeadline   = 20 * time.Second
)

// Runs the harness host child when this binary was started as one, exiting with its status
func DispatchEntry() {
	if len(os.Args) > 1 && os.Args[1] == Entry {
		os.Exit(childMain(os.Args[2:]))
	}
}

// Hosts python like python -c with the venv and PYTHONPATH honoured, then runs the harness as __main__
func childMain(args []string) int {
	if len(args) != 4 || args[0] != "--python" || args[2] != "--activate" {
		fmt.Fprintf(os.Stderr, "%s: usage: %s --python <interpreter> --activate never|always|if-cloud\n", Entry, Entry)
		return exitUsage
	}
	python, mode := args[1], args[3]
	in, err := pyhost.Discover(python)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", Entry, err)
		return exitStartFailed
	}
	h, err := pyhost.Start(pyhost.Options{Interpreter: in, Environment: true, ExtraPaths: []string{""}, Argv: []string{"-c"}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", Entry, err)
		return exitStartFailed
	}
	code, interrupted := 0, false
	if err := h.Do(func(g *pyhost.GIL) error {
		code, interrupted = runHarness(g, mode)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", Entry, err)
		code = 1
	}
	if rc, err := h.Finalize(finalizeDeadline); (err != nil || rc < 0) && code == 0 {
		code = exitFinalizeFailed
	}
	if interrupted {
		return exitInterrupted()
	}
	return code
}

// Installs the helpers and runs browser_harness.run, returning the status an uncaught exception leaves
func runHarness(g *pyhost.GIL, mode string) (int, bool) {
	f := &frame{g: g}
	defer f.release()
	err := func() error {
		helpers, err := f.imp("browser_harness.helpers")
		if err != nil {
			return err
		}
		if err := install(f, helpers, mode); err != nil {
			return err
		}
		run, err := f.member("runpy", "run_module")
		if err != nil {
			return err
		}
		_, err = f.call(run, []any{"browser_harness.run"}, map[string]any{"run_name": "__main__", "alter_sys": true})
		return err
	}()
	if err == nil {
		return 0, false
	}
	return uncaught(f, err)
}

// Status Python's main gives an exception that left the program, printing it as sys.excepthook does
func uncaught(f *frame, err error) (int, bool) {
	exc, ok := f.g.ExceptionObject(err)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s: %v\n", Entry, err)
		return 1, false
	}
	if exit, err2 := f.raised(err, "SystemExit"); err2 == nil && exit {
		return systemExitCode(f, exc), false
	}
	interrupted, _ := f.raised(err, "KeyboardInterrupt")
	if err2 := excepthook(f, exc); err2 != nil {
		fmt.Fprintf(os.Stderr, "Error in sys.excepthook:\n%s\nOriginal exception was:\n%s", tracebackText(err2), tracebackText(err))
	}
	return 1, interrupted
}

// Calls sys.excepthook with the exception's type, value and traceback
func excepthook(f *frame, exc uintptr) error {
	hook, err := f.member("sys", "excepthook")
	if err != nil {
		return err
	}
	typeOf, err := f.member("builtins", "type")
	if err != nil {
		return err
	}
	cls, err := f.call(typeOf, []any{pyhost.Borrowed(exc)}, nil)
	if err != nil {
		return err
	}
	tb, err := f.attr(exc, "__traceback__")
	if err != nil {
		return err
	}
	_, err = f.call(hook, []any{pyhost.Borrowed(cls), pyhost.Borrowed(exc), pyhost.Borrowed(tb)}, nil)
	return err
}

// Formatted traceback of a Python error, its message alone when it carries none
func tracebackText(err error) string {
	if pe, ok := err.(*pyhost.PyError); ok && pe.Traceback != "" {
		return pe.Traceback
	}
	return err.Error() + "\n"
}

// Exit status of SystemExit as _Py_HandleSystemExit reads code, None as 0, an int as itself, else printed as 1
func systemExitCode(f *frame, exc uintptr) int {
	target := exc
	if code, err := f.attr(exc, "code"); err == nil {
		if f.g.IsNone(code) {
			return 0
		}
		target = code
	}
	integer, err := f.member("builtins", "int")
	if err == nil {
		if isInt, err := f.g.IsInstance(target, integer); err == nil && isInt {
			value, err := f.g.FromPython(target)
			switch n := value.(type) {
			case bool:
				if n {
					return 1
				}
				return 0
			case int64:
				if err == nil {
					return int(int32(n))
				}
			}
			return -1
		}
	}
	stderr, err := f.member("sys", "stderr")
	if err == nil && !f.g.IsNone(stderr) {
		printer, err := f.member("builtins", "print")
		if err == nil {
			if _, err = f.call(printer, []any{pyhost.Borrowed(target)}, map[string]any{"file": pyhost.Borrowed(stderr)}); err == nil {
				return 1
			}
		}
	}
	text, err := f.g.Str(target)
	if err != nil {
		text = err.Error()
	}
	fmt.Fprintln(os.Stderr, text)
	return 1
}
