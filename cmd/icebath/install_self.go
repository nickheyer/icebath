package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/spf13/cobra"
)

// Runs install-self, the update hand off, the uninstall hooks or the install stages as the flags select
func installSelfGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	str := func(name string) string { v, _ := cmd.Flags().GetString(name); return v }
	flag := func(name string) bool { v, _ := cmd.Flags().GetBool(name); return v }
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if flag("release-info") {
		data, err := json.Marshal(runningRelease())
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, string(data))
		return 0, nil
	}
	if flag("protocol-version") {
		fmt.Fprintln(stdout, install.ProtocolVersion)
		return 0, nil
	}
	if flag("from-checkout") {
		return installFromCheckout(ctx, str("dir"), stdout, stderr), nil
	}
	if flag("uninstall") {
		if err := install.UninstallHooks(func(line string) { fmt.Fprintln(stdout, line) }); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, nil
		}
		return 0, nil
	}
	if flag("update") {
		argv := []string{"--update"}
		for _, name := range []string{"channel", "branch", "target-app"} {
			if v := str(name); v != "" {
				argv = append(argv, "--"+name, v)
			}
		}
		if h := str("icebath-home"); h != "" {
			if err := os.Setenv("ICEBATH_HOME", h); err != nil {
				return 1, err
			}
		}
		err := install.RunUpdate(ctx, install.UpdateHost{Args: argv, Emit: func(e install.Event) {
			switch {
			case e.Type == install.EventLog && e.Stream == install.Stderr:
				fmt.Fprintln(stderr, e.Line)
			case e.Type == install.EventLog:
				fmt.Fprintln(stdout, e.Line)
			case e.Type == install.EventFailed && e.Error != nil:
				fmt.Fprintln(stderr, *e.Error)
			}
		}})
		if err != nil {
			return 1, nil
		}
		return 0, nil
	}
	opts := install.Options{
		Channel: str("channel"), Branch: str("branch"), Commit: str("commit"), Dir: str("dir"), Home: str("icebath-home"),
		Binary: str("binary"), Version: str("version"), FromSelf: true, Stage: str("stage"), Manifest: flag("manifest"), JSON: flag("json"),
		NonInteractive: flag("non-interactive") || flag("skip-setup"), IncludeDesktop: flag("include-desktop"),
		Verbose: flag("verbose"), SkipBrowser: flag("skip-browser") || flag("no-playwright"), SkipComputerUse: flag("skip-computer-use"),
		InstallMethod: install.MethodScript,
	}
	if flag("show-resolved-paths") {
		report, err := install.ResolvedPathsJSON(opts)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2, nil
		}
		fmt.Fprintln(stdout, report)
		return 0, nil
	}
	return install.Main(opts, os.Stdout, os.Stderr), nil
}

// Publishes a source checkout's built binary, then syncs bundled skills, copying them when the sync fails
func installFromCheckout(ctx context.Context, checkout string, stdout, stderr io.Writer) int {
	if checkout == "" {
		fmt.Fprintln(stderr, "--from-checkout needs --dir CHECKOUT")
		return 2
	}
	report := func(line string) { fmt.Fprintln(stdout, line) }
	if err := install.SetupFromCheckout(ctx, checkout, report); err != nil {
		fmt.Fprintln(stderr, "✗ "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Syncing bundled skills to ~/.icebath/skills/ ...")
	if _, err := skills.SyncSkills(ctx, true); err == nil {
		fmt.Fprintln(stdout, "✓ Skills synced")
		return 0
	}
	src := filepath.Join(checkout, "skills")
	if st, err := os.Stat(src); err == nil && st.IsDir() {
		if err := install.CopySkillsNoClobber(src, filepath.Join(install.Home(), "skills")); err == nil {
			fmt.Fprintln(stdout, "✓ Skills copied")
		}
	}
	return 0
}

// Release identity of the running binary, its stamped tag and embedded vcs revision
func runningRelease() install.ReleaseInfo {
	info := install.ReleaseInfo{Tag: updaterecord.BuildVersion()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && install.IsFullSHA(s.Value) {
				info.Commit = s.Value
			}
		}
	}
	return info
}
