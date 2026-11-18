//go:build !icebath_core && !icebath_noextras

package commands

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/spf13/cobra"
)

// Hands the pm verb binder to the command tree
func init() { pmHandlers = bindPM }

// Binds every pm verb to its pkg/pm command, the worker entry included
func bindPM(c *CLI) {
	c.Bind("pm lock", func(cmd *cobra.Command, args []string) error {
		lockArgs := pmLockArgs(cmd, args)
		if msg := pm.LockUsageError(lockArgs); msg != "" {
			return &ArgparseError{Cmd: cmd, Msg: msg}
		}
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdLock(ctx, lockArgs) })
	})
	c.Bind("pm install", func(cmd *cobra.Command, args []string) error {
		installArgs := pmInstallArgs(cmd, args)
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdInstall(ctx, installArgs) })
	})
	c.Bind("pm env", func(cmd *cobra.Command, args []string) error {
		names := append([]string{}, args...)
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdEnv(ctx, names) })
	})
	c.Bind("pm doctor", func(cmd *cobra.Command, args []string) error {
		repair, _ := cmd.Flags().GetBool("repair")
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdDoctor(ctx, repair) })
	})
	c.Bind("pm repair", func(cmd *cobra.Command, args []string) error { return runPM(cmd, pm.CmdRepair) })
	c.Bind("pm environment", func(cmd *cobra.Command, args []string) error {
		envArgs := pmEnvironmentArgs(cmd, args)
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdEnvironment(ctx, envArgs) })
	})
	c.Bind("pm gc", func(cmd *cobra.Command, args []string) error { return runPM(cmd, pm.CmdGC) })
	c.Bind("pm status", func(cmd *cobra.Command, args []string) error { return runPM(cmd, pm.CmdStatus) })
	c.Bind("pm update", func(cmd *cobra.Command, args []string) error {
		updateArgs := pmUpdateArgs(cmd, args)
		return runPM(cmd, func(ctx context.Context) (int, error) { return pm.CmdUpdateArgs(ctx, updateArgs) })
	})
	c.Bind("pm worker", func(cmd *cobra.Command, args []string) error {
		os.Exit(pm.WorkerMain())
		return nil
	})
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
