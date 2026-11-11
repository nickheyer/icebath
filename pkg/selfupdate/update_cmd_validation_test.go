package selfupdate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Answers the import health probe when this test binary is started as an install's probe child
func init() {
	if len(os.Args) > 2 && strings.HasPrefix(os.Args[1], pm.PinDefaultHomeFlag) && os.Args[2] == ImportHealthEntry {
		pm.SetCommandArgv(func(argv []string) []string {
			for len(argv) > 0 && strings.HasPrefix(argv[0], "--") {
				argv = argv[1:]
			}
			return argv
		})
		if !pm.ShouldSkipExternalSecretSources() {
			os.Exit(3)
		}
		if os.Getenv("ICEBATH_UC_PROBE_FORGE") == "1" {
			os.Stdout.WriteString("__ICEBATH_IMPORT_HEALTH__[]")
		}
		if code, err := strconv.Atoi(os.Getenv("ICEBATH_UC_PROBE_EXIT")); err == nil {
			os.Exit(code)
		}
		os.Exit(ImportHealthMain(os.Args[3:], os.Stdout))
	}
}

// Publishes this test binary as the install binary of a fixture checkout
func ucInstallBinary(t *testing.T, repo string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "icebath"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	selected := filepath.Join(repo, name)
	if os.Link(self, selected) == nil {
		return selected
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(selected, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	return selected
}

func TestUpdateImportProbeUsesSelectedDependencies(t *testing.T) {
	repo := t.TempDir()
	selected := ucInstallBinary(t, repo)
	var ran []string
	saved := importHealthRun
	importHealthRun = func(ctx context.Context, argv []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		ran = argv
		return saved(ctx, argv, o)
	}
	t.Cleanup(func() { importHealthRun = saved })
	order, failures, err := criticalModuleImportFailures(context.Background(), repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 0 || len(failures) != 0 {
		t.Fatalf("failures = %v %v", order, failures)
	}
	if len(ran) == 0 || ran[0] != selected {
		t.Fatalf("the probe ran %v, not the checkout's selected binary %s", ran, selected)
	}
}

func TestUpdateProbeChildrenSkipExternalSecretSources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command secret source is POSIX only")
	}
	tmp := t.TempDir()
	h := filepath.Join(tmp, "icebath-home")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	hit := filepath.Join(tmp, "helper_hit")
	config := "secrets:\n  command:\n    enabled: true\n    command: 'touch " + hit + "'\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	repo := filepath.Join(tmp, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	ucInstallBinary(t, repo)
	ok, module, detail, err := validateCriticalModulesImport(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || module != "" || detail != "" {
		t.Fatalf("probe = (%v, %q, %q)", ok, module, detail)
	}
	if _, err := os.Stat(hit); err == nil {
		t.Fatal("the import probe resolved external secret sources")
	}
}

// Replaces the probe's runner and command builder for one test
func ucPatchProbe(t *testing.T, run func(context.Context, []string, pysubprocess.Options) (*pysubprocess.Completed, error),
	command func(string, []string, pm.LaunchCommandOptions) []string) {
	t.Helper()
	savedRun, savedCommand := importHealthRun, importHealthCommand
	t.Cleanup(func() { importHealthRun, importHealthCommand = savedRun, savedCommand })
	if run != nil {
		importHealthRun = run
	}
	if command != nil {
		importHealthCommand = command
	}
}

// Asserts the probe reported its own failure whose detail names the child's exit code
func ucProbeTerminated(t *testing.T, reportRuntimeErrors bool, code string) {
	t.Helper()
	ok, module, detail, err := validateCriticalModulesImport(context.Background(), t.TempDir(), reportRuntimeErrors)
	if err != nil {
		t.Fatal(err)
	}
	if ok || module != "critical-module probe" || !strings.Contains(detail, code) {
		t.Fatalf("probe = (%v, %q, %q)", ok, module, detail)
	}
}

func TestImportGuardReportsProbeTerminationWhenComparingStates(t *testing.T) {
	t.Setenv("ICEBATH_UC_PROBE_EXIT", "7")
	ucProbeTerminated(t, true, "7")
}

func TestImportGuardReportsProbeTerminationByDefault(t *testing.T) {
	t.Setenv("ICEBATH_UC_PROBE_EXIT", "9")
	ucProbeTerminated(t, false, "9")
}

func TestImportGuardDoesNotAcceptForgedStaticMarker(t *testing.T) {
	t.Setenv("ICEBATH_UC_PROBE_FORGE", "1")
	t.Setenv("ICEBATH_UC_PROBE_EXIT", "7")
	ucProbeTerminated(t, false, "7")
}

func TestImportGuardRejectsMalformedHealthPayload(t *testing.T) {
	ucPatchProbe(t, func(ctx context.Context, argv []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		marker := argv[len(argv)-2]
		return &pysubprocess.Completed{Args: argv, Stdout: []byte(marker + "{}")}, nil
	}, nil)
	ok, module, _, err := validateCriticalModulesImport(context.Background(), t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if ok || module != "critical-module probe" {
		t.Fatalf("probe = (%v, %q)", ok, module)
	}
}

func TestImportGuardReportsProbeTimeout(t *testing.T) {
	ucPatchProbe(t, func(ctx context.Context, argv []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		return nil, &pysubprocess.TimeoutExpired{Cmd: []string{"python"}, Timeout: 120}
	}, nil)
	ok, module, _, err := validateCriticalModulesImport(context.Background(), t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if ok || module != "critical-module probe" {
		t.Fatalf("probe = (%v, %q)", ok, module)
	}
}

func TestImportGuardIsNonFatalWhenProbeCannotRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the mode bits that refuse execution are POSIX only")
	}
	tmp := t.TempDir()
	selected := filepath.Join(tmp, "pm", "bin", "icebath")
	if err := os.MkdirAll(filepath.Dir(selected), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ucPatchProbe(t, nil, func(root string, args []string, opts pm.LaunchCommandOptions) []string {
		return append([]string{selected}, args...)
	})
	ok, module, detail, err := validateCriticalModulesImport(context.Background(), tmp, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || module != "" || detail != "" {
		t.Fatalf("probe = (%v, %q, %q)", ok, module, detail)
	}
}

func TestImportGuardUsesTheSelectedRuntimeCommand(t *testing.T) {
	tmp := t.TempDir()
	var seenCommand []string
	var seenCwd string
	ucPatchProbe(t, func(ctx context.Context, argv []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		seenCommand, seenCwd = argv, o.Dir
		return &pysubprocess.Completed{Args: argv}, nil
	}, func(root string, args []string, opts pm.LaunchCommandOptions) []string {
		if root != tmp {
			t.Fatalf("command built for %q", root)
		}
		return append(append([]string{"/pm/icebath", pm.PinDefaultHomeFlag}, opts.Subcommand...), args...)
	})
	if _, _, _, err := validateCriticalModulesImport(context.Background(), tmp, false); err != nil {
		t.Fatal(err)
	}
	if len(seenCommand) < 3 || seenCommand[0] != "/pm/icebath" || seenCommand[1] != pm.PinDefaultHomeFlag || seenCommand[2] != ImportHealthEntry {
		t.Fatalf("command = %v", seenCommand)
	}
	if seenCwd != tmp {
		t.Fatalf("cwd = %q", seenCwd)
	}
}
