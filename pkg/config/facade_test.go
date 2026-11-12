package config

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/home"
)

const facadeConfig = "# hand-tuned\nmodel:\n  default: my-org/custom-model\ndisplay:\n  skin: mono\nterminal:\n  backend: docker\n"

// Fresh home with cold facade caches, the moa hooks registered and parse warnings silenced
func facadeHome(t *testing.T, text string) (string, string) {
	t.Helper()
	h := backendHome(t)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	InvalidateManagedCache()
	quietParseFailures(t)
	ResetCache()
	t.Cleanup(ResetCache)
	prev := hooks()
	RegisterConfigFacade(FacadeHooks{
		SkipDeepMerge: func(prefix, key string) bool { return prefix == "moa" && key == "presets" },
		ApplyUserMoAPresets: func(merged map[string]any, raw any) {
			rawMoA, _ := AsMap(raw)["moa"].(map[string]any)
			presets, ok := rawMoA["presets"].(map[string]any)
			if mergedMoA, has := merged["moa"].(map[string]any); ok && has {
				mergedMoA["presets"] = Clone(presets)
			}
		},
		Stamp: func() StampHooks {
			return StampHooks{
				WorkerSnapshotActive: func() bool { return WorkerConfigSnapshot() != nil },
				RecordCoreUse:        func(string, map[string]any, int) bool { return false },
				AdoptAgentBrowserEnv: func(string, map[string]any) []string { return nil },
			}
		},
	})
	t.Cleanup(func() { RegisterConfigFacade(prev) })
	path := filepath.Join(h, "config.yaml")
	if text != "" {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h, path
}

func facadeLookup(name string) (string, bool) { return os.LookupEnv(name) }

// Makes the config unreadable with an mtime outside the racy window so only the read fails
func denyConfigReads(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads files whatever their mode")
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
}

func TestMissingConfigFieldsIgnoreUserNamedPresetsButCheckSchemaOptions(t *testing.T) {
	facadeHome(t, "")
	missing := map[string]bool{}
	for _, f := range MissingConfigFields(map[string]any{"moa": map[string]any{"presets": map[string]any{"keep_a": map[string]any{}}}, "terminal": map[string]any{}}) {
		missing[f.Key] = true
		if strings.HasPrefix(f.Key, "moa.presets.") {
			t.Fatalf("named preset compared against the schema: %s", f.Key)
		}
	}
	if !missing["terminal.backend"] {
		t.Fatal("terminal.backend should be reported missing")
	}
}

func TestUserConfigOverridesDefaultAndEnvRefsExpand(t *testing.T) {
	h, _ := facadeHome(t, "display:\n  skin: ares\nmodel:\n  default: ${FACADE_MODEL}\n")
	t.Setenv("FACADE_MODEL", "org/expanded")
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.String("display.skin", "") != "ares" || cfg.String("model.default", "") != "org/expanded" {
		t.Fatalf("got skin %q model %q", cfg.String("display.skin", ""), cfg.String("model.default", ""))
	}
	t.Setenv("FACADE_MODEL", "org/rotated")
	if cfg, _ = Load(h, facadeLookup); cfg.String("model.default", "") != "org/rotated" {
		t.Fatal("a cache hit kept the expansion of a rotated env value")
	}
}

func TestLoadedMappingsKeepFileOrderAfterDefaults(t *testing.T) {
	h, _ := facadeHome(t, "moa:\n  presets:\n    zeta: {}\n    alpha: {}\n    mid: {}\nzz_custom: 1\naa_custom: 2\n")
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	presets := cfg.Map("moa.presets")
	if got := MappingKeys(presets); !reflect.DeepEqual(got, []string{"zeta", "alpha", "mid"}) {
		t.Fatalf("preset order %v", got)
	}
	root := MappingKeys(cfg)
	if root[0] != "model" || root[len(root)-2] != "zz_custom" || root[len(root)-1] != "aa_custom" {
		t.Fatalf("root order should be the defaults then the file: %v", root)
	}
	if got := MappingKeys(Clone(presets).(map[string]any)); !reflect.DeepEqual(got, []string{"zeta", "alpha", "mid"}) {
		t.Fatalf("clone lost the order %v", got)
	}
}

func TestFailedReadMarkerSurvivesDeepCopyButNotMerge(t *testing.T) {
	failed := map[string]any{"a": 1}
	markFailedRead(failed, os.ErrPermission)
	if FailedReadError(Clone(failed).(map[string]any)) == nil {
		t.Fatal("a deep copy of a failed read must stay refused")
	}
	if FailedReadError(DeepMerge(failed, map[string]any{})) != nil {
		t.Fatal("a merged copy is a plain dict")
	}
}

func TestTransientReadErrorIsNotRecordedAsACorruptConfig(t *testing.T) {
	h, path := facadeHome(t, facadeConfig)
	denyConfigReads(t, path)
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	if FailedReadError(cfg) == nil {
		t.Fatal("an unreadable config must load as a failed read")
	}
	os.Chmod(path, 0o600)
	cfg, err = Load(h, facadeLookup)
	if err != nil || cfg.String("display.skin", "") != "mono" || FailedReadError(cfg) != nil {
		t.Fatalf("readable again: %v %v", cfg.String("display.skin", ""), err)
	}
	if _, active := ActiveConfigParseFailure(h); active {
		t.Fatal("the transient failure stayed recorded")
	}
	if matches, _ := filepath.Glob(filepath.Join(h, "backups", "*", "*corrupt*")); len(matches) > 0 {
		t.Fatalf("an OS error copied the good file away: %v", matches)
	}
}

func TestUnreadableConfigServesOneCachedFallbackUntilItReads(t *testing.T) {
	h, path := facadeHome(t, facadeConfig)
	if _, err := Load(h, facadeLookup); err != nil {
		t.Fatal(err)
	}
	ResetCache()
	rebuilds := 0
	prev := loadNewestGoodBackupFn
	loadNewestGoodBackupFn = func(ctx context.Context, p string) (map[string]any, error) { rebuilds++; return prev(ctx, p) }
	t.Cleanup(func() { loadNewestGoodBackupFn = prev })
	denyConfigReads(t, path)
	for range 5 {
		cfg, err := Load(h, facadeLookup)
		if err != nil || FailedReadError(cfg) == nil || cfg.String("display.skin", "") != "mono" {
			t.Fatalf("fallback %v %v", cfg.String("display.skin", ""), err)
		}
	}
	if rebuilds != 1 {
		t.Fatalf("backup parsed %d times", rebuilds)
	}
	os.Chmod(path, 0o600)
	if cfg, err := Load(h, facadeLookup); err != nil || FailedReadError(cfg) != nil {
		t.Fatalf("a readable file must load as plain data: %v", err)
	}
}

func TestAFreshConfigTurnedUnreadableKeepsTheLastGoodSettings(t *testing.T) {
	h, path := facadeHome(t, facadeConfig)
	if _, err := Load(h, facadeLookup); err != nil {
		t.Fatal(err)
	}
	denyConfigReads(t, path)
	cfg, err := Load(h, facadeLookup)
	if err != nil || cfg.String("terminal.backend", "") != "docker" || FailedReadError(cfg) == nil {
		t.Fatalf("last good settings not served: %v %v", cfg.String("terminal.backend", ""), err)
	}
}

func TestSaveRefusalForBadYAMLAsksForAnEditNotARetry(t *testing.T) {
	h, path := facadeHome(t, "model: [unclosed\n")
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Map("display")["skin"] = "ares"
	err = SaveConfig(context.Background(), h, cfg, SaveOptions{})
	if err == nil || !strings.Contains(err.Error(), "has a formatting error") || !strings.Contains(err.Error(), "icebath config edit") || strings.Contains(err.Error(), "Try again") {
		t.Fatalf("refusal %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "model: [unclosed\n" {
		t.Fatalf("file changed: %q", data)
	}
}

func TestAuthoredNullsSurviveConfigWrites(t *testing.T) {
	for _, op := range []string{"save", "partial_save"} {
		t.Run(op, func(t *testing.T) {
			seed := "_config_version: 52\nprompt_caching:\n  cache_ttl: null\nruntime:\n  nofile_soft_limit: null\nx_null_preservation:\n  keep: value\n  nested:\n    optional: null\n"
			h, path := facadeHome(t, seed)
			ctx := context.Background()
			if op == "partial_save" {
				if err := SaveConfig(ctx, h, map[string]any{"display": map[string]any{"skin": "mono"}}, SaveOptions{MergeExisting: true}); err != nil {
					t.Fatal(err)
				}
			} else {
				cfg, err := Load(h, facadeLookup)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Map("display")["skin"] = "mono"
				if err := SaveConfig(ctx, h, cfg, SaveOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			data, _ := os.ReadFile(path)
			var raw map[string]any
			if err := yaml.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			rt := Tree(raw)
			if v, ok := rt.Lookup("runtime.nofile_soft_limit"); !ok || v != nil {
				t.Fatalf("runtime null lost: %s", data)
			}
			if v, ok := rt.Lookup("prompt_caching.cache_ttl"); !ok || v != nil {
				t.Fatalf("cache_ttl null lost: %s", data)
			}
			if v, ok := rt.Lookup("x_null_preservation.nested.optional"); !ok || v != nil || rt.String("x_null_preservation.keep", "") != "value" {
				t.Fatalf("nested nulls lost: %s", data)
			}
			if _, has := raw["terminal"]; has {
				t.Fatalf("terminal defaults written: %s", data)
			}
			if _, has := raw["agent"]; has {
				t.Fatalf("an agent section the user never wrote: %s", data)
			}
			if rt.String("display.skin", "") != "mono" {
				t.Fatalf("skin not saved: %s", data)
			}
		})
	}
}

func TestSameSizeRewriteInOneTickReachesEveryConfigReader(t *testing.T) {
	h, path := facadeHome(t, "display:\n  skin: aaaa\n")
	st, _ := os.Stat(path)
	if cfg, _ := Load(h, facadeLookup); cfg.String("display.skin", "") != "aaaa" {
		t.Fatal("first load")
	}
	if raw, _ := ReadRawConfig(context.Background(), h); Tree(raw).String("display.skin", "") != "aaaa" {
		t.Fatal("first raw read")
	}
	if err := os.WriteFile(path, []byte("display:\n  skin: bbbb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, st.ModTime(), st.ModTime())
	if cfg, _ := Load(h, facadeLookup); cfg.String("display.skin", "") != "bbbb" {
		t.Fatal("load served the stale parse")
	}
	if raw, _ := ReadRawConfig(context.Background(), h); Tree(raw).String("display.skin", "") != "bbbb" {
		t.Fatal("raw read served the stale parse")
	}
}

func TestSaveConfigStripsManagedKeysAndPreservesEnvTemplates(t *testing.T) {
	h, path := facadeHome(t, "model:\n  default: org/m\nauxiliary:\n  vision:\n    api_key: ${FACADE_KEY}\n")
	t.Setenv("FACADE_KEY", "sk-secret")
	cfg, err := Load(h, facadeLookup)
	if err != nil || cfg.String("auxiliary.vision.api_key", "") != "sk-secret" {
		t.Fatalf("expansion %v", err)
	}
	cfg.Map("display")["skin"] = "ares"
	if err := SaveConfig(context.Background(), h, cfg, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "sk-secret") || !strings.Contains(string(data), "${FACADE_KEY}") {
		t.Fatalf("plaintext secret written back: %s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 && !home.ContainerOrChmodSkipped() {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

func TestOmittedPathsRefuseAtomicConfigWrite(t *testing.T) {
	h, path := facadeHome(t, "plugins:\n  a: 1\n  b: 2\n")
	err := AtomicConfigWrite(context.Background(), h, path, map[string]any{"plugins": map[string]any{"a": 1}}, "")
	if err == nil || !strings.Contains(err.Error(), "omitted config paths: plugins.b") {
		t.Fatalf("refusal %v", err)
	}
	if err := AtomicConfigReplace(context.Background(), h, path, map[string]any{"plugins": map[string]any{"a": 1}}, ""); err != nil {
		t.Fatal(err)
	}
}

func TestValidateLoadedConfigStructureReportsStorageFailures(t *testing.T) {
	err := &HomeInitializationError{Message: "Cannot initialize Icebath directory /x: boom"}
	if issue := ConfigLoadIssue(err); !strings.HasPrefix(issue.Message, "Icebath storage is unavailable: ") {
		t.Fatalf("issue %+v", issue)
	}
	if issue := ConfigLoadIssue(io.ErrUnexpectedEOF); issue.Message != "Could not load config.yaml" {
		t.Fatalf("issue %+v", issue)
	}
	h, _ := facadeHome(t, "custom_providers: nope\n")
	if issues := ValidateLoadedConfigStructure(context.Background(), h, facadeLookup, nil); len(issues) == 0 {
		t.Fatal("structure issues of the loaded config were not reported")
	}
}

func TestReloadKeepsKnownKeysWhenUserEnvIsUnreadable(t *testing.T) {
	h, _ := facadeHome(t, "")
	env := filepath.Join(h, ".env")
	os.WriteFile(env, []byte("OPENAI_API_KEY=from-file\n"), 0o600)
	t.Setenv("OPENAI_API_KEY", "from-launcher")
	prev := hooks()
	unreadable := prev
	unreadable.DotenvUnreadable = func(p string) bool { return p == env }
	RegisterConfigFacade(unreadable)
	os.Chmod(env, 0)
	t.Cleanup(func() { os.Chmod(env, 0o600) })
	if os.Geteuid() == 0 {
		t.Skip("root reads files whatever their mode")
	}
	if n, err := ReloadEnv(h); n != 0 || err != nil || os.Getenv("OPENAI_API_KEY") != "from-launcher" {
		t.Fatalf("changed %d %v %q", n, err, os.Getenv("OPENAI_API_KEY"))
	}
}

func TestReloadStillRemovesKeysAReadableEnvDropped(t *testing.T) {
	h, _ := facadeHome(t, "")
	os.WriteFile(filepath.Join(h, ".env"), []byte("ANTHROPIC_API_KEY=from-file\n"), 0o600)
	for _, name := range OptionalEnvVars.Names() {
		if _, set := os.LookupEnv(name); set {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	prev := hooks()
	withExtra := prev
	withExtra.ExtraEnvKeys = func() []string {
		return []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN"}
	}
	RegisterConfigFacade(withExtra)
	t.Cleanup(func() { RegisterConfigFacade(prev) })
	t.Setenv("OPENAI_API_KEY", "stale")
	t.Setenv("ANTHROPIC_API_KEY", "stale")
	if n, err := ReloadEnv(h); n != 2 || err != nil {
		t.Fatalf("changed %d %v", n, err)
	}
	if _, ok := os.LookupEnv("OPENAI_API_KEY"); ok || os.Getenv("ANTHROPIC_API_KEY") != "from-file" {
		t.Fatal("reload did not converge on the file")
	}
}

func TestLoadEnvPreservesConcatenatedTextAsValueData(t *testing.T) {
	h, _ := facadeHome(t, "")
	os.WriteFile(filepath.Join(h, ".env"), []byte("TELEGRAM_BOT_TOKEN=0123456789:testANTHROPIC_API_KEY=sk-ant-test123\n"), 0o600)
	env := LoadEnv(h)
	if env["TELEGRAM_BOT_TOKEN"] != "0123456789:testANTHROPIC_API_KEY=sk-ant-test123" {
		t.Fatalf("token %q", env["TELEGRAM_BOT_TOKEN"])
	}
	if _, split := env["ANTHROPIC_API_KEY"]; split {
		t.Fatal("an assignment was inferred inside one line")
	}
}

func TestApplyTerminalConfigToEnvBridgesExplicitKeysOnly(t *testing.T) {
	h, _ := facadeHome(t, "terminal:\n  backend: ssh\n  cwd: ~/work\n  docker_volumes: [\"/a:/b\"]\n")
	env := map[string]string{"TERMINAL_ENV": "docker", "TERMINAL_TIMEOUT": "5"}
	if err := ApplyTerminalConfigToEnv(context.Background(), h, TerminalEnvOptions{Env: env}); err != nil {
		t.Fatal(err)
	}
	if env["TERMINAL_ENV"] != "ssh" || env["TERMINAL_CWD"] != "~/work" || env["TERMINAL_DOCKER_VOLUMES"] != `["/a:/b"]` {
		t.Fatalf("bridge %v", env)
	}
	if env["TERMINAL_TIMEOUT"] != "5" || env["TERMINAL_DOCKER_IMAGE_PINNED"] != "0" {
		t.Fatalf("defaults must only backfill %v", env)
	}
}
