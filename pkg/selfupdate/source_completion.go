package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Second phase marker of the entry, the re-executed binary rather than the bootstrap one
const sourceCompletionPrepared = "--prepared"

const sourceCompletionProg = "icebath source-completion"

// File whose presence marks an icebath source checkout
const sourceCompletionMarker = "pkg/selfupdate/source_completion.go"

const sourceCompletionDescription = "The tail every source install and update shares. A fresh install (``scripts/install.sh`` / " +
	"``scripts/install.ps1``) and a finished update must land in the same state: launchers published, product builds " +
	"current, and the post-build maintenance (skills sync, config migration) applied. One implementation, two callers -- " +
	"an install and an update cannot drift apart. ``main()`` is the installer entry. It is handed the bootstrap interpreter " +
	"the installer happens to have (uv, PM and no application dependencies), so it re-executes itself in PM's selected " +
	"interpreter before touching any product."

var (
	sourceCompletionBuildProducts = BuildUpdateProducts
	sourceCompletionMaintenance   = RunPostUpdateMaintenance
	exposePMGitPlatform           = runtime.GOOS
	exposePMGitStoreRoot          = pm.StoreRoot
	exposePMGitLocate             = tools.LocateCommand
	exposePMGitEnsure             = pm.Ensure
	sourceCompletionCall          = callSourceCompletion
	sourceCompletionLocked        = completeSourceLocked
	sourceCompletionWriteStamp    = WriteSourceStamp
)

// Inputs of the post build housekeeping an update or install tail runs
type PostUpdateMaintenance struct {
	AssumeYes, GatewayMode, HadDesktopAppBeforeUpdate bool
	PreUpdateSnapshotID, PreUpdateVersion             *string
	CompletionMessage                                 *string
	Followups                                         *[][2]string
	Stdout, Stderr                                    io.Writer
}

// Options of the tail every source install and update shares
type SourceCompletion struct {
	Desktop, AssumeYes, GatewayMode       bool
	PreUpdateSnapshotID, PreUpdateVersion *string
	CompletionMessage                     *string
	Announce                              string
	Followups                             *[][2]string
	BeforeBuild                           func()
	Stdout, Stderr                        io.Writer
}

// str of an exception, its class name when the message is empty
func failureReason(err error) string {
	if text := err.Error(); text != "" {
		return text
	}
	return strings.TrimPrefix(reflect.TypeOf(err).String(), "*")
}

// Publishes commands, builds the products, then runs post build maintenance under the shared update lock
func CompleteSourceCheckout(ctx context.Context, root string, o SourceCompletion) (bool, error) {
	lock := updaterecord.NewUpdateLock("", root, true)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, runtimeErr(fmt.Sprintf("an update is still running (%s); wait for it to exit, then relaunch Icebath", updaterecord.DescribeHolder(lock.Holder)))
	}
	defer lock.Release(ctx)
	return sourceCompletionLocked(ctx, root, o)
}

// Completion body run under a lock the caller already holds
func completeSourceLocked(ctx context.Context, root string, o SourceCompletion) (bool, error) {
	if err := ExposePMGit(ctx, root); err != nil {
		fmt.Fprintf(o.Stderr, "⚠ Could not provide git for the source completion: %s\n", err)
	}
	var owed [][2]string
	step := func(name string, run func() error) {
		if err := run(); err != nil {
			reason := failureReason(err)
			updaterecord.RecordFollowup(ctx, name, reason, updaterecord.FollowupRetryDefault)
			owed = append(owed, [2]string{name, reason})
		}
	}
	step("launchers", func() error { return publishLaunchers(ctx, root) })
	if o.BeforeBuild != nil {
		o.BeforeBuild()
	}
	step("build", func() error { return sourceCompletionBuildProducts(ctx, root, o.Desktop) })
	if o.Announce != "" {
		fmt.Fprintln(o.Stdout, o.Announce)
	}
	var verdict []bool
	step("maintenance", func() error {
		ok, err := sourceCompletionMaintenance(ctx, PostUpdateMaintenance{
			AssumeYes: o.AssumeYes, GatewayMode: o.GatewayMode, PreUpdateSnapshotID: o.PreUpdateSnapshotID,
			HadDesktopAppBeforeUpdate: o.Desktop, PreUpdateVersion: o.PreUpdateVersion,
			CompletionMessage: o.CompletionMessage, Followups: &owed, Stdout: o.Stdout, Stderr: o.Stderr})
		if err != nil {
			return err
		}
		verdict = append(verdict, ok)
		return nil
	})
	tailDone := len(owed) == 0
	if tailDone {
		if _, err := sourceCompletionWriteStamp(root, false); err != nil {
			if class := launchErrorClass(err); !pypath.IsOSError(err) && !osErrorClasses[class] && class != "ValueError" {
				return false, err
			}
			fmt.Fprintf(o.Stderr, "⚠ Source update completed, but the install stamp could not be written: %s\n", launchErrorText(err))
		}
	}
	if o.Followups != nil {
		*o.Followups = append(*o.Followups, owed...)
	}
	return tailDone && !slices.Contains(verdict, false), nil
}

// Puts the pm git on PATH and in its facts for a Windows checkout without a git of its own
func ExposePMGit(ctx context.Context, projectRoot string) error {
	if exposePMGitPlatform != "windows" || !existsFollow(filepath.Join(projectRoot, ".git")) {
		return nil
	}
	if found := exposePMGitLocate("git"); len(found) > 0 {
		resolved, err := pypath.Resolve(found[0])
		if err != nil {
			return err
		}
		store, err := exposePMGitStoreRoot(ctx)
		if err != nil {
			return err
		}
		if _, inside := pypath.PathRelativeTo(resolved, store); !inside {
			return nil
		}
	}
	runner, err := exposePMGitEnsure(ctx, "git", pm.EnsureOptions{Explicit: true})
	if err != nil {
		return err
	}
	for key, value := range runner.Env {
		if strings.ToUpper(key) == "PATH" {
			if value != "" {
				return os.Setenv("PATH", value)
			}
			return nil
		}
	}
	return nil
}

// Command re-entering the checkout through its own binary with the prepared marker
func sourceCompletionBootstrapCommand(root string, argv []string) ([]string, error) {
	binary := resolveStorePython(root)
	if binary == "" {
		return nil, runtimeErr("source update has no managed Python; run `icebath pm install`")
	}
	return append(append([]string{binary, SourceCompletionEntry, "--source", root}, argv...), sourceCompletionPrepared), nil
}

// Parsed arguments of the source completion entry
type sourceCompletionArgs struct {
	source                             string
	desktop, interactive, finishUpdate bool
}

var sourceCompletionLongOptions = []string{"--help", "--source", "--desktop", "--interactive", "--finish-update"}

// Option an argument names exactly, by an equals split or by a unique long prefix, with its explicit value
func sourceCompletionMatch(arg string) (string, *string, bool) {
	exact := map[string]bool{"-h": true}
	for _, option := range sourceCompletionLongOptions {
		exact[option] = true
	}
	if exact[arg] {
		return arg, nil, true
	}
	name, value, hasValue := strings.Cut(arg, "=")
	if hasValue && exact[name] {
		return name, &value, true
	}
	if strings.HasPrefix(arg, "--") {
		var matches []string
		for _, option := range sourceCompletionLongOptions {
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

// Reports an argument argparse reads as an option string rather than a value
func sourceCompletionLooksOptional(arg string) bool {
	if arg == "" || arg[0] != '-' || len(arg) == 1 {
		return false
	}
	if _, _, ok := sourceCompletionMatch(arg); ok {
		return true
	}
	return !venvSyncNegativeNumber.MatchString(arg) && !strings.Contains(arg, " ")
}

// Usage line of the entry wrapped the way argparse wraps optionals past the text width
func sourceCompletionUsage(t pm.ArgparseColors) string {
	parts := []string{
		fmt.Sprintf("[%s-h%s]", t.SummaryShort, t.Reset),
		fmt.Sprintf("%s--source%s %sSOURCE%s", t.SummaryLong, t.Reset, t.SummaryLabel, t.Reset),
		fmt.Sprintf("[%s--desktop%s]", t.SummaryLong, t.Reset),
		fmt.Sprintf("[%s--interactive%s]", t.SummaryLong, t.Reset),
		fmt.Sprintf("[%s--finish-update%s]", t.SummaryLong, t.Reset),
	}
	prefix := t.Usage + "usage: " + t.Reset
	prefixLen := len("usage: ")
	prog := t.Prog + sourceCompletionProg + t.Reset
	progLen := len(sourceCompletionProg)
	width := pm.ArgparseWidth()
	if line := prog + " " + strings.Join(parts, " "); prefixLen+decolorLen(line) <= width {
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

// Help of the entry in the argparse layout with the module description filled to the width
func sourceCompletionHelp(t pm.ArgparseColors) string {
	width := pm.ArgparseWidth()
	helpPosition := min(len("--finish-update")+2+2, min(24, max(width-20, 4)))
	actions := []struct {
		header string
		length int
		help   string
	}{
		{fmt.Sprintf("%s-h%s, %s--help%s", t.ShortOption, t.Reset, t.LongOption, t.Reset), len("-h, --help"), "show this help message and exit"},
		{fmt.Sprintf("%s--source%s %sSOURCE%s", t.LongOption, t.Reset, t.Label, t.Reset), len("--source SOURCE"), ""},
		{fmt.Sprintf("%s--desktop%s", t.LongOption, t.Reset), len("--desktop"), "Also build the packaged desktop app."},
		{fmt.Sprintf("%s--interactive%s", t.LongOption, t.Reset), len("--interactive"), "Allow prompts; installers pass no flag and stay unattended."},
		{fmt.Sprintf("%s--finish-update%s", t.LongOption, t.Reset), len("--finish-update"), "Complete a source UPDATE tail instead of an install: the same launchers/products/maintenance with update wording."},
	}
	var b strings.Builder
	b.WriteString(sourceCompletionUsage(t))
	b.WriteString("\n" + strings.Join(wrapHelp(sourceCompletionDescription, width), "\n") + "\n")
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

// Parses the entry's arguments with argparse semantics, an exit code when help or an error ends the run
func parseSourceCompletionArgs(argv []string, stdout, stderr io.Writer) (sourceCompletionArgs, int, bool) {
	fail := func(message string) (sourceCompletionArgs, int, bool) {
		io.WriteString(stderr, sourceCompletionUsage(pm.ArgparseThemeFor(stderr)))
		fmt.Fprintf(stderr, "%s: error: %s\n", sourceCompletionProg, message)
		return sourceCompletionArgs{}, 2, false
	}
	var args sourceCompletionArgs
	var unknown []string
	sourceGiven := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			unknown = append(unknown, argv[i+1:]...)
			break
		}
		if !sourceCompletionLooksOptional(arg) {
			unknown = append(unknown, arg)
			continue
		}
		option, value, ok := sourceCompletionMatch(arg)
		if !ok {
			unknown = append(unknown, arg)
			continue
		}
		if option == "-h+" {
			if explicit, help := venvSyncShortHelp(*value); !help {
				return fail("argument -h/--help: ignored explicit argument " + pyrepr.StrRepr(explicit))
			}
			io.WriteString(stdout, sourceCompletionHelp(pm.ArgparseThemeFor(stdout)))
			return sourceCompletionArgs{}, 0, false
		}
		if option == "--source" {
			if value == nil {
				if i+1 >= len(argv) || argv[i+1] == "--" || sourceCompletionLooksOptional(argv[i+1]) {
					return fail("argument --source: expected one argument")
				}
				i++
				value = &argv[i]
			}
			args.source, sourceGiven = *value, true
			continue
		}
		label := option
		if option == "-h" || option == "--help" {
			label = "-h/--help"
		}
		if value != nil {
			return fail("argument " + label + ": ignored explicit argument " + pyrepr.StrRepr(*value))
		}
		switch option {
		case "-h", "--help":
			io.WriteString(stdout, sourceCompletionHelp(pm.ArgparseThemeFor(stdout)))
			return sourceCompletionArgs{}, 0, false
		case "--desktop":
			args.desktop = true
		case "--interactive":
			args.interactive = true
		case "--finish-update":
			args.finishUpdate = true
		}
	}
	if !sourceGiven {
		return fail("the following arguments are required: --source")
	}
	if len(unknown) > 0 {
		return fail("unrecognized arguments: " + strings.Join(unknown, " "))
	}
	return args, 0, true
}

// Exit status of the prepared child, the negative signal number of a killed one
func callSourceCompletion(ctx context.Context, command []string, dir string, env []string, files []*os.File, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir, cmd.Env, cmd.ExtraFiles = dir, env, files
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return pysubprocess.Returncode(exit), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Installer and launch entry of the tail, bootstrapping into the checkout's binary before any product work
func SourceCompletionMain(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	prepared := slices.Contains(argv, sourceCompletionPrepared)
	var rest []string
	for _, a := range argv {
		if a != sourceCompletionPrepared {
			rest = append(rest, a)
		}
	}
	args, code, ok := parseSourceCompletionArgs(rest, stdout, stderr)
	if !ok {
		return code
	}
	uncaught := func(err error) int {
		fmt.Fprintf(stderr, "%s: %s\n", launchErrorClass(err), launchErrorText(err))
		return 1
	}
	root, err := pypath.Resolve(args.source)
	if err != nil {
		return uncaught(err)
	}
	if !isRegular(filepath.Join(root, sourceCompletionMarker)) {
		fmt.Fprintf(stderr, "✗ %s is not a Icebath source checkout\n", root)
		return 1
	}
	if prepared {
		if err := pm.ActivateDependencies(ctx, root); err != nil {
			return uncaught(err)
		}
		var done bool
		if args.finishUpdate {
			var owed [][2]string
			if _, err := CompleteSourceCheckout(ctx, root, SourceCompletion{Desktop: args.desktop, AssumeYes: true,
				Announce: "\n✓ Code updated!", Followups: &owed, Stdout: stdout, Stderr: stderr}); err != nil {
				return uncaught(err)
			}
			done = len(owed) == 0
		} else {
			message := "✓ Install complete!"
			if done, err = CompleteSourceCheckout(ctx, root, SourceCompletion{Desktop: args.desktop, AssumeYes: !args.interactive,
				CompletionMessage: &message, Stdout: stdout, Stderr: stderr}); err != nil {
				return uncaught(err)
			}
		}
		if done {
			return 0
		}
		return 1
	}
	var passthrough []string
	if args.desktop {
		passthrough = append(passthrough, "--desktop")
	}
	if args.finishUpdate {
		passthrough = append(passthrough, "--finish-update")
	}
	command, err := sourceCompletionBootstrapCommand(root, passthrough)
	if err != nil {
		return uncaught(err)
	}
	files, err := updaterecord.CheckoutLockFiles(root)
	if err != nil {
		return uncaught(err)
	}
	env, err := pm.ActivationEnvironment(ctx, root)
	if err != nil {
		return uncaught(err)
	}
	code, err = sourceCompletionCall(ctx, command, root, env, files, stdout, stderr)
	if err != nil {
		return uncaught(err)
	}
	return code
}
