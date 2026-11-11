package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/spf13/pflag"
)

// An option string that carries over a relaunch and whether a value follows it
type inheritedFlag struct {
	flag       string
	takesValue bool
}

var (
	inheritedFlagsOnce  sync.Once
	inheritedFlagsCache []inheritedFlag
	relaunchArgv        = func() []string { return os.Args }
	relaunchExecvp      = execvp
	relaunchRun         = runAndWait
	relaunchExit        = os.Exit
)

// Option strings of every root and chat flag tagged to carry over, plus the flags read before parsing
func buildInheritedFlagTable() []inheritedFlag {
	cli := commands.NewCLI()
	var table []inheritedFlag
	seen := map[inheritedFlag]bool{}
	add := func(entry inheritedFlag) {
		if !seen[entry] {
			seen[entry] = true
			table = append(table, entry)
		}
	}
	parsers := []*pflag.FlagSet{cli.Root.Flags()}
	if chat, _, err := cli.Root.Find([]string{"chat"}); err == nil && chat != cli.Root {
		parsers = append(parsers, chat.Flags())
	}
	for _, flags := range parsers {
		flags.VisitAll(func(fl *pflag.Flag) {
			if fl.Annotations["inherit_on_relaunch"] == nil {
				return
			}
			kind := fl.Value.Type()
			takesValue := kind != "bool" && kind != "count"
			if fl.Shorthand != "" {
				add(inheritedFlag{"-" + fl.Shorthand, takesValue})
			}
			add(inheritedFlag{"--" + fl.Name, takesValue})
		})
	}
	for _, pre := range commands.PreArgparseInheritedFlags {
		table = append(table, inheritedFlag{pre[0], pre[1] != ""})
	}
	return table
}

// Table of the flags a relaunch carries over, built once from the command tree
func inheritedFlagsTable() []inheritedFlag {
	inheritedFlagsOnce.Do(func() { inheritedFlagsCache = buildInheritedFlagTable() })
	return inheritedFlagsCache
}

// Flags of an argv that should carry over into a relaunched icebath, each with its value
func extractInheritedFlags(argv []string) []string {
	table := inheritedFlagsTable()
	flags := []string{}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if name, _, hasValue := strings.Cut(arg, "="); hasValue {
			for _, entry := range table {
				if name == entry.flag {
					flags = append(flags, arg)
					break
				}
			}
			continue
		}
		for _, entry := range table {
			if arg != entry.flag {
				continue
			}
			flags = append(flags, arg)
			if entry.takesValue && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				flags = append(flags, argv[i+1])
				i++
			}
			break
		}
	}
	return flags
}

// First bytes of a file, false when it cannot be read
func readHead(path string, size int) ([]byte, bool) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer fh.Close()
	head := make([]byte, size)
	n, err := io.ReadFull(fh, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, false
	}
	return head[:n], true
}

// Interpreter path and flags of a shebang line
func shebangTokens(shebang string) []string {
	return pystr.SplitWhitespace(pystr.Strip(shebang[2:]), -1)
}

// Whether a python shebang resolves outside the directory of the running executable
func shebangEscapesRunningEnv(shebang string) bool {
	tokens := shebangTokens(shebang)
	if len(tokens) == 0 {
		return true
	}
	interp := tokens[0]
	if name := pypath.PathName(interp); name == "env" || name == "env.exe" {
		var rest []string
		for _, t := range tokens[1:] {
			if !strings.HasPrefix(t, "-") {
				rest = append(rest, t)
			}
		}
		if len(rest) == 0 || !filepath.IsAbs(rest[0]) {
			return true
		}
		interp = rest[0]
	}
	running, err := os.Executable()
	if err != nil {
		return true
	}
	if abs, err := filepath.Abs(running); err == nil {
		running = abs
	}
	return strings.ToLower(pypath.PathParent(interp)) != strings.ToLower(filepath.Dir(running))
}

// Whether a file is a Python script whose shebang escapes the running environment
func needsInterpreter(binPath string) bool {
	head, ok := readHead(binPath, 256)
	if !ok || !strings.HasPrefix(string(head), "#!") {
		return false
	}
	lines := pystr.Splitlines(strings.ToValidUTF8(string(head), "�"), false)
	shebang := pystr.Strip(lines[0])
	if !strings.Contains(strings.ToLower(shebang), "python") {
		return false
	}
	return shebangEscapesRunningEnv(shebang)
}

// Whether a path names a regular file
func relaunchIsFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// Icebath entry point to exec again, the invoked program when executable, else the one on PATH, else none
func resolveHermesBin() (string, bool) {
	argv := relaunchArgv()
	argv0 := ""
	if len(argv) > 0 {
		argv0 = argv[0]
	}
	isWindows := runtime.GOOS == "windows"
	isPythonScript := func(p string) bool {
		lower := strings.ToLower(p)
		return strings.HasSuffix(lower, ".py") || strings.HasSuffix(lower, ".pyc")
	}
	isUnsafePythonLauncher := func(p string) bool {
		if isWindows {
			return isPythonScript(p)
		}
		return needsInterpreter(p)
	}
	if filepath.IsAbs(argv0) && relaunchIsFile(argv0) && accessExecutable(argv0) && !isUnsafePythonLauncher(argv0) {
		return argv0, true
	}
	if !strings.HasPrefix(argv0, "-") && relaunchIsFile(argv0) {
		if absPath, err := filepath.Abs(argv0); err == nil && accessExecutable(absPath) && !isUnsafePythonLauncher(absPath) {
			return absPath, true
		}
	}
	if pathBin, ok := pyshutil.Which("icebath", nil); ok && pathBin != "" && !isUnsafePythonLauncher(pathBin) {
		return pathBin, true
	}
	return "", false
}

// Argv that replaces the current process with icebath, the carried over flags before the extra arguments
func buildRelaunchArgv(extraArgs []string, preserveInherited bool, originalArgv []string) ([]string, error) {
	var argv []string
	if binPath, ok := resolveHermesBin(); ok {
		argv = []string{binPath}
	} else {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		argv = []string{exe}
	}
	src := originalArgv
	if src == nil {
		if all := relaunchArgv(); len(all) > 1 {
			src = all[1:]
		}
	}
	if preserveInherited {
		argv = append(argv, extractInheritedFlags(src)...)
	}
	return append(argv, extraArgs...), nil
}

// Runs a command to completion on the inherited streams, 130 when an interrupt arrived while it ran
func runAndWait(argv []string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	err := cmd.Run()
	signal.Stop(interrupts)
	select {
	case <-interrupts:
		return 130, nil
	default:
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Replaces the current process with a fresh icebath invocation, spawning and exiting on Windows
func relaunch(args []string, preserveInherited bool) {
	newArgv, err := buildRelaunchArgv(args, preserveInherited, nil)
	if err != nil {
		panic(err)
	}
	if runtime.GOOS == "windows" {
		code, err := relaunchRun(newArgv)
		if err != nil {
			fmt.Fprintf(stderrWriter(), "\nIcebath relaunch failed: %s\nCommand: %s\nFix: open a new terminal so PATH picks up, then re-run icebath.\n",
				sessionsExcText(err), strings.Join(newArgv, " "))
			relaunchExit(1)
			return
		}
		relaunchExit(code)
		return
	}
	observability.MarkInPlaceRelaunch()
	if err := relaunchExecvp(newArgv[0], newArgv); err != nil {
		panic(err)
	}
}
