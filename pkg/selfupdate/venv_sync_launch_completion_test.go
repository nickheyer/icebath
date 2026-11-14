//go:build !windows

package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Splits launch arguments with the CLI parser the boot installs
func vsCLICommandArgv(t *testing.T) {
	t.Helper()
	pm.SetCommandArgv(commands.NewCLI().CommandArgv)
	t.Cleanup(func() { pm.SetCommandArgv(func(argv []string) []string { return argv }) })
}

// A committed checkout under an isolated home whose store binary is this process
func vsCompletedTree(t *testing.T) (string, string) {
	t.Helper()
	tmp := vsResolvedTemp(t)
	root := vsCommittedCheckout(t, tmp)
	vsHome(t, tmp)
	vsUserHome(t, tmp)
	vsCLICommandArgv(t)
	vsStoreIsThisProcess(t)
	return tmp, root
}

// Self updating checkout under an isolated home, the user's home directory being the test directory
func vsSelfCheckout(t *testing.T) (string, string) {
	t.Helper()
	tmp := vsResolvedTemp(t)
	root := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='example'\n")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"updateMechanism": "self"}`)
	vsHome(t, tmp)
	vsUserHome(t, tmp)
	vsCLICommandArgv(t)
	return tmp, root
}

// Checkout with a bare project whose stamp names its update owner, under an isolated home
func vsOwnedCheckout(t *testing.T, owner string) (string, string) {
	t.Helper()
	tmp := vsResolvedTemp(t)
	root := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\n")
	if owner != "" {
		encoded, _ := json.Marshal(map[string]string{"updateMechanism": owner})
		vsWrite(t, filepath.Join(root, "install-stamp.json"), string(encoded))
	}
	vsHome(t, tmp)
	vsUserHome(t, tmp)
	vsCLICommandArgv(t)
	return tmp, root
}

// Stdout and launch stderr written while launch runs
func vsCaptureLaunch(t *testing.T, launch func()) (string, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	savedOut, savedErr := os.Stdout, launchStderr
	os.Stdout, launchStderr = w, &stderr
	launch()
	os.Stdout, launchStderr = savedOut, savedErr
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	return string(out), stderr.String()
}

func TestCompletedTreeDischargesOnlyAMarkerOlderThanItsStamp(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		owed bool
	}{{-60 * time.Second, false}, {60 * time.Second, true}} {
		t.Run(tc.age.String(), func(t *testing.T) {
			_, root := vsCompletedTree(t)
			vsNoToolDownloads(t)
			tail := vsCompletionTail(t)
			vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
			pending, err := ArmCompletion(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(filepath.Join(root, "install-stamp.json"))
			if err != nil {
				t.Fatal(err)
			}
			stamp := st.ModTime().Add(tc.age)
			if err := os.Chtimes(pending, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			if target := vsPrepare(t, root); target != "" {
				t.Fatalf("target %q", target)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.owed]; len(tail.commands) != want {
				t.Fatalf("tail ran %d times", len(tail.commands))
			}
			if existsFollow(pending) {
				t.Fatal("the marker survived")
			}
		})
	}
}

func TestPristineHomeProvisionsDependenciesWithoutRebuild(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "edited"}[edited], func(t *testing.T) {
			_, root := vsCompletedTree(t)
			vsNoToolDownloads(t)
			tail := vsCompletionTail(t)
			if edited {
				vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='edited'\n")
			}
			if existsFollow(CompletionPendingPath(context.Background(), root)) {
				t.Fatal("fixture owes a tail")
			}
			vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return false, nil })
			syncs := 0
			vsSwap(t, &syncVenv, func(context.Context, pm.SyncVenvOptions) error { syncs++; return nil })
			exe, _ := runningExecutable()
			if target := vsPrepare(t, root); target != exe {
				t.Fatalf("target %q", target)
			}
			if want := map[bool]int{true: 1, false: 0}[edited]; syncs != 1 || len(tail.commands) != want {
				t.Fatalf("syncs %d tail %d", syncs, len(tail.commands))
			}
			if existsFollow(CompletionPendingPath(context.Background(), root)) {
				t.Fatal("the provisioning left a pending tail")
			}
		})
	}
}

func TestAdoptionStampNeverDischargesAnOwedTail(t *testing.T) {
	tmp, root := vsCompletedTree(t)
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	blessed := filepath.Join(tmp, "home", "icebath-agent")
	if err := os.MkdirAll(filepath.Dir(blessed), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, blessed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(blessed, "install-stamp.json")); err != nil {
		t.Fatal(err)
	}
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
	pending, err := ArmCompletion(context.Background(), blessed)
	if err != nil {
		t.Fatal(err)
	}
	tail.exitCode = 1
	for range 2 {
		if _, err := PrepareLaunch(context.Background(), blessed, []string{}); err == nil || !strings.Contains(err.Error(), "completion failed") {
			t.Fatalf("owed tail %v", err)
		}
	}
	if !strings.Contains(vsRead(t, filepath.Join(blessed, "install-stamp.json")), `"adoptedAt"`) || len(tail.commands) != 2 || !existsFollow(pending) {
		t.Fatalf("adoption discharged the tail: %d runs", len(tail.commands))
	}
}

func TestMetadataQueryNeverWaitsOnSourceCompletion(t *testing.T) {
	for _, argv := range [][]string{{"--version"}, {"-V"}, {"--help"}, {"-p", "work", "-h"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			_, root := vsSelfCheckout(t)
			vsNoToolDownloads(t)
			vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) {
				t.Fatal("metadata query reached PM")
				return false, nil
			})
			if target, err := PrepareLaunch(context.Background(), root, argv); target != "" || err != nil {
				t.Fatalf("metadata query %q %v", target, err)
			}
		})
	}
}

func TestFailedCompletionTailIsRetriedWithoutRebuildingDependencies(t *testing.T) {
	_, root := vsSelfCheckout(t)
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	ctx := context.Background()
	fact := pm.EnvironmentRuntimeFactsPath(ctx, root)
	var syncs [][]string
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return isRegular(fact), nil })
	vsStoreIsThisProcess(t)
	vsSwap(t, &syncVenv, func(_ context.Context, opts pm.SyncVenvOptions) error {
		syncs = append(syncs, opts.Extras)
		vsWrite(t, fact, vsCompleteFacts)
		return nil
	})
	tail.exitCode = 1
	if _, err := PrepareLaunch(ctx, root, []string{}); err == nil || !strings.Contains(err.Error(), "run `icebath update`") {
		t.Fatalf("failed tail %v", err)
	}
	if len(syncs) != 1 || len(tail.commands) != 1 {
		t.Fatalf("syncs %d tail %d", len(syncs), len(tail.commands))
	}
	if current, err := venvIsCurrent(ctx, root); err != nil || !current {
		t.Fatalf("dependencies not current after the sync %v", err)
	}

	if _, err := PrepareLaunch(ctx, root, []string{}); err == nil || !strings.Contains(err.Error(), "run `icebath update`") {
		t.Fatalf("retried tail %v", err)
	}
	if len(syncs) != 1 {
		t.Fatal("current dependencies were rebuilt for a tail retry")
	}
	if len(tail.commands) != 2 {
		t.Fatalf("tail ran %d times", len(tail.commands))
	}

	tail.exitCode = 0
	attempts := completionAttemptsPath(ctx, root)
	old := time.Unix(1, 0)
	if err := os.Chtimes(attempts, old, old); err != nil {
		t.Fatal(err)
	}
	if target := vsPrepare(t, root); target != "" {
		t.Fatalf("tail alone re-execs into %q", target)
	}
	if len(syncs) != 1 || len(tail.commands) != 3 {
		t.Fatalf("syncs %d tail %d", len(syncs), len(tail.commands))
	}
	if target := vsPrepare(t, root); target != "" {
		t.Fatalf("finished launch re-execs into %q", target)
	}
	if len(tail.commands) != 3 {
		t.Fatal("a finished tail was run again")
	}
}

func TestPreparedCompletionImportDoesNotStartAnotherTail(t *testing.T) {
	for _, script := range []string{SourceCompletionEntry, UpdateCompletionEntry} {
		t.Run(script, func(t *testing.T) {
			_, root := vsSelfCheckout(t)
			vsNoToolDownloads(t)
			tail := vsCompletionTail(t)
			ctx := context.Background()
			worker := filepath.Join(root, "icebath")
			vsWrite(t, worker, "")
			if _, err := ArmCompletion(ctx, root); err != nil {
				t.Fatal(err)
			}
			vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
			vsStoreIsThisProcess(t)
			vsSwap(t, &runningExecutable, func() (string, error) { return worker, nil })
			vsSwap(t, &os.Args, []string{worker, script, "--source", root, "--finish-update", "--prepared"})

			if target, err := PrepareLaunch(ctx, root, os.Args[1:]); err != nil || target != "" {
				t.Fatalf("tail import %q %v", target, err)
			}
			if len(tail.commands) != 0 {
				t.Fatalf("a tail started inside the tail %v", tail.commands)
			}
			if !isRegular(CompletionPendingPath(ctx, root)) {
				t.Fatal("the tail's own marker was discharged")
			}
		})
	}
}

func TestSameNamedScriptOutsideTheCheckoutStillRepairs(t *testing.T) {
	tmp, root := vsSelfCheckout(t)
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	ctx := context.Background()
	foreign := filepath.Join(tmp, "other", "icebath")
	vsWrite(t, foreign, "")
	if _, err := ArmCompletion(ctx, root); err != nil {
		t.Fatal(err)
	}
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
	vsStoreIsThisProcess(t)
	vsSwap(t, &runningExecutable, func() (string, error) { return foreign, nil })
	vsSwap(t, &os.Args, []string{foreign, SourceCompletionEntry, "--prepared"})

	if target, err := PrepareLaunch(ctx, root, os.Args[1:]); err != nil || target != "" {
		t.Fatalf("foreign tail %q %v", target, err)
	}
	if len(tail.commands) != 1 {
		t.Fatalf("tail ran %d times", len(tail.commands))
	}
	if isRegular(CompletionPendingPath(ctx, root)) {
		t.Fatal("this checkout's repair stayed owed")
	}
}

func TestCompletionTailOutputStaysOffStdout(t *testing.T) {
	_, root := vsSelfCheckout(t)
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return false, nil })
	vsSwap(t, &syncVenv, func(context.Context, pm.SyncVenvOptions) error { return nil })
	vsStoreIsThisProcess(t)
	if _, err := PrepareLaunch(context.Background(), root, []string{}); err != nil {
		t.Fatal(err)
	}
	if tail.kwargs.Stdout != io.Writer(os.Stderr) {
		t.Fatalf("tail stdout %v", tail.kwargs.Stdout)
	}
}

func TestFirstLaunchSyncsWithoutMarkerThenUsesCompletionFact(t *testing.T) {
	_, root := vsSelfCheckout(t)
	vsWrite(t, filepath.Join(root, "uv.lock"), "lock\n")
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	ctx := context.Background()
	fact := pm.EnvironmentRuntimeFactsPath(ctx, root)
	var calls []pm.SyncVenvOptions
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return isRegular(fact), nil })
	vsStoreIsThisProcess(t)
	vsSwap(t, &syncVenv, func(_ context.Context, opts pm.SyncVenvOptions) error {
		calls = append(calls, opts)
		vsWrite(t, fact, vsCompleteFacts)
		return nil
	})
	vsWrite(t, filepath.Join(root, ".update-incomplete"), "pid=-1\n")
	exe, _ := runningExecutable()
	if target := vsPrepare(t, root); target != exe {
		t.Fatalf("first launch target %q", target)
	}
	want := []pm.SyncVenvOptions{{Extras: []string{"all"}, Explicit: true, ProjectRoot: root, EvictIncompatiblePlugins: true}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %+v", calls)
	}
	if existsFollow(filepath.Join(root, ".update-incomplete")) {
		t.Fatal("the obsolete updater marker survived the sync")
	}
	ran := false
	for _, command := range tail.commands {
		for _, part := range command {
			ran = ran || strings.Contains(part, SourceCompletionEntry)
		}
	}
	if !ran {
		t.Fatalf("no source completion tail in %v", tail.commands)
	}
	if target := vsPrepare(t, root); target != "" {
		t.Fatalf("second launch target %q", target)
	}
	if len(calls) != 1 {
		t.Fatalf("calls %d", len(calls))
	}
}

func TestRelaunchKeepsInvocationAndCheckoutImports(t *testing.T) {
	for _, mode := range []string{"script", "module", "command"} {
		t.Run(mode, func(t *testing.T) {
			tmp := vsResolvedTemp(t)
			root := filepath.Join(tmp, "source")
			if err := os.Mkdir(root, 0o777); err != nil {
				t.Fatal(err)
			}
			binary := vsInstallBinary(t, root)
			argv := []string{binary, "--profile", "name with spaces", "-c", "session"}
			switch mode {
			case "module":
				argv[0] = filepath.Base(binary)
			case "command":
				argv[0] = "-c"
			}
			command := RelaunchCommand(binary, argv)
			cmd := exec.Command(command[0], command[1:]...)
			cmd.Dir = tmp
			cmd.Env = append(os.Environ(), "VS_BOOT_PROBE=relaunch-imports")
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("relaunch %v: %s", err, stderr.String())
			}
			var got []any
			if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
				t.Fatalf("relaunch output %q", stdout.String())
			}
			want := []any{root, []any{"--profile", "name with spaces", "-c", "session"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("relaunch saw %v, want %v", got, want)
			}
		})
	}
}

func TestNonSelfOrPmLaunchCannotTriggerUpdate(t *testing.T) {
	for _, tc := range []struct {
		owner string
		argv  []string
	}{{"", []string{}}, {"external", []string{}}, {"electron-updater", []string{}}, {"self", []string{"-p", "coder", "pm", "repair"}}} {
		t.Run(tc.owner+" "+strings.Join(tc.argv, " "), func(t *testing.T) {
			_, root := vsOwnedCheckout(t, tc.owner)
			vsNoToolDownloads(t)
			vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) {
				t.Fatal("unowned launch reached PM")
				return false, nil
			})
			if target, err := PrepareLaunch(context.Background(), root, tc.argv); err != nil || target != "" {
				t.Fatalf("unowned launch %q %v", target, err)
			}
		})
	}
}

func TestFailedLaunchKeepsPreviousCompletionAndRetries(t *testing.T) {
	_, root := vsOwnedCheckout(t, "self")
	vsNoToolDownloads(t)
	ctx := context.Background()
	fact := pm.EnvironmentRuntimeFactsPath(ctx, root)
	if err := os.MkdirAll(filepath.Dir(fact), 0o777); err != nil {
		t.Fatal(err)
	}
	previous := `{"packages":{"venv":{"stamp":"previous","extras":["all","anthropic"]}}}`
	vsWrite(t, fact, previous)
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return false, nil })
	var calls [][]string
	vsSwap(t, &syncVenv, func(_ context.Context, opts pm.SyncVenvOptions) error {
		calls = append(calls, opts.Extras)
		return runtimeErr("network unavailable")
	})
	for range 2 {
		if _, err := PrepareLaunch(ctx, root, []string{}); err == nil || !strings.Contains(err.Error(), "network unavailable") {
			t.Fatalf("failed sync %v", err)
		}
		if vsRead(t, fact) != previous {
			t.Fatal("the previous completion was replaced")
		}
	}
	if !reflect.DeepEqual(calls, [][]string{nil, nil}) {
		t.Fatalf("calls %#v", calls)
	}
	if existsFollow(filepath.Join(root, ".update-incomplete")) {
		t.Fatal("a failed launch wrote an updater marker")
	}
}

func TestFailedDependencySyncBacksOffOnlyForTheSameInputs(t *testing.T) {
	_, root := vsSelfCheckout(t)
	vsNoToolDownloads(t)
	ctx := context.Background()
	syncs := 0
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return false, nil })
	vsSwap(t, &syncVenv, func(context.Context, pm.SyncVenvOptions) error {
		syncs++
		return runtimeErr("sync failed")
	})
	vsStoreIsThisProcess(t)
	for range CompletionRetryBackoffAttempts {
		if _, err := PrepareLaunch(ctx, root, []string{}); err == nil || !strings.Contains(err.Error(), "sync failed") {
			t.Fatalf("failing sync %v", err)
		}
	}
	if !isRegular(CompletionPendingPath(ctx, root)) {
		t.Fatal("the failed syncs left no pending tail")
	}

	if target, err := PrepareLaunch(ctx, root, []string{}); err != nil || target != "" {
		t.Fatalf("a backed-off stale launch must not relaunch %q %v", target, err)
	}
	if syncs != CompletionRetryBackoffAttempts {
		t.Fatalf("the same failing repair re-ran: %d syncs", syncs)
	}

	vsWrite(t, filepath.Join(root, "uv.lock"), "# corrected source update\n")
	if _, err := PrepareLaunch(ctx, root, []string{}); err == nil || !strings.Contains(err.Error(), "sync failed") {
		t.Fatalf("changed inputs %v", err)
	}
	if syncs != CompletionRetryBackoffAttempts+1 {
		t.Fatalf("changed inputs were not retried: %d syncs", syncs)
	}
}

func TestBlessedLegacyInstallIsAdoptedBeforeSync(t *testing.T) {
	tmp := vsResolvedTemp(t)
	vsUserHome(t, tmp)
	h := vsHome(t, tmp)
	vsCLICommandArgv(t)
	vsNoToolDownloads(t)
	vsCompletionTail(t)
	root := filepath.Join(h, "icebath-agent")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\n")
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return false, nil })
	var calls [][]string
	vsSwap(t, &syncVenv, func(_ context.Context, opts pm.SyncVenvOptions) error { calls = append(calls, opts.Extras); return nil })
	vsStoreIsThisProcess(t)
	exe, _ := runningExecutable()
	if target := vsPrepare(t, root); target != exe {
		t.Fatalf("adopted launch target %q", target)
	}
	var stamp map[string]any
	if err := json.Unmarshal([]byte(vsRead(t, filepath.Join(root, "install-stamp.json"))), &stamp); err != nil || stamp["source"] != "adoption" {
		t.Fatalf("stamp %v %v", stamp, err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"all"}}) {
		t.Fatalf("calls %v", calls)
	}
}

func TestRelaunchRunsZipLaunchersAndPreservesInterpreterOptions(t *testing.T) {
	for _, form := range []string{"script", "module", "command", "wrapper launcher", "symlink launcher"} {
		t.Run(form, func(t *testing.T) {
			tmp := vsResolvedTemp(t)
			root := filepath.Join(tmp, "source")
			if err := os.Mkdir(root, 0o777); err != nil {
				t.Fatal(err)
			}
			binary := vsInstallBinary(t, root)
			launcher := filepath.Join(tmp, "bin", "icebath")
			if err := os.Mkdir(filepath.Dir(launcher), 0o777); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "arg with spaces")
			switch form {
			case "module":
				cmd.Args[0] = filepath.Base(binary)
			case "command":
				cmd.Args[0] = "-c"
			case "wrapper launcher":
				vsWrite(t, launcher, "#!/bin/sh\nexec "+pyshlex.Quote(binary)+" \"$@\"\n")
				if err := os.Chmod(launcher, 0o755); err != nil {
					t.Fatal(err)
				}
				cmd = exec.Command(launcher, "arg with spaces")
			case "symlink launcher":
				if err := os.Symlink(binary, launcher); err != nil {
					t.Fatal(err)
				}
				cmd = exec.Command(launcher, "arg with spaces")
			}
			cmd.Env = append(os.Environ(), "VS_BOOT_PROBE=relaunch-options", "VS_BOOT_ROOT="+root, "GOMAXPROCS=3", "GOMEMLIMIT=1GiB")
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("relaunch %v: %s", err, stderr.String())
			}
			var got []any
			if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
				t.Fatalf("relaunch output %q %s", stdout.String(), stderr.String())
			}
			want := []any{[]any{"arg with spaces"}, 3.0, float64(1 << 30), binary, true}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("relaunch saw %v, want %v", got, want)
			}
		})
	}
}

func TestLiveOldUpdateBlocksLaunchSync(t *testing.T) {
	_, root := vsOwnedCheckout(t, "self")
	vsNoToolDownloads(t)
	marker := filepath.Join(root, ".update-incomplete")
	vsWrite(t, marker, fmt.Sprintf("pid=%d\n", os.Getpid()))
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return false, nil })
	vsSwap(t, &syncVenv, func(context.Context, pm.SyncVenvOptions) error {
		t.Fatal("raced old updater")
		return nil
	})
	if _, err := PrepareLaunch(context.Background(), root, []string{}); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("launch under a live updater %v", err)
	}
	if !isRegular(marker) {
		t.Fatal("the live updater's marker was removed")
	}
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
	vsStoreIsThisProcess(t)
	if target, err := PrepareLaunch(context.Background(), root, []string{}); err != nil || target != "" {
		t.Fatalf("verification child %q %v", target, err)
	}
	if !isRegular(marker) {
		t.Fatal("the live updater's marker was removed")
	}
}

func TestLaunchUnderTheOwningUpdateDoesNotRunTheTailAgain(t *testing.T) {
	_, root := vsSelfCheckout(t)
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	ctx := context.Background()
	pending := CompletionPendingPath(ctx, root)
	vsWrite(t, pending, "owed\n")
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
	vsWrite(t, updaterecord.UpdateMarkerPath(), fmt.Sprintf("%d\n%d\n", os.Getppid(), time.Now().Unix()))

	if target, err := PrepareLaunch(ctx, root, []string{}); err != nil || target != "" {
		t.Fatalf("launch under the owning update %q %v", target, err)
	}
	if len(tail.commands) != 0 {
		t.Fatalf("tail ran %v", tail.commands)
	}
	if !isRegular(pending) {
		t.Fatal("the owning update's obligation was discharged by its own tail")
	}
}

func TestGatewayStatusSkipsPendingSourceUpdateCompletion(t *testing.T) {
	_, root := vsSelfCheckout(t)
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	pending, err := ArmCompletion(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) {
		t.Fatal("status started a dependency sync")
		return false, nil
	})

	if target, err := PrepareLaunch(context.Background(), root, []string{"gateway", "status"}); err != nil || target != "" {
		t.Fatalf("status launch %q %v", target, err)
	}
	if len(tail.commands) != 0 {
		t.Fatalf("tail ran %v", tail.commands)
	}
	if !isRegular(pending) {
		t.Fatal("a status query must leave the update tail for `icebath gateway restart`")
	}
}

func TestSupervisedLaunchLeavesAPendingTailToTheCli(t *testing.T) {
	for _, tc := range []struct {
		marker, value string
		owedByCLI     bool
	}{
		{"ICEBATH_SUPERVISED_CHILD", "1", true},
		{"ICEBATH_S6_SUPERVISED_CHILD", "true", true},
		{"ICEBATH_SUPERVISED_CHILD", "0", false},
		{"ICEBATH_SUPERVISED_CHILD", "false", false},
	} {
		t.Run(tc.marker+"="+tc.value, func(t *testing.T) {
			_, root := vsSelfCheckout(t)
			vsNoToolDownloads(t)
			tail := vsCompletionTail(t)
			ctx := context.Background()
			pending := CompletionPendingPath(ctx, root)
			vsWrite(t, pending, "owed\n")
			vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
			vsStoreIsThisProcess(t)
			for _, name := range []string{"ICEBATH_SUPERVISED_CHILD", "ICEBATH_S6_SUPERVISED_CHILD"} {
				t.Setenv(name, "")
				os.Unsetenv(name)
			}
			t.Setenv(tc.marker, tc.value)

			var target string
			var err error
			out, stderr := vsCaptureLaunch(t, func() { target, err = PrepareLaunch(ctx, root, []string{"gateway", "run"}) })
			if err != nil || target != "" {
				t.Fatalf("supervised launch %q %v", target, err)
			}
			if tc.owedByCLI {
				if len(tail.commands) != 0 {
					t.Fatalf("tail ran %v", tail.commands)
				}
				if !isRegular(pending) {
					t.Fatal("a supervised child discharged an obligation the CLI still owes")
				}
				if out != "" || strings.Count(stderr, "run `icebath update`") != 1 {
					t.Fatalf("stdout %q stderr %q", out, stderr)
				}
			} else {
				if len(tail.commands) != 1 {
					t.Fatalf("tail ran %d times", len(tail.commands))
				}
				if isRegular(pending) {
					t.Fatal("an off marker left the tail owed")
				}
			}
		})
	}
}

func TestSupervisedLaunchWithStaleDependenciesStillSyncs(t *testing.T) {
	_, root := vsSelfCheckout(t)
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	syncs := 0
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return false, nil })
	vsSwap(t, &syncVenv, func(context.Context, pm.SyncVenvOptions) error { syncs++; return nil })
	vsStoreIsThisProcess(t)
	t.Setenv("ICEBATH_SUPERVISED_CHILD", "1")

	if _, err := PrepareLaunch(context.Background(), root, []string{"gateway", "run"}); err != nil {
		t.Fatal(err)
	}
	if syncs == 0 {
		t.Fatal("a supervised child booted on a stale dependency graph without syncing")
	}
	if len(tail.commands) == 0 {
		t.Fatal("the tail armed by that sync was never finished")
	}
	if isRegular(CompletionPendingPath(context.Background(), root)) {
		t.Fatal("the supervised sync left its tail owed")
	}
}
