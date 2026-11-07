package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

func TestPublishAssumeConsentSkipsReplacementVeto(t *testing.T) {
	tmp := t.TempDir()
	staged, target := filepath.Join(tmp, "staged"), filepath.Join(tmp, "target")
	for _, d := range []string{staged, target} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(staged, "plugin.yaml"), []byte("name: active-plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pyproject := "[project]\nname=\"active-plugin\"\nversion=\"1.0\"\nrequires-python=\">=3.11\"\n" +
		"dependencies=[\"never-install-without-consent==1\"]\n"
	if err := os.WriteFile(filepath.Join(staged, "pyproject.toml"), []byte(pyproject), 0o644); err != nil {
		t.Fatal(err)
	}
	prevDirs, prevSync, prevTTY := enabledPluginDirs, syncVenv, isTTY
	t.Cleanup(func() { enabledPluginDirs, syncVenv, isTTY = prevDirs, prevSync, prevTTY })
	enabledPluginDirs = func(context.Context, pm.EnabledPluginsOptions) ([]string, error) { return []string{target}, nil }
	var syncs []pm.SyncVenvOptions
	syncVenv = func(_ context.Context, opts pm.SyncVenvOptions) error {
		syncs = append(syncs, opts)
		return nil
	}
	isTTY = func() bool { return false }
	ctx := context.Background()
	newMeta := map[string]any{"active-plugin": map[string]any{}}

	if err := publishPlugin(ctx, staged, target, map[string]any{}, newMeta, publishOptions{RequireConsent: true, AssumeConsent: true}); err != nil {
		t.Fatalf("assumed consent publication: %v", err)
	}
	if len(syncs) != 1 {
		t.Fatalf("syncs = %d, want 1", len(syncs))
	}

	err := publishPlugin(ctx, staged, target, map[string]any{}, newMeta, publishOptions{RequireConsent: true})
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) || !strings.Contains(opErr.Error(), "Reinstall declined") {
		t.Fatalf("default consent publication = %v, want the Reinstall declined refusal", err)
	}
	if len(syncs) != 1 {
		t.Fatalf("a refused publication reached PM: syncs = %d", len(syncs))
	}
}
