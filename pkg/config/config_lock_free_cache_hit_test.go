package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const maxBlockedReadSecs = 2.0

// Home whose config.yaml carries the hook timeout, max turns and a hundred filler sections
func lockFreeConfigHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath-home")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{"plugins:", "  hook_callback_timeout: 30", "agent:", "  max_turns: 500"}
	for i := range 100 {
		lines = append(lines, fmt.Sprintf("section_%d:", i), fmt.Sprintf("  key_a: value_%d", i))
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	ResetCache()
	t.Cleanup(ResetCache)
	return h
}

// Primes the reader then times one more call while another goroutine holds the config lock
func timeCachedReadUnderHeldLock[T any](t *testing.T, reader func() T) (T, float64) {
	t.Helper()
	reader()
	holding, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		configLock.Lock()
		defer configLock.Unlock()
		close(holding)
		select {
		case <-release:
		case <-time.After(30 * time.Second):
		}
	}()
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("lock holder never started")
	}
	started := time.Now()
	result := reader()
	elapsed := time.Since(started).Seconds()
	close(release)
	<-done
	return result, elapsed
}

func TestCachedReadCompletesWhileAnotherThreadHoldsTheConfigLock(t *testing.T) {
	h := lockFreeConfigHome(t)
	cfg, elapsed := timeCachedReadUnderHeldLock(t, func() Tree {
		tree, err := LoadReadonly(context.Background(), h, EnvRefLookup)
		if err != nil {
			t.Error(err)
		}
		return tree
	})
	if len(cfg) == 0 {
		t.Fatalf("config %v", cfg)
	}
	if elapsed >= maxBlockedReadSecs {
		t.Fatalf("a cached load_config_readonly() blocked %.1fs behind a held _CONFIG_LOCK; cache hits must not serialize against config writers", elapsed)
	}
}

func TestCachedRawReadCompletesWhileAnotherThreadHoldsTheConfigLock(t *testing.T) {
	h := lockFreeConfigHome(t)
	raw, elapsed := timeCachedReadUnderHeldLock(t, func() map[string]any {
		m, err := ReadRawConfigReadonly(context.Background(), h)
		if err != nil {
			t.Error(err)
		}
		return m
	})
	if !reflect.DeepEqual(AsMap(raw["plugins"]), map[string]any{"hook_callback_timeout": 30}) {
		t.Fatalf("plugins %#v", raw["plugins"])
	}
	if elapsed >= maxBlockedReadSecs {
		t.Fatalf("a cached read_raw_config_readonly() blocked %.1fs behind a held _CONFIG_LOCK; cache hits must not serialize against config writers", elapsed)
	}
}
