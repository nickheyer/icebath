package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Writes a sessions config that archives after the given idle days
func writeArchiveConfig(t *testing.T, dir string, days int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "sessions:\n  auto_archive: true\n  auto_archive_days: " + strconv.Itoa(days) + "\n  min_interval_hours: 0\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Resets the in process auto archive throttle for one test
func resetAutoArchiveThrottle(t *testing.T) {
	lastAutoArchiveCheck.Lock()
	lastAutoArchiveCheck.at = map[string]float64{}
	lastAutoArchiveCheck.Unlock()
	t.Cleanup(func() {
		lastAutoArchiveCheck.Lock()
		lastAutoArchiveCheck.at = map[string]float64{}
		lastAutoArchiveCheck.Unlock()
	})
}

func TestAutoArchiveUsesTheSweptProfilesOwnRetentionConfig(t *testing.T) {
	ctx := context.Background()
	homeA := filepath.Join(t.TempDir(), "a")
	t.Setenv("ICEBATH_HOME", homeA)
	homeB := filepath.Join(homeA, "profiles", "b")
	writeArchiveConfig(t, homeA, 3650)
	writeArchiveConfig(t, homeB, 0)
	resetAutoArchiveThrottle(t)
	db, err := state.NewSessionDB(ctx, filepath.Join(homeB, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "s1", map[string]any{"role": "user", "content": "hello"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	profile := "b"
	maybeAutoArchiveForProfile(ctx, &profile)

	db, err = state.NewSessionDB(ctx, filepath.Join(homeB, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	row, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	archived, _ := row.Get("archived")
	if !pyTruthy(archived) {
		t.Fatal("profile b's store was swept with another profile's sessions.auto_archive_days")
	}
}

func TestServeTimerRunsDueCuratorOnceAndHonorsPause(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "curator:\n  enabled: true\n  consolidate: false\n  interval_hours: 168\n  min_idle_hours: 0\n  prune_builtins: false\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	memory.SaveCuratorState(ctx, pyjson.ObjectOf("last_run_at", "2020-01-01T00:00:00+00:00", "run_count", 0, "paused", true))
	runCount := func() int64 {
		st, err := memory.LoadCuratorState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := st.Get("run_count")
		count, err := pyint.Int64(n)
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); autoArchiveTickerLoop(loopCtx, .02, 0) }()
	defer func() { cancel(); <-done }()
	tick := func() { time.Sleep(200 * time.Millisecond) }

	tick()
	if runCount() != 0 {
		t.Fatal("paused curator ran")
	}
	tuigateway.SetSessionRecord("maintenance-test", tuigateway.NewSession(map[string]any{"running": true}))
	if err := memory.SetCuratorPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	tick()
	count := runCount()
	tuigateway.PopSessionRecord("maintenance-test")
	if count != 0 {
		t.Fatal("curator ran while a chat turn was running")
	}
	deadline := time.Now().Add(8 * time.Second)
	for runCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("curator never ran once idle")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	raw, err := os.ReadFile(filepath.Join(root, "skills", ".curator_state"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := pyjson.LoadsDict(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := st.Get("run_count"); pyrepr.Str(n) != "1" {
		t.Fatalf("run_count %v", n)
	}
	if summary, _ := st.Get("last_run_summary"); !strings.Contains(pyrepr.Str(summary), "consolidation off") {
		t.Fatalf("summary %v", summary)
	}
}
