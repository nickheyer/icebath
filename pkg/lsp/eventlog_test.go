package lsp

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestDisabledEmitsAtDebug(t *testing.T) {
	resetCaches(t)
	rt, h, _ := testRuntime(t)
	logDisabled(rt, "pyright", "/x.py", "feature off")
	logDisabled(rt, "pyright", "/x.py", "ext not mapped")
	for _, r := range h.all() {
		if r.level != slog.LevelDebug || r.logger != eventLogger {
			t.Fatalf("%+v", r)
		}
	}
}

func TestDiagnosticsAlwaysInfo(t *testing.T) {
	resetCaches(t)
	rt, h, _ := testRuntime(t)
	for i := range 5 {
		logDiagnostics(rt, "pyright", "/x"+string(rune('0'+i))+".py", 1)
	}
	info := 0
	for _, r := range h.all() {
		if r.level == slog.LevelInfo && strings.Contains(r.msg, "diags") {
			info++
		}
	}
	if info != 5 {
		t.Fatalf("info %d", info)
	}
}

func TestSpawnFailedWarns(t *testing.T) {
	resetCaches(t)
	rt, h, _ := testRuntime(t)
	logSpawnFailed(rt, "pyright", "/proj", &pyExc{typ: "FileNotFoundError", text: "nope"})
	recs := h.all()
	if len(recs) != 1 || recs[0].level != slog.LevelWarn || recs[0].msg != "lsp[pyright] spawn/initialize failed for /proj: FileNotFoundError: nope" {
		t.Fatalf("%+v", recs)
	}
	logServerError(rt, "pyright", "/proj/x.py", errors.New("boom"))
	logTimeout(rt, "pyright", "/proj/x.py", "diagnostics")
}

func TestThousandCleanWritesEmitOneInfo(t *testing.T) {
	resetCaches(t)
	rt, h, _ := testRuntime(t)
	logActive(rt, "pyright", "/proj")
	for range 1000 {
		logClean(rt, "pyright", "/proj/x.py")
	}
	var info []logRecord
	for _, r := range h.all() {
		if r.level == slog.LevelInfo {
			info = append(info, r)
		}
	}
	if len(info) != 1 || !strings.Contains(info[0].msg, "active for") {
		t.Fatalf("%+v", info)
	}
}

func TestAnnounceBucketIsCapped(t *testing.T) {
	resetCaches(t)
	rt, h, _ := testRuntime(t)
	prev := announceCap
	announceCap = 4
	t.Cleanup(func() { announceCap = prev })
	for i := range 6 {
		logNoProjectRoot(rt, "pyright", "/proj/f"+string(rune('0'+i))+".py")
	}
	announceMu.Lock()
	n := len(announcedNoRoot)
	announceMu.Unlock()
	if n > 4 {
		t.Fatalf("bucket %d", n)
	}
	logNoProjectRoot(rt, "pyright", "/proj/f5.py")
	count := 0
	for _, r := range h.all() {
		if r.level == slog.LevelInfo && r.msg == "lsp[pyright] no project root for /proj/f5.py" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("f5 info lines %d", count)
	}
}

func TestReapedForgetsActiveAnnouncement(t *testing.T) {
	resetCaches(t)
	rt, h, _ := testRuntime(t)
	logActive(rt, "pyright", "/proj")
	logReaped(rt, []clientKey{{"pyright", "/proj"}}, 600)
	logActive(rt, "pyright", "/proj")
	if got := len(h.matching("active for /proj")); got != 2 {
		t.Fatalf("respawn must re-announce at info, got %d", got)
	}
	if len(h.matching("lsp[reaper] reaped 1 idle client(s) after 600s: pyright (/proj)")) != 1 {
		t.Fatalf("%+v", h.all())
	}
}
