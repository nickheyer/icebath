package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/selfupdate"
)

// Exit status of the source build helper verb named by argv, or false when argv names none
func runBuildHelperVerb(argv []string) (int, bool) {
	if len(argv) == 0 {
		return 0, false
	}
	switch argv[0] {
	case "electron-runtime":
		return runElectronRuntime(context.Background(), argv[1:], os.Stdout, os.Stderr), true
	case "webapp-build":
		return runWebappBuild(context.Background(), argv[1:], os.Stdout, os.Stderr), true
	case "channel-request-validate":
		return runChannelRequestValidate(os.Stdin, os.Stderr), true
	}
	return 0, false
}

// Prepares the locked Electron runtime of a checkout as scripts/build/electron-runtime.mjs does
func runElectronRuntime(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("electron-runtime", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "", "checkout whose installed desktop dependencies hold Electron")
	noInstall := flags.Bool("no-install", false, "refuse to run the Electron installer")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *source == "" {
		fmt.Fprintln(stderr, "--source is required")
		return 1
	}
	root, err := filepath.Abs(*source)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	env, err := selfupdate.SourceBuildEnv(ctx, nil, !*noInstall, true)
	if err == nil {
		var executable string
		if executable, err = selfupdate.PrepareElectronRuntime(ctx, root, !*noInstall, env); err == nil {
			fmt.Fprintln(stdout, executable)
			return 0
		}
	}
	fmt.Fprintln(stderr, err)
	return 1
}

// Prepares the private Webapp workspace and builds the browser renderer as apps/desktop/scripts/build-webapp.mjs does
func runWebappBuild(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("webapp-build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", ".", "checkout to build from")
	noInstall := flags.Bool("no-install", false, "reuse the installed workspace dependencies only")
	depsOnly := flags.Bool("deps-only", false, "prepare the workspace without compiling")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	root, err := filepath.Abs(*source)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	env, err := selfupdate.SourceBuildEnv(ctx, nil, !*noInstall, true)
	if err == nil {
		if *depsOnly {
			var workspace string
			if workspace, err = selfupdate.PrepareWebappWorkspace(ctx, root, "", !*noInstall, env); err == nil {
				fmt.Fprintln(stdout, "prepared "+workspace)
				return 0
			}
		} else if err = selfupdate.BuildSourceWebapp(ctx, root, "", !*noInstall, env); err == nil {
			fmt.Fprintln(stdout, "built "+filepath.Join(selfupdate.WebappDistDir(root), "index.html"))
			return 0
		}
	}
	fmt.Fprintln(stderr, err)
	return 1
}

// Admits the channel request on stdin as scripts/bundles/desktop_prepare.py validate_channel_request does
func runChannelRequestValidate(stdin io.Reader, stderr io.Writer) int {
	body, err := io.ReadAll(stdin)
	if err == nil {
		_, err = selfupdate.ValidateDesktopChannelRequest(body)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
