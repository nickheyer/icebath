package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

type deadlineFence struct {
	deadline float64
	set      bool
	extended []float64
}

func (f *deadlineFence) DeadlineMonotonic() (float64, bool) { return f.deadline, f.set }
func (f *deadlineFence) ExtendDeadline(seconds float64) {
	f.extended = append(f.extended, seconds)
	f.deadline += seconds
}

// Context of a fresh home whose config.yaml holds the given compression section
func compressionHome(t *testing.T, section map[string]any) context.Context {
	t.Helper()
	h := t.TempDir()
	writeCompressionSection(t, h, section)
	return home.WithOverride(context.Background(), h)
}

// Writes the compression section into the config.yaml of home h
func writeCompressionSection(t *testing.T, h string, section map[string]any) {
	t.Helper()
	doc, err := config.SafeDump(map[string]any{"compression": section})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveContextCompressionTimeoutsClampsToTheAuxBudget(t *testing.T) {
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{})
	aux := provider.EffectiveAuxTimeout(context.Background(), "compression", nil)
	defaultCeiling := settings.CompressionContextTotalCeilingSeconds.Default.Seconds()
	idle, ceiling := ResolveContextCompressionTimeouts(compressionHome(t, map[string]any{"context_timeout_seconds": "0", "context_total_ceiling_seconds": -5}))
	if idle != 0 || ceiling != defaultCeiling {
		t.Fatalf("disabled idle %v %v", idle, ceiling)
	}
	idle, ceiling = ResolveContextCompressionTimeouts(compressionHome(t, map[string]any{"context_timeout_seconds": 30, "context_total_ceiling_seconds": "bad"}))
	if ceiling != max(600, aux) || idle != max(30, min(aux, ceiling)) {
		t.Fatalf("aux clamp %v %v with aux %v", idle, ceiling, aux)
	}
	idle, ceiling = ResolveContextCompressionTimeouts(compressionHome(t, map[string]any{"context_timeout_seconds": 5000.0, "context_total_ceiling_seconds": 100}))
	if idle != 5000 || ceiling != 5000 {
		t.Fatalf("ceiling at least one idle window %v %v", idle, ceiling)
	}
	if idle, _ := ResolveContextCompressionTimeouts(compressionHome(t, map[string]any{})); idle != max(settings.CompressionContextTimeoutSeconds.Default.Seconds(), min(aux, max(600, aux))) {
		t.Fatalf("an empty section keeps the defaults %v", idle)
	}
	if idle, _ := ResolveContextCompressionTimeouts(compressionHome(t, map[string]any{"context_timeout_seconds": -1})); idle != -1 {
		t.Fatal("a negative idle window is kept")
	}
}

func TestEffectiveCeilingAndDeadlineScopeFollowTheFence(t *testing.T) {
	f := &deadlineFence{}
	if EffectiveCeiling(f, 600, 10) != 600 {
		t.Fatal("no deadline keeps the ceiling")
	}
	f.deadline, f.set = 1000, true
	if EffectiveCeiling(f, 600, 10) != 990 || EffectiveCeiling(f, 600, 900) != 600 {
		t.Fatal("pushed deadline lengthens the ceiling")
	}
	ctx := context.Background()
	if CompressionDeadlineScope(ctx, nil) != ctx {
		t.Fatal("nil fence leaves the context")
	}
	f.deadline = pythread.Monotonic() + 3600
	scoped := CompressionDeadlineScope(ctx, f)
	provider.CurrentPromptReadingHook(scoped)(12)
	if len(f.extended) != 1 || f.extended[0] != 12 {
		t.Fatal("prompt reading pushes the fence deadline back")
	}
	at, ok := provider.CurrentAuxStreamDeadline(scoped)
	if !ok || time.Until(at) < 3590*time.Second {
		t.Fatalf("live deadline %v %v", at, ok)
	}
	f.set = false
	if _, ok := provider.CurrentAuxStreamDeadline(scoped); ok {
		t.Fatal("a cleared fence deadline reads live as none")
	}
}

// Pins the aux compression request budget the resolver floors the idle window at
func pinCompressionAuxTimeout(t *testing.T, budget float64) {
	prev := compressionAuxTimeout
	compressionAuxTimeout = func(context.Context, string, *float64) float64 { return budget }
	t.Cleanup(func() { compressionAuxTimeout = prev })
}

func TestIdleIsFlooredAtTheAuxCompressionRequestBudget(t *testing.T) {
	check := func(cfg map[string]any, wantIdle, wantCeiling float64) {
		t.Helper()
		if idle, ceiling := ResolveContextCompressionTimeouts(compressionHome(t, cfg)); idle != wantIdle || ceiling != wantCeiling {
			t.Fatalf("%v resolved to (%v, %v), want (%v, %v)", cfg, idle, ceiling, wantIdle, wantCeiling)
		}
	}
	pinCompressionAuxTimeout(t, 300)
	check(map[string]any{}, 300, 600)
	check(map[string]any{"context_timeout_seconds": 900}, 900, 900)
	pinCompressionAuxTimeout(t, 900)
	check(map[string]any{}, 900, 900)
	check(map[string]any{"context_timeout_seconds": 0}, 0, 600)
}

func TestZeroIdleDisablesWrapper(t *testing.T) {
	pinCompressionAuxTimeout(t, 0)
	idle, ceiling := ResolveContextCompressionTimeouts(compressionHome(t, map[string]any{"context_timeout_seconds": 0}))
	if idle != 0 || ceiling <= 0 {
		t.Fatalf("idle %v ceiling %v", idle, ceiling)
	}
}

func TestCeilingClampedToIdle(t *testing.T) {
	pinCompressionAuxTimeout(t, 0)
	idle, ceiling := ResolveContextCompressionTimeouts(compressionHome(t, map[string]any{"context_timeout_seconds": 90, "context_total_ceiling_seconds": 30}))
	if idle != 90 || ceiling != 90 {
		t.Fatalf("idle %v ceiling %v", idle, ceiling)
	}
}
