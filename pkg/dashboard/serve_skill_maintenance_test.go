package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

func TestRemovedSessionsKeepProfileIdleWatermark(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("HOME", root)
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "curator:\n  enabled: true\n  interval_hours: 168\n  min_idle_hours: 0.002\n  prune_builtins: false\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	memory.SaveCuratorState(ctx, pyjson.ObjectOf("last_run_at", "2020-01-01T00:00:00+00:00", "run_count", 0))
	loadState := func() *pyjson.Object {
		st, err := memory.LoadCuratorState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	summary := func() string {
		raw, _ := loadState().Get("last_run_summary")
		if raw == nil {
			return ""
		}
		return pyrepr.Str(raw)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); autoArchiveTickerLoop(loopCtx, .05, 8) }()
	defer func() {
		cancel()
		<-done
		tuigateway.PopSessionRecord("other-profile")
		tuigateway.PopSessionRecord("watermark")
	}()

	time.Sleep(7500 * time.Millisecond)
	var recent float64
	for _, reason := range []string{"tui_close", "ws_orphan_reap"} {
		recent = float64(time.Now().UnixNano()) / 1e9
		tuigateway.SetSessionRecord("watermark", tuigateway.NewSession(map[string]any{
			"last_active": recent, "running": false, "profile_home": root}))
		if !tuiserver.CloseSessionByID(ctx, "watermark", reason) {
			t.Fatalf("close %s returned false", reason)
		}
		idleFor, idle, err := skillMaintenanceIdleFor(ctx, recent-3600)
		if err != nil || !idle || idleFor >= 2 {
			t.Fatalf("idle after %s: %v %v %v", reason, idleFor, idle, err)
		}
	}
	tuigateway.SetSessionRecord("other-profile", tuigateway.NewSession(map[string]any{
		"running": true, "profile_home": filepath.Join(root, "other")}))
	if _, idle, err := skillMaintenanceIdleFor(ctx, recent-3600); err != nil || !idle {
		t.Fatalf("sibling profile held the idle clock: %v %v", idle, err)
	}
	time.Sleep(2 * time.Second)
	n, _ := loadState().Get("run_count")
	if count, err := pyint.Int64(n); err != nil || count != 0 {
		t.Fatalf("run_count %v %v", n, err)
	}
	deadline := time.Now().Add(12 * time.Second)
	for !strings.Contains(summary(), "consolidation off") {
		if time.Now().After(deadline) {
			t.Fatalf("curator never ran, summary %q", summary())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
