package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Program name of the dependency sync entry in its usage and errors
const venvSyncProg = "icebath venv-sync"

// Python exception classes that derive from RuntimeError among the ones PM raises
var runtimeErrorClasses = map[string]bool{
	"RuntimeError": true, "InstallError": true, "ResolutionConflict": true, "BuildFailure": true, "DownloadError": true,
	"DownloadPaused": true, "HashError": true, "_RangeError": true, "DownloadTransportError": true,
	"NotImplementedError": true, "RecursionError": true, "PythonFinalizationError": true,
}

// Python OSError subclasses by name
var osErrorClasses = map[string]bool{
	"OSError": true, "FileNotFoundError": true, "FileExistsError": true, "PermissionError": true, "TimeoutError": true,
	"IsADirectoryError": true, "NotADirectoryError": true, "InterruptedError": true, "BlockingIOError": true,
	"ChildProcessError": true, "ProcessLookupError": true, "ConnectionError": true, "BrokenPipeError": true,
	"ConnectionAbortedError": true, "ConnectionRefusedError": true, "ConnectionResetError": true,
}

// Python class name of an error, the errno subclass for an OS error
func launchErrorClass(err error) string {
	if pypath.IsOSError(err) {
		return pypath.OSErrorClass(err)
	}
	return pm.ErrorClass(err)
}

// str of the exception an error stands for
func launchErrorText(err error) string {
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// Starts the relaunch command in place of this process, a Windows parent waiting for the child's status
var relaunchExec = func(binary string, command []string) (int, error) {
	os.Stdout.Sync()
	os.Stderr.Sync()
	if runtime.GOOS != "windows" {
		return 0, execSelf(binary, command)
	}
	cmd := exec.Command(binary, command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Finishes a source update and activates the committed dependencies before the app runs, the exit status when it must stop
func BootLaunch(ctx context.Context, root string, args []string) (int, bool) {
	command, err := pm.CommandArgv(args[1:])
	if err != nil {
		sayLaunch("%s: %s", launchErrorClass(err), launchErrorText(err))
		return 1, true
	}
	if len(command) >= 2 && command[0] == "pm" && command[1] == "repair" {
		return 0, false
	}
	binary, err := PrepareLaunch(ctx, root, args[1:])
	if err == nil && binary != "" {
		var code int
		if code, err = relaunchExec(binary, RelaunchCommand(binary, args)); err == nil {
			return code, true
		}
	}
	if err != nil {
		if launchErrorClass(err) == "PermissionError" {
			if message := pm.InstallStatePermissionMessage(ctx, root, err); message != "" {
				sayLaunch("icebath: %s", message)
				return 1, true
			}
		}
		sayLaunch("icebath: source-update completion failed: %s; running with the previous dependencies — run `icebath update` to finish it", launchErrorText(err))
	}
	_, err = pm.RecoverIfNeeded(ctx, root, nil, false)
	if err == nil {
		err = pm.ActivateDependencies(ctx, root)
	}
	if err == nil {
		return 0, false
	}
	class := launchErrorClass(err)
	if !runtimeErrorClasses[class] && !osErrorClasses[class] {
		sayLaunch("%s: %s", class, launchErrorText(err))
		return 1, true
	}
	if class == "PermissionError" {
		if message := pm.InstallStatePermissionMessage(ctx, root, err); message != "" {
			sayLaunch("icebath: %s", message)
			return 1, true
		}
	}
	if len(command) == 0 || command[0] != "pm" {
		sayLaunch("icebath: %s; run `icebath pm repair`", launchErrorText(err))
		return 1, true
	}
	return 0, false
}

// Parsed arguments of the dependency sync entry
type venvSyncArgs struct {
	projectRoot string
	check, json bool
}

var venvSyncNegativeNumber = regexp.MustCompile(`^-\.?\p{Nd}`)

// Option strings of the entry with the long ones in declaration order
var venvSyncLongOptions = []string{"--help", "--project-root", "--check", "--json"}

// Usage parts of the optionals, each colored by the stream's theme
func venvSyncUsageParts(t pm.ArgparseColors) []string {
	return []string{
		fmt.Sprintf("[%s-h%s]", t.SummaryShort, t.Reset),
		fmt.Sprintf("[%s--project-root%s %sPROJECT_ROOT%s]", t.SummaryLong, t.Reset, t.SummaryLabel, t.Reset),
		fmt.Sprintf("[%s--check%s]", t.SummaryLong, t.Reset),
		fmt.Sprintf("[%s--json%s]", t.SummaryLong, t.Reset),
	}
}

var venvSyncANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func decolorLen(s string) int { return utf8.RuneCountInString(venvSyncANSI.ReplaceAllString(s, "")) }

// Usage line of the entry wrapped the way argparse wraps optionals past the text width
func venvSyncUsage(t pm.ArgparseColors) string {
	prefix := t.Usage + "usage: " + t.Reset
	prefixLen := len("usage: ")
	prog := t.Prog + venvSyncProg + t.Reset
	progLen := len(venvSyncProg)
	parts := venvSyncUsageParts(t)
	width := pm.ArgparseWidth()
	line := prog + " " + strings.Join(parts, " ")
	if prefixLen+decolorLen(line) <= width {
		return prefix + line + "\n"
	}
	getLines := func(parts []string, indent int, withPrefix bool) []string {
		var lines, current []string
		lineLen := indent - 1
		if withPrefix {
			lineLen = prefixLen - 1
		}
		for _, part := range parts {
			if lineLen+1+decolorLen(part) > width && len(current) > 0 {
				lines = append(lines, strings.Repeat(" ", indent)+strings.Join(current, " "))
				current, lineLen = nil, indent-1
			}
			current = append(current, part)
			lineLen += decolorLen(part) + 1
		}
		if len(current) > 0 {
			lines = append(lines, strings.Repeat(" ", indent)+strings.Join(current, " "))
		}
		if withPrefix && len(lines) > 0 {
			lines[0] = lines[0][indent:]
		}
		return lines
	}
	var lines []string
	if float64(prefixLen+progLen) <= 0.75*float64(width) {
		lines = getLines(append([]string{prog}, parts...), prefixLen+progLen+1, true)
	} else {
		lines = append([]string{prog}, getLines(parts, prefixLen, false)...)
	}
	return prefix + strings.Join(lines, "\n") + "\n"
}

// textwrap.wrap of help text, long words broken at the width
func wrapHelp(text string, width int) []string {
	var lines []string
	current := ""
	for _, word := range strings.Fields(text) {
		for utf8.RuneCountInString(word) > width {
			if current != "" {
				lines = append(lines, current)
				current = ""
			}
			runes := []rune(word)
			lines = append(lines, string(runes[:width]))
			word = string(runes[width:])
		}
		switch {
		case current == "":
			current = word
		case utf8.RuneCountInString(current)+1+utf8.RuneCountInString(word) <= width:
			current += " " + word
		default:
			lines = append(lines, current)
			current = word
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// Help of the entry in the argparse layout
func venvSyncHelp(t pm.ArgparseColors) string {
	width := pm.ArgparseWidth()
	helpPosition := min(len("--project-root PROJECT_ROOT")+2+2, min(24, max(width-20, 4)))
	actions := []struct {
		header string
		length int
		help   string
	}{
		{fmt.Sprintf("%s-h%s, %s--help%s", t.ShortOption, t.Reset, t.LongOption, t.Reset), len("-h, --help"), "show this help message and exit"},
		{fmt.Sprintf("%s--project-root%s %sPROJECT_ROOT%s", t.LongOption, t.Reset, t.Label, t.Reset), len("--project-root PROJECT_ROOT"), ""},
		{fmt.Sprintf("%s--check%s", t.LongOption, t.Reset), len("--check"), "report; change nothing"},
		{fmt.Sprintf("%s--json%s", t.LongOption, t.Reset), len("--json"), ""},
	}
	var b strings.Builder
	b.WriteString(venvSyncUsage(t))
	b.WriteString("\n" + t.Heading + "options:" + t.Reset + "\n")
	actionWidth := helpPosition - 2 - 2
	helpWidth := max(width-helpPosition, 11)
	for _, action := range actions {
		if action.help == "" {
			b.WriteString("  " + action.header + "\n")
			continue
		}
		lines := wrapHelp(action.help, helpWidth)
		if action.length <= actionWidth {
			b.WriteString("  " + action.header + strings.Repeat(" ", actionWidth-action.length+2) + lines[0] + "\n")
		} else {
			b.WriteString("  " + action.header + "\n" + strings.Repeat(" ", helpPosition) + lines[0] + "\n")
		}
		for _, line := range lines[1:] {
			b.WriteString(strings.Repeat(" ", helpPosition) + line + "\n")
		}
	}
	return b.String()
}

// Reports an argument argparse reads as an option string rather than a value
func venvSyncLooksOptional(arg string) bool {
	if arg == "" || arg[0] != '-' || len(arg) == 1 {
		return false
	}
	if _, _, ok := venvSyncMatch(arg); ok {
		return true
	}
	if venvSyncNegativeNumber.MatchString(arg) || strings.Contains(arg, " ") {
		return false
	}
	return true
}

// Option an argument names, exact, by an equals split or by a unique long prefix, with its explicit value
func venvSyncMatch(arg string) (string, *string, bool) {
	exact := map[string]bool{"-h": true, "--help": true, "--project-root": true, "--check": true, "--json": true}
	if exact[arg] {
		return arg, nil, true
	}
	name, value, hasValue := strings.Cut(arg, "=")
	if hasValue && exact[name] {
		return name, &value, true
	}
	if strings.HasPrefix(arg, "--") {
		var matches []string
		for _, option := range venvSyncLongOptions {
			if strings.HasPrefix(option, name) {
				matches = append(matches, option)
			}
		}
		if len(matches) == 1 {
			if hasValue {
				return matches[0], &value, true
			}
			return matches[0], nil, true
		}
		return "", nil, false
	}
	if len(arg) > 2 && arg[:2] == "-h" {
		rest := arg[2:]
		return "-h+", &rest, true
	}
	return "", nil, false
}

// Outcome of a short help flag with attached characters, chained as argparse chains single dash flags
func venvSyncShortHelp(rest string) (string, bool) {
	sep := false
	for {
		if rest == "" && !sep {
			return "", true
		}
		if sep || rest == "" || rest[0] == '-' {
			return rest, false
		}
		if rest[0] != 'h' {
			return "", true
		}
		rest = rest[1:]
		if strings.HasPrefix(rest, "=") {
			sep, rest = true, rest[1:]
		}
	}
}

// Parses the entry's arguments with argparse semantics, an exit code when help or an error ends the run
func parseVenvSyncArgs(argv []string, stdout, stderr io.Writer) (venvSyncArgs, int, bool) {
	fail := func(message string) (venvSyncArgs, int, bool) {
		io.WriteString(stderr, venvSyncUsage(pm.ArgparseThemeFor(stderr)))
		fmt.Fprintf(stderr, "%s: error: %s\n", venvSyncProg, message)
		return venvSyncArgs{}, 2, false
	}
	var args venvSyncArgs
	var unknown []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			unknown = append(unknown, argv[i+1:]...)
			break
		}
		if !venvSyncLooksOptional(arg) {
			unknown = append(unknown, arg)
			continue
		}
		option, value, ok := venvSyncMatch(arg)
		if !ok {
			unknown = append(unknown, arg)
			continue
		}
		label := map[string]string{"-h": "-h/--help", "--help": "-h/--help", "--project-root": "--project-root", "--check": "--check", "--json": "--json"}[option]
		if option == "-h+" {
			if explicit, help := venvSyncShortHelp(*value); !help {
				return fail(fmt.Sprintf("argument -h/--help: ignored explicit argument %s", pyrepr.StrRepr(explicit)))
			}
			io.WriteString(stdout, venvSyncHelp(pm.ArgparseThemeFor(stdout)))
			return venvSyncArgs{}, 0, false
		}
		switch option {
		case "-h", "--help", "--check", "--json":
			if value != nil {
				return fail(fmt.Sprintf("argument %s: ignored explicit argument %s", label, pyrepr.StrRepr(*value)))
			}
			switch option {
			case "--check":
				args.check = true
			case "--json":
				args.json = true
			default:
				io.WriteString(stdout, venvSyncHelp(pm.ArgparseThemeFor(stdout)))
				return venvSyncArgs{}, 0, false
			}
		case "--project-root":
			if value == nil {
				if i+1 >= len(argv) || argv[i+1] == "--" || venvSyncLooksOptional(argv[i+1]) {
					return fail("argument --project-root: expected one argument")
				}
				i++
				value = &argv[i]
			}
			args.projectRoot = *value
		}
	}
	if len(unknown) > 0 {
		return fail("unrecognized arguments: " + strings.Join(unknown, " "))
	}
	return args, 0, true
}

// Reports or syncs a tree's dependencies as text or JSON, nonzero when the sync failed
func VenvSyncMain(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	args, code, ok := parseVenvSyncArgs(argv, stdout, stderr)
	if !ok {
		return code
	}
	result, err := SyncTree(ctx, args.projectRoot, args.check)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", launchErrorClass(err), launchErrorText(err))
		return 1
	}
	if args.json {
		text, err := pyjson.Dumps(result.object())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, text)
	} else {
		detail := ""
		if result.Detail != "" {
			detail = " (" + result.Detail + ")"
		}
		fmt.Fprintf(stdout, "venv sync: %s%s\n", result.State, detail)
	}
	if result.OK {
		return 0
	}
	return 1
}
