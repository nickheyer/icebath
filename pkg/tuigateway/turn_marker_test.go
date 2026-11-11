package tuigateway

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMarkerRoundtrip(t *testing.T) {
	home := t.TempDir()
	RecordTurnStart(home, "abc", "fix the bug", 1, true, "")
	marker := ReadTurnMarker(home, "abc")
	if marker == nil {
		t.Fatal("marker missing after record")
	}
	if marker["prompt"] != "fix the bug" || marker["attempts"] != int64(1) {
		t.Fatalf("marker = %#v", marker)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if math.Abs(marker["started_at"].(float64)-now) > 5 {
		t.Fatalf("started_at = %v, now %v", marker["started_at"], now)
	}
	ClearTurnMarker(home, "abc")
	if got := ReadTurnMarker(home, "abc"); got != nil {
		t.Fatalf("marker survived clear: %#v", got)
	}
}

func TestMarkerSurvivesCorruptSidecar(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "desktop", "interrupted_turns.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadTurnMarker(home, "abc"); got != nil {
		t.Fatalf("corrupt sidecar read as %#v", got)
	}
	RecordTurnStart(home, "abc", "prompt", 0, true, "")
	if got := ReadTurnMarker(home, "abc"); got == nil || got["prompt"] != "prompt" {
		t.Fatalf("marker = %#v", got)
	}
}

func TestMarkerWriterStateRejectsARecycledPid(t *testing.T) {
	yesterday := float64(time.Now().UnixNano())/1e9 - 86400
	if state := MarkerWriterState(map[string]any{"writer_pid": int64(os.Getpid()), "writer_start_time": yesterday}); state == "alive" {
		t.Fatalf("recycled pid read as %q", state)
	}
}
