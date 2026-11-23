package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Home whose config lets notify-plugin inject, with a fresh manager and the plugin's context on it
func injectHostCase(t *testing.T) (*plugins.Context, *plugins.Manager, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "plugins:\n  entries:\n    notify-plugin:\n      allow_gateway_injection: true\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	manager := plugins.NewManager(h)
	manifest := &plugins.Manifest{Name: "notify-plugin", Key: "notify-plugin", Source: "user"}
	return plugins.NewContext(manifest, manager), manager, h
}

// Gateway slot recording every call and answering true
type recordingInjector struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (r *recordingInjector) inject(_ context.Context, kwargs map[string]any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, kwargs)
	return true
}

func queuedText(session *tuihost.Session) any {
	q, _ := session.Get("queued_prompt").(map[string]any)
	return q["text"]
}
