package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

func memoryWarnHome(t *testing.T) (context.Context, string, *bytes.Buffer) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	warnedUnavailableProvidersMu.Lock()
	clear(warnedUnavailableProviders)
	warnedUnavailableProvidersMu.Unlock()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return home.WithOverride(context.Background(), h), h, &logs
}

func unavailableWarnings(logs *bytes.Buffer) []string {
	var out []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "unavailable") {
			out = append(out, line)
		}
	}
	return out
}

func TestMemoryProviderUnavailableWarnsOnceAndDedupes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, _, logs := memoryWarnHome(t)
	warnMemoryProviderUnavailable(ctx, "hindsight", "", nil)
	warnMemoryProviderUnavailable(ctx, "hindsight", "", nil)
	if w := unavailableWarnings(logs); len(w) != 1 || !strings.Contains(w[0], "hindsight") {
		t.Fatalf("should warn exactly once per provider (gateway dedup): %q", w)
	}
}

func TestMemoryProviderUnavailableDistinctProvidersEachWarn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, _, logs := memoryWarnHome(t)
	warnMemoryProviderUnavailable(ctx, "hindsight", "", nil)
	warnMemoryProviderUnavailable(ctx, "mem0", "", nil)
	if w := unavailableWarnings(logs); len(w) != 2 {
		t.Fatalf("warnings %q", w)
	}
}

func TestMemoryProviderUnavailableReasonIsAppended(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, _, logs := memoryWarnHome(t)
	hint := "Install the embedded runtime with: uv pip install hindsight-all."
	var said []string
	warnMemoryProviderUnavailable(ctx, "hindsight", hint, func(s string) { said = append(said, s) })
	if w := unavailableWarnings(logs); len(w) != 1 || len(said) != 1 || !strings.HasSuffix(said[0], " "+hint) || !strings.Contains(said[0], "'hindsight'") {
		t.Fatalf("warnings %q said %q", w, said)
	}
}

func TestParkedMemoryProviderIsReportedAndNeverReinstalled(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, h, _ := memoryWarnHome(t)
	plugin := filepath.Join(h, "plugins", "parkedmem")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(plugin, "__init__.py"), []byte("from agent.memory_provider import MemoryProvider\n"), 0o644)
	_ = os.WriteFile(filepath.Join(h, "config.yaml"), []byte("memory:\n  provider: parkedmem\nplugins:\n  disabled: [parkedmem]\n  disabled_by_update:\n"+
		"    parkedmem: {enabled: true, reason: requires Python <3.14}\n"), 0o644)
	saved := recoverMemoryProviderAtStartup
	t.Cleanup(func() { recoverMemoryProviderAtStartup = saved })
	recoverMemoryProviderAtStartup = func(context.Context, string, func(string)) (bool, error) {
		t.Fatal("a parked provider must not be re-installed")
		return false, nil
	}
	var said []string
	say := func(s string) { said = append(said, s) }
	env := &tools.Env{Home: h}
	for range 2 {
		if p, err := resolveMemoryProvider(ctx, env, "parkedmem", say); p != nil || err != nil {
			t.Fatalf("resolved %v %v", p, err)
		}
	}
	if len(said) != 1 || !strings.Contains(said[0], "icebath plugins enable parkedmem") || !strings.Contains(said[0], "requires Python <3.14") {
		t.Fatalf("said %q", said)
	}
}

func TestMemoryProviderThatStillFailsAfterRecoveryIsReported(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, h, _ := memoryWarnHome(t)
	plugin := filepath.Join(h, "plugins", "brokenmem")
	saved := recoverMemoryProviderAtStartup
	t.Cleanup(func() { recoverMemoryProviderAtStartup = saved })
	recoverMemoryProviderAtStartup = func(_ context.Context, name string, say func(string)) (bool, error) {
		if err := os.MkdirAll(plugin, 0o755); err != nil {
			return false, err
		}
		_ = os.WriteFile(filepath.Join(plugin, "__init__.py"), []byte("from agent.memory_provider import MemoryProvider\nimport icebath_missing_package_for_test\n"), 0o644)
		say("Installed memory provider '" + name + "'.")
		return true, nil
	}
	var said []string
	if p, err := resolveMemoryProvider(ctx, &tools.Env{Home: h}, "brokenmem", func(s string) { said = append(said, s) }); p != nil || err != nil {
		t.Fatalf("resolved %v %v", p, err)
	}
	if len(said) != 2 || !strings.Contains(said[1], "failed to load") {
		t.Fatalf("said %q", said)
	}
}
