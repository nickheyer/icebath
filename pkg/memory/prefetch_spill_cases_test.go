package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// External provider whose prefetch answers a settable recall text
type spillRecallProvider struct {
	BaseProvider
	mu       sync.Mutex
	prefetch string
}

func (p *spillRecallProvider) Name() string                                          { return "external" }
func (p *spillRecallProvider) Available(context.Context) bool                        { return true }
func (p *spillRecallProvider) Initialize(context.Context, string, InitOptions) error { return nil }
func (p *spillRecallProvider) ToolSchemas(context.Context) ([]map[string]any, error) { return nil, nil }
func (p *spillRecallProvider) Prefetch(context.Context, string, string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prefetch, nil
}

func (p *spillRecallProvider) setPrefetch(text string) {
	p.mu.Lock()
	p.prefetch = text
	p.mu.Unlock()
}

// Writes a config document through atomic_config_write
func writeSpillConfig(t *testing.T, ctx context.Context, h string, data map[string]any) {
	t.Helper()
	if err := config.AtomicConfigWrite(ctx, h, filepath.Join(h, "config.yaml"), data, ""); err != nil {
		t.Fatal(err)
	}
}

func spillExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestMemoryManagerRunawayRecallStillSpillsWithoutOptIn(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	writeSpillConfig(t, ctx, h, map[string]any{"memory": map[string]any{}})
	p := &spillRecallProvider{prefetch: strings.Repeat("x", 100_001)}
	mgr := NewMemoryManager(ctx, nil, nil)
	if err := mgr.AddProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	result := mgr.PrefetchAll(ctx, "recall", "runaway")
	if len(result) >= 100_000 {
		t.Fatalf("runaway recall kept %d chars", len(result))
	}
	saved, _ := filepath.Glob(filepath.Join(h, "hook_outputs", "runaway", "*.txt"))
	if len(saved) != 1 {
		t.Fatalf("saved %v", saved)
	}
	raw, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(string(raw), "\n") != p.prefetch {
		t.Fatalf("saved %d chars", len(raw))
	}
}

func TestMemoryManagerPrefetchSpillProfileSnapshotPreservesOptIn(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", a)
	shared := map[string]any{"max_chars": 40, "preview_head": 12, "preview_tail": 7, "directory": filepath.Join(a, "recall")}
	writeSpillConfig(t, context.Background(), a, map[string]any{
		"memory": map[string]any{"prefetch_spill_enabled": true}, "hooks": map[string]any{"output_spill": shared}})
	writeSpillConfig(t, context.Background(), b, map[string]any{"memory": map[string]any{"prefetch_spill_enabled": false}})
	text := "HEAD-CONTEXT\n" + strings.Repeat("ranked middle\n", 2000) + "TAIL!!\n"
	managers := map[string]*MemoryManager{}
	wasMultiplex := tools.MultiplexActive()
	tools.SetMultiplexActive(true)
	defer tools.SetMultiplexActive(wasMultiplex)
	for _, h := range []string{a, b, a} {
		ctx := home.WithOverride(context.Background(), h)
		mgr, ok := managers[h]
		if !ok {
			mgr = NewMemoryManager(ctx, nil, nil)
			if err := mgr.AddProvider(ctx, &spillRecallProvider{prefetch: text}); err != nil {
				t.Fatal(err)
			}
			mgr.InitializeAll(ctx, "s", InitOptions{})
			managers[h] = mgr
		}
		if h == b {
			if got := mgr.PrefetchAll(ctx, "recall", "s"); got != text {
				t.Fatalf("profile b recall %d chars", len(got))
			}
			if spillExists(filepath.Join(b, "hook_outputs")) {
				t.Fatal("profile b spilled")
			}
			continue
		}
		result := mgr.PrefetchAll(ctx, "recall", "s")
		saved, _ := filepath.Glob(filepath.Join(a, "recall", "s", "*.txt"))
		if len(saved) == 0 {
			t.Fatal("profile a recall was not spilled")
		}
		named := false
		for _, path := range saved {
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != text {
				t.Fatalf("saved %s %d chars %v", path, len(raw), err)
			}
			named = named || strings.Contains(result, path)
		}
		if !named {
			t.Fatalf("result names no saved file %q", result)
		}
		if !strings.HasSuffix(result, "--- head ---\n"+text[:12]+"\n--- tail ---\n"+text[len(text)-7:]) {
			t.Fatalf("result tail %q", result[max(0, len(result)-80):])
		}
		if strings.Contains(result, "ranked middle") {
			t.Fatal("ranked middle reached the prompt")
		}
		writeSpillConfig(t, ctx, a, map[string]any{
			"memory": map[string]any{"prefetch_spill_enabled": false},
			"hooks":  map[string]any{"output_spill": map[string]any{"max_chars": 40, "preview_head": 12, "preview_tail": 7, "directory": filepath.Join(a, "recall"), "enabled": false}}})
	}
	ctx := home.WithOverride(context.Background(), a)
	fresh := NewMemoryManager(ctx, nil, nil)
	p := &spillRecallProvider{}
	if err := fresh.AddProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	fresh.InitializeAll(ctx, "fresh", InitOptions{})
	p.setPrefetch(text)
	if got := fresh.PrefetchAll(ctx, "recall", "fresh"); got != text {
		t.Fatalf("fresh manager recall %d chars", len(got))
	}
	if spillExists(filepath.Join(a, "recall", "fresh")) {
		t.Fatal("fresh manager spilled after the opt out")
	}
}
