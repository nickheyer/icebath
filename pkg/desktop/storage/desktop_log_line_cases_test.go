package storage

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/desktoplog"
)

type recordingHost struct {
	events []string
	shown  []string
	picked []string
	pick   string
}

func (h *recordingHost) Broadcast(event string, _ any) { h.events = append(h.events, event) }
func (h *recordingHost) ShowItemInFolder(path string)  { h.shown = append(h.shown, path) }
func (h *recordingHost) PickDirectory(title, defaultPath string) (string, bool, error) {
	h.picked = append(h.picked, title+"|"+defaultPath)
	return h.pick, h.pick != "", nil
}

func serviceForTest(t *testing.T) (*Service, *recordingHost) {
	t.Helper()
	home := t.TempDir()
	host := &recordingHost{}
	log := desktoplog.New(filepath.Join(home, "logs", "desktop.log"), time.Now)
	s := NewService(Options{HermesHome: home, UserData: t.TempDir(), Log: log, Host: host})
	t.Cleanup(s.Close)
	return s, host
}

func TestForwardsFullRendererErrorLinesThroughTheExposedBridge(t *testing.T) {
	s, _ := serviceForTest(t)
	line := "[renderer error:main] Prompt failed: database is locked\n    at saveSession (session.ts:12)"
	raw, _ := json.Marshal(line)
	s.LogLine(raw)
	lines := s.log.Recent(200)
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " [icebath] [renderer error:main] Prompt failed: database is locked") ||
		!strings.HasSuffix(lines[1], " [icebath]     at saveSession (session.ts:12)") {
		t.Fatalf("lines %q", lines)
	}
}

func TestRendererErrorBoundaryReportsAreClampedAndCarryTheComponentStack(t *testing.T) {
	s, host := serviceForTest(t)
	s.ReportRendererError(json.RawMessage(`{"label":"main","boundary":7,"message":"","componentStack":"  at App\n"}`))
	s.ReportRendererError(json.RawMessage(`null`))
	lines := s.log.Recent(200)
	want := []string{"[renderer crash:main] [error-boundary:7] (no message)", "at App", "[renderer crash:unknown] [error-boundary:unknown] (no message)"}
	if len(lines) != 3 {
		t.Fatalf("lines %q", lines)
	}
	for i, w := range want {
		if !strings.HasSuffix(lines[i], " [icebath] "+w) {
			t.Fatalf("line %d %q", i, lines[i])
		}
	}
	recent := s.GetRecentLogs()
	if p, _ := recent.Get("path"); p != s.log.Path() {
		t.Fatalf("path %v", p)
	}
	reveal := s.RevealLogs()
	if ok, _ := reveal.Get("ok"); ok != true || len(host.shown) != 1 || host.shown[0] != s.log.Path() {
		t.Fatalf("reveal %s %v", jsStringify(reveal, ""), host.shown)
	}
	long := strings.Repeat("x", 7000)
	raw, _ := json.Marshal(long)
	s.LogLine(raw)
	if last := s.log.Recent(1)[0]; !strings.HasSuffix(last, " [icebath] "+strings.Repeat("x", 6000)) {
		t.Fatalf("clamp %d", len(last))
	}
}
