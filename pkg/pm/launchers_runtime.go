package pm

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Hidden root flag pinning ICEBATH_HOME when unset, to its value or else to the install's default root
const PinDefaultHomeFlag = "--icebath-pin-default-home"

// LaunchCommandOptions selects the subcommand, the binary and a literal default home of an install command
type LaunchCommandOptions struct {
	Subcommand []string
	Binary     string
	Home       string
}

// Installation bound argv safe to persist, the install binary pinning its home before the subcommand and args
func InstallRuntimeCommand(repoRoot string, args []string, opts LaunchCommandOptions) []string {
	root := resolvePath(repoRoot)
	binary := opts.Binary
	if binary == "" {
		binary = resolveInstallBinary(root)
	}
	if binary == "" {
		binary, _ = os.Executable()
	}
	pin := PinDefaultHomeFlag
	if opts.Home != "" {
		pin += "=" + opts.Home
	}
	command := append([]string{binary, pin}, opts.Subcommand...)
	return append(command, args...)
}

// Persisted install command, the install's own binary when present and the runtime command otherwise
func InstallationCommand(repoRoot string, args []string, opts LaunchCommandOptions) []string {
	binary := resolveInstallBinary(repoRoot)
	if binary == "" {
		return InstallRuntimeCommand(repoRoot, args, opts)
	}
	command := append([]string{binary}, opts.Subcommand...)
	return append(command, args...)
}

// Launcher steps before the bootstrap, argv returned without a runtime command's leading home pin and whether it had one
func LauncherStart(repoRoot string, args []string) ([]string, bool) {
	os.Unsetenv("PYTHONHOME")
	os.Unsetenv("PYTHONPATH")
	pinned := len(args) > 1 && (args[1] == PinDefaultHomeFlag || strings.HasPrefix(args[1], PinDefaultHomeFlag+"="))
	rest := args
	if pinned {
		os.Unsetenv("VIRTUAL_ENV")
		if value, ok := strings.CutPrefix(args[1], PinDefaultHomeFlag+"="); ok && os.Getenv(home.EnvHome) == "" {
			os.Setenv(home.EnvHome, value)
		}
		rest = append([]string{args[0]}, args[2:]...)
	}
	importPath.Lock()
	importPath.entries = append([]string{resolvePath(repoRoot)}, importPath.entries...)
	importPath.Unlock()
	return rest, pinned
}

// Pins an unset ICEBATH_HOME to the install's default root, the home every launch of an install command settles on
func PinLauncherHome() {
	if os.Getenv(home.EnvHome) == "" {
		os.Setenv(home.EnvHome, home.Root())
	}
}

const runtimeCommandUsage = "usage: icebath --print-runtime-command [-h] [--module MODULE] ...\n"

// Prints the install's launch command as a JSON list, exit 2 with usage on a bad option
func printRuntimeCommand(repoRoot string, argv []string, stdout, stderr io.Writer) int {
	var subcommand []string
	rest := []string{}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name, value, hasValue := strings.Cut(arg, "=")
		isModule := len(name) >= 3 && strings.HasPrefix("--module", name)
		switch {
		case arg == "-h" || (len(arg) >= 3 && strings.HasPrefix("--help", arg)):
			fmt.Fprint(stdout, runtimeCommandUsage+"\nResolve this installation's launch command.\n\npositional arguments:\n  args\n\n"+
				"options:\n  -h, --help       show this help message and exit\n  --module MODULE\n")
			return 0
		case isModule && hasValue:
			subcommand = strings.Fields(value)
		case isModule:
			if i+1 >= len(argv) || (strings.HasPrefix(argv[i+1], "-") && argv[i+1] != "-") {
				fmt.Fprint(stderr, runtimeCommandUsage+"icebath --print-runtime-command: error: argument --module: expected one argument\n")
				return 2
			}
			i++
			subcommand = strings.Fields(argv[i])
		case strings.HasPrefix(arg, "-") && arg != "--" && arg != "-":
			fmt.Fprint(stderr, runtimeCommandUsage+"icebath --print-runtime-command: error: unrecognized arguments: "+arg+"\n")
			return 2
		default:
			rest = argv[i:]
			i = len(argv)
		}
	}
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	command := InstallRuntimeCommand(repoRoot, rest, LaunchCommandOptions{Subcommand: subcommand})
	values := make([]any, len(command))
	for i, part := range command {
		values[i] = part
	}
	text, err := pyjson.Dumps(values)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, text)
	return 0
}

// Prints the runtime command or publishes the install's launchers into an output directory
func LaunchersMain(ctx context.Context, repoRoot string, argv []string, stdout, stderr io.Writer) int {
	if len(argv) > 0 && argv[0] == "--print-runtime-command" {
		return printRuntimeCommand(repoRoot, argv[1:], stdout, stderr)
	}
	usage := "usage: icebath pm publish-launchers [-h] out_dir\n"
	var positional, extras []string
	options := true
	for _, arg := range argv {
		switch {
		case options && arg == "--":
			options = false
		case options && (arg == "-h" || (len(arg) >= 3 && strings.HasPrefix("--help", arg))):
			fmt.Fprint(stdout, usage+"\nPublish source-install launchers.\n\npositional arguments:\n  out_dir\n\noptions:\n  -h, --help  show this help message and exit\n")
			return 0
		case options && strings.HasPrefix(arg, "-") && arg != "-":
			extras = append(extras, arg)
		case len(positional) == 0:
			positional = append(positional, arg)
		default:
			extras = append(extras, arg)
		}
	}
	if len(positional) == 0 {
		fmt.Fprint(stderr, usage+"icebath pm publish-launchers: error: the following arguments are required: out_dir\n")
		return 2
	}
	if len(extras) > 0 {
		fmt.Fprint(stderr, usage+"icebath pm publish-launchers: error: unrecognized arguments: "+strings.Join(extras, " ")+"\n")
		return 2
	}
	root := resolvePath(repoRoot)
	if resolveInstallBinary(root) == "" {
		fmt.Fprint(stderr, "icebath: store interpreter is missing; finish pm install before publishing launchers\n")
		return 1
	}
	if err := os.MkdirAll(positional[0], 0o777); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	written, err := EnsureInstallLaunchers(ctx, root, positional[0])
	if err != nil || len(written) != len(entryPoints) {
		fmt.Fprint(stderr, "icebath: launcher publication failed\n")
		return 1
	}
	fmt.Fprintln(stdout, strings.Join(written, "\n"))
	return 0
}
