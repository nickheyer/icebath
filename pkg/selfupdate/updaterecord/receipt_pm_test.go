package updaterecord

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// A pm sync that runs while the update receipt is open
func rcSync(ctx context.Context, rebuild *[2]any, features []string, warnings []string, refusal *[2]string) {
	token := pm.BeginReceipt(ctx, "sync")
	if rebuild != nil {
		pm.RecordVenvRebuild(ctx, rebuild[0].(bool), rebuild[1].(string))
	}
	if features != nil {
		pm.RecordFeatureList(ctx, features)
	}
	for _, w := range warnings {
		pm.RecordWarning(ctx, w)
	}
	if refusal != nil {
		pm.RecordRefusal(ctx, refusal[0], refusal[1])
	}
	pm.FinalizeReceipt(ctx, "ok", 0, token)
}

func TestReceiptEmbedsPMSections(t *testing.T) {
	ctx, h := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	rcSync(ctx, &[2]any{true, ""}, []string{"web", "acp"}, nil, nil)
	RecordStep(ctx, "git-pull", true, "")
	path := FinalizeUpdateReceipt(ctx, "success", nil, "")
	data := rcLoad(t, filepath.Join(h, "logs", "update_receipts", "latest.json"))
	if !reflect.DeepEqual(rcLoad(t, path), data) {
		t.Fatal("archive and latest differ")
	}
	if data["outcome"] != "success" || !reflect.DeepEqual(data["pm_venv_rebuild"], map[string]any{"ok": true, "reason": ""}) ||
		!reflect.DeepEqual(data["pm_feature_list"], []any{"web", "acp"}) || data["pm_sync_outcome"] != "ok" || data["update_id"] == "" {
		t.Fatalf("data %v", data)
	}
}

func TestReceiptLegacyWorkerReceiptFieldsStillEmbed(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	id := *CurrentCorrelationID(ctx)
	bisect := []any{map[string]any{"plugin": "old-plugin", "action": "disabled"}}
	if err := pm.AcceptWorkerReceipt(ctx, map[string]any{"update_id": id, "outcome": "ok", "plugin_bisect": bisect}, &id); err != nil {
		t.Fatal(err)
	}
	if data := rcLoad(t, FinalizeUpdateReceipt(ctx, "success", nil, "")); !reflect.DeepEqual(data["pm_plugin_bisect"], bisect) {
		t.Fatalf("bisect %v", data["pm_plugin_bisect"])
	}
}

func TestReceiptStaleSyncFromBeforeTheUpdateIsNeverEmbedded(t *testing.T) {
	ctx, _ := rcHome(t)
	rcSync(ctx, &[2]any{true, "stale run"}, nil, nil, nil)
	if pm.LatestReceipt(ctx)["venv_rebuild"].(map[string]any)["reason"] != "stale run" {
		t.Fatal("stale sync not on disk")
	}
	BeginUpdateReceipt(ctx, nil, "")
	data := rcLoad(t, FinalizeUpdateReceipt(ctx, "success", nil, ""))
	if _, ok := data["pm_venv_rebuild"]; ok {
		t.Fatal("stale rebuild embedded")
	}
	if _, ok := data["pm_sync_outcome"]; ok {
		t.Fatal("stale outcome embedded")
	}
}

func TestReceiptConcurrentThreadSyncIsNeverEmbedded(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	var seen string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		other := pm.WithReceiptScope(context.Background())
		token := pm.BeginReceipt(other, "sync")
		pm.RecordVenvRebuild(other, true, "not my update")
		seen = pm.FinalizeReceipt(other, "ok", 0, token)
	}()
	wg.Wait()
	if seen == "" {
		t.Fatal("concurrent sync wrote nothing")
	}
	if data := rcLoad(t, FinalizeUpdateReceipt(ctx, "success", nil, "")); data["pm_venv_rebuild"] != nil {
		t.Fatal("concurrent sync embedded")
	}
}

func TestReceiptNestedUpdateIDsDoNotCrossEmbed(t *testing.T) {
	ctx, h := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	outerID := *CurrentCorrelationID(ctx)
	token := pm.BeginReceipt(ctx, "sync")
	pm.RecordVenvRebuild(ctx, true, "outer rebuild")
	pm.RecordStep(ctx, "outer-sync", true, "")
	pm.FinalizeReceipt(ctx, "ok", 0, token)
	BeginUpdateReceipt(ctx, nil, "")
	innerID := *CurrentCorrelationID(ctx)
	if innerID == outerID {
		t.Fatal("nested update reused the outer id")
	}
	token = pm.BeginReceipt(ctx, "sync")
	pm.RecordVenvRebuild(ctx, true, "inner rebuild")
	pm.RecordStep(ctx, "inner-sync", true, "")
	pm.FinalizeReceipt(ctx, "ok", 0, token)
	innerPath := FinalizeUpdateReceipt(ctx, "success", nil, "")
	if rcLoad(t, innerPath)["pm_venv_rebuild"].(map[string]any)["reason"] != "inner rebuild" {
		t.Fatal("inner rebuild missing")
	}
	if pm.LastForUpdate(ctx, innerID, false) != nil {
		t.Fatal("inner completion not consumed")
	}
	if id := CurrentCorrelationID(ctx); id == nil || *id != outerID {
		t.Fatal("outer correlation not restored")
	}
	if steps := pm.LastForUpdate(ctx, outerID, false)["steps"].([]any); steps[0].(map[string]any)["name"] != "outer-sync" {
		t.Fatal("outer completion lost")
	}
	outerPath := FinalizeUpdateReceipt(ctx, "partial", nil, "")
	outer := rcLoad(t, outerPath)
	raw, _ := json.Marshal(outer)
	if outer["update_id"] != outerID || outer["pm_venv_rebuild"].(map[string]any)["reason"] != "outer rebuild" ||
		strings.Contains(string(raw), "inner rebuild") || rcFirst(t, outer["pm_steps"], "pm_steps")["name"] != "outer-sync" {
		t.Fatalf("outer %v", outer)
	}
	if pm.LastForUpdate(ctx, outerID, false) != nil || innerPath == outerPath {
		t.Fatal("outer completion not consumed or paths collide")
	}
	inner := rcLoad(t, innerPath)
	if inner["update_id"] != innerID || rcFirst(t, inner["pm_steps"], "pm_steps")["name"] != "inner-sync" {
		t.Fatalf("inner %v", inner)
	}
	if latest := rcLoad(t, filepath.Join(h, "logs", "update_receipts", "latest.json")); !reflect.DeepEqual(latest, outer) {
		t.Fatal("latest is not the outer receipt")
	}
	if CurrentCorrelationID(ctx) != nil {
		t.Fatal("receipt left open")
	}
}

func TestReceiptSyncAfterNestedUpdateFinalizesEmbedsIntoOuter(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	BeginUpdateReceipt(ctx, nil, "")
	FinalizeUpdateReceipt(ctx, "success", nil, "")
	rcSync(ctx, &[2]any{false, "already in sync"}, nil, nil, nil)
	data := rcLoad(t, FinalizeUpdateReceipt(ctx, "success", nil, ""))
	if !reflect.DeepEqual(data["pm_venv_rebuild"], map[string]any{"ok": false, "reason": "already in sync"}) {
		t.Fatalf("rebuild %v", data["pm_venv_rebuild"])
	}
}

func TestReceiptPMWarningsAndRefusalEmbed(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	rcSync(ctx, nil, nil, []string{"shim quarantine skipped: file locked"}, &[2]string{"lazy-install", "extras outside the frozen feature set"})
	data := rcLoad(t, FinalizeUpdateReceipt(ctx, "success", nil, ""))
	refusal := data["pm_refusal"].(map[string]any)
	if rcFirst(t, data["pm_warnings"], "pm_warnings")["message"] != "shim quarantine skipped: file locked" ||
		refusal["code"] != "lazy-install" || refusal["detail"] != "extras outside the frozen feature set" {
		t.Fatalf("data %v", data)
	}
}

func TestReceiptEmbedFailureNeverBreaksTheUpdateReceipt(t *testing.T) {
	ctx, _ := rcHome(t)
	pmLastForUpdate = func(context.Context, string, bool) *jsonutil.Object { panic("pm import exploded") }
	t.Cleanup(func() { pmLastForUpdate = pm.LastForUpdateObject })
	BeginUpdateReceipt(ctx, nil, "")
	path := FinalizeUpdateReceipt(ctx, "success", nil, "")
	if path == "" || rcLoad(t, path)["outcome"] != "success" {
		t.Fatal("embed failure broke the receipt")
	}
}

func TestReceiptGitStopClassFromArgvAndFixedPhrases(t *testing.T) {
	cases := []struct {
		stderr string
		argv   []string
		code   int
		want   string
	}{
		{"fatal: Unable to create '/x/.git/index.lock': File exists.", []string{"git", "merge"}, 128, "git_index_locked"},
		{"git fetch timed out after 300s (a stalled remote)", []string{"git", "fetch"}, 124, "git_timeout"},
		{"error: No space left on device", []string{"git", "checkout"}, 1, "disk_full"},
		{"error: could not write index", []string{"git", "stash", "push"}, 1, "local_changes_blocked"},
		{"fatal: bad object", []string{"git", "reset", "--hard"}, 128, "checkout_move_failed"},
		{"whatever", []string{"uv", "pip", "install"}, 1, ""},
		{"error: open(\"a\"): Permission denied", []string{"git", "checkout"}, 1, "permission_denied"},
		{"git@github.com: Permission denied (publickey).", []string{"git", "fetch"}, 128, ""},
	}
	for _, c := range cases {
		got := GitErrorStopClass(fmt.Errorf("wrapped: %w", &executil.StatusError{Args: c.argv, ExitCode: c.code, Stderr: []byte(c.stderr)}))
		if (got == nil) != (c.want == "") || (got != nil && *got != c.want) {
			t.Fatalf("%q %v: %v want %q", c.stderr, c.argv, got, c.want)
		}
	}
	if GitErrorStopClass(os.ErrNotExist) != nil {
		t.Fatal("non process error classified")
	}
}

func TestReceiptReconcileInterruptedRunNamesItsLastStage(t *testing.T) {
	ctx, h := rcHome(t)
	out := &strings.Builder{}
	receiptStdout = out
	t.Cleanup(func() { receiptStdout = os.Stdout })
	dir := filepath.Join(h, "logs", "update_receipts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dead := `{"update_id": "dead", "outcome": "running", "pid": 2147483000, "started_at": "s", "stages": [{"name": "apply"}]}`
	for _, name := range []string{"update_20260101_000000_1_dead.json", "latest.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(dead), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := ReconcileInterruptedRuns(ctx)
	if len(got) != 1 || got[0]["stop_reason"] != "process exited during the update (last stage: apply)" {
		t.Fatalf("interrupted %v", got)
	}
	if ReadLatestReceipt(ctx)["outcome"] != "interrupted" || !strings.Contains(out.String(), "The previous update (s) was interrupted") {
		t.Fatalf("latest not reconciled: %s", out)
	}
}
