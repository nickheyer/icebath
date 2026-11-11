package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/logging"
)

func TestParseSinceHours(t *testing.T) {
	cutoff, ok := parseLogSince("2h")
	if !ok {
		t.Fatal("no cutoff")
	}
	if d := time.Since(cutoff).Seconds() - 7200; d > 2 || d < -2 {
		t.Fatalf("cutoff off by %v", d)
	}
}

func TestParseSinceInvalidReturnsNone(t *testing.T) {
	for _, s := range []string{"abc", "", "10x"} {
		if _, ok := parseLogSince(s); ok {
			t.Fatalf("%q parsed", s)
		}
	}
}

func TestParseSinceWhitespaceTolerance(t *testing.T) {
	if _, ok := parseLogSince("  5m  "); !ok {
		t.Fatal("not parsed")
	}
}

func TestParseLineTimestampStandardFormat(t *testing.T) {
	ts, ok := parseLogLineTimestamp("2026-04-11 10:23:45 INFO gateway.run: msg")
	if !ok || !ts.Equal(time.Date(2026, 4, 11, 10, 23, 45, 0, time.Local)) {
		t.Fatalf("ts %v", ts)
	}
}

func TestIsoTSeparatedUpdateAndHandoffStamps(t *testing.T) {
	want := time.Date(2026, 9, 29, 21, 36, 18, 0, time.Local)
	for _, line := range []string{"2026-09-29T21:36:18+08:00 update| step done", "2026-09-29T21:36:18Z step stalled"} {
		if ts, ok := parseLogLineTimestamp(line); !ok || !ts.Equal(want) {
			t.Fatalf("%q gave %v", line, ts)
		}
	}
}

func TestExtractLevelInfo(t *testing.T) {
	if got := extractLogLevel("2026-01-01 00:00:00 INFO gateway.run: msg"); got != "INFO" {
		t.Fatalf("level %q", got)
	}
}

func TestExtractLoggerNameStandardLine(t *testing.T) {
	if name, ok := extractLoggerName("2026-04-11 10:23:45 INFO gateway.run: Starting gateway"); !ok || name != "gateway.run" {
		t.Fatalf("name %q", name)
	}
}

func TestExtractLoggerNameNoMatch(t *testing.T) {
	if _, ok := extractLoggerName("random text"); ok {
		t.Fatal("matched")
	}
}

func TestGatewayNested(t *testing.T) {
	line := "2026-04-11 10:23:45 INFO plugins.platforms.telegram.adapter: msg"
	if !lineMatchesComponent(line, logging.ComponentPrefixes["gateway"]) {
		t.Fatal("gateway component missed")
	}
}

func TestUnparseableLine(t *testing.T) {
	if lineMatchesComponent("random text", []string{"gateway"}) {
		t.Fatal("matched")
	}
}

func strPtr(s string) *string { return &s }

func TestLevelFilter(t *testing.T) {
	f := logFilters{minLevel: strPtr("WARNING")}
	if !matchesLogFilters("2026-01-01 00:00:00 WARNING x: msg", f) || matchesLogFilters("2026-01-01 00:00:00 INFO x: msg", f) {
		t.Fatal("level filter")
	}
}

func TestCombinedFilters(t *testing.T) {
	line := "2026-04-11 10:00:00 WARNING [sess_1] gateway.run: connection lost"
	f := logFilters{minLevel: strPtr("WARNING"), sessionFilter: strPtr("sess_1"), componentPrefixes: []string{"gateway"}, hasComponent: true}
	if !matchesLogFilters(line, f) {
		t.Fatal("gateway rejected")
	}
	f.componentPrefixes = []string{"tools"}
	if matchesLogFilters(line, f) {
		t.Fatal("tools matched")
	}
}

func TestSinceFilter(t *testing.T) {
	cutoff := time.Now().Add(-time.Hour)
	f := logFilters{since: &cutoff}
	if matchesLogFilters("2020-01-01 00:00:00 INFO x: old msg", f) {
		t.Fatal("old line kept")
	}
	if !matchesLogFilters(time.Now().Format("2006-01-02 15:04:05")+" INFO x: recent msg", f) {
		t.Fatal("recent line dropped")
	}
}

func TestReadSmallFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("2026-01-01 00:00:0" + string(rune('0'+i)) + " INFO x: line " + string(rune('0'+i)) + "\n")
	}
	os.WriteFile(path, []byte(b.String()), 0o644)
	result, err := readLastNLogLines(path, 5)
	if err != nil || len(result) != 5 || !strings.Contains(result[4], "line 9") {
		t.Fatalf("result %v %v", result, err)
	}
}

func TestUnstampedLinesShareTheVerdictOfTheRecordAbove(t *testing.T) {
	old := time.Now().Add(-3*time.Hour).Format("2006-01-02 15:04:05") + ",000"
	recent := time.Now().Format("2006-01-02 15:04:05") + ",000"
	frames := "Traceback (most recent call last):\n  File \"x.py\", line 1, in f\n"
	path := filepath.Join(t.TempDir(), "errors.log")
	os.WriteFile(path, []byte("orphan tail of a record that started before the window\n"+
		old+" ERROR gateway.run: old failure\n"+frames+"ValueError: old\n"+
		recent+" INFO tools.x: multi-line info\n"+"  info continuation\n"+
		recent+" ERROR gateway.run: new failure\n"+frames+"ValueError: new\n"), 0o644)
	since := time.Now().Add(-time.Hour)
	read := func(f logFilters) string {
		lines, err := readLogTail(path, 50, true, f)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(lines, "")
	}
	newFailure := recent + " ERROR gateway.run: new failure\n" + frames + "ValueError: new\n"
	if got := read(logFilters{since: &since}); got != recent+" INFO tools.x: multi-line info\n  info continuation\n"+newFailure {
		t.Fatalf("since %q", got)
	}
	if got := read(logFilters{since: &since, minLevel: strPtr("WARNING")}); got != newFailure {
		t.Fatalf("level %q", got)
	}
	if got := read(logFilters{since: &since, componentPrefixes: logging.ComponentPrefixes["gateway"], hasComponent: true}); got != newFailure {
		t.Fatalf("component %q", got)
	}
	if got := read(logFilters{sessionFilter: strPtr("orphan")}); got != "orphan tail of a record that started before the window\n" {
		t.Fatalf("orphan %q", got)
	}
}

func TestUpdateLogsAreReadFromTheRootHomeUnderAProfile(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	profile := filepath.Join(root, "profiles", "coder")
	os.MkdirAll(filepath.Join(root, "logs"), 0o755)
	os.MkdirAll(filepath.Join(profile, "logs"), 0o755)
	os.WriteFile(filepath.Join(root, "logs", "update.log"), []byte("=== icebath update started 2026-10-03T10:00:00 ===\n✓ Update complete!\n"), 0o644)
	os.WriteFile(filepath.Join(profile, "logs", "agent.log"), []byte("2026-10-03 10:00:00,000 INFO run_agent: hi\n"), 0o644)
	t.Setenv("ICEBATH_HOME", profile)
	ctx := context.Background()
	for name, want := range map[string]string{"update": filepath.Join(root, "logs", "update.log"),
		"handoff": filepath.Join(root, "logs", "desktop-update-handoff.log"), "agent": filepath.Join(profile, "logs", "agent.log")} {
		if got, _ := logFilePath(ctx, name); got != want {
			t.Fatalf("%s at %s want %s", name, got, want)
		}
	}
	out := &bytes.Buffer{}
	if code, err := TailLog(ctx, out, TailLogOptions{Name: "update", Lines: 5}); err != nil || code != 0 || !strings.Contains(out.String(), "✓ Update complete!") {
		t.Fatalf("tail %d %v %q", code, err, out.String())
	}
	out.Reset()
	if err := ListLogs(ctx, out); err != nil || !strings.Contains(out.String(), "agent.log") || !strings.Contains(out.String(), "update.log (root)") {
		t.Fatalf("list %v %q", err, out.String())
	}
}
