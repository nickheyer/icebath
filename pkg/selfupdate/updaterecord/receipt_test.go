package updaterecord

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Isolated ICEBATH_HOME with test serve obligation hooks and a fresh receipt scope
func rcHome(t *testing.T) (context.Context, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_UPDATE_CORRELATION_ID", "")
	rcHooks(t)
	return pm.WithReceiptScope(UpdateReceiptScope(context.Background())), h
}

// Serve obligation hooks keeping manual serve rows that carry no creation time
func rcHooks(t *testing.T) {
	t.Helper()
	isReminder := func(row any) bool {
		m, ok := row.(map[string]any)
		return ok && (m["kind"] == "serve" || m["kind"] == "dashboard") && m["supervisor"] == "manual-serve"
	}
	SetServeObligationHooks(ServeObligationHooks{
		IsReminderRow: isReminder,
		RetainReceiptManualServes: func(_ context.Context, receipt map[string]any) []any {
			pending := []any{}
			plan, _ := receipt["plan"].(map[string]any)
			rows := append(listItems(mapGet(plan, "runtimes")), listItems(receipt["pending_manual_serves"])...)
			for _, row := range append(rows, listItems(receipt["carried_manual_serves"])...) {
				if isReminder(row) {
					pending = append(pending, row)
				}
			}
			return pending
		},
	})
	t.Cleanup(func() { SetServeObligationHooks(ServeObligationHooks{}) })
}

func rcLoad(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

func rcCurrent(ctx context.Context) *UpdateReceipt {
	st := stateOf(ctx)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.current
}

func rcArchives(t *testing.T, dir string) []string {
	t.Helper()
	paths, _ := globDir(dir, "update_*.json")
	return paths
}

func rcFirst(t *testing.T, v any, key string) map[string]any {
	t.Helper()
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("%s: not a non empty list: %#v", key, v)
	}
	return list[0].(map[string]any)
}

func TestReceiptBeginRecordFinalizeRoundtrip(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "pre_update_backup", true, "snapshot=abc123")
	RecordSkip(ctx, "gateway_restart", "no gateways running")
	RecordGatewayRestart(ctx, GatewayRestart{RestartedServices: []string{"icebath-gateway"},
		RelaunchedProfiles: []string{"work"}, KilledPIDs: []int{123}, FailedUnits: []string{}})
	path := FinalizeUpdateReceipt(ctx, "success", []any{map[string]any{"profile": "default", "state": "current"}}, "")
	payload := rcLoad(t, path)
	if payload["schema"] != 1.0 || payload["outcome"] != "success" || payload["finished_at"] == nil {
		t.Fatalf("payload %v", payload)
	}
	step := rcFirst(t, payload["steps"], "steps")
	if step["name"] != "pre_update_backup" || step["ok"] != true {
		t.Fatalf("step %v", step)
	}
	if rcFirst(t, payload["skips"], "skips")["reason"] != "no gateways running" {
		t.Fatal(payload["skips"])
	}
	gr := payload["gateway_restart"].(map[string]any)
	if !reflect.DeepEqual(gr["restarted_services"], []any{"icebath-gateway"}) || !reflect.DeepEqual(gr["relaunched_profiles"], []any{"work"}) ||
		!reflect.DeepEqual(gr["killed_pids"], []any{123.0}) || gr["incomplete"] != false {
		t.Fatalf("gateway restart %v", gr)
	}
	if rcFirst(t, payload["fleet"], "fleet")["profile"] != "default" {
		t.Fatal(payload["fleet"])
	}
}

func TestReceiptFreshRecoveryResultReachesPersistedReceipt(t *testing.T) {
	ctx, _ := rcHome(t)
	recovery := map[string]any{
		"requested": []any{"coder", "default", "ops"}, "verified": []any{"default"},
		"relaunch_attempted": []any{"ops"}, "failed": []any{"coder"},
		"skipped": []any{map[string]any{"profile": "desk", "kind": "serve", "supervisor": "desktop",
			"reason": "desktop app owns and respawns this serve backend"}},
	}
	BeginUpdateReceipt(ctx, nil, "")
	RecordGatewayRestart(ctx, GatewayRestart{Incomplete: true, PhaseError: "boom: module vanished mid-pull", FreshRecovery: recovery})
	persisted := rcLoad(t, FinalizeUpdateReceipt(ctx, "partial", nil, ""))["gateway_restart"].(map[string]any)["fresh_recovery"].(map[string]any)
	for key, want := range recovery {
		if !reflect.DeepEqual(persisted[key], want) {
			t.Fatalf("%s: %v != %v", key, persisted[key], want)
		}
	}
	if !reflect.DeepEqual(persisted["serve_units"], map[string]any{"verified": []any{}, "failed": []any{}}) ||
		!reflect.DeepEqual(persisted["stale_runtimes"], []any{}) {
		t.Fatalf("persisted %v", persisted)
	}
	if _, ok := persisted["succeeded"]; ok {
		t.Fatal("succeeded bucket persisted")
	}
}

func TestReceiptLatestPointerWrittenAndReadable(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "git_pull", true, "")
	FinalizeUpdateReceipt(ctx, "partial", nil, "")
	if latest := ReadLatestReceipt(ctx); latest == nil || latest["outcome"] != "partial" {
		t.Fatalf("latest %v", latest)
	}
}

func TestReceiptRecordWithoutBeginIsNoop(t *testing.T) {
	ctx, _ := rcHome(t)
	RecordStep(ctx, "orphan", true, "")
	RecordSkip(ctx, "orphan", "no receipt")
	RecordGatewayRestart(ctx, GatewayRestart{})
	if path := FinalizeUpdateReceipt(ctx, "success", nil, ""); path != "" {
		t.Fatalf("finalized %s", path)
	}
}

func TestReceiptPruningKeepsRecent(t *testing.T) {
	_, h := rcHome(t)
	old := receiptKeep
	receiptKeep = 3
	t.Cleanup(func() { receiptKeep = old })
	directory := filepath.Join(h, "logs", "update_receipts")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(directory, "update_2026010"+string(rune('0'+i))+"_000000_1.json")
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(1000+i), 0)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	pruneOldReceipts(directory)
	var names []string
	for _, p := range rcArchives(t, directory) {
		names = append(names, filepath.Base(p))
	}
	sort.Strings(names)
	want := []string{"update_20260103_000000_1.json", "update_20260104_000000_1.json", "update_20260105_000000_1.json"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("remaining %v", names)
	}
}

func rcExit(code int) *int { return &code }

func TestReceiptPendingPersistedOnExit2Refusal(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "windows_preflight", false, "another icebath.exe running")
	payload := rcLoad(t, FinalizePendingUpdateReceipt(ctx, rcExit(2), "sys.exit(2)"))
	if payload["outcome"] != "refused" || payload["exit_code"] != 2.0 || payload["stop_reason"] != "sys.exit(2)" || payload["finished_at"] == nil {
		t.Fatalf("payload %v", payload)
	}
	if CurrentCorrelationID(ctx) != nil {
		t.Fatal("receipt still open")
	}
}

func TestReceiptPendingPersistedOnExit1Failure(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	payload := rcLoad(t, FinalizePendingUpdateReceipt(ctx, rcExit(1), "sys.exit(1)"))
	if payload["outcome"] != "failed" || payload["exit_code"] != 1.0 {
		t.Fatalf("payload %v", payload)
	}
}

func TestReceiptNoopWhenInnerPathAlreadyFinalized(t *testing.T) {
	ctx, h := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	if FinalizeUpdateReceipt(ctx, "success", nil, "") == "" {
		t.Fatal("first finalize wrote nothing")
	}
	if second := FinalizePendingUpdateReceipt(ctx, rcExit(0), "boundary"); second != "" {
		t.Fatalf("second finalize %s", second)
	}
	if n := len(rcArchives(t, filepath.Join(h, "logs", "update_receipts"))); n != 1 {
		t.Fatalf("%d archives", n)
	}
}

func TestReceiptNoopWhenNeverBegun(t *testing.T) {
	ctx, _ := rcHome(t)
	if FinalizePendingUpdateReceipt(ctx, rcExit(2), "sys.exit(2)") != "" || ReadLatestReceipt(ctx) != nil {
		t.Fatal("never begun receipt wrote something")
	}
}

const rcCorrelation = "1e7cb007-a6f8-4f6c-badb-d80e204942bf"

// Desktop's managed SSH receipt lookup, newest finished archive carrying the correlation id
func rcDesktopCorrelated(t *testing.T, h, correlation string) map[string]any {
	t.Helper()
	paths := rcArchives(t, filepath.Join(h, "logs", "update_receipts"))
	ordered, err := byMtime(paths)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(ordered) - 1; i >= 0; i-- {
		payload := rcLoad(t, ordered[i])
		if payload["correlation_id"] != correlation || payload["outcome"] == "running" || payload["finished_at"] == nil {
			continue
		}
		return payload
	}
	return nil
}

func TestReceiptCarriesLauncherCorrelation(t *testing.T) {
	ctx, h := rcHome(t)
	t.Setenv("ICEBATH_UPDATE_CORRELATION_ID", rcCorrelation)
	BeginUpdateReceipt(ctx, nil, "")
	updateID := *CurrentCorrelationID(ctx)
	FinalizeUpdateReceipt(ctx, "success", nil, "")
	payload := rcDesktopCorrelated(t, h, rcCorrelation)
	if payload == nil || payload["outcome"] != "success" || payload["update_id"] != updateID || updateID == rcCorrelation {
		t.Fatalf("payload %v", payload)
	}
}

func TestReceiptHandoffKeepsCorrelation(t *testing.T) {
	ctx, h := rcHome(t)
	t.Setenv("ICEBATH_UPDATE_CORRELATION_ID", rcCorrelation)
	BeginUpdateReceipt(ctx, nil, "")
	preSwap := rcCurrent(ctx).Data.Copy()
	inner := UpdateReceiptScope(ctx)
	t.Setenv("ICEBATH_UPDATE_CORRELATION_ID", "")
	BeginUpdateReceipt(inner, preSwap, preSwap.Vals["update_id"].(string))
	FinalizeUpdateReceipt(inner, "success", nil, "")
	if rcDesktopCorrelated(t, h, rcCorrelation) == nil {
		t.Fatal("handoff receipt lost the launcher correlation")
	}
}

func TestReceiptLegacyHandoffGainsCorrelation(t *testing.T) {
	ctx, h := rcHome(t)
	t.Setenv("ICEBATH_UPDATE_CORRELATION_ID", rcCorrelation)
	BeginUpdateReceipt(ctx, pyjson.ObjectOf("update_id", "abc123", "steps", []any{}), "abc123")
	FinalizeUpdateReceipt(ctx, "success", nil, "")
	if payload := rcDesktopCorrelated(t, h, rcCorrelation); payload == nil || payload["update_id"] != "abc123" {
		t.Fatalf("payload %v", payload)
	}
}

func TestReceiptNoLauncherCorrelationIsNull(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	FinalizeUpdateReceipt(ctx, "success", nil, "")
	if latest := ReadLatestReceipt(ctx); latest["correlation_id"] != nil {
		t.Fatalf("correlation %v", latest["correlation_id"])
	}
}

func TestReceiptNestedBeginDoesNotReconcileLiveOuterReceipt(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	outer := rcCurrent(ctx).Data
	BeginUpdateReceipt(ctx, nil, "")
	before := rcLoad(t, runFile(receiptDir(ctx), outer))
	FinalizePendingUpdateReceipt(ctx, rcExit(0), "")
	restored := CurrentCorrelationID(ctx)
	FinalizePendingUpdateReceipt(ctx, rcExit(0), "")
	if before["outcome"] != "running" || restored == nil || *restored != outer.Vals["update_id"] {
		t.Fatalf("outcome %v restored %v outer %v", before["outcome"], restored, outer.Vals["update_id"])
	}
}

func TestReceiptWriterIdentityCannotResurrectAMismatchedOwner(t *testing.T) {
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	ct := ProcessCreateTime(child.Process.Pid)
	if ct == nil {
		t.Fatal("child creation time unreadable")
	}
	pid := int64(child.Process.Pid)
	same := pyjson.ObjectOf("pid", pid, "writer_pid", pid, "pid_create_time", *ct-60)
	writer := pyjson.ObjectOf("pid", int64(0), "writer_pid", pid, "writer_create_time", *ct-60)
	control := pyjson.ObjectOf("pid", pid, "writer_pid", pid, "pid_create_time", *ct)
	if ownerAlive(same) || ownerAlive(writer) || !ownerAlive(control) {
		t.Fatalf("same %v writer %v control %v", ownerAlive(same), ownerAlive(writer), ownerAlive(control))
	}
}

func TestReceiptUserActionSurvivesInterruptBeforeNextStage(t *testing.T) {
	ctx, _ := rcHome(t)
	receiptStdout = &strings.Builder{}
	t.Cleanup(func() { receiptStdout = os.Stdout })
	BeginUpdateReceipt(ctx, nil, "")
	RecordUserAction(ctx, "local_changes", "Reapply stash@{0} by hand")
	running := ReadLatestReceipt(ctx)
	FinalizeInterruptedUpdateReceipt(ctx, "operator interrupted", 130)
	terminal := ReadLatestReceipt(ctx)
	want := map[string]any{"step": "local_changes", "reason": "Reapply stash@{0} by hand"}
	if !reflect.DeepEqual(running["user_action"], want) || !reflect.DeepEqual(terminal["user_action"], want) || terminal["outcome"] != "interrupted" {
		t.Fatalf("running %v terminal %v", running["user_action"], terminal)
	}
	if !strings.Contains(receiptStdout.(*strings.Builder).String(), "Interrupted after the code was updated") {
		t.Fatal("interrupt notice not printed")
	}
}

func TestReceiptCommandScopeRetainsOuterReceipt(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	outer := *CurrentCorrelationID(ctx)
	defer FinalizeUpdateReceipt(ctx, "success", nil, "")
	scoped := UpdateReceiptScope(ctx)
	BeginUpdateReceipt(scoped, nil, "")
	FinalizeUpdateReceipt(scoped, "success", nil, "")
	if FinalizePendingUpdateReceipt(scoped, rcExit(0), "") != "" {
		t.Fatal("scope finalized twice")
	}
	if id := CurrentCorrelationID(ctx); id == nil || *id != outer || rcCurrent(ctx).Data.Vals["outcome"] != "running" {
		t.Fatalf("outer receipt lost: %v", id)
	}
}

func TestReceiptCopiedContextFinalizeDoesNotMutateParent(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	defer FinalizeUpdateReceipt(ctx, "success", nil, "")
	FinalizePendingUpdateReceipt(CopyReceiptContext(ctx), rcExit(1), "child failed")
	parent := rcCurrent(ctx).Data.Vals
	_, hasExit := parent["exit_code"]
	_, hasStop := parent["stop_reason"]
	if parent["outcome"] != "running" || hasExit || hasStop {
		t.Fatalf("parent mutated %v", parent)
	}
}

func rcSteps(t *testing.T, path string) []any {
	var steps []any
	for _, f := range listItems(rcLoad(t, path)["followups"]) {
		steps = append(steps, f.(map[string]any)["step"])
	}
	return steps
}

func TestReceiptFollowUpOnAnOpenRunIsRecordedOnceNotAmendedAgain(t *testing.T) {
	ctx, _ := rcHome(t)
	receiptStdout = &strings.Builder{}
	t.Cleanup(func() { receiptStdout = os.Stdout })
	BeginUpdateReceipt(ctx, nil, "")
	run := rcCurrent(ctx).Data
	archive := runFile(receiptDir(ctx), run)
	id := run.Vals["update_id"].(string)
	if err := OweFollowup(ctx, &id, "channel_adoption", "remote refused", "the next `icebath update` adopts it again"); err != nil {
		t.Fatal(err)
	}
	if got := rcSteps(t, archive); !reflect.DeepEqual(got, []any{"channel_adoption"}) {
		t.Fatalf("open run followups %v", got)
	}
	FinalizeUpdateReceipt(ctx, "success", nil, "")
	if got := rcSteps(t, archive); !reflect.DeepEqual(got, []any{"channel_adoption"}) {
		t.Fatalf("terminal followups %v", got)
	}
	if !strings.Contains(receiptStdout.(*strings.Builder).String(), "Update follow-up 'channel_adoption' did not finish: remote refused (the next `icebath update` adopts it again)") {
		t.Fatal(receiptStdout)
	}
}

func TestReceiptFollowUpAfterTheRunClosedAmendsItsTerminalReceipt(t *testing.T) {
	ctx, _ := rcHome(t)
	receiptStdout = &strings.Builder{}
	t.Cleanup(func() { receiptStdout = os.Stdout })
	BeginUpdateReceipt(ctx, nil, "")
	run := rcCurrent(ctx).Data.Copy()
	FinalizeUpdateReceipt(ctx, "success", nil, "")
	id := run.Vals["update_id"].(string)
	if err := OweFollowup(ctx, &id, "windows_resume", "Windows gateway recovery failed: boom", FollowupRetryDefault); err != nil {
		t.Fatal(err)
	}
	if got := rcSteps(t, runFile(receiptDir(ctx), run)); !reflect.DeepEqual(got, []any{"windows_resume"}) {
		t.Fatalf("archive followups %v", got)
	}
	latest := ReadLatestReceipt(ctx)
	if latest["outcome"] != "success" || rcFirst(t, latest["followups"], "followups")["step"] != "windows_resume" {
		t.Fatalf("latest %v", latest)
	}
}

func TestReceiptNamedProfileSeesUpdateAndPMReceiptsInBothDirections(t *testing.T) {
	account := t.TempDir()
	profile := filepath.Join(account, ".icebath", "profiles", "foo")
	old := filepath.Join(profile, "logs", "update_receipts")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profile)
	t.Setenv("ICEBATH_UPDATE_CORRELATION_ID", "")
	rcHooks(t)
	action := strings.Repeat("a", 32)
	if err := os.WriteFile(filepath.Join(old, "update_20260101_000000_1_old.json"),
		[]byte(`{"update_id": "old", "action_id": "`+action+`", "outcome": "success"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := pm.WithReceiptScope(UpdateReceiptScope(context.Background()))
	BeginUpdateReceipt(ctx, nil, "")
	FinalizeUpdateReceipt(ctx, "success", nil, "")
	updateID := ReadLatestReceipt(ctx)["update_id"]
	pmSees := mapGet(pm.LatestReceipt(ctx), "update_id")
	time.Sleep(50 * time.Millisecond)
	token := pm.BeginReceipt(ctx, "sync")
	pm.FinalizeReceipt(ctx, "ok", 0, token)
	var roots []string
	for _, p := range rcArchives(t, receiptDir(ctx)) {
		roots = append(roots, filepath.Base(p))
	}
	found := false
	for _, name := range roots {
		found = found || strings.Contains(name, updateID.(string))
	}
	if !found || pmSees != updateID {
		t.Fatalf("root archives %v, pm sees %v, update %v", roots, pmSees, updateID)
	}
	if kind := mapGet(ReadLatestReceipt(ctx), "kind"); kind != "sync" {
		t.Fatalf("updater sees %v", kind)
	}
	if got := mapGet(ReadReceiptForAction(ctx, action), "update_id"); got != "old" {
		t.Fatalf("old archive %v", got)
	}
}

func TestReceiptFinalizeWritesParseableReceiptForOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "failed", "refused"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, _ := rcHome(t)
			BeginUpdateReceipt(ctx, nil, "")
			RecordStep(ctx, "git_pull", outcome == "success", "detail")
			payload := rcLoad(t, FinalizeUpdateReceipt(ctx, outcome, nil, ""))
			if payload["outcome"] != outcome || payload["finished_at"] == nil || rcFirst(t, payload["steps"], "steps")["name"] != "git_pull" {
				t.Fatalf("payload %v", payload)
			}
			if latest := ReadLatestReceipt(ctx); latest["outcome"] != outcome {
				t.Fatalf("latest %v", latest)
			}
		})
	}
}

// Receipt home the config getter names, a directory the receipt store never writes
func rcConfigHome(t *testing.T) (context.Context, string) {
	ctx, _ := rcHome(t)
	other := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	return ctx, other
}

func rcReceiptFiles(dir string) []string {
	paths, _ := filepath.Glob(filepath.Join(dir, "logs", "update_receipts", "*.json"))
	return paths
}

func TestReceiptNothingOnDiskUntilFinalize(t *testing.T) {
	ctx, configHome := rcConfigHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "pre_update_backup", true, "")
	if files := rcReceiptFiles(configHome); len(files) != 0 {
		t.Fatalf("files %v", files)
	}
}

func TestReceiptCrashWithoutFinalizeNeverClaimsSuccess(t *testing.T) {
	ctx, configHome := rcConfigHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "git_pull", true, "")
	RecordStep(ctx, "pip_install", true, "")
	stateOf(ctx).current = nil
	if files := rcReceiptFiles(configHome); len(files) != 0 {
		t.Fatalf("files %v", files)
	}
	if latest := ReadLatestReceipt(ctx); latest != nil && latest["outcome"] == "success" {
		t.Fatalf("latest %v", latest)
	}
}

func TestReceiptBoundarySafetyNetRecordsCrashAsNotSuccess(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "git_pull", false, "network died")
	if payload := rcLoad(t, FinalizePendingUpdateReceipt(ctx, rcExit(1), "sys.exit(1)")); payload["outcome"] != "failed" {
		t.Fatalf("payload %v", payload)
	}
	if ReadLatestReceipt(ctx)["outcome"] != "failed" {
		t.Fatal("latest not failed")
	}
}

func TestReceiptRefusalReceiptDistinguishableFromFailure(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "git_fetch", false, "remote unreachable")
	failed := rcLoad(t, FinalizePendingUpdateReceipt(ctx, rcExit(1), "sys.exit(1)"))
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "venv_preflight", false, "another icebath holds venv")
	refused := rcLoad(t, FinalizePendingUpdateReceipt(ctx, rcExit(2), "sys.exit(2)"))
	if failed["outcome"] != "failed" || failed["exit_code"] != 1.0 || refused["outcome"] != "refused" || refused["exit_code"] != 2.0 {
		t.Fatalf("failed %v refused %v", failed, refused)
	}
	if latest := ReadLatestReceipt(ctx); latest["outcome"] != "refused" || latest["stop_reason"] != "sys.exit(2)" {
		t.Fatalf("latest %v", latest)
	}
}

func TestReceiptRefusedReceiptSurvivesWithItsSteps(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "windows_preflight", false, "icebath.exe running")
	payload := rcLoad(t, FinalizePendingUpdateReceipt(ctx, rcExit(2), "concurrent instance"))
	step := rcFirst(t, payload["steps"], "steps")
	if step["name"] != "windows_preflight" || step["ok"] != false || payload["outcome"] != "refused" {
		t.Fatalf("payload %v", payload)
	}
}
