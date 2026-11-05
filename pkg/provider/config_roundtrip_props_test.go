package provider

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/roundtrip"
	"github.com/nickheyer/icebath/pkg/plugins"
)

var propSchema = sync.OnceValue(func() *roundtrip.Schema {
	return roundtrip.NewSchema(map[string]any(config.Defaults()), config.MappingKeys, config.FastSafeLoad)
})

// Isolated CLI home holding the case text and its env references, nothing managed
func propCLIHome(t *testing.T, c *roundtrip.Case) (string, string) {
	t.Helper()
	h, _, _ := moaKeysHome(t, map[string]any{})
	t.Setenv(config.EnvManagedDir, filepath.Join(filepath.Dir(h), "no-managed"))
	config.InvalidateManagedCache()
	for k, v := range c.Env {
		t.Setenv(k, v)
	}
	cfg := filepath.Join(h, "config.yaml")
	writeLeftCoreFile(t, cfg, c.Text)
	config.ResetCache()
	return h, cfg
}

// Runs icebath config set through the command entry with the production collaborators
func propConfigSet(h, key, value string) (int, error) {
	src := moaCommandSources()
	src.Language = LanguageCatalog()
	src.RuntimeKeyOwners.ActiveManifests = func() []config.PluginConfigKeys { return plugins.ActiveConfigKeyManifests(h) }
	err := config.ConfigCommand(context.Background(), h, config.ConfigArgs{Command: "set", Key: key, Value: &value}, src)
	if exit, ok := err.(*config.ConfigExit); ok {
		return exit.Code, err
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func propRead(t *testing.T, path string) (string, any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.FastSafeLoad(data)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), doc
}

func propDottedProvider(c *roundtrip.Case) string {
	for k := range c.Tree["providers"].(map[string]any) {
		return k
	}
	return ""
}

func TestP2RealCLIConfigSetChangesExactlyOneKey(t *testing.T) {
	cases := []struct {
		id    string
		seed  int64
		key   func(*roundtrip.Case) string
		value string
		want  any
	}{
		{"documented-existing", 401, func(c *roundtrip.Case) string {
			for _, p := range c.LeafOrder {
				if p[0] != "providers" && p[0] != "_config_version" {
					return roundtrip.CLIKey(p)
				}
			}
			return ""
		}, "c18-cli-value", "c18-cli-value"},
		{"documented-absent-float", 402, func(*roundtrip.Case) string { return "compression.threshold" }, "0.61", 0.61},
		{"documented-absent-bool", 408, func(*roundtrip.Case) string { return "compression.enabled" }, "false", false},
		{"dotted-provider-name", 403, func(c *roundtrip.Case) string { return "providers." + propDottedProvider(c) + ".api_mode" }, "codex_responses", "codex_responses"},
		{"yaml11-word-stays-a-string", 404, func(*roundtrip.Case) string { return "approvals.mode" }, "off", "off"},
		{"unicode-value", 405, func(*roundtrip.Case) string { return "agent.system_prompt" }, "Réponds en français — 日本語 😀", "Réponds en français — 日本語 😀"},
		{"custom-top-level-key", 406, func(*roundtrip.Case) string { return "c18_custom_root" }, "changed ü", "changed ü"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			c, err := propSchema().GenCase(tc.seed, roundtrip.GenOptions{ExcludeTop: []string{"compression"}})
			if err != nil {
				t.Fatal(err)
			}
			h, cfg := propCLIHome(t, c)
			key := tc.key(c)
			path := config.SplitPath(key)
			if tc.id == "dotted-provider-name" {
				path = []string{"providers", propDottedProvider(c), "api_mode"}
			}
			if code, err := propConfigSet(h, key, tc.value); code != 0 {
				t.Fatalf("[%s seed=%d] config set %s %s exit %d: %v", tc.id, tc.seed, key, tc.value, code, err)
			}
			after, got := propRead(t, cfg)
			changed := roundtrip.ChangedPaths(c.Tree, got)
			if len(changed) != 1 || !changed[roundtrip.Key(path...)] {
				t.Fatalf("[%s seed=%d] changed %v:\n%s", tc.id, tc.seed, roundtrip.Paths(changed), roundtrip.UDiff(c.Text, after))
			}
			if v := roundtrip.Get(got, path); !roundtrip.Same(v, tc.want) {
				t.Fatalf("[%s] stored %#v, want %#v", tc.id, v, tc.want)
			}
			if err := roundtrip.OneKeyWrite(fmt.Sprintf("%s seed=%d", tc.id, tc.seed), c.Text, after, [][]string{path}, c.LinesOf(path), !c.HasLine(path)); err != nil {
				t.Fatal(err)
			}
			keys := config.MappingKeys(got.(map[string]any)["providers"].(map[string]any))
			if !slices.Equal(keys, []string{propDottedProvider(c)}) {
				t.Fatalf("[%s] providers keys: %v", tc.id, keys)
			}
		})
	}
}

func TestP2RealCLIEnvSettingChangesExactlyOneEnvLineAndNoConfigBytes(t *testing.T) {
	c, err := propSchema().GenCase(407, roundtrip.GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h, cfg := propCLIHome(t, c)
	t.Setenv("TELEGRAM_HOME_CHANNEL", "")
	os.Unsetenv("TELEGRAM_HOME_CHANNEL")
	envFile := filepath.Join(h, ".env")
	before := "# hand-written\nC18_KEEP=one\nexport C18_OTHER=\"two # not comment\"\nTELEGRAM_HOME_CHANNEL=5\n"
	writeLeftCoreFile(t, envFile, before)
	if code, err := propConfigSet(h, "TELEGRAM_HOME_CHANNEL", "4242"); code != 0 {
		t.Fatalf("exit %d: %v", code, err)
	}
	if text, _ := propRead(t, cfg); text != c.Text {
		t.Fatalf("an env-routed config set rewrote config.yaml:\n%s", roundtrip.UDiff(c.Text, text))
	}
	afterEnv, _ := os.ReadFile(envFile)
	touched, inserted := roundtrip.LineDiff(before, string(afterEnv))
	if len(touched) != 1 || !touched[3] || roundtrip.InsertedLines(inserted) != 1 {
		t.Fatal(roundtrip.UDiff(before, string(afterEnv)))
	}
	if !slices.Contains(roundtrip.Lines(string(afterEnv)), "TELEGRAM_HOME_CHANNEL=4242") {
		t.Fatalf(".env:\n%s", afterEnv)
	}
}

func TestP2EnvLockRefusalIsNotReportedAsSuccess(t *testing.T) {
	for _, key := range []string{"DEEPSEEK_API_KEY", "TELEGRAM_HOME_CHANNEL"} {
		t.Run(key, func(t *testing.T) {
			h, _, _ := moaKeysHome(t, map[string]any{})
			managed := filepath.Join(filepath.Dir(h), "managed")
			writeLeftCoreFile(t, filepath.Join(managed, ".env"), "DEEPSEEK_API_KEY=sk-admin\nTELEGRAM_HOME_CHANNEL=111\n")
			t.Setenv(config.EnvManagedDir, managed)
			config.InvalidateManagedCache()
			t.Cleanup(config.InvalidateManagedCache)
			for _, k := range []string{"DEEPSEEK_API_KEY", "TELEGRAM_HOME_CHANNEL"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			envPath, cfgPath := filepath.Join(h, ".env"), filepath.Join(h, "config.yaml")
			envBefore := "DEEPSEEK_API_KEY=sk-old\nTELEGRAM_HOME_CHANNEL=5\n"
			cfgText := "model:\n  default: deepseek-chat\n  api_key: sk-old\n"
			writeLeftCoreFile(t, envPath, envBefore)
			writeLeftCoreFile(t, cfgPath, cfgText)
			config.ResetCache()
			code, err := propConfigSet(h, key, "c18-new")
			envAfter, _ := os.ReadFile(envPath)
			wrote := string(envAfter) != envBefore
			if (code == 0) != wrote {
				t.Fatalf("config set %s exit=%d (%v) but .env changed=%v:\n%s", key, code, err, wrote, envAfter)
			}
			if !wrote {
				if text, _ := propRead(t, cfgPath); text != cfgText {
					t.Fatalf("a refused env write still rewrote config.yaml:\n%s", text)
				}
			}
		})
	}
}

func TestP5MigrationIsIdempotentAndKeepsUserValues(t *testing.T) {
	for _, version := range roundtrip.MigrationStarts(config.MigrationVersions(), config.SupportFloorVersion) {
		name, seed := roundtrip.MigrationStartName(version)
		t.Run(name, func(t *testing.T) {
			h, _, _ := moaKeysHome(t, map[string]any{})
			t.Setenv(config.EnvManagedDir, filepath.Join(filepath.Dir(h), "no-managed"))
			config.InvalidateManagedCache()
			cfg, envPath := filepath.Join(h, "config.yaml"), filepath.Join(h, ".env")
			if err := roundtrip.MigrationProperty(propMigrationHome(cfg, envPath, func() error {
				config.ResetCache()
				_, err := config.MigrateConfig(context.Background(), h, false, true)
				return err
			}), version, seed, propSchema().Latest); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Migration home over real files with caches dropped before every migrate
func propMigrationHome(cfg, envPath string, migrate func() error) roundtrip.MigrationHome {
	read := func(path string) func() (string, error) {
		return func() (string, error) { data, err := os.ReadFile(path); return string(data), err }
	}
	write := func(path string) func(string) error {
		return func(text string) error { return os.WriteFile(path, []byte(text), 0o600) }
	}
	return roundtrip.MigrationHome{
		WriteConfig: write(cfg), WriteEnv: write(envPath), Migrate: migrate,
		ReadConfig: read(cfg), ReadEnv: read(envPath), Load: config.FastSafeLoad,
	}
}

func TestP4LoadIcebathDotenvIsIdempotent(t *testing.T) {
	for _, seed := range []int64{801, 802, 803, 804, 805, 806} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			_, h := selfReferenceHome(t)
			text, keys, shell := roundtrip.GenDotenv(seed)
			envLoaderUnset(t, slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return k == "PATH" })...)
			for k, v := range shell {
				t.Setenv(k, v)
			}
			writeEnvBytes(t, filepath.Join(h, ".env"), []byte(text))
			snapshot := func() map[string]string {
				out := map[string]string{}
				for _, k := range keys {
					if v, ok := os.LookupEnv(k); ok {
						out[k] = v
					}
				}
				return out
			}
			var snaps []map[string]string
			for range 4 {
				mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
				snaps = append(snaps, snapshot())
			}
			for i, s := range snaps[1:] {
				if !maps.Equal(s, snaps[0]) {
					t.Fatalf("[seed=%d] env drifted on reload %d:\n%v\n%v", seed, i+1, snaps[0], s)
				}
			}
			first, p := snaps[0], fmt.Sprintf("C18_%d_", seed)
			if strings.Count(first["PATH"], fmt.Sprintf("/c18/%d/bin", seed)) != 1 || !strings.HasSuffix(first["PATH"], roundtrip.BasePath) {
				t.Fatalf("[seed=%d] PATH=%s", seed, first["PATH"])
			}
			if first[p+"SELF"] != "pre-shellval" {
				t.Fatalf("[seed=%d] self-ref: %s", seed, first[p+"SELF"])
			}
			if first[p+"D"] != first[p+"A"]+"-chained" {
				t.Fatalf("[seed=%d] chain %q from %q", seed, first[p+"D"], first[p+"A"])
			}
		})
	}
}
