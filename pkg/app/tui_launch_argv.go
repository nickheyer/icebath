package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

// Launch arguments of the TUI the chat entry points pass through
type TUILaunch struct {
	Resume, Model, Provider, Toolsets, Query, Image                                string
	Dev, Native, AcceptHooks, Verbose, Quiet, Worktree, Checkpoints, PassSessionID bool
	Skills                                                                         []string
	MaxTurns                                                                       *int
}

// Source build steps of a TUI launch, the receipt reader, the build env, the dependency union and the compiler
var tuiSourceBuild = struct {
	productCurrent func(ctx context.Context, projectRoot, product, out string) bool
	buildEnv       func(ctx context.Context) (map[string]string, error)
	prepare        func(ctx context.Context, projectRoot string, env map[string]string) error
	buildTUI       func(ctx context.Context, projectRoot string, env map[string]string) error
	runInk         func(npm, dir string, env map[string]string) error
}{
	productCurrent: selfupdate.SourceProductCurrent,
	buildEnv: func(ctx context.Context) (map[string]string, error) {
		return selfupdate.SourceBuildEnv(ctx, nil, false, true)
	},
	prepare:  selfupdate.PrepareLaunchDependencies,
	buildTUI: selfupdate.BuildSourceTUI,
	runInk: func(npm, dir string, env map[string]string) error {
		cmd := exec.Command(npm, "run", "build")
		cmd.Dir, cmd.Env = dir, pm.EnvList(env)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	},
}

// Bundled TUI entry of the package dir, the _find_bundled_tui hermes tests replace
var tuiBundledEntry = func() string { return findBundledTUI("") }

// True when a forced build is asked for or the TUI receipt no longer matches its inputs
func tuiNeedRebuild(ctx context.Context, root string) bool {
	switch strings.ToLower(pystr.Strip(os.Getenv("ICEBATH_TUI_FORCE_BUILD"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return !tuiSourceBuild.productCurrent(ctx, filepath.Dir(root), "tui", filepath.Join(root, "dist"))
}

// Regular file test of a TUI entry point
func tuiEntryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// TUI argv and cwd, tsx sources under dev, else a prebuilt, bundled, current or freshly compiled dist
func makeTUIArgvTo(stdout, stderr io.Writer) func(ctx context.Context, tuiDir string, dev bool) ([]string, string, error) {
	return func(ctx context.Context, tuiDir string, dev bool) ([]string, string, error) {
		extDir := os.Getenv("ICEBATH_TUI_DIR")
		if dev && extDir != "" {
			fmt.Fprintf(stderr, "Error: --dev is incompatible with ICEBATH_TUI_DIR=%s\n"+
				"The prebuilt TUI has no source code to hot-reload.\n"+
				"Unset ICEBATH_TUI_DIR (e.g. `unset ICEBATH_TUI_DIR`) to use --dev from a checkout.\n", extDir)
			return nil, "", &tuiExitError{1}
		}
		nodeArgv := func(entry, cwd string) ([]string, string, error) {
			node, err := tuiNodeBin(ctx, "node", stdout)
			if err != nil {
				return nil, "", err
			}
			return []string{node, "--expose-gc", entry}, cwd, nil
		}
		if !dev {
			if extDir != "" {
				if entry := filepath.Join(extDir, "dist", "entry.js"); tuiEntryExists(entry) {
					return nodeArgv(entry, extDir)
				}
			}
			if bundled := tuiBundledEntry(); bundled != "" {
				return nodeArgv(bundled, filepath.Dir(bundled))
			}
		}
		if extDir == "" {
			if err := ensureTUIWorkspace(tuiDir, stdout, stderr); err != nil {
				return nil, "", err
			}
		}
		if !dev && !tuiNeedRebuild(ctx, tuiDir) {
			return nodeArgv(filepath.Join(tuiDir, "dist", "entry.js"), tuiDir)
		}
		projectRoot := filepath.Dir(tuiDir)
		env, err := tuiSourceBuild.buildEnv(ctx)
		if err != nil {
			return nil, "", err
		}
		if err := tuiSourceBuild.prepare(ctx, projectRoot, env); err != nil {
			return nil, "", err
		}
		path, hasPath := env["PATH"]
		if !hasPath {
			return nil, "", fmt.Errorf("KeyError: 'PATH'")
		}
		if dev {
			npm, found := pyshutil.Which("npm", &path)
			if !found {
				return nil, "", fmt.Errorf("TypeError: expected str, bytes or os.PathLike object, not NoneType")
			}
			if err := tuiSourceBuild.runInk(npm, filepath.Join(tuiDir, "packages", "hermes-ink"), env); err != nil {
				return nil, "", err
			}
			tsx := filepath.Join(tuiDir, "node_modules", ".bin", "tsx")
			if _, err := os.Stat(tsx); err == nil {
				return []string{tsx, "src/entry.tsx"}, tuiDir, nil
			}
			return []string{npm, "start"}, tuiDir, nil
		}
		if err := tuiSourceBuild.buildTUI(ctx, projectRoot, env); err != nil {
			return nil, "", err
		}
		node, found := pyshutil.Which("node", &path)
		if !found {
			return nil, "", fmt.Errorf("TypeError: expected str, bytes or os.PathLike object, not NoneType")
		}
		return []string{node, "--expose-gc", filepath.Join(tuiDir, "dist", "entry.js")}, tuiDir, nil
	}
}

// Runs the TUI over the real argv builder, worktree setup, child runner, exit summary and relaunch
func LaunchTUI(ctx context.Context, l TUILaunch) int {
	return launchTUIWith(ctx, l, tuiLaunchDeps{
		makeArgv: makeTUIArgvTo(os.Stdout, os.Stderr),
		setupWorktree: func(ctx context.Context) (map[string]any, error) {
			return setupTUIWorktree(ctx, os.Stderr)
		},
		call: tuiSubprocessCall,
		printSummary: func(ctx context.Context, sessionID, activeSessionFile string) {
			printTUIExitSummary(ctx, os.Stdout, sessionID, activeSessionFile)
		},
		relaunch: relaunch,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	})
}
