package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/logging"
)

const (
	dulUpdateLog  = "=== icebath update started 2026-09-02T03:21:09 ===\n→ Fetching updates...\n→ Building desktop packaged app...\n✗ Desktop GUI build failed\n"
	dulHandoffLog = "2026-09-02T03:21:08 desktop rebuild exit code: 1\n2026-09-02T03:21:08 rebuild!| npm ERR! code ELIFECYCLE\n"
)

func dulHomeWithUpdateLogs(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath_test_home")
	dbgWrite(t, filepath.Join(h, "logs", "agent.log"), "2026-09-02 03:21:00 INFO agent.conversation_loop: API call #1\n")
	dbgWrite(t, filepath.Join(h, "logs", "update.log"), dulUpdateLog)
	dbgWrite(t, filepath.Join(h, "logs", "desktop-update-handoff.log"), dulHandoffLog)
	t.Setenv("ICEBATH_HOME", h)
	dbgRestoreSeams(t)
	return h
}

func dulHomeWithoutUpdateLogs(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath_test_home")
	for _, name := range []string{"agent.log", "errors.log", "gateway.log", "gui.log", "desktop.log"} {
		dbgWrite(t, filepath.Join(h, "logs", name), "2026-09-02 03:21:00 INFO x: one line\n")
	}
	t.Setenv("ICEBATH_HOME", h)
	dbgRestoreSeams(t)
	return h
}

func TestCaptureDefaultLogSnapshotsSnapshotsIncludeUpdateAndHandoffKeys(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	snaps := debugCaptureDefaultLogSnapshots(context.Background(), 100, true)
	if !strings.Contains(snaps["update"].TailText, "Desktop GUI build failed") || !strings.Contains(snaps["handoff"].TailText, "ELIFECYCLE") {
		t.Fatal(snaps)
	}
}

func TestCaptureDefaultLogSnapshotsSnapshotsReadTheRootHomeUpdateLogsUnderAProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	profile := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(filepath.Join(profile, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbgWrite(t, filepath.Join(root, "logs", "update.log"), dulUpdateLog)
	dbgWrite(t, filepath.Join(root, "logs", "desktop-update-handoff.log"), dulHandoffLog)
	t.Setenv("ICEBATH_HOME", profile)
	dbgRestoreSeams(t)
	snaps := debugCaptureDefaultLogSnapshots(context.Background(), 100, true)
	if !strings.Contains(snaps["update"].TailText, "Desktop GUI build failed") || !strings.Contains(snaps["handoff"].TailText, "ELIFECYCLE") {
		t.Fatal(snaps)
	}
}

func TestCaptureDefaultLogSnapshotsMissingFilesReportAbsenceWithoutRaising(t *testing.T) {
	dulHomeWithoutUpdateLogs(t)
	snaps := debugCaptureDefaultLogSnapshots(context.Background(), 100, true)
	if !strings.Contains(snaps["update"].TailText, "not found") || !strings.Contains(snaps["handoff"].TailText, "not found") {
		t.Fatal(snaps)
	}
}

func TestCollectDebugReportReportContainsUpdateAndHandoffSections(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	report, err := debugCollectDebugReport(context.Background(), 50, "dump", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--- update.log", "Desktop GUI build failed", "--- desktop-update-handoff.log", "ELIFECYCLE"} {
		if !strings.Contains(report, want) {
			t.Fatalf("missing %q in %s", want, report)
		}
	}
}

func TestCollectDebugReportReportOmitsUpdateSectionsWhenLogsAbsent(t *testing.T) {
	dulHomeWithoutUpdateLogs(t)
	report, err := debugCollectDebugReport(context.Background(), 50, "dump", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--- agent.log", "--- update.log", "--- desktop-update-handoff.log"} {
		if !strings.Contains(report, want) {
			t.Fatalf("missing %q in %s", want, report)
		}
	}
}

func TestCollectDebugReportReportToleratesAHandBuiltSnapshotsDict(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	empty := ""
	legacy := map[string]LogSnapshot{}
	for _, k := range []string{"agent", "errors", "gateway", "gui", "desktop"} {
		legacy[k] = LogSnapshot{FullText: &empty}
	}
	report, err := debugCollectDebugReport(context.Background(), 50, "dump", legacy)
	if err != nil || !strings.Contains(report, "--- agent.log") || strings.Contains(report, "--- update.log") {
		t.Fatal(report, err)
	}
}

func TestCollectShareBundleBundleIncludesUpdateLogsWhenPresent(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	dbgPinDump("")
	b, err := debugCollectShareBundle(context.Background(), 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.vals["update.log"], "Desktop GUI build failed") || !strings.Contains(b.vals["update.log"], "--- full update.log ---") ||
		!strings.Contains(b.vals["desktop-update-handoff.log"], "ELIFECYCLE") {
		t.Fatal(b.keys)
	}
}

func TestCollectShareBundleBundleOmitsUpdateKeysWhenFilesAbsent(t *testing.T) {
	dulHomeWithoutUpdateLogs(t)
	dbgPinDump("")
	b, err := debugCollectShareBundle(context.Background(), 50, false)
	if err != nil {
		t.Fatal(err)
	}
	_, upd := b.vals["update.log"]
	_, hand := b.vals["desktop-update-handoff.log"]
	_, agent := b.vals["agent.log"]
	if upd || hand || !agent {
		t.Fatal(b.keys)
	}
}

func TestFullLogUploadLabelsUploadLabelListIncludesUpdateLogs(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	dbgPinDump("")
	captured := dbgRecordUploads()
	debugScheduleAutoDelete = func(context.Context, []string, int) error { return nil }
	result, err := BuildDebugShare(context.Background(), 50, 1, false)
	if err != nil || result.URLs["update.log"] == "" || result.URLs["desktop-update-handoff.log"] == "" || len(*captured) != 4 {
		t.Fatal(result, err)
	}
}

func TestFullLogUploadLabelsLocalPrintLabelListIncludesUpdateLogs(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	dbgPinDump("")
	out, _, _ := dbgRun(t, map[string]any{"lines": 50, "expire": 1, "local": true, "nous": false, "no_redact": true, "yes": false})
	if !strings.Contains(out, "FULL update.log") || !strings.Contains(out, "FULL desktop-update-handoff.log") {
		t.Fatal(out)
	}
}

func TestLogsRegistryUpdateAndHandoffKeysResolveRealFilenames(t *testing.T) {
	if logging.LogFiles["update"] != "update.log" || logging.LogFiles["handoff"] != "desktop-update-handoff.log" {
		t.Fatal(logging.LogFiles)
	}
}
