package codeexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ComputerUseArgs is the argparse namespace the computer-use parser tree produces
type ComputerUseArgs struct {
	Action       string
	PermsAction  string
	ScreenAction string
	Include      []string
	Skip         []string
	JSON         bool
	Force        bool
	Yes          bool
}

// ComputerUseCLI is the process one computer-use command runs in with its streams and help printer
type ComputerUseCLI struct {
	Env       *tools.Env
	Console   *cliui.Console
	Stderr    io.Writer
	PrintHelp func(path string) error
}

// ComputerUseCommand is the set_defaults func of the computer-use parser answering the exit code
type ComputerUseCommand func(ctx context.Context, cli *ComputerUseCLI, ns ComputerUseArgs) (int, error)

// Permission status the CLI reads at call time, swapped by tests as hermes patches the permissions module
var cliComputerUseStatus = computerUseStatus

// Leaf command paths of the computer-use tree the CLI binds to the dispatcher
var ComputerUseCommandPaths = []string{
	"computer-use install", "computer-use status", "computer-use doctor",
	"computer-use permissions status", "computer-use permissions grant",
	"computer-use screen status", "computer-use screen start", "computer-use screen stop", "computer-use screen install",
}

// Namespace a command path and its parsed flags produce, each flag read only where the path declares it
func ParseComputerUseArgs(path string, flags *pflag.FlagSet) (ComputerUseArgs, error) {
	var ns ComputerUseArgs
	parts := strings.Fields(path)
	if len(parts) > 1 {
		ns.Action = parts[1]
	}
	if len(parts) > 2 {
		switch ns.Action {
		case "permissions":
			ns.PermsAction = parts[2]
		case "screen":
			ns.ScreenAction = parts[2]
		}
	}
	if flags == nil {
		return ns, nil
	}
	for name, dst := range map[string]*[]string{"include": &ns.Include, "skip": &ns.Skip} {
		if flags.Lookup(name) != nil {
			v, err := flags.GetStringArray(name)
			if err != nil {
				return ns, err
			}
			*dst = v
		}
	}
	for name, dst := range map[string]*bool{"json": &ns.JSON, "force": &ns.Force, "yes": &ns.Yes} {
		if flags.Lookup(name) != nil {
			v, err := flags.GetBool(name)
			if err != nil {
				return ns, err
			}
			*dst = v
		}
	}
	return ns, nil
}

func (cli *ComputerUseCLI) printf(format string, a ...any) {
	fmt.Fprintf(cli.Console.Out, format, a...)
}
func (cli *ComputerUseCLI) println(s string) { fmt.Fprintln(cli.Console.Out, s) }

// Reports a plugin backend and its availability or doctor result, exited false for the built in cua backend
func exitIfPluginBackend(cli *ComputerUseCLI, doctor bool) (rc int, exited bool, err error) {
	if configuredBackendName(cli.Env) == defaultComputerUseBackend {
		return 0, false, nil
	}
	provider, err := getActiveProvider(cli.Env)
	if err != nil {
		var le *lookupError
		if errors.As(err, &le) {
			cli.println("Computer Use: " + le.Error())
			return 2, true, nil
		}
		return 0, true, err
	}
	if doctor {
		code, ok, err := provider.Doctor()
		if err != nil {
			return 0, true, err
		}
		if ok {
			return code, true, nil
		}
	}
	avail, err := provider.IsAvailable()
	if err != nil {
		return 0, true, err
	}
	state := "not available"
	if avail {
		state = "available"
	}
	cli.printf("Computer Use backend: %s (%s) — %s. cua-driver checks do not apply to this backend.\n",
		provider.Name(), provider.DisplayName(), state)
	if avail {
		return 0, true, nil
	}
	return 1, true, nil
}

// Explicit install or repair of the pinned driver
func cuInstall(ctx context.Context, cli *ComputerUseCLI, _ ComputerUseArgs) (int, error) {
	ok, err := installCuaDriver(ctx, cli.Env, cli.Console, true)
	if err != nil || !ok {
		return 1, err
	}
	return 0, nil
}

// Selected driver, its origin and runtime contract, and dead serve units on Linux
func cuStatus(_ context.Context, cli *ComputerUseCLI, _ ComputerUseArgs) (int, error) {
	if rc, exited, err := exitIfPluginBackend(cli, false); exited || err != nil {
		return rc, err
	}
	path, err := resolveCuaDriverCmd(nil)
	if err != nil {
		return 0, err
	}
	override := pystr.Strip(os.Getenv(cuaDriverCmdEnv))
	if path == "" {
		cli.println("cua-driver: not installed")
		cli.println("  Run: icebath computer-use install")
		return 1, nil
	}
	contract, err := cuaDriverContractStatus(cli.Env, path)
	if err != nil {
		return 0, err
	}
	rawVersion := ""
	if v, _ := contract.Get("version"); pyrepr.Truthy(v) {
		rawVersion = pyrepr.Str(v)
	}
	version := cuaVersionSummary(rawVersion, 120)
	origin := ""
	if override != "" {
		origin = " [custom binary from ICEBATH_CUA_DRIVER_CMD]"
	}
	line := "cua-driver: installed at " + path + origin
	if version != "" {
		line += " (" + version + ")"
	}
	cli.println(line)
	if v, _ := contract.Get("ready"); !pyrepr.Truthy(v) {
		reason := "runtime contract is incomplete"
		if r, _ := contract.Get("reason"); pyrepr.Truthy(r) {
			reason = pyrepr.Str(r)
		}
		cli.println("  ⚠ Repair required: " + reason)
		if override != "" {
			cli.println("    Update the binary selected by ICEBATH_CUA_DRIVER_CMD, or unset the override and run: icebath computer-use install --upgrade")
		} else {
			cli.println("    Run: icebath computer-use install")
		}
		return 1, nil
	}
	rc := 0
	if cuPlatform == "linux" {
		for _, u := range cuaDaemonUnits("") {
			if !u.runsServe {
				continue
			}
			sock := ""
			if u.socket != nil {
				sock = *u.socket
			}
			if l := cuaDaemonListening(cli.Env, path, sock, 3); l != nil && !*l {
				where := sock
				if where == "" {
					where = "the default socket"
				}
				cli.printf("  ✗ Daemon unit %s: no `cua-driver serve` is listening on %s\n", u.unit, where)
				cli.printf("    Check: systemctl --user status %s  (reinstalling the driver does not start it)\n", u.unit)
				rc = 1
			}
		}
	}
	if override != "" {
		cli.println("  ✓ Runtime contract ready (externally managed).")
	} else {
		cli.println("  ✓ Runtime contract ready (Icebath PM pin).")
	}
	return rc, nil
}

// Health report of the selected backend through its doctor or the cua-driver check matrix
func cuDoctor(_ context.Context, cli *ComputerUseCLI, ns ComputerUseArgs) (int, error) {
	if rc, exited, err := exitIfPluginBackend(cli, true); exited || err != nil {
		return rc, err
	}
	include := append([]string{}, ns.Include...)
	skip := append([]string{}, ns.Skip...)
	return runComputerUseDoctor(cli.Env, cli.Console.Out, cli.Stderr, nil, include, skip, ns.JSON, nil)
}

// Field of a status check row, dict or ordered object alike
func cuRowField(row any, key string) any {
	switch r := row.(type) {
	case *pyjson.Object:
		return cuGet(r, key)
	case map[string]any:
		return r[key]
	}
	return nil
}

// Glyph of a tri state grant, ticked for true, crossed for false and a dot for unknown
func cuGrantGlyph(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "✅"
		}
		return "❌"
	}
	return "•"
}

// Grant state of the selected backend as JSON or the human readable checklist
func cuPermsStatus(_ context.Context, cli *ComputerUseCLI, ns ComputerUseArgs) (int, error) {
	if !ns.JSON {
		if rc, exited, err := exitIfPluginBackend(cli, false); exited || err != nil {
			return rc, err
		}
	}
	st, err := cliComputerUseStatus(cli.Env, nil)
	if err != nil {
		return 0, err
	}
	ready := pyrepr.Truthy(cuGet(st, "ready"))
	exit := 1
	if ready {
		exit = 0
	}
	if ns.JSON {
		text, err := pyjson.Dumps(st, pyjson.Indent(2), pyjson.SortKeys(true))
		if err != nil {
			return 0, err
		}
		cli.println(text)
		return exit, nil
	}
	plat := pyrepr.Str(cuGet(st, "platform"))
	if !pyrepr.Truthy(cuGet(st, "platform_supported")) {
		cli.println("Computer Use is not supported on " + plat + ".")
		return 1, nil
	}
	if !pyrepr.Truthy(cuGet(st, "installed")) {
		cli.println("cua-driver: not installed. Run: icebath computer-use install")
		return 1, nil
	}
	version := "installed"
	if v := cuGet(st, "version"); pyrepr.Truthy(v) {
		version = pyrepr.Str(v)
	}
	cli.printf("cua-driver: %s (%s)\n", version, plat)
	if pyrepr.Truthy(cuGet(st, "can_grant")) {
		cli.printf("  %s Accessibility\n", cuGrantGlyph(cuGet(st, "accessibility")))
		cli.printf("  %s Screen Recording\n", cuGrantGlyph(cuGet(st, "screen_recording")))
		if !ready {
			cli.println("  Grant: icebath computer-use permissions grant")
			var missing []string
			for _, f := range tccFields {
				if b, ok := cuGet(st, f).(bool); ok && !b {
					missing = append(missing, f)
				}
			}
			if hint := staleTCCGrantHint(missing...); hint != "" {
				cli.println("  " + hint)
			}
		}
	} else {
		cli.printf("  %s driver health (no permission toggles on %s)\n", cuGrantGlyph(cuGet(st, "ready")), plat)
	}
	checks, _ := cuGet(st, "checks").([]any)
	for _, c := range checks {
		if pyrepr.Str(cuRowField(c, "status")) != "ok" {
			cli.printf("  ⚠ %s: %s\n", pyrepr.Str(cuRowField(c, "label")), pyrepr.Str(cuRowField(c, "message")))
		}
	}
	if e := cuGet(st, "error"); pyrepr.Truthy(e) {
		cli.println("  ⚠ " + pyrepr.Str(e))
	}
	return exit, nil
}

// Opens the grant dialog attributed to the driver process
func cuPermsGrant(_ context.Context, cli *ComputerUseCLI, _ ComputerUseArgs) (int, error) {
	if rc, exited, err := exitIfPluginBackend(cli, false); exited || err != nil {
		return rc, err
	}
	return requestPermissionsGrant(cli.Env, nil)
}

// Dispatcher of the computer-use tree, help for a missing action at the parser that lacks it
func BuildComputerUseParser() ComputerUseCommand {
	permsActions := map[string]ComputerUseCommand{"grant": cuPermsGrant, "status": cuPermsStatus}
	screen := buildScreenParser()
	cuPermissions := func(ctx context.Context, cli *ComputerUseCLI, ns ComputerUseArgs) (int, error) {
		if h, ok := permsActions[ns.PermsAction]; ok {
			return h(ctx, cli, ns)
		}
		return 0, cli.PrintHelp("computer-use permissions")
	}
	actions := map[string]ComputerUseCommand{"install": cuInstall, "status": cuStatus, "doctor": cuDoctor,
		"permissions": cuPermissions, "screen": screen}
	return func(ctx context.Context, cli *ComputerUseCLI, ns ComputerUseArgs) (int, error) {
		if h, ok := actions[ns.Action]; ok {
			return h(ctx, cli, ns)
		}
		return 0, cli.PrintHelp("computer-use")
	}
}
