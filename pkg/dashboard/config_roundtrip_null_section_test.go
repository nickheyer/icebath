package dashboard

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/roundtrip"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Writes config text on a new inode and drops every config cache
func writeNullSectionConfig(t *testing.T, path, text string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

func readNullSectionConfig(t *testing.T, path string) (string, any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := config.SafeLoad(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), tree
}

func TestP6NullSectionSurvivesEverySurface(t *testing.T) {
	schema := roundtrip.NewSchema(map[string]any(config.Defaults()), config.MappingKeys, config.FastSafeLoad)
	defaults := map[string]any(config.Defaults())
	for idx, section := range schema.TopLevel {
		t.Run(section, func(t *testing.T) {
			c := newTestClient(t, dashboardTestApp(t))
			h := home.Root()
			t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
			config.InvalidateManagedCache()
			prevOut, prevNotice := config.FacadeStdout, config.FacadeNoticeOut
			config.FacadeStdout, config.FacadeNoticeOut = io.Discard, io.Discard
			t.Cleanup(func() { config.FacadeStdout, config.FacadeNoticeOut = prevOut, prevNotice; config.ResetCache() })
			gen, err := schema.GenCase(int64(1000+idx), roundtrip.GenOptions{NSections: [2]int{3, 5}, NoNullLeaves: true, ExcludeTop: []string{section, "display", "agent"}})
			if err != nil {
				t.Fatal(err)
			}
			text, err := roundtrip.BeforeVersion(gen.Text, section+": null\n")
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range gen.Env {
				t.Setenv(k, v)
			}
			cfg := home.ConfigPath(h)
			writeNullSectionConfig(t, cfg, text)
			tree, err := config.SafeLoad(text)
			if err != nil {
				t.Fatal(err)
			}
			nullLine := slices.Index(roundtrip.Lines(text), section+": null")
			ctxName := section + ": null"
			ctx := context.Background()

			effective, err := config.Load(h, config.EnvRefLookup)
			if err != nil {
				t.Fatalf("[%s] load_config: %v", ctxName, err)
			}
			if _, isMap := defaults[section].(map[string]any); isMap {
				if _, ok := effective[section].(map[string]any); !ok {
					t.Fatalf("[%s] load_config()[%q] = %#v", ctxName, section, effective[section])
				}
			}
			if _, err := config.LoadUserEffective(cfg, config.EnvRefLookup); err != nil {
				t.Fatalf("[%s] load_user_config_effective: %v", ctxName, err)
			}
			raw, err := config.ReadUserConfigRaw(ctx, cfg)
			if err != nil {
				t.Fatalf("[%s] read_user_config_raw: %v", ctxName, err)
			}
			for _, platform := range []string{"telegram", "discord", "cli"} {
				config.ResolveDisplaySetting(ctx, raw, platform, "tool_progress", nil)
				config.ResolveToolProgress(ctx, raw, platform, "")
			}

			loaded, err := config.Load(h, config.EnvRefLookup)
			if err != nil {
				t.Fatal(err)
			}
			if err := config.SaveConfig(ctx, h, map[string]any(loaded), config.SaveOptions{}); err != nil {
				t.Fatalf("[%s] save_config: %v", ctxName, err)
			}
			after, _ := readNullSectionConfig(t, cfg)
			touched, inserted := roundtrip.LineDiff(text, after)
			delete(touched, nullLine)
			if len(touched) > 0 || len(inserted) > 1 {
				t.Fatalf("[%s] save touched other lines:\n%s", ctxName, roundtrip.UDiff(text, after))
			}
			if err := roundtrip.NoDuplicateTopLevel(after, ctxName); err != nil {
				t.Fatal(err)
			}

			writers := []struct {
				name   string
				run    func(t *testing.T)
				target []string
			}{
				{"cli_set", func(t *testing.T) {
					if err := config.SetConfigValue(ctx, h, "terminal.timeout", "4242", false, config.ConfigCommandSources{}); err != nil {
						t.Fatalf("[%s] cli_set: %v", ctxName, err)
					}
				}, []string{"terminal", "timeout"}},
				{"tui_rpc", func(t *testing.T) {
					res := tuiserver.Method("config.set")(ctx, 1, map[string]any{"key": "skin", "value": "ares"})
					if _, ok := res["result"]; !ok {
						t.Fatalf("[%s] tui config.set: %v", ctxName, res)
					}
				}, []string{"display", "skin"}},
				{"dashboard_put", func(t *testing.T) {
					resp := appJSON(t, c, "PUT", "/api/config", pyjson.ObjectOf("config", pyjson.ObjectOf("display", pyjson.ObjectOf("skin", "ares"))))
					if resp.StatusCode != 200 {
						t.Fatalf("[%s] PUT /api/config: %s", ctxName, resp.Text())
					}
				}, []string{"display", "skin"}},
			}
			for _, w := range writers {
				writeNullSectionConfig(t, cfg, text)
				w.run(t)
				_, got := readNullSectionConfig(t, cfg)
				if v := roundtrip.Get(got, w.target); v == roundtrip.Missing || v == nil {
					t.Fatalf("[%s] %s did not write %v", ctxName, w.name, w.target)
				}
				for k := range roundtrip.ChangedPaths(tree, got) {
					if !roundtrip.Under(roundtrip.Split(k), [][]string{w.target, {section}}) {
						after, _ := readNullSectionConfig(t, cfg)
						t.Fatalf("[%s] %s changed untargeted path %v:\n%s", ctxName, w.name, roundtrip.Split(k), roundtrip.UDiff(text, after))
					}
				}
			}
		})
	}
}
