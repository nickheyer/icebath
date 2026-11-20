//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Sets the feature refresh steps to no ops for a re-executed test child
func frNoSteps() {
	featureRefreshSteps.featureDeps = func(context.Context, string) error { return nil }
	featureRefreshSteps.memoryMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
	featureRefreshSteps.leftCoreMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
}

// Sets the feature refresh steps to no ops for one test
func frQuiet(t *testing.T) {
	t.Helper()
	prev := featureRefreshSteps
	frNoSteps()
	t.Cleanup(func() { featureRefreshSteps = prev })
}

// Feature refresh steps appending their names to calls, each failing with its given error and the migrations speaking once
func frRecording(t *testing.T, calls *[]string, depsErr, memoryErr, leftCoreErr error) {
	t.Helper()
	prev := featureRefreshSteps
	t.Cleanup(func() { featureRefreshSteps = prev })
	featureRefreshSteps.featureDeps = func(_ context.Context, root string) error {
		*calls = append(*calls, "feature_deps")
		return depsErr
	}
	featureRefreshSteps.memoryMigrate = func(_ context.Context, say func(string)) ([]string, error) {
		*calls = append(*calls, "memory")
		say("  memory provider migrated")
		return nil, memoryErr
	}
	featureRefreshSteps.leftCoreMigrate = func(_ context.Context, say func(string)) ([]string, error) {
		*calls = append(*calls, "left_core")
		say("  plugin migrated")
		return nil, leftCoreErr
	}
}

// Followup steps and reasons of a receipt
func frFollowups(t *testing.T, receipt *pyjson.Object) [][2]string {
	t.Helper()
	out := [][2]string{}
	items, _ := receipt.GetOr("followups", nil).([]any)
	for _, item := range items {
		entry := item.(*pyjson.Object)
		out = append(out, [2]string{entry.GetOr("step", nil).(string), entry.GetOr("reason", nil).(string)})
	}
	return out
}

// Outcome the receipt recorded for one stage
func frStage(receipt *pyjson.Object, name string) any {
	items, _ := receipt.GetOr("stages", nil).([]any)
	for _, item := range items {
		if entry, ok := item.(*pyjson.Object); ok && entry.GetOr("name", nil) == name {
			return entry.GetOr("outcome", nil)
		}
	}
	return nil
}

func TestFeatureRefreshRunsTheInstallThenBothMigrations(t *testing.T) {
	var calls []string
	frRecording(t, &calls, nil, nil, nil)
	var out bytes.Buffer
	if err := refreshConfiguredFeatures(context.Background(), t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"feature_deps", "memory", "left_core"}) {
		t.Fatalf("calls %v", calls)
	}
	if out.String() != "  memory provider migrated\n  plugin migrated\n" {
		t.Fatalf("out %q", out.String())
	}
}

func TestFeatureRefreshMigrationFailuresAreSkippedNotFailed(t *testing.T) {
	var calls []string
	frRecording(t, &calls, nil, errors.New("catalog unreachable"), errors.New("plugin index offline"))
	var out bytes.Buffer
	if err := refreshConfiguredFeatures(context.Background(), t.TempDir(), &out); err != nil {
		t.Fatalf("err %v", err)
	}
	want := "  memory provider migrated\n  ⚠ Memory provider migration skipped: catalog unreachable\n" +
		"  plugin migrated\n  ⚠ Plugin migration skipped: plugin index offline\n"
	if out.String() != want || len(calls) != 3 {
		t.Fatalf("calls %v out %q", calls, out.String())
	}
}

func TestFeatureRefreshInstallFailureStillRunsBothMigrations(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		text string
	}{
		{"command", &pm.CalledProcessError{Argv: []string{"uv", "sync"}, Code: 2}, "uv sync exited 2"},
		{"classed", &pm.PyError{Class: "InstallError", Message: "extra resolution failed"}, "InstallError: extra resolution failed"},
		{"plain", errors.New("disk full"), "RuntimeError: disk full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			frRecording(t, &calls, tc.err, nil, nil)
			var out bytes.Buffer
			err := refreshConfiguredFeatures(context.Background(), t.TempDir(), &out)
			if err == nil || err.Error() != "feature dependencies: "+tc.text {
				t.Fatalf("err %v", err)
			}
			if !reflect.DeepEqual(calls, []string{"feature_deps", "memory", "left_core"}) {
				t.Fatalf("calls %v", calls)
			}
			if !strings.HasPrefix(out.String(), "  ⚠ feature dependencies failed: "+tc.text+"\n") {
				t.Fatalf("out %q", out.String())
			}
		})
	}
}

func TestFeatureRefreshInterruptStopsAtOnce(t *testing.T) {
	var calls []string
	frRecording(t, &calls, context.Canceled, nil, nil)
	err := refreshConfiguredFeatures(context.Background(), t.TempDir(), io.Discard)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(calls, []string{"feature_deps"}) {
		t.Fatalf("err %v calls %v", err, calls)
	}
}

func TestSourceCompletionRunsFeatureRefreshBetweenTheHookAndMaintenance(t *testing.T) {
	root := scRepo(t)
	var calls []string
	scDependencies(t, func(context.Context, PostUpdateMaintenance) (bool, error) {
		calls = append(calls, "maintenance")
		return true, nil
	})
	publishLaunchers = func(context.Context, string) error {
		calls = append(calls, "launchers")
		return nil
	}
	frRecording(t, &calls, nil, nil, nil)
	var stdout bytes.Buffer
	ok, err := CompleteSourceCheckout(context.Background(), root, SourceCompletion{AssumeYes: true, Announce: "announced",
		BeforeMaintenance: func() { calls = append(calls, "before") }, Stdout: &stdout, Stderr: &bytes.Buffer{}})
	if err != nil || !ok || !scStampExists(root) {
		t.Fatalf("ok %v err %v stamp %v", ok, err, scStampExists(root))
	}
	want := []string{"launchers", "before", "feature_deps", "memory", "left_core", "maintenance"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
	if stdout.String() != "  memory provider migrated\n  plugin migrated\nannounced\n" {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestSourceCompletionOwesFailedFeatureDependenciesAndFinishesTheTail(t *testing.T) {
	root := scRepo(t)
	maintained := false
	scDependencies(t, func(context.Context, PostUpdateMaintenance) (bool, error) {
		maintained = true
		return true, nil
	})
	var calls []string
	frRecording(t, &calls, &pm.CalledProcessError{Argv: []string{"uv", "sync"}, Code: 2}, nil, nil)
	ctx := updaterecord.UpdateReceiptScope(context.Background())
	updaterecord.BeginUpdateReceipt(ctx, nil, "fr-completion")
	owed := [][2]string{}
	ok, err := CompleteSourceCheckout(ctx, root, scOptions(&owed))
	want := [][2]string{{"feature_dependencies", "feature dependencies: uv sync exited 2"}}
	if err != nil || ok || !reflect.DeepEqual(owed, want) {
		t.Fatalf("ok %v err %v owed %v", ok, err, owed)
	}
	if !maintained || len(calls) != 3 || scStampExists(root) {
		t.Fatalf("maintained %v calls %v stamp %v", maintained, calls, scStampExists(root))
	}
	if got := frFollowups(t, updaterecord.CurrentReceipt(ctx).Data); !reflect.DeepEqual(got, want) {
		t.Fatalf("receipt followups %v", got)
	}
}

// Finish request for a takeover of root in a temp directory, returning the request and result paths
func frFinishRequest(t *testing.T, root, updateID string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	request := filepath.Join(dir, "request.json")
	text, err := pyjson.Dumps(pyjson.ObjectOf("root", root, "update_id", updateID, "assume_yes", true, "gateway_mode", false))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(request, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return request, filepath.Join(dir, "result.json")
}

// Replaces the maintenance, stamp and fleet steps of a finished update with stand ins appending to calls
func frFinishSteps(t *testing.T, calls *[]string) {
	t.Helper()
	vsSwap(t, &finishMaintenance, func(_ context.Context, o PostUpdateMaintenance) (bool, error) {
		*calls = append(*calls, "maintenance")
		return true, nil
	})
	vsSwap(t, &finishWriteSourceStamp, func(string, bool) (*pyjson.Object, error) {
		*calls = append(*calls, "stamp")
		return pyjson.ObjectOf(), nil
	})
	vsSwap(t, &finishRestartFleet, func(context.Context, io.Writer, *UpdatePlan, bool) (*gatewayRestartOutcome, error) {
		*calls = append(*calls, "restart")
		return newGatewayRestartOutcome(), nil
	})
	vsSwap(t, &finishMergeResume, func(context.Context, io.Writer, *gatewayRestartOutcome, map[string]any, bool) {})
	vsSwap(t, &finishVerifyFleet, func(context.Context, io.Writer, *gatewayRestartOutcome, *UpdatePlan, any, bool) error {
		return nil
	})
	vsSwap(t, &finishResumeWindows, func(context.Context, io.Writer, map[string]any) error { return nil })
}

func TestUpdateFinishRunsFeatureRefreshBeforeMaintenance(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, filepath.Join(tmp, ".icebath"))
	root := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	frFinishSteps(t, &calls)
	frRecording(t, &calls, nil, nil, nil)
	request, result := frFinishRequest(t, root, "fr-finish-ok")
	var stdout bytes.Buffer
	code, err := updateFinish(updaterecord.UpdateReceiptScope(context.Background()), request, result, &stdout, &bytes.Buffer{})
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if want := []string{"feature_deps", "memory", "left_core", "maintenance", "stamp", "restart"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
	if !strings.Contains(stdout.String(), "  memory provider migrated\n  plugin migrated\n") {
		t.Fatalf("stdout %q", stdout.String())
	}
	receipt := updaterecord.FinalizedReceipt("fr-finish-ok")
	if receipt == nil || len(frFollowups(t, receipt)) != 0 || frStage(receipt, "build") != "success" {
		t.Fatalf("receipt %v", receipt)
	}
}

func TestUpdateFinishOwesFailedFeatureDependenciesAndFinishesTheTail(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, filepath.Join(tmp, ".icebath"))
	root := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	frFinishSteps(t, &calls)
	frRecording(t, &calls, &pm.PyError{Class: "InstallError", Message: "extra resolution failed"}, nil, nil)
	request, result := frFinishRequest(t, root, "fr-finish-owed")
	var stdout bytes.Buffer
	code, err := updateFinish(updaterecord.UpdateReceiptScope(context.Background()), request, result, &stdout, &bytes.Buffer{})
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if want := []string{"feature_deps", "memory", "left_core", "maintenance", "restart"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
	if !strings.Contains(stdout.String(), "  ⚠ feature dependencies failed: InstallError: extra resolution failed\n") {
		t.Fatalf("stdout %q", stdout.String())
	}
	receipt := updaterecord.FinalizedReceipt("fr-finish-owed")
	want := [][2]string{{"feature_dependencies", "feature dependencies: InstallError: extra resolution failed"}}
	if receipt == nil || !reflect.DeepEqual(frFollowups(t, receipt), want) || frStage(receipt, "build") != "failed" {
		t.Fatalf("receipt %v", receipt)
	}
}
