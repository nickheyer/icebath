package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestFeedAndPipChecksUseTheSameVersionOrder(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		current, offered string
		expected         *bool
	}{
		{"2.0", "1.0", &no}, {"1.0", "1.0.0", &no}, {"1.0rc1", "1.0", &yes}, {"1.0", "1.0rc1", &no},
		{"1.0", "2.0", &yes}, {"1.0", "garbage!", nil}, {"garbage!", "1.0", nil},
	} {
		plugins := filepath.Join(t.TempDir(), "plugins")
		plugin, metadata, feedURL := installedFeedPlugin(t, plugins)
		manifest := writeFeedManifest(t, plugin, tc.current, feedURL)
		before := map[string][]byte{}
		for _, f := range []string{manifest, metadata} {
			before[f], _ = os.ReadFile(f)
		}
		var fetched []string
		offered := tc.offered
		current := tc.current
		results := RunChecks(context.Background(), plugins, RunChecksOptions{
			Fetch: func(_ context.Context, url string) (string, error) {
				fetched = append(fetched, url)
				raw, err := config.SafeDump(map[string]any{"version": offered})
				return string(raw), err
			},
			LsRemote: func(context.Context, string) (string, error) {
				t.Fatal("a saved feed must not fall back to Git")
				return "", nil
			},
			PipEntryPoints:      []PipEntryPoint{{Name: "pip-plugin", DistName: "plugin-dist"}},
			PipInstalledVersion: func(string) (string, bool) { return current, true },
			PipPypiLatest:       func(context.Context, string) (string, bool) { return offered, true },
		})
		if !reflect.DeepEqual(fetched, []string{feedURL}) {
			t.Fatalf("%s->%s fetched %v", tc.current, tc.offered, fetched)
		}
		names := map[string]bool{}
		for _, r := range results {
			names[r.Name] = true
			row := r.ToJSON()
			if row["current"] != tc.current || row["latest"] != tc.offered {
				t.Fatalf("%s->%s row %v", tc.current, tc.offered, row)
			}
			if !reflect.DeepEqual(row["update_available"], boolOrNil(tc.expected)) {
				t.Fatalf("%s->%s %s update_available %v", tc.current, tc.offered, r.Name, row["update_available"])
			}
			reason, _ := row["reason"].(string)
			if strings.Contains(reason, "cannot compare") != (tc.expected == nil) {
				t.Fatalf("%s->%s %s reason %q", tc.current, tc.offered, r.Name, reason)
			}
		}
		if !reflect.DeepEqual(names, map[string]bool{"feed-plugin": true, "pip-plugin": true}) {
			t.Fatalf("names %v", names)
		}
		for f, b := range before {
			if now, _ := os.ReadFile(f); !bytes.Equal(now, b) {
				t.Fatalf("%s changed", f)
			}
		}
	}
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
			return RunChecks(ctx, dir, RunChecksOptions{SkipPip: true,
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
