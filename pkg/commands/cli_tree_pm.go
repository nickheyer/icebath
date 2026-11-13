package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/spf13/cobra"
)

// Adds the pm tree bound to pkg/pm handlers plus the hidden update takeover entry
func (c *CLI) addPM(root *cobra.Command) {
	pm.SetCommandArgv(c.CommandArgv)
	pmCmd := c.newCmd(root, "pm", "Package manager: lock, install, repair, env, doctor, gc", "")
	c.Bind("pm", func(cmd *cobra.Command, args []string) error {
		return &ExitError{Code: 2, Msg: fmt.Sprintf("usage: %s\n%s: error: the following arguments are required: cmd", cmd.UseLine(), cmd.CommandPath())}
	})

	lock := c.newCmd(pmCmd, "lock", "relock uv.lock from pyproject.toml (or --bump a tool pin in pm/lock.json)",
		"With no arguments: re-resolve uv.lock from pyproject.toml; changes no environment and writes nothing when the lock is current. With --bump NAME VERSION: pin a pm tool's artifacts in pm/lock.json; uv.lock is untouched.")
	sf(lock, "bump", "", "", "pin tool NAME at VERSION in pm/lock.json instead of relocking uv.lock")
	c.pos(lock, pOpt("VERSION"))
	c.Bind("pm lock", func(cmd *cobra.Command, args []string) error {
		lockArgs := pmLockArgs(cmd, args)
		if msg := pm.LockUsageError(lockArgs); msg != "" {
			return &ExitError{Code: 2, Msg: fmt.Sprintf("usage: %s\n%s: error: %s", cmd.UseLine(), cmd.CommandPath(), msg)}
		}
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdLock(ctx, lockArgs) })
	})

	install := c.newCmd(pmCmd, "install", "install packages (default: all required + optional defaults)", "")
	c.pos(install, pAny("names"))
	af(install, "extra", "", "enable a declared dependency extra in the venv (repeatable)")
	af(install, "without", "", "leave an optional default package (chromium, cua-driver) or a recorded extra outside [all] out of this and every later default install and update; `icebath pm install NAME` / `--extra NAME` opts back in (repeatable)")
	bf(install, "tools-only", "", "install the tool closure, put it on PATH, and stop before the venv sync")
	bf(install, "trust-recorded", "", "trust the recorded tool digest instead of re-hashing every entry. shell activation only. a deliberate install re-checks the bytes")
	sf(install, "test-environment", "", "", "also make this checkout's isolated test environment current (activation). EXTRAS is comma-separated; omitted selects [all]")
	noOpt(install, "test-environment", FlagPresent)
	sf(install, "target", "", "", "stage for a cross target (e.g. linux-arm64-bionic on a glibc CI host); requires explicit package names")
	c.Bind("pm install", func(cmd *cobra.Command, args []string) error {
		installArgs := pmInstallArgs(cmd, args)
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdInstall(ctx, installArgs) })
	})

	env := c.newCmd(pmCmd, "env", "print composed env of installed packages", "")
	c.pos(env, pAny("names"))
	c.Bind("pm env", func(cmd *cobra.Command, args []string) error {
		names := append([]string{}, args...)
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdEnv(ctx, names) })
	})

	doctor := c.newCmd(pmCmd, "doctor", "check installed state against the lockfile", "")
	bf(doctor, "repair", "", "remove corrupt entries from PM's uv cache so they are fetched again")
	c.Bind("pm doctor", func(cmd *cobra.Command, args []string) error {
		repair, _ := cmd.Flags().GetBool("repair")
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdDoctor(ctx, repair) })
	})

	c.newCmd(pmCmd, "repair", "rebuild the recorded dependency environment without changing its graph", "")
	c.Bind("pm repair", func(cmd *cobra.Command, args []string) error { return runPM(cmd, pm.CmdRepair) })

	environment := c.newCmd(pmCmd, "environment", "make a named side environment current and print its python",
		"Build or reuse ICEBATH_HOME/environments/NAME, an isolated venv that never touches icebath's own: --extra installs the pins of a declared extra, --requirement an ad hoc bounded requirement (both repeatable). Prints only the interpreter path on stdout.")
	c.pos(environment, p("name"))
	af(environment, "extra", "", "")
	af(environment, "requirement", "", "")
	c.Bind("pm environment", func(cmd *cobra.Command, args []string) error {
		envArgs := pmEnvironmentArgs(cmd, args)
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdEnvironment(ctx, envArgs) })
	})

	c.newCmd(pmCmd, "gc", "remove store entries nothing references", "")
	c.Bind("pm gc", func(cmd *cobra.Command, args []string) error { return runPM(cmd, pm.CmdGC) })

	c.newCmd(pmCmd, "status", "print the latest pm sync receipt (machine-readable)", "")
	c.Bind("pm status", func(cmd *cobra.Command, args []string) error { return runPM(cmd, pm.CmdStatus) })

	update := c.newCmd(pmCmd, "update", "resolve latest versions and re-pin the lockfile", "")
	c.pos(update, pAny("names"))
	bf(update, "check", "", "dry-run: print what would change, write nothing (exit 1 if updates exist)")
	sf(update, "target", "", "", "resolve for a different target instead of this machine (e.g. win32-arm64)")
	af(update, "skip-target", "", "leave TARGET's pins as they are, e.g. linux-arm64-bionic while the termux pool lags upstream (repeatable)")
	c.choice(update, "skip-target", pm.AllTargets...)
	bf(update, "uv", "", "also refresh uv.lock + venv (uv update + sync)")
	bf(update, "npm", "", "also refresh package-lock.json (npm update)")
	bf(update, "termux", "", "repin the termux pool archives the pool has retired (the runtime-lib table + the bionic lock rows); --check reports without writing")
	c.Bind("pm update", func(cmd *cobra.Command, args []string) error {
		updateArgs := pmUpdateArgs(cmd, args)
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdUpdateArgs(ctx, updateArgs) })
	})

	worker := c.newCmd(pmCmd, "worker", "isolated package manager worker", "")
	worker.Hidden = true
	c.Bind("pm worker", func(cmd *cobra.Command, args []string) error {
		os.Exit(pm.WorkerMain())
		return nil
	})

	takeover := c.newCmd(root, "update-takeover", "finish an update a historical updater handed off", "")
	takeover.Hidden = true
	c.pos(takeover, p("context"), p("result"))
	serveResume := c.newCmd(root, "update-serve-resume", "relaunch the manual backends a historical updater paused", "")
	serveResume.Hidden = true
	c.pos(serveResume, p("context"), p("result"))
	finish := c.newCmd(root, "update-finish", "finish an update in the selected checkout", "")
	finish.Hidden = true
	c.pos(finish, p("context"), p("result"))
	restartRecovery := c.newCmd(root, "update-restart-recovery", "restart supervised gateways and serve units in a fresh process", "")
	restartRecovery.Hidden, restartRecovery.DisableFlagParsing = true, true
	c.pos(restartRecovery, pRem("args"))
	updateCompletion := c.newCmd(root, "update-completion", "run a source update's post commit completion", "")
	updateCompletion.Hidden, updateCompletion.DisableFlagParsing = true, true
	c.pos(updateCompletion, pRem("args"))
	venvSync := c.newCmd(root, "venv-sync", "report or sync a checkout's Python dependencies", "")
	venvSync.Hidden, venvSync.DisableFlagParsing = true, true
	c.pos(venvSync, pRem("args"))
	completion := c.newCmd(root, "source-completion", "finish a source update's launchers, products and maintenance", "")
	completion.Hidden, completion.DisableFlagParsing = true, true
	c.pos(completion, pRem("args"))
	importHealth := c.newCmd(root, "import-health", "answer an update's import health probe", "")
	importHealth.Hidden, importHealth.DisableFlagParsing = true, true
	c.pos(importHealth, pRem("args"))
}

// Runs one pm command through pm Main, a nonzero status becoming an ExitError
func runPM(cmd *cobra.Command, command func(context.Context) (int, error)) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	code, err := pm.Main(ctx, command)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// Lock arguments with a nil name or version for an absent bump flag or positional
func pmLockArgs(cmd *cobra.Command, args []string) pm.LockArgs {
	var lockArgs pm.LockArgs
	if cmd.Flags().Changed("bump") {
		name, _ := cmd.Flags().GetString("bump")
		lockArgs.Name = &name
	}
	if len(args) > 0 {
		version := args[0]
		lockArgs.Version = &version
	}
	return lockArgs
}

// Install arguments with a bare test environment flag meaning all extras and absent optional flags nil
func pmInstallArgs(cmd *cobra.Command, args []string) pm.InstallArgs {
	f := cmd.Flags()
	installArgs := pm.InstallArgs{Names: append([]string{}, args...)}
	installArgs.Extra, _ = f.GetStringArray("extra")
	installArgs.Without, _ = f.GetStringArray("without")
	installArgs.ToolsOnly, _ = f.GetBool("tools-only")
	installArgs.TrustRecorded, _ = f.GetBool("trust-recorded")
	if f.Changed("test-environment") {
		extras, _ := f.GetString("test-environment")
		if extras == FlagPresent {
			extras = ""
		}
		installArgs.TestEnvironment = &extras
	}
	if f.Changed("target") {
		target, _ := f.GetString("target")
		installArgs.Target = &target
	}
	return installArgs
}

// Side environment name with its repeatable extras and requirements
func pmEnvironmentArgs(cmd *cobra.Command, args []string) pm.EnvironmentArgs {
	envArgs := pm.EnvironmentArgs{Name: args[0]}
	envArgs.Extra, _ = cmd.Flags().GetStringArray("extra")
	envArgs.Requirement, _ = cmd.Flags().GetStringArray("requirement")
	return envArgs
}

// Update names, target, skipped targets and the uv, npm and termux passes
func pmUpdateArgs(cmd *cobra.Command, args []string) pm.UpdateArgs {
	f := cmd.Flags()
	updateArgs := pm.UpdateArgs{Names: append([]string{}, args...)}
	updateArgs.Check, _ = f.GetBool("check")
	updateArgs.Target, _ = f.GetString("target")
	updateArgs.SkipTarget, _ = f.GetStringArray("skip-target")
	updateArgs.UV, _ = f.GetBool("uv")
	updateArgs.NPM, _ = f.GetBool("npm")
	updateArgs.Termux, _ = f.GetBool("termux")
	return updateArgs
}
