//go:build !windows

package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func TestSourcePythonPinUpdateSurvivesRealGC(t *testing.T) {
	for _, update := range []string{"launch", "sync", "pm-update"} {
		t.Run(update, func(t *testing.T) {
			ctx := context.Background()
			tmp, root, binary := vsSourceLaunch(t)
			store, _ := pm.StoreRoot(ctx)
			oldPython := filepath.Join(store, "python-test", "bin", "python3")
			pin := pm.NewLockfile(pm.LockfilePath())
			pin.SetPin("python", "A", map[string]any{})
			if err := pin.Save(); err != nil {
				t.Fatal(err)
			}
			vsSync(t, root, nil)
			if _, err := pm.EnsureInstallLaunchers(ctx, root, filepath.Join(root, ".icebath", "bin")); err != nil {
				t.Fatal(err)
			}
			command := pm.InstallationCommand(root, nil, pm.LaunchCommandOptions{Subcommand: []string{vsChildEntry, "probe", root}})
			vsWrite(t, filepath.Join(vsSelectedSite(t, root), "selected_probe.py"), "VALUE = 'A'\n")
			if before := vsRunInstallCommand(t, command); before["executable"] != binary {
				t.Fatalf("before %v", before)
			}
			newPython := vsRecordPython(t, "B", "python-B")
			if update != "pm-update" {
				pin.SetPin("python", "B", map[string]any{})
				if err := pin.Save(); err != nil {
					t.Fatal(err)
				}
				if vsCurrent(t, root) {
					t.Fatal("a python pin change left the venv current")
				}
			}
			previous, _ := pm.SelectedVenv(ctx, root)
			switch update {
			case "launch":
				if target := vsPrepare(t, root); target != binary {
					t.Fatalf("launch target %q", target)
				}
				if calls := vsRead(t, filepath.Join(tmp, "completion-calls")); calls != fmt.Sprintf("[\"--source\",%q,\"--finish-update\"]\n", root) {
					t.Fatalf("completion calls %q", calls)
				}
			case "sync":
				if result, err := SyncTree(ctx, root, false); err != nil || result != (VenvSyncResult{State: "synced", OK: true}) {
					t.Fatalf("sync %+v %v", result, err)
				}
			default:
				t.Cleanup(pm.PatchRepoRoot(root))
				version := "B"
				t.Cleanup(pm.SwapUpdateResolution(func(pm.Package, []string, *string, map[string]any) (*pm.Resolved, error) {
					locked := "A"
					return &pm.Resolved{Name: "python", Locked: &locked, Style: "semver", Version: &version}, nil
				}, func(pm.Package, *pm.Resolved, map[string]any) (map[string]any, error) { return map[string]any{}, nil }))
				t.Cleanup(pm.SwapInstallCommandSteps(pm.InstallCommandSteps{
					InstallNames: func(context.Context, []string, *string, bool) (int, error) {
						t.Fatal("the update reached the install command's tool step")
						return 1, nil
					},
					Activate: pm.Activate, SyncVenv: pm.SyncVenv,
				}))
				if code, err := pm.CmdUpdate(ctx, pm.UpdateArgs{Names: []string{"python"}}, io.Discard); code != 0 || err != nil {
					t.Fatalf("pm update %d %v", code, err)
				}
				if got, _ := pm.NewLockfile(pm.LockfilePath()).Version("python"); got != "B" {
					t.Fatalf("pinned %q", got)
				}
			}
			if !vsCurrent(t, root) {
				t.Fatal("the update left the venv stale")
			}
			if selected, _ := pm.SelectedVenv(ctx, root); selected == previous {
				t.Fatal("the update kept the previous generation selected")
			}
			vsWrite(t, filepath.Join(vsSelectedSite(t, root), "selected_probe.py"), "VALUE = 'B'\n")
			t.Cleanup(pm.PatchRepoRoot(root))
			if code, err := pm.CmdGC(ctx); code != 0 || err != nil {
				t.Fatalf("gc %d %v", code, err)
			}
			if _, err := os.Lstat(oldPython); err == nil {
				t.Fatal("real PM GC did not remove the superseded Python")
			}
			if !isRegular(newPython) {
				t.Fatal("GC removed the selected Python")
			}
			if after := vsRunInstallCommand(t, command); !reflect.DeepEqual(after, map[string]any{"executable": binary, "value": "B"}) {
				t.Fatalf("after %v", after)
			}
		})
	}
}

func TestLauncherPublicationFailureRetriesWithoutRebuildingDependencies(t *testing.T) {
	_, root, binary := vsSourceLaunch(t)
	previous := pm.SwapPublishOwnLaunchers(func(context.Context, string, string) ([]string, error) { return []string{}, nil })
	result, err := SyncTree(context.Background(), root, false)
	pm.SwapPublishOwnLaunchers(previous)
	if err != nil || result.OK || !strings.Contains(result.Detail, "launcher publication failed") {
		t.Fatalf("failed publication %+v %v", result, err)
	}
	if !vsCurrent(t, root) {
		t.Fatal("a failed publication left the dependencies stale")
	}
	committed := vsRead(t, pm.EnvironmentRuntimeFactsPath(context.Background(), root))
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("target %q", target)
	}
	if vsRead(t, pm.EnvironmentRuntimeFactsPath(context.Background(), root)) != committed {
		t.Fatal("the retry rebuilt dependencies")
	}
	if !isRegular(filepath.Join(root, ".icebath", "bin", "icebath-acp")) {
		t.Fatal("the retry did not publish the launchers")
	}
}

func TestSourcePublicationLeavesExternalInstallLaunchersAlone(t *testing.T) {
	for _, checkout := range []bool{false, true} {
		t.Run(fmt.Sprint(checkout), func(t *testing.T) {
			_, root, _ := vsSourceLaunch(t)
			vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"updateMechanism": "external", "distribution": "nix"}`)
			if !checkout {
				os.Remove(filepath.Join(root, ".git"))
			}
			launcher := filepath.Join(root, ".icebath", "bin", "icebath-acp")
			vsWrite(t, launcher, "externally owned launcher\n")
			if result, err := SyncTree(context.Background(), root, false); err != nil || !result.OK {
				t.Fatalf("sync %+v %v", result, err)
			}
			if vsRead(t, launcher) != "externally owned launcher\n" {
				t.Fatal("the source publication rewrote an externally owned launcher")
			}
		})
	}
}

func TestLaunchWithoutMarkerPublishesThenSkipsAndRebuildsOnLockChange(t *testing.T) {
	tmp, root, binary := vsSourceLaunch(t)
	ctx := context.Background()
	if isRegular(pm.EnvironmentRuntimeFactsPath(ctx, root)) || existsFollow(filepath.Join(root, ".update-incomplete")) || vsCurrent(t, root) {
		t.Fatal("fixture is not a fresh source tree")
	}
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("target %q", target)
	}
	first := vsFact(t, root)
	if !reflect.DeepEqual(vsStrings(first["extras"]), []string{"all"}) || !vsCurrent(t, root) {
		t.Fatalf("first launch %v", first["extras"])
	}
	factsBytes := vsRead(t, pm.EnvironmentRuntimeFactsPath(ctx, root))
	generations := vsGenerations(t, root)
	receipts := vsReceipts(tmp)
	if len(receipts) == 0 {
		t.Fatal("the launch sync wrote no receipt")
	}
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("second target %q", target)
	}
	if vsRead(t, pm.EnvironmentRuntimeFactsPath(ctx, root)) != factsBytes || !reflect.DeepEqual(vsGenerations(t, root), generations) ||
		!reflect.DeepEqual(vsReceipts(tmp), receipts) {
		t.Fatal("current launch performed another sync")
	}
	vsAppend(t, filepath.Join(root, "uv.lock"), "\n# source update changes the committed lock\n")
	if vsCurrent(t, root) {
		t.Fatal("a lock change left the venv current")
	}
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("rebuild target %q", target)
	}
	rebuilt := vsFact(t, root)
	if rebuilt["stamp"] == first["stamp"] || rebuilt["environment"] == first["environment"] ||
		!reflect.DeepEqual(vsStrings(rebuilt["extras"]), vsStrings(first["extras"])) {
		t.Fatalf("rebuilt %v first %v", rebuilt, first)
	}
	if !isDirectoryPath(pyrepr.Str(first["environment"])) || !vsCurrent(t, root) || existsFollow(filepath.Join(root, ".update-incomplete")) {
		t.Fatal("the rebuild lost the previous generation or left a marker")
	}
}

func TestProcessSpawnedByTheUpdateCommitsDependenciesButNotTheTail(t *testing.T) {
	tmp, root, binary := vsSourceLaunch(t)
	ctx := context.Background()
	vsWrite(t, updaterecord.UpdateMarkerPath(), fmt.Sprintf("%d\n%d\n", os.Getppid(), time.Now().Unix()))
	if committed, _ := pm.CommittedVenv(ctx, root); committed != "" {
		t.Fatalf("committed %q", committed)
	}
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("target %q", target)
	}
	if committed, _ := pm.CommittedVenv(ctx, root); committed != pyrepr.Str(vsFact(t, root)["environment"]) {
		t.Fatalf("committed %q", committed)
	}
	if existsFollow(filepath.Join(tmp, "completion-calls")) {
		t.Fatal("the tail is the updater's, not its child's")
	}
	if existsFollow(CompletionPendingPath(ctx, root)) {
		t.Fatal("the child armed the updater's tail")
	}
	facts, receipts := vsRead(t, pm.EnvironmentRuntimeFactsPath(ctx, root)), vsReceipts(tmp)
	vsPrepare(t, root)
	if vsRead(t, pm.EnvironmentRuntimeFactsPath(ctx, root)) != facts || !reflect.DeepEqual(vsReceipts(tmp), receipts) {
		t.Fatal("a committed child synced again under the updater's claim")
	}
}

func TestFailedRealSyncPreservesPreviousSelectionAndRetries(t *testing.T) {
	tmp, root, binary := vsSourceLaunch(t)
	ctx := context.Background()
	vsSync(t, root, []string{"launch-extra"})
	previous := vsFact(t, root)
	facts := vsRead(t, pm.EnvironmentRuntimeFactsPath(ctx, root))
	generations := vsGenerations(t, root)
	lock := filepath.Join(root, "uv.lock")
	valid := vsRead(t, lock)
	vsWrite(t, lock, "version = 1\nnot valid TOML\n")
	markers := []string{filepath.Join(root, ".update-incomplete"), filepath.Join(root, ".lazy-refresh-incomplete")}
	for _, marker := range markers {
		vsWrite(t, marker, "legacy pending install")
	}
	for range 2 {
		receipts := vsReceipts(tmp)
		_, err := PrepareLaunch(ctx, root, nil)
		var install *pm.InstallError
		if !errors.As(err, &install) || !strings.Contains(err.Error(), "uv sync exited") {
			t.Fatalf("failed sync %v", err)
		}
		failed := vsNewReceipts(receipts, vsReceipts(tmp))
		if len(failed) != 1 || vsReceiptOutcome(t, failed[0]) != "failed" {
			t.Fatalf("failed receipts %v", failed)
		}
		if vsRead(t, pm.EnvironmentRuntimeFactsPath(ctx, root)) != facts || !reflect.DeepEqual(vsFact(t, root), previous) ||
			!reflect.DeepEqual(vsGenerations(t, root), generations) || vsCurrent(t, root) {
			t.Fatal("a failed sync changed the previous selection")
		}
		for _, marker := range markers {
			if vsRead(t, marker) != "legacy pending install" {
				t.Fatal("a failed sync cleared a legacy marker")
			}
		}
	}
	vsWrite(t, lock, valid+"\n# corrected source update\n")
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("target %q", target)
	}
	rebuilt := vsFact(t, root)
	if rebuilt["environment"] == previous["environment"] || !reflect.DeepEqual(vsStrings(rebuilt["extras"]), []string{"launch-extra"}) || !vsCurrent(t, root) {
		t.Fatalf("rebuilt %v", rebuilt)
	}
	for _, marker := range markers {
		if existsFollow(marker) {
			t.Fatal("a committed sync kept a legacy marker")
		}
	}
}

func TestSourceUpdateThatRemovesARecordedExtraStillSyncs(t *testing.T) {
	_, root, binary := vsSourceLaunch(t)
	vsSync(t, root, []string{"all", "launch-extra"})
	if extras := vsStrings(vsFact(t, root)["extras"]); !reflect.DeepEqual(extras, []string{"all", "launch-extra"}) {
		t.Fatalf("extras %v", extras)
	}
	pyproject := filepath.Join(root, "pyproject.toml")
	vsWrite(t, pyproject, strings.Replace(vsRead(t, pyproject), "launch-extra = []\n", "", 1))
	if err := pm.LockProject(context.Background(), pm.LockProjectOptions{Source: root, Offline: true, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if vsCurrent(t, root) {
		t.Fatal("a dropped extra left the venv current")
	}
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("target %q", target)
	}
	if extras := vsStrings(vsFact(t, root)["extras"]); !reflect.DeepEqual(extras, []string{"all"}) || !vsCurrent(t, root) {
		t.Fatalf("extras %v", extras)
	}
}

func TestRecordedExtraSpelledDifferentlyFromItsDeclarationSurvives(t *testing.T) {
	_, root, binary := vsSourceLaunch(t)
	vsSync(t, root, []string{"all", "launch_extra"})
	if extras := vsStrings(vsFact(t, root)["extras"]); !reflect.DeepEqual(extras, []string{"all", "launch_extra"}) {
		t.Fatalf("extras %v", extras)
	}
	vsAppend(t, filepath.Join(root, "uv.lock"), "\n# source update changes the committed lock\n")
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("target %q", target)
	}
	if extras := vsStrings(vsFact(t, root)["extras"]); !reflect.DeepEqual(extras, []string{"all", "launch_extra"}) || !vsCurrent(t, root) {
		t.Fatalf("extras %v", extras)
	}
}

func TestRealBootstrapReexecsBeforeAppImports(t *testing.T) {
	for _, mode := range []string{"script", "module", "command"} {
		t.Run(mode, func(t *testing.T) {
			tmp, root, binary := vsSourceLaunch(t)
			ctx := context.Background()
			args := []string{"--profile", "name with spaces", "-c", "session"}
			if isRegular(pm.EnvironmentRuntimeFactsPath(ctx, root)) {
				t.Fatal("fixture already has facts")
			}
			activatedOld := filepath.Join(tmp, "activated-old-dependencies")
			if mode == "script" {
				vsSync(t, root, []string{"all"})
				vsWrite(t, filepath.Join(vsSelectedSite(t, root), "old_dependency.pth"), "import pathlib; pathlib.Path("+pyrepr.StrRepr(activatedOld)+").touch()\n")
				if code, _, stderr := vsRunProbe(t, root, "activate", nil); code != 0 {
					t.Fatalf("activation probe %d %s", code, stderr)
				}
				if !isRegular(activatedOld) {
					t.Fatal("positive control did not execute the old activation hook")
				}
				os.Remove(activatedOld)
				vsAppend(t, filepath.Join(root, "uv.lock"), "\n# update before activation\n")
			}
			if mode == "module" {
				for _, name := range legacyMarkers {
					vsWrite(t, filepath.Join(root, name), "legacy pending install")
				}
			}
			before := vsReceipts(tmp)
			code, stdout, stderr := vsRunProbe(t, root, "full", args)
			if code != 0 {
				t.Fatalf("probe %d %s", code, stderr)
			}
			var output map[string]any
			if err := json.Unmarshal([]byte(stdout), &output); err != nil {
				t.Fatalf("probe output %q %s", stdout, stderr)
			}
			if output["executable"] != binary || !reflect.DeepEqual(vsStrings(output["args"]), args) || output["launch_current"] != true {
				t.Fatalf("probe %v", output)
			}
			site := pyrepr.Str(output["site"])
			selected, _ := pm.SelectedVenv(ctx, root)
			if !strings.HasPrefix(site, selected+string(filepath.Separator)) || !strings.Contains(strings.Join(vsStrings(output["path"]), "\x00"), site) {
				t.Fatalf("site %s outside %s or off the import path", site, selected)
			}
			if existsFollow(activatedOld) {
				t.Fatal("old dependencies activated before source-update completion")
			}
			if extras := vsStrings(vsFact(t, root)["extras"]); !reflect.DeepEqual(extras, []string{"all"}) {
				t.Fatalf("extras %v", extras)
			}
			receipts := vsNewReceipts(before, vsReceipts(tmp))
			if len(receipts) != 1 || vsReceiptOutcome(t, receipts[0]) != "ok" {
				t.Fatalf("receipts %v", receipts)
			}
			for _, name := range legacyMarkers {
				if existsFollow(filepath.Join(root, name)) {
					t.Fatal("a legacy marker survived the completion")
				}
			}
		})
	}
}

func TestFailedLaunchCompletionDegradesToAWarning(t *testing.T) {
	for _, argv := range [][]string{{}, {"--version"}} {
		t.Run(strings.Join(append([]string{"argv"}, argv...), " "), func(t *testing.T) {
			tmp, root, _ := vsSourceLaunch(t)
			vsSync(t, root, []string{"all"})
			previous := vsFact(t, root)
			vsWrite(t, filepath.Join(root, "uv.lock"), "version = 1\nnot valid TOML\n")
			if vsCurrent(t, root) {
				t.Fatal("an invalid lock left the venv current")
			}
			before := vsReceipts(tmp)
			code, stdout, stderr := vsRunProbe(t, root, "brief", argv)
			if code != 0 {
				t.Fatalf("probe %d %s", code, stderr)
			}
			var output map[string]any
			if err := json.Unmarshal([]byte(stdout), &output); err != nil {
				t.Fatalf("probe output %q", stdout)
			}
			if len(output) != 2 || output["executable"] != os.Args[0] || strings.Join(vsStrings(output["args"]), "\x00") != strings.Join(argv, "\x00") {
				t.Fatalf("probe %v", output)
			}
			if !reflect.DeepEqual(vsFact(t, root), previous) {
				t.Fatal("a failed completion changed the selection")
			}
			if len(argv) > 0 {
				if strings.Contains(stderr, "source-update completion failed") || !reflect.DeepEqual(vsReceipts(tmp), before) {
					t.Fatal("a metadata query attempted a dependency sync")
				}
			} else if !strings.Contains(stderr, "source-update completion failed") || !strings.Contains(stderr, "icebath update") {
				t.Fatalf("stderr %s", stderr)
			}
		})
	}
}

func TestCappedCompletionAttemptsLeaveMarkerForExplicitUpdate(t *testing.T) {
	tmp, root, binary := vsSourceLaunch(t)
	ctx := context.Background()
	vsSync(t, root, []string{"all"})
	if _, err := ArmCompletion(ctx, root); err != nil {
		t.Fatal(err)
	}
	attempts := completionAttemptsPath(ctx, root)
	vsWrite(t, attempts, "99\n")
	ancient := time.Unix(1, 0)
	os.Chtimes(attempts, ancient, ancient)
	var stderr strings.Builder
	vsSwap(t, &launchStderr, io.Writer(&stderr))
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("target %q", target)
	}
	if out := stderr.String(); !strings.Contains(out, "could not be finished automatically") || !strings.Contains(out, "icebath update") {
		t.Fatalf("stderr %s", out)
	}
	if !isRegular(CompletionPendingPath(ctx, root)) {
		t.Fatal("the owed completion is still recorded")
	}
	if existsFollow(filepath.Join(tmp, "completion-calls")) {
		t.Fatal("a capped launch re-ran the tail")
	}
}

func TestFailedTailAttemptIsCountedAndSuccessClearsIt(t *testing.T) {
	_, root, binary := vsSourceLaunch(t)
	ctx := context.Background()
	vsSync(t, root, []string{"all"})
	if _, err := ArmCompletion(ctx, root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VS_COMPLETION_EXIT", "3")
	if _, err := PrepareLaunch(ctx, root, nil); err == nil || !strings.Contains(err.Error(), "source update completion failed") {
		t.Fatalf("failed tail %v", err)
	}
	if got := strings.TrimSpace(vsRead(t, completionAttemptsPath(ctx, root))); got != "1" {
		t.Fatalf("attempts %q", got)
	}
	if !isRegular(CompletionPendingPath(ctx, root)) {
		t.Fatal("a failed attempt must keep the obligation")
	}
	t.Setenv("VS_COMPLETION_EXIT", "0")
	old := time.Unix(1, 0)
	os.Chtimes(completionAttemptsPath(ctx, root), old, old)
	if target := vsPrepare(t, root); target != binary {
		t.Fatalf("target %q", target)
	}
	if existsFollow(CompletionPendingPath(ctx, root)) || existsFollow(completionAttemptsPath(ctx, root)) {
		t.Fatal("success left the obligation or its attempt record")
	}
}
