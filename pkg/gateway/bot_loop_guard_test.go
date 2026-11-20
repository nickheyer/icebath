package gateway

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConcurrentAdmitsRespectTheBudget(t *testing.T) {
	now := 1_000_000.0
	guard := NewBotLoopGuard(func(context.Context) BotLoopGuardSettings {
		return BotLoopGuardSettings{Enabled: true, MaxEvents: 20, WindowSeconds: 60, CooldownSeconds: 60}
	}, func() float64 { return now })
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 80 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := guard.Admit(context.Background(), "c"); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 20 {
		t.Fatalf("allowed = %d", allowed.Load())
	}
}

func TestLoadSettingsReadsConfigYaml(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("gateway:\n  bot_loop_guard:\n    max_events: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadBotLoopGuardSettings(context.Background()).MaxEvents; got != 3 {
		t.Fatalf("max_events = %d", got)
	}
	broken := t.TempDir()
	t.Setenv("ICEBATH_HOME", broken)
	if err := os.Mkdir(filepath.Join(broken, "config.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := LoadBotLoopGuardSettings(context.Background()); got != DefaultBotLoopGuardSettings() {
		t.Fatalf("settings = %+v", got)
	}
}
