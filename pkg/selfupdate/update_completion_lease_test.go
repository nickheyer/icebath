package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Runs the bootstrap prepare as on Windows with the checkout join, PM, venv sync and prepared child recording events
func clFakeWindowsPrepare(t *testing.T, join func(events *[]string, root string) *updaterecord.UpdateHolder) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	events := []string{}
	swap := func(restore func()) { t.Cleanup(restore) }
	oldPlatform, oldJoin, oldArm, oldTools, oldSync, oldCollect := completionPlatform, completionJoinCheckout, completionArm, completionEnsureTools, completionSyncVenv, completionCollect
	oldScope, oldLast, oldActivation, oldBinary, oldRun := completionWorkerScope, completionLastReceipt, completionActivation, completionBinary, completionRunPrepared
	swap(func() {
		completionPlatform, completionJoinCheckout, completionArm, completionEnsureTools, completionSyncVenv, completionCollect = oldPlatform, oldJoin, oldArm, oldTools, oldSync, oldCollect
		completionWorkerScope, completionLastReceipt, completionActivation, completionBinary, completionRunPrepared = oldScope, oldLast, oldActivation, oldBinary, oldRun
	})
	completionPlatform = "windows"
	completionJoinCheckout = func(root string) (*updaterecord.UpdateHolder, error) { return join(&events, root), nil }
	completionArm = func(context.Context, string) (string, error) {
		events = append(events, "arm_completion")
		return "", nil
	}
	completionEnsureTools = func(context.Context) error { events = append(events, "tools"); return nil }
	completionSyncVenv = func(context.Context, pm.SyncVenvOptions) error { events = append(events, "sync_venv"); return nil }
	completionCollect = func(context.Context, string) error {
		events = append(events, "collect_superseded_generations")
		return nil
	}
	completionWorkerScope = func(context.Context, *string) func() { return func() {} }
	completionLastReceipt = func(context.Context, string, bool) map[string]any { return nil }
	completionActivation = func(context.Context, string) ([]string, error) { return os.Environ(), nil }
	completionBinary = func(string) string { return os.Args[0] }
	resultPath := filepath.Join(dir, "result.json")
	completionRunPrepared = func(*exec.Cmd) error {
		events = append(events, "prepared")
		return os.WriteFile(resultPath, []byte("{}"), 0o644)
	}
	root := filepath.Join(dir, "checkout")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"source": root, "receipt": pyjson.ObjectOf("update_id", "u-l3"), "bytecode_cache": filepath.Join(dir, "bc")}
	if _, err := prepareCompletion(context.Background(), io.Discard, request, filepath.Join(dir, "request.json"), resultPath); err != nil {
		events = append(events, "raised: "+launchErrorText(err))
	}
	return root, events
}

func TestWindowsBootstrapHoldsACheckoutLeaseBeforeItWritesTheCheckout(t *testing.T) {
	root, events := clFakeWindowsPrepare(t, func(events *[]string, root string) *updaterecord.UpdateHolder {
		*events = append(*events, "lease "+root)
		return nil
	})
	if len(events) == 0 || events[0] != "lease "+root {
		t.Fatalf("%v", events)
	}
	if !slices.Equal(events[1:], []string{"arm_completion", "tools", "sync_venv", "collect_superseded_generations", "prepared"}) {
		t.Fatalf("%v", events)
	}
}

func TestWindowsBootstrapThatCannotJoinTheCheckoutLockWritesNothing(t *testing.T) {
	_, events := clFakeWindowsPrepare(t, func(*[]string, string) *updaterecord.UpdateHolder {
		return &updaterecord.UpdateHolder{PID: 4242, AgeSeconds: 0.0}
	})
	want := fmt.Sprintf("raised: could not join the update's checkout lock (held by process %d)", 4242)
	if !slices.Equal(events, []string{want}) {
		t.Fatalf("%v", events)
	}
}
