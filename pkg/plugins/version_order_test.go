package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Installed git plugin with a saved feed url in the install metadata
func installedFeedPlugin(t *testing.T, plugins string) (string, string, string) {
	t.Helper()
	plugin := filepath.Join(plugins, "feed-plugin")
	if err := os.MkdirAll(filepath.Join(plugin, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	feedURL := "https://example.invalid/plugin.yml"
	metadata := filepath.Join(plugins, ".install-metadata.json")
	raw, _ := json.Marshal(map[string]any{"feed-plugin": map[string]any{
		"source": "https://example.invalid/plugin.git", "revision": strings.Repeat("a", 40),
		"update_url": feedURL, "pinned": false,
	}})
	if err := os.WriteFile(metadata, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return plugin, metadata, feedURL
}

func writeFeedManifest(t *testing.T, plugin, version, feedURL string) string {
	t.Helper()
	manifest := filepath.Join(plugin, "plugin.yaml")
	text, err := config.SafeDump(map[string]any{"name": filepath.Base(plugin), "version": version, "update_url": feedURL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, text, 0o644); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func boolOrNil(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func TestCadenceNeverAppliesAnUnparseableVersion(t *testing.T) {
	ctx, h, _ := cadenceHome(t)
	t.Setenv(pm.EnvRuntimeDir, filepath.Join(filepath.Dir(h), "tools"))
	plugins := filepath.Join(h, "plugins")
	plugin, metadata, feedURL := installedFeedPlugin(t, plugins)
	writeFeedManifest(t, plugin, "1.0", feedURL)
	before, _ := os.ReadFile(metadata)
	var applied []string
	results, ran := RunScheduledCheck(ctx, ScheduledCheckOptions{
		PluginsDir: plugins,
		RunChecks: func(ctx context.Context, dir string) ([]*CheckResult, error) {
			return RunChecks(ctx, dir, RunChecksOptions{
				Fetch: func(context.Context, string) (string, error) { return "version: \"not-a-version\"\n", nil },
				LsRemote: func(context.Context, string) (string, error) {
					t.Fatal("feed version failure is not a Git update")
					return "", nil
				}}), nil
		},
		ConfigGet: func(section, key string) (any, error) {
			if key == "auto_apply" {
				return true, nil
			}
			return 24, nil
		},
		ApplyUpdate: func(_ context.Context, name string) error { applied = append(applied, name); return nil },
	})
	if !ran || len(results) != 1 || results[0].UpdateAvailable != nil || !strings.Contains(results[0].Reason, "cannot compare") {
		t.Fatalf("%v %+v", ran, results)
	}
	if len(applied) != 0 {
		t.Fatalf("applied %v", applied)
	}
	if got := pm.LatestReceipt(ctx)["outcome"]; got != "ok" {
		t.Fatalf("outcome %v", got)
	}
	if now, _ := os.ReadFile(metadata); !bytes.Equal(now, before) {
		t.Fatal("metadata changed")
	}
}
