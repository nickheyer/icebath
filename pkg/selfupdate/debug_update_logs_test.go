package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/logging"
)

const dulUpdateLog = "=== icebath update started 2026-09-02T03:21:09 ===\n→ Fetching updates...\n→ Restarting gateways...\n✗ Gateway restart failed\n"

func dulHomeWithUpdateLogs(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath_test_home")
	dbgWrite(t, filepath.Join(h, "logs", "agent.log"), "2026-09-02 03:21:00 INFO agent.conversation_loop: API call #1\n")
	dbgWrite(t, filepath.Join(h, "logs", "update.log"), dulUpdateLog)
	t.Setenv("ICEBATH_HOME", h)
	dbgRestoreSeams(t)
	return h
}

func dulHomeWithoutUpdateLogs(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath_test_home")
	for _, name := range []string{"agent.log", "errors.log", "gateway.log"} {
		dbgWrite(t, filepath.Join(h, "logs", name), "2026-09-02 03:21:00 INFO x: one line\n")
	}
	t.Setenv("ICEBATH_HOME", h)
	dbgRestoreSeams(t)
	return h
}

func TestCaptureDefaultLogSnapshotsSnapshotsIncludeTheUpdateKey(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	snaps := debugCaptureDefaultLogSnapshots(context.Background(), 100, true)
	if !strings.Contains(snaps["update"].TailText, "Gateway restart failed") {
		t.Fatal(snaps)
	}
}

func TestCaptureDefaultLogSnapshotsSnapshotsReadTheRootHomeUpdateLogUnderAProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	profile := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(filepath.Join(profile, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbgWrite(t, filepath.Join(root, "logs", "update.log"), dulUpdateLog)
	t.Setenv("ICEBATH_HOME", profile)
	dbgRestoreSeams(t)
	snaps := debugCaptureDefaultLogSnapshots(context.Background(), 100, true)
	if !strings.Contains(snaps["update"].TailText, "Gateway restart failed") {
		t.Fatal(snaps)
	}
}

func TestCaptureDefaultLogSnapshotsMissingFilesReportAbsenceWithoutRaising(t *testing.T) {
	dulHomeWithoutUpdateLogs(t)
	snaps := debugCaptureDefaultLogSnapshots(context.Background(), 100, true)
	if snaps["update"].TailText != "(file not found)" {
		t.Fatal(snaps)
	}
}

func TestCollectDebugReportReportContainsTheUpdateSection(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	report, err := debugCollectDebugReport(context.Background(), 50, "dump", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--- update.log", "Gateway restart failed"} {
		if !strings.Contains(report, want) {
			t.Fatalf("missing %q in %s", want, report)
		}
	}
}

func TestCollectDebugReportReportKeepsTheUpdateSectionWhenTheLogIsAbsent(t *testing.T) {
	dulHomeWithoutUpdateLogs(t)
	report, err := debugCollectDebugReport(context.Background(), 50, "dump", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--- agent.log", "--- update.log"} {
		if !strings.Contains(report, want) {
			t.Fatalf("missing %q in %s", want, report)
		}
	}
}

func TestCollectDebugReportReportToleratesAHandBuiltSnapshotsDict(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	empty := ""
	legacy := map[string]LogSnapshot{}
	for _, k := range []string{"agent", "errors", "gateway"} {
		legacy[k] = LogSnapshot{FullText: &empty}
	}
	report, err := debugCollectDebugReport(context.Background(), 50, "dump", legacy)
	if err != nil || !strings.Contains(report, "--- agent.log") || strings.Contains(report, "--- update.log") {
		t.Fatal(report, err)
	}
}

func TestCollectShareBundleBundleIncludesTheUpdateLogWhenPresent(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	dbgPinDump("")
	b, err := debugCollectShareBundle(context.Background(), 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.vals["update.log"], "Gateway restart failed") || !strings.Contains(b.vals["update.log"], "--- full update.log ---") {
		t.Fatal(b.keys)
	}
}

func TestCollectShareBundleBundleOmitsTheUpdateKeyWhenTheFileIsAbsent(t *testing.T) {
	dulHomeWithoutUpdateLogs(t)
	dbgPinDump("")
	b, err := debugCollectShareBundle(context.Background(), 50, false)
	if err != nil {
		t.Fatal(err)
	}
	_, upd := b.vals["update.log"]
	_, agent := b.vals["agent.log"]
	if upd || !agent {
		t.Fatal(b.keys)
	}
}

func TestFullLogUploadLabelsUploadLabelListIncludesTheUpdateLog(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	dbgPinDump("")
	captured := dbgRecordUploads()
	debugScheduleAutoDelete = func(context.Context, []string, int) error { return nil }
	result, err := BuildDebugShare(context.Background(), 50, 1, false)
	if err != nil || result.URLs["update.log"] == "" || result.URLs["agent.log"] == "" || len(*captured) != 3 {
		t.Fatal(result, err)
	}
}

func TestFullLogUploadLabelsLocalPrintLabelListIncludesTheUpdateLog(t *testing.T) {
	dulHomeWithUpdateLogs(t)
	dbgPinDump("")
	out, _, _ := dbgRun(t, map[string]any{"lines": 50, "expire": 1, "local": true, "no_redact": true, "yes": false})
	if !strings.Contains(out, "FULL update.log") || !strings.Contains(out, "FULL agent.log") {
		t.Fatal(out)
	}
}

func TestLogsRegistryUpdateKeyResolvesTheRealFilename(t *testing.T) {
	if logging.LogFiles["update"] != "update.log" {
		t.Fatal(logging.LogFiles)
	}
}
