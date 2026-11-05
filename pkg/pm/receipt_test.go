package pm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Resets every receipt binding of the process scope for one test, the isolated receipt context fixture
func rptIsolateReceipts(t *testing.T) {
	t.Helper()
	saved := defaultReceiptScope
	defaultReceiptScope = &receiptScope{}
	t.Cleanup(func() { defaultReceiptScope = saved })
}

// Context whose receipts land in a fresh temp home, the homed fixture
func rptHomed(t *testing.T) (context.Context, string) {
	t.Helper()
	dir := t.TempDir()
	return home.WithOverride(context.Background(), dir), dir
}

// Decoded JSON object of a receipt file
func rptReadReceipt(t *testing.T, path string) map[string]any {
	t.Helper()
	v, err := loadJSON(readFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("receipt %s is not an object", path)
	}
	return m
}

// Field of the nth element of a receipt list
func rptItem(t *testing.T, list any, index int, key string) any {
	t.Helper()
	items, ok := list.([]any)
	if !ok || len(items) <= index {
		t.Fatalf("no element %d in %v", index, list)
	}
	row, ok := items[index].(map[string]any)
	if !ok {
		t.Fatalf("element %d of %v is not an object", index, list)
	}
	return row[key]
}

// Names of the steps of a receipt
func rptStepNames(t *testing.T, receipt map[string]any) []string {
	t.Helper()
	steps, _ := receipt["steps"].([]any)
	names := []string{}
	for i := range steps {
		name, _ := rptItem(t, steps, i, "name").(string)
		names = append(names, name)
	}
	return names
}

// Open update receipt stand in whose correlation id the provider reports until it finalizes
type rptUpdateReceipt struct {
	mu sync.Mutex
	id string
}

func (u *rptUpdateReceipt) begin() {
	u.mu.Lock()
	u.id = randomHex32()
	u.mu.Unlock()
}

func (u *rptUpdateReceipt) finalize() {
	u.mu.Lock()
	u.id = ""
	u.mu.Unlock()
}

func (u *rptUpdateReceipt) correlationID() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.id
}

func TestReceiptBeginRecordFinalizeRoundtrip(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		code    int
	}{{"failed", 1}, {"refused", 2}} {
		t.Run(tc.outcome, func(t *testing.T) {
			isolateHome(t)
			rptIsolateReceipts(t)
			base, homed := rptHomed(t)
			ctx := WithReceiptScope(base)
			if LatestReceipt(ctx) != nil {
				t.Fatal("latest before any receipt")
			}
			if _, err := os.Stat(filepath.Join(homed, "logs")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("logs dir exists before any write: %v", err)
			}
			if FinalizeReceipt(ctx, "ok", 0, nil) != "" {
				t.Fatal("finalize without begin wrote a receipt")
			}
			RecordWarning(ctx, "orphan")
			RecordRefusal(ctx, "x", "y")
			if LatestReceipt(ctx) != nil {
				t.Fatal("records without begin produced a receipt")
			}
			BeginReceipt(ctx, "sync")
			if v, ok := ReceiptSnapshot(ctx)["update_id"]; !ok || v != nil {
				t.Fatalf("update_id %v present %v", v, ok)
			}
			RecordStep(ctx, "uv-lock", true, "")
			RecordVenvRebuild(ctx, true, "")
			RecordFeatureList(ctx, []string{"web", "acp"})
			if ReceiptSnapshot(ctx)["kind"] != "sync" {
				t.Fatal("kind is not sync")
			}
			path := FinalizeReceipt(ctx, "ok", 0, nil)
			if path == "" || !isRegularFile(path) {
				t.Fatalf("receipt path %q", path)
			}
			data := rptReadReceipt(t, path)
			if !jsonDeepEqual(LatestReceipt(ctx), data) {
				t.Fatalf("latest %v differs from %v", LatestReceipt(ctx), data)
			}
			if data["kind"] != "sync" || data["outcome"] != "ok" {
				t.Fatalf("kind %v outcome %v", data["kind"], data["outcome"])
			}
			if !jsonDeepEqual(data["venv_rebuild"], map[string]any{"ok": true, "reason": ""}) {
				t.Fatalf("venv_rebuild %v", data["venv_rebuild"])
			}
			if rptItem(t, data["steps"], 0, "name") != "uv-lock" {
				t.Fatalf("steps %v", data["steps"])
			}
			if !jsonDeepEqual(data["feature_list"], []any{"web", "acp"}) {
				t.Fatalf("feature_list %v", data["feature_list"])
			}
			if ReceiptSnapshot(ctx) != nil {
				t.Fatal("snapshot after finalize")
			}
			if FinalizeReceipt(ctx, "ok", 0, nil) != "" {
				t.Fatal("second finalize wrote a receipt")
			}
			if LastForUpdate(ctx, "any-id", false) != nil || LastForUpdate(ctx, "", false) != nil {
				t.Fatal("standalone sync filed under an update")
			}
			BeginReceipt(ctx, "sync")
			RecordVenvRebuild(ctx, false, "uv sync exited 1")
			RecordWarning(ctx, "first")
			RecordWarning(ctx, "second")
			RecordRefusal(ctx, "lazy-install", "extras outside frozen set")
			failed := FinalizeReceipt(ctx, tc.outcome, tc.code, nil)
			latest := LatestReceipt(ctx)
			if !jsonDeepEqual(latest, rptReadReceipt(t, failed)) {
				t.Fatalf("latest %v differs from the failed receipt", latest)
			}
			if latest["outcome"] != tc.outcome || !jsonEquals(latest["exit_code"], tc.code) {
				t.Fatalf("outcome %v exit_code %v", latest["outcome"], latest["exit_code"])
			}
			if reason := latest["venv_rebuild"].(map[string]any)["reason"]; reason != "uv sync exited 1" {
				t.Fatalf("reason %v", reason)
			}
			refusal := latest["refusal"].(map[string]any)
			if refusal["code"] != "lazy-install" || refusal["detail"] != "extras outside frozen set" {
				t.Fatalf("refusal %v", refusal)
			}
			warnings, _ := latest["warnings"].([]any)
			var messages []string
			for i := range warnings {
				messages = append(messages, rptItem(t, warnings, i, "message").(string))
				if !truthy(rptItem(t, warnings, i, "at")) {
					t.Fatalf("warning %d has no time", i)
				}
			}
			if strings.Join(messages, ",") != "first,second" {
				t.Fatalf("warnings %v", messages)
			}
			if !jsonDeepEqual(rptReadReceipt(t, path), data) {
				t.Fatal("the first receipt changed")
			}
		})
	}
}

// Child half of the bootstrap test, reporting a failed sync from a fresh process
func TestReceiptBootstrapChild(t *testing.T) {
	if os.Getenv("RPT_RECEIPT_CHILD") != "1" {
		return
	}
	ctx := context.Background()
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		os.Exit(1)
	}
	runSync := func() (err error) {
		token := BeginReceipt(ctx, "sync")
		defer FinalizeReceipt(ctx, "failed", 1, token)
		err = NewInstallError("venv", "original dependency failure", "")
		RecordStep(ctx, "dependency-sync", false, err.Error())
		return err
	}
	var installErr *InstallError
	if err := runSync(); !errors.As(err, &installErr) || !strings.Contains(err.Error(), "original dependency failure") {
		fail("unexpected error %v", err)
	}
	row := LatestReceipt(ctx)
	if row == nil || row["outcome"] != "failed" || !jsonEquals(row["exit_code"], 1) {
		fail("row %v", row)
	}
	detail, _ := row["steps"].([]any)[0].(map[string]any)["detail"].(string)
	if !strings.Contains(detail, "original dependency failure") {
		fail("detail %q", detail)
	}
	body, err := dumpsJSON(row, false)
	if err != nil {
		fail("%v", err)
	}
	os.Stdout.Write(body)
	os.Exit(0)
}

func TestReceiptBarePythonCanReportAFailedBootstrap(t *testing.T) {
	tmp := isolateHome(t)
	rptIsolateReceipts(t)
	t.Setenv(home.EnvHome, filepath.Join(tmp, "home"))
	child := exec.Command(os.Args[0], "-test.run=^TestReceiptBootstrapChild$")
	child.Env = append(os.Environ(), "RPT_RECEIPT_CHILD=1")
	var stdout, stderr strings.Builder
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err != nil {
		t.Fatalf("%v\n%s%s", err, stdout.String(), stderr.String())
	}
	v, err := loadJSON([]byte(stdout.String()))
	if err != nil {
		t.Fatalf("%v: %s", err, stdout.String())
	}
	row := v.(map[string]any)
	if rptItem(t, row["steps"], 0, "ok") != false {
		t.Fatalf("steps %v", row["steps"])
	}
}

func TestReceiptConcurrentFinalizeWritesUniqueNames(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	base, _ := rptHomed(t)
	var mu sync.Mutex
	var paths []string
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := WithReceiptScope(base)
			BeginReceipt(ctx, "sync")
			RecordStep(ctx, fmt.Sprintf("step-%d", i), true, "")
			path := FinalizeReceipt(ctx, "ok", 0, nil)
			mu.Lock()
			paths = append(paths, path)
			mu.Unlock()
		}()
	}
	wg.Wait()
	names := map[string]bool{}
	for _, p := range paths {
		if p == "" || !isRegularFile(p) {
			t.Fatalf("receipt path %q", p)
		}
		names[filepath.Base(p)] = true
	}
	if len(names) != 8 {
		t.Fatalf("%d unique names among %v", len(names), paths)
	}
}

func TestReceiptRotationOnlyRemovesPMReceipts(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	_, homed := rptHomed(t)
	d := filepath.Join(homed, "logs", "update_receipts")
	updater := filepath.Join(d, "update_20260101_000000_123.json")
	writeFile(t, updater, []byte(`{"kind": "update"}`+"\n"), 0o666)
	for i := range 25 {
		writeFile(t, filepath.Join(d, fmt.Sprintf("pm_20260101T0000%02dZ-sync-1-0001.json", i)), []byte(`{"kind": "sync"}`+"\n"), 0o666)
	}
	rotateReceipts(d)
	if !isRegularFile(updater) {
		t.Fatal("the updater receipt was rotated")
	}
	kept, _ := filepath.Glob(filepath.Join(d, "pm_*.json"))
	if len(kept) != 20 {
		t.Fatalf("kept %d pm receipts", len(kept))
	}
}

func TestReceiptStateIsThreadScoped(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	base, _ := rptHomed(t)
	ctx := WithReceiptScope(base)
	mine, theirs := "mine", "theirs"
	restore := WorkerContext(ctx, &mine)
	BeginReceipt(ctx, "sync")
	restore()
	RecordStep(ctx, "mine", true, "")
	var snapshotFromOther, afterOther map[string]any
	done := make(chan struct{})
	go func() {
		defer close(done)
		other := WithReceiptScope(base)
		snapshotFromOther = ReceiptSnapshot(other)
		restore := WorkerContext(other, &theirs)
		BeginReceipt(other, "update")
		restore()
		RecordStep(other, "theirs", false, "")
		FinalizeReceipt(other, "failed", 0, nil)
		afterOther = ReceiptSnapshot(other)
	}()
	<-done
	if snapshotFromOther != nil {
		t.Fatalf("the other thread saw %v", snapshotFromOther)
	}
	if afterOther != nil {
		t.Fatalf("the other thread kept %v", afterOther)
	}
	snap := ReceiptSnapshot(ctx)
	if snap == nil || snap["kind"] != "sync" {
		t.Fatalf("snapshot %v", snap)
	}
	if names := rptStepNames(t, snap); strings.Join(names, ",") != "mine" {
		t.Fatalf("steps %v", names)
	}
	path := FinalizeReceipt(ctx, "ok", 0, nil)
	if path == "" || !isRegularFile(path) {
		t.Fatalf("receipt path %q", path)
	}
	if LastForUpdate(ctx, "theirs", false) != nil {
		t.Fatal("the other thread's receipt reached this scope")
	}
	if got := LastForUpdate(ctx, "mine", false); got == nil || rptItem(t, got["steps"], 0, "name") != "mine" {
		t.Fatalf("last for mine %v", got)
	}
}

func TestReceiptCopiedContextDoesNotCorruptParentReceipt(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	ctx := WithReceiptScope(context.Background())
	BeginReceipt(ctx, "sync")
	RecordStep(ctx, "parent", true, "")
	parentBefore := ReceiptSnapshot(ctx)
	child := WithReceiptScope(ctx)
	RecordStep(child, "child", false, "")
	childSeen := ReceiptSnapshot(child)
	if names := rptStepNames(t, childSeen); strings.Join(names, ",") != "parent,child" {
		t.Fatalf("child steps %v", names)
	}
	parentAfter := ReceiptSnapshot(ctx)
	if names := rptStepNames(t, parentAfter); strings.Join(names, ",") != "parent" {
		t.Fatalf("parent steps %v", names)
	}
	if !jsonDeepEqual(parentAfter, parentBefore) {
		t.Fatalf("parent changed from %v to %v", parentBefore, parentAfter)
	}
	FinalizeReceipt(ctx, "ok", 0, nil)
}

func TestReceiptCopiedContextFinalizeDoesNotFinishParent(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	base, _ := rptHomed(t)
	ctx := WithReceiptScope(base)
	BeginReceipt(ctx, "sync")
	FinalizeReceipt(WithReceiptScope(ctx), "failed", 1, nil)
	if v, ok := ReceiptSnapshot(ctx)["outcome"]; !ok || v != nil {
		t.Fatalf("parent outcome %v", v)
	}
}

func TestReceiptRecordedValuesAreNotMutableThroughTheInput(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	ctx := WithReceiptScope(context.Background())
	BeginReceipt(ctx, "sync")
	checks := []any{map[string]any{"plugin": "a", "result": map[string]any{"compatible": true}}}
	RecordPluginChecks(ctx, checks)
	checks[0].(map[string]any)["result"].(map[string]any)["compatible"] = false
	result := rptItem(t, ReceiptSnapshot(ctx)["plugin_checks"], 0, "result").(map[string]any)
	if result["compatible"] != true {
		t.Fatalf("recorded result %v", result)
	}
}

func TestReceiptSnapshotReturnsACopy(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	ctx := WithReceiptScope(context.Background())
	BeginReceipt(ctx, "sync")
	RecordStep(ctx, "a", true, "")
	snap := ReceiptSnapshot(ctx)
	snap["steps"] = append(snap["steps"].([]any), map[string]any{"name": "injected", "ok": true})
	snap["kind"] = "hijacked"
	live := ReceiptSnapshot(ctx)
	if names := rptStepNames(t, live); strings.Join(names, ",") != "a" {
		t.Fatalf("live steps %v", names)
	}
	if live["kind"] != "sync" {
		t.Fatalf("live kind %v", live["kind"])
	}
}

func TestReceiptNestedBeginWithTokenRestoresOuter(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	ctx := WithReceiptScope(context.Background())
	outer := BeginReceipt(ctx, "sync")
	RecordStep(ctx, "outer-step", true, "")
	inner := BeginReceipt(ctx, "update")
	RecordStep(ctx, "inner-step", true, "")
	FinalizeReceipt(ctx, "ok", 0, inner)
	live := ReceiptSnapshot(ctx)
	if live == nil || live["kind"] != "sync" {
		t.Fatalf("live %v", live)
	}
	if names := rptStepNames(t, live); strings.Join(names, ",") != "outer-step" {
		t.Fatalf("outer steps %v", names)
	}
	path := FinalizeReceipt(ctx, "ok", 0, outer)
	if path == "" || !isRegularFile(path) {
		t.Fatalf("receipt path %q", path)
	}
	if ReceiptSnapshot(ctx) != nil {
		t.Fatal("receipt left after the outer finalize")
	}
}

func TestReceiptSyncBeginAfterUpdateFinalizedHasNoUpdateID(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	base, _ := rptHomed(t)
	update := &rptUpdateReceipt{}
	ctx := WithReceiptScope(WithUpdateCorrelation(base, update.correlationID))
	update.begin()
	update.finalize()
	if update.correlationID() != "" {
		t.Fatal("correlation id survived the update finalize")
	}
	BeginReceipt(ctx, "sync")
	if v, ok := ReceiptSnapshot(ctx)["update_id"]; !ok || v != nil {
		t.Fatalf("update_id %v", v)
	}
}

func TestReceiptLastForUpdateReturnsACopy(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	base, _ := rptHomed(t)
	update := &rptUpdateReceipt{}
	ctx := WithReceiptScope(WithUpdateCorrelation(base, update.correlationID))
	update.begin()
	myID := update.correlationID()
	BeginReceipt(ctx, "sync")
	FinalizeReceipt(ctx, "ok", 0, nil)
	snap := LastForUpdate(ctx, myID, false)
	snap["outcome"] = "tampered"
	if got := LastForUpdate(ctx, myID, false); got["outcome"] != "ok" {
		t.Fatalf("outcome %v", got["outcome"])
	}
}

// Token misuse raises like ContextVar reset, a reused token or one from another scope
func TestReceiptTokenMisuseMatchesContextVarReset(t *testing.T) {
	isolateHome(t)
	rptIsolateReceipts(t)
	ctx := WithReceiptScope(context.Background())
	expectPanic := func(want string, run func()) {
		t.Helper()
		defer func() {
			if got := fmt.Sprint(recover()); got != want {
				t.Fatalf("panic %q, want %q", got, want)
			}
		}()
		run()
	}
	token := BeginReceipt(ctx, "sync")
	FinalizeReceipt(ctx, "ok", 0, token)
	BeginReceipt(ctx, "sync")
	expectPanic("receipt token has already been used once", func() { FinalizeReceipt(ctx, "ok", 0, token) })
	other := WithReceiptScope(ctx)
	foreign := BeginReceipt(ctx, "sync")
	expectPanic("receipt token was created in a different receipt scope", func() { FinalizeReceipt(other, "ok", 0, foreign) })
	if ReceiptSnapshot(other) == nil || ReceiptSnapshot(ctx) == nil {
		t.Fatal("a refused token popped a receipt")
	}
}

func TestReceiptWorkerReceiptIsAttributedOnlyToItsInvokingUpdate(t *testing.T) {
	tmp := isolateHome(t)
	rptIsolateReceipts(t)
	t.Setenv(home.EnvHome, tmp)
	ctx := WithReceiptScope(context.Background())
	worker := func() map[string]any {
		w := WithReceiptScope(context.Background())
		updateA := "update-a"
		restore := WorkerContext(w, &updateA)
		defer restore()
		token := BeginReceipt(w, "sync")
		RecordStep(w, "dependency-sync", false, "candidate refused")
		FinalizeReceipt(w, "failed", 1, token)
		return LastCompleted(w)
	}
	data := worker()
	if data["update_id"] != "update-a" || data["outcome"] != "failed" {
		t.Fatalf("worker receipt %v", data)
	}
	updateA, updateB := "update-a", "update-b"
	if err := AcceptWorkerReceipt(ctx, data, &updateA); err != nil {
		t.Fatal(err)
	}
	if got := LastForUpdate(ctx, "update-a", true); !jsonDeepEqual(got, data) {
		t.Fatalf("last for update-a %v", got)
	}
	if LastForUpdate(ctx, "update-b", false) != nil {
		t.Fatal("receipt filed under another update")
	}
	if err := AcceptWorkerReceipt(ctx, data, &updateB); err == nil || !strings.Contains(err.Error(), "correlation") {
		t.Fatalf("mismatched correlation accepted with %v", err)
	}
}
