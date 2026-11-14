package tools

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func discoveryEnv(t *testing.T, logs *bytes.Buffer) *Env {
	cfg := config.Defaults()
	cfg.Set("web.backend", "nonexistent-backend")
	return &Env{Home: t.TempDir(), Config: cfg, Values: &sync.Map{}, Log: slog.New(slog.NewTextHandler(logs, nil))}
}

func TestEnsureWebPluginsLoadedRunsDiscoveryFirst(t *testing.T) {
	stubPublicHosts(t)
	swapDDGS(t, &ddgsClientAvailable, func(context.Context) bool { return false })
	var logs bytes.Buffer
	env := discoveryEnv(t, &logs)
	var mu sync.Mutex
	var homes []string
	prev := EnsurePluginsDiscovered
	t.Cleanup(func() { EnsurePluginsDiscovered = prev })
	EnsurePluginsDiscovered = func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		homes = append(homes, home.Dir(ctx))
		return nil
	}
	stubPublicHosts(t)
	swapDDGS(t, &ddgsClientAvailable, func(context.Context) bool { return false })
	WebSearchTool(context.Background(), env, "q", 3)
	WebExtractTool(context.Background(), env, []any{"https://example.com/page"}, "", nil)
	CheckWebAPIKey(env)
	keylessWebBackend(env)
	mu.Lock()
	defer mu.Unlock()
	if len(homes) < 4 {
		t.Fatalf("discovery ran %d times", len(homes))
	}
	for _, h := range homes {
		if h != env.Home {
			t.Fatalf("discovery home %q want %q", h, env.Home)
		}
	}
	if strings.Contains(logs.String(), "Web plugin discovery failed") {
		t.Fatalf("unexpected warning %s", logs.String())
	}
}

func TestEnsureWebPluginsLoadedFailureIsWarning(t *testing.T) {
	var logs bytes.Buffer
	env := discoveryEnv(t, &logs)
	prev := EnsurePluginsDiscovered
	t.Cleanup(func() { EnsurePluginsDiscovered = prev })
	EnsurePluginsDiscovered = func(context.Context) error { return errors.New("boom import") }
	ensureWebPluginsLoaded(context.Background(), env)
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "Web plugin discovery failed (non-fatal): boom import") {
		t.Fatalf("warning %q", out)
	}
	logs.Reset()
	EnsurePluginsDiscovered = nil
	ensureWebPluginsLoaded(context.Background(), env)
	if !strings.Contains(logs.String(), "Web plugin discovery failed (non-fatal): plugin discovery is not installed") {
		t.Fatalf("unwired warning %q", logs.String())
	}
}
