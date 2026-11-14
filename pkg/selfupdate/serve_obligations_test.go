package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Answers every liveness probe with the given tri state and captures both warning streams
func pinServeLiveness(t *testing.T, alive, known bool) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	savedProbe, savedOut, savedErr := servePidAliveMatches, serveStdout, serveStderr
	t.Cleanup(func() { servePidAliveMatches, serveStdout, serveStderr = savedProbe, savedOut, savedErr })
	servePidAliveMatches = func(int64, *float64) (bool, bool) { return alive, known }
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	serveStdout, serveStderr = out, errOut
	return out, errOut
}

func manualServeRow(kind string, created any) map[string]any {
	return map[string]any{"kind": kind, "profile": "work", "pid": int64(900), "supervisor": "manual-serve", "code_sha": nil, "code_version": nil,
		"restart_via": "respawn-argv", "detail": map[string]any{"create_time": created}}
}

func TestUnreadableCreateTimeDischargesOnlyAProvenDeadPid(t *testing.T) {
	for _, kind := range []string{"serve", "dashboard"} {
		for _, c := range []struct {
			name         string
			alive, known bool
		}{{"alive", true, true}, {"unknown", false, false}, {"dead", false, true}} {
			t.Run(kind+"_"+c.name, func(t *testing.T) {
				pinServeLiveness(t, c.alive, c.known)
				ctx := context.Background()
				runtime := manualServeRow(kind, nil)
				pending, err := RetainReceiptManualServes(ctx, map[string]any{"plan": map[string]any{"runtimes": []any{runtime}}})
				if err != nil {
					t.Fatal(err)
				}
				want := []any{runtime}
				if c.known && !c.alive {
					want = []any{}
				}
				if !reflect.DeepEqual(pending, want) {
					t.Fatalf("pending %v want %v", pending, want)
				}
				if DeferManualServe(ctx, runtime, true) {
					t.Fatal("an unidentified row counted as a live handoff")
				}
			})
		}
	}
}

func TestDeferManualServeFilesOneReminderPerIncarnation(t *testing.T) {
	pinServeLiveness(t, true, true)
	ctx := context.Background()
	runtime := manualServeRow("serve", 1000.0)
	if !DeferManualServe(ctx, runtime, false) || !DeferManualServe(ctx, runtime, true) {
		t.Fatal("live identified serve not deferred")
	}
	path := filepath.Join(serveRestartPendingDir(ctx), "900-0x1.f400000000000p+9.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"kind": "serve", "profile": "work", "pid": 900, "create_time": 1000.0}` {
		t.Fatalf("reminder %s", raw)
	}
	entries, _ := os.ReadDir(serveRestartPendingDir(ctx))
	if len(entries) != 1 {
		t.Fatalf("reminder directory holds %d files", len(entries))
	}
	for _, row := range []map[string]any{
		{"kind": "gateway", "supervisor": "manual-serve", "restart_via": "respawn-argv", "pid": int64(900), "detail": map[string]any{"create_time": 1000.0}},
		{"kind": "serve", "supervisor": "systemd", "restart_via": "respawn-argv", "pid": int64(900), "detail": map[string]any{"create_time": 1000.0}},
		{"kind": "serve", "supervisor": "manual-serve", "restart_via": "manual", "pid": int64(900), "detail": map[string]any{"create_time": 1000.0}},
		{"kind": "serve", "supervisor": "manual-serve", "restart_via": "respawn-argv", "pid": int64(900), "detail": "x"},
		{"kind": "serve", "supervisor": "manual-serve", "restart_via": "respawn-argv", "pid": true, "detail": map[string]any{"create_time": 1000.0}},
		{"kind": "serve", "supervisor": "manual-serve", "restart_via": "respawn-argv", "pid": int64(0), "detail": map[string]any{"create_time": 1000.0}},
	} {
		if DeferManualServe(ctx, row, false) {
			t.Fatalf("row %v deferred", row)
		}
	}
}

func TestDeferralStaysOwedWhenTheReminderCannotBeWrittenAndDischargesADeadIncarnation(t *testing.T) {
	pinServeLiveness(t, true, true)
	ctx := context.Background()
	if err := os.WriteFile(serveRestartPendingDir(ctx), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime := manualServeRow("dashboard", 1000.0)
	if DeferManualServe(ctx, runtime, false) {
		t.Fatal("deferral reported filed with no reminder directory")
	}
	servePidAliveMatches = func(int64, *float64) (bool, bool) { return false, true }
	if !DeferManualServe(ctx, runtime, false) || DeferManualServe(ctx, runtime, true) {
		t.Fatal("dead incarnation not discharged, or counted as a live handoff")
	}
}

func TestUnitServeRowsCarryTheirRestartCommand(t *testing.T) {
	pinServeLiveness(t, true, true)
	ctx := context.Background()
	unit := map[string]any{"kind": "dashboard", "profile": "ops", "pid": int64(77), "supervisor": "systemd",
		"detail": map[string]any{"create_time": int64(5), "systemd_unit": "icebath-dashboard.service", "systemd_scope": "system"}}
	if !IsUnitServeRow(unit) || !IsReminderRow(unit) || IsUnitServeRow(manualServeRow("serve", 1.0)) || !IsReminderRow(manualServeRow("serve", 1.0)) {
		t.Fatal("row classification")
	}
	if IsReminderRow(map[string]any{"kind": "gateway", "supervisor": "manual-serve"}) || IsReminderRow("serve") {
		t.Fatal("a row outside the reminder flow classified into it")
	}
	deferred, err := DeferUnitServe(ctx, unit)
	if err != nil || !deferred {
		t.Fatalf("unit serve deferral %v %v", deferred, err)
	}
	raw, err := os.ReadFile(filepath.Join(serveRestartPendingDir(ctx), "77-0x1.4000000000000p+2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"kind": "dashboard", "profile": "ops", "pid": 77, "create_time": 5, "restart": ["sudo systemctl restart icebath-dashboard.service"]}` {
		t.Fatalf("reminder %s", raw)
	}
	if deferred, _ := DeferUnitServe(ctx, manualServeRow("serve", 1.0)); deferred {
		t.Fatal("manual row deferred as a unit")
	}
}

func TestRetentionKeepsFailedTransfersOnceAcrossEveryReceiptSection(t *testing.T) {
	pinServeLiveness(t, false, false)
	ctx := context.Background()
	manual := manualServeRow("serve", 1000.0)
	other := manualServeRow("dashboard", 2000.0)
	receipt := map[string]any{"plan": map[string]any{"runtimes": []any{manual, map[string]any{"kind": "gateway", "profile": "default"}}},
		"pending_manual_serves": []any{manualServeRow("serve", 1000.0)}, "carried_manual_serves": []any{other, "junk"}}
	servePidAliveMatches = func(int64, *float64) (bool, bool) { return true, true }
	if err := os.WriteFile(serveRestartPendingDir(ctx), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	pending, err := RetainReceiptManualServes(ctx, receipt)
	if err != nil || !reflect.DeepEqual(pending, []any{manual, other}) {
		t.Fatalf("pending %v %v", pending, err)
	}
	if pending, err := RetainReceiptManualServes(ctx, map[string]any{}); err != nil || len(pending) != 0 {
		t.Fatalf("empty receipt %v %v", pending, err)
	}
	if _, err := RetainReceiptManualServes(ctx, map[string]any{"plan": []any{"x"}}); err == nil {
		t.Fatal("a plan that is not an object accepted")
	}
}

func TestWarnPendingManualServesNamesUnsavedTransfersAndFiledReminders(t *testing.T) {
	out, errOut := pinServeLiveness(t, true, true)
	ctx := context.Background()
	unit := map[string]any{"kind": "dashboard", "profile": "ops", "pid": int64(77), "supervisor": "systemd",
		"detail": map[string]any{"systemd_unit": "icebath-dashboard.service", "systemd_scope": "user"}}
	if err := WarnPendingManualServes(ctx, true, []any{manualServeRow("serve", 1000.0), manualServeRow("serve", nil), unit}, true); err != nil {
		t.Fatal(err)
	}
	want := "  ⚠ serve [work] pid 900: manual restart reminder could not be saved; restart remains pending in the update receipt.\n" +
		"    Ask its owner to relaunch `icebath serve` / `icebath dashboard`; check reminder storage permissions and free space.\n" +
		"  ⚠ serve [work] pid 900: manual restart reminder could not be saved; restart remains pending in the update receipt.\n" +
		"    This host could not read the process creation time, so no durable reminder could be filed; ask its owner to relaunch `icebath serve` / `icebath dashboard`, and the warning clears once the pid is confirmed gone.\n" +
		"  ⚠ dashboard [ops] pid 77: unit restart reminder could not be saved; restart remains pending in the update receipt.\n" +
		"    Restart its unit: `systemctl --user restart icebath-dashboard.service`\n"
	if errOut.String() != want || out.Len() != 0 {
		t.Fatalf("startup warning\n got %q\nwant %q\nstdout %q", errOut.String(), want, out.String())
	}
	errOut.Reset()
	if !DeferManualServe(ctx, manualServeRow("serve", 1000.0), false) {
		t.Fatal("reminder not filed")
	}
	if deferred, err := DeferUnitServe(ctx, map[string]any{"kind": "dashboard", "profile": "ops", "pid": int64(77), "supervisor": "systemd",
		"detail": map[string]any{"create_time": int64(5), "systemd_unit": "icebath-dashboard.service"}}); err != nil || !deferred {
		t.Fatal("unit reminder not filed")
	}
	dir := serveRestartPendingDir(ctx)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keyless.json"), []byte(`{"pid": 5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WarnPendingManualServes(ctx, false, nil, true); err != nil {
		t.Fatal(err)
	}
	want = "  ⚠ dashboard [ops] pid 77: unit restart still pending; this process may still serve pre-update code.\n" +
		"    Restart its unit: `systemctl --user restart icebath-dashboard.service` or `sudo systemctl restart icebath-dashboard.service`\n" +
		"  ⚠ serve [work] pid 900: manual restart still pending; this process may still serve pre-update code.\n" +
		"    Ask its owner to relaunch `icebath serve` / `icebath dashboard` (reconnect Desktop for an SSH backend).\n" +
		"  ⚠ Manual serve restart reminder could not be verified: broken.json\n" +
		"  ⚠ Manual serve restart reminder could not be verified: keyless.json\n"
	if out.String() != want || errOut.Len() != 0 {
		t.Fatalf("warning\n got %q\nwant %q\nstderr %q", out.String(), want, errOut.String())
	}
	servePidAliveMatches = func(int64, *float64) (bool, bool) { return false, true }
	out.Reset()
	if err := WarnPendingManualServes(ctx, false, nil, true); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(left) != 2 || strings.Contains(out.String(), "900") || strings.Contains(out.String(), "pid 77") {
		t.Fatalf("dead incarnations kept: files %v output %q", left, out.String())
	}
}
