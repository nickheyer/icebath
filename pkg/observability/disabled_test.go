package observability

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// disabledMarks captures feature disabled marks under a switchable collection policy
type disabledMarks struct {
	mu   sync.Mutex
	on   bool
	rows []Mark
	home string
}

// Captures marks with the catalogs the transitions read and the process surface restored afterwards
func captureDisabled(t *testing.T) *disabledMarks {
	t.Helper()
	m := &disabledMarks{on: true, home: filepath.Join(t.TempDir(), "home")}
	t.Setenv(home.EnvHome, m.home)
	savedEnabled, savedRecord, savedKeys := relayEnabled, recordProcessMark, pluginToolsetKeys
	savedSurface := rawSurface()
	t.Cleanup(func() {
		featureDisabledWG.Wait()
		relayEnabled, recordProcessMark, pluginToolsetKeys = savedEnabled, savedRecord, savedKeys
		surfaceMu.Lock()
		processSurface = savedSurface
		surfaceMu.Unlock()
		ResetCatalogCaches()
	})
	relayEnabled = func(context.Context) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.on
	}
	recordProcessMark = func(_ context.Context, mark string, data map[string]string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.rows = append(m.rows, Mark{mark, data})
	}
	pluginToolsetKeys = func(context.Context) map[string]bool { return map[string]bool{} }
	setCatalog(t, "bundled_platform_names", "telegram")
	setCatalog(t, "bundled_plugin_names", "disk-cleanup", "telegram")
	setCatalog(t, "bundled_skill_names", "arxiv")
	return m
}

// Settled feature disabled rows
func (m *disabledMarks) settled() []Mark {
	featureDisabledWG.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Mark
	for _, r := range m.rows {
		if r.Name == FeatureDisabledMark {
			out = append(out, r)
		}
	}
	return out
}

// Sets the raw process surface
func setSurface(surface string) {
	surfaceMu.Lock()
	processSurface = surface
	surfaceMu.Unlock()
}

func transitionSet(ts []Transition) map[Transition]bool {
	out := map[Transition]bool{}
	for _, t := range ts {
		out[t] = true
	}
	return out
}

func TestSettingsSkillsPluginsTransitionsOnlyCountMovesFromAndBackToDefault(t *testing.T) {
	captureDisabled(t)
	ctx := context.Background()
	old := map[string]any{"skills": map[string]any{"disabled": []any{"my-private-skill"}}, "plugins": map[string]any{"disabled": []any{}}}
	new := map[string]any{
		"memory": map[string]any{"memory_enabled": false}, "compression": map[string]any{"enabled": "false"},
		"curator": map[string]any{"enabled": true}, "display": map[string]any{"show_reasoning": false, "skin": "mono"},
		"skills":  map[string]any{"disabled": []any{"my-private-skill", "arxiv"}, "platform_disabled": map[string]any{"telegram": []any{"totally-custom"}}},
		"plugins": map[string]any{"disabled": []any{"disk-cleanup", "telegram", "some-private-plugin"}},
	}
	got := transitionSet(ConfigTransitions(ctx, old, new))
	want := transitionSet([]Transition{
		{"memory", "memory.memory_enabled", "disabled"}, {"compression", "compression.enabled", "disabled"},
		{"setting", "display.show_reasoning", "disabled"}, {"skill", "arxiv", "disabled"}, {"skill", "custom", "disabled"},
		{"plugin", "disk-cleanup", "disabled"}, {"platform", "telegram", "disabled"}, {"plugin", "custom", "disabled"},
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions %v", got)
	}
	back := transitionSet(ConfigTransitions(ctx, new, old))
	if !back[Transition{"memory", "memory.memory_enabled", "re_enabled"}] || !back[Transition{"skill", "arxiv", "re_enabled"}] {
		t.Fatalf("reverse transitions %v", back)
	}
	for _, set := range []map[Transition]bool{got, back} {
		for tr := range set {
			dims := map[string]any{"event": tr.Event, "kind": tr.Kind, "name": tr.Name, "surface": "cli_config"}
			if !CounterDimensionsAreValid(FeatureDisabledMetric, dims) {
				t.Fatalf("invalid row %v", dims)
			}
		}
	}
}

func TestToolsetTransitionsUseTheRealResolver(t *testing.T) {
	captureDisabled(t)
	ctx := context.Background()
	opts := tools.PlatformToolsOptions{PluginToolsetKeys: map[string]bool{}}
	var defaults []any
	var names []string
	for ts := range tools.Default.PlatformTools(ctx, "", config.Tree{}, "cli", opts) {
		names = append(names, ts)
	}
	sort.Strings(names)
	hasMemory := false
	for _, ts := range names {
		if ts == "memory" {
			hasMemory = true
			continue
		}
		defaults = append(defaults, ts)
	}
	if !hasMemory {
		t.Fatalf("memory missing from the cli defaults %v", names)
	}
	new := map[string]any{"platform_toolsets": map[string]any{"cli": defaults}}
	if !transitionSet(ConfigTransitions(ctx, map[string]any{}, new))[Transition{"toolset", "memory", "disabled"}] {
		t.Fatal("dropping memory is not a disable")
	}
	if !transitionSet(ConfigTransitions(ctx, new, map[string]any{}))[Transition{"toolset", "memory", "re_enabled"}] {
		t.Fatal("restoring memory is not a re-enable")
	}
	all := make([]any, 0, len(names))
	for _, n := range names {
		all = append(all, n)
	}
	if got := ConfigTransitions(ctx, map[string]any{}, map[string]any{"platform_toolsets": map[string]any{"cli": all}}); len(got) != 0 {
		t.Fatalf("saving the defaults moved %v", got)
	}
}

func TestRecordConfigSavedNeedsASurfaceAndCollection(t *testing.T) {
	m := captureDisabled(t)
	setSurface("")
	RecordConfigSaved(m.home, map[string]any{}, map[string]any{"memory": map[string]any{"memory_enabled": false}})
	if rows := m.settled(); len(rows) != 0 {
		t.Fatalf("no surface recorded %v", rows)
	}
	SetProcessSurface("config")
	m.mu.Lock()
	m.on = false
	m.mu.Unlock()
	RecordConfigSaved(m.home, map[string]any{}, map[string]any{"memory": map[string]any{"memory_enabled": false}})
	if rows := m.settled(); len(rows) != 0 {
		t.Fatalf("collection off recorded %v", rows)
	}
	m.mu.Lock()
	m.on = true
	m.mu.Unlock()
	RecordConfigSaved(m.home, map[string]any{}, map[string]any{"memory": map[string]any{"memory_enabled": false}})
	want := []Mark{{FeatureDisabledMark, map[string]string{"event": "disabled", "kind": "memory", "name": "memory.memory_enabled", "surface": "cli_config"}}}
	if rows := m.settled(); !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %v", rows)
	}
	SetProcessSurface(nil)
	if s, ok := CurrentSurface(); !ok || s != "cli_slash" {
		t.Fatalf("chat surface %q %v", s, ok)
	}
}

func TestDiffAndRecordRunOffTheCallersLockInTheOwningProfile(t *testing.T) {
	m := captureDisabled(t)
	setSurface("cli_config")
	owner := filepath.Join(t.TempDir(), "b")
	gate := make(chan struct{})
	var seen []string
	recordProcessMark = func(ctx context.Context, mark string, data map[string]string) {
		<-gate
		m.mu.Lock()
		defer m.mu.Unlock()
		seen = append(seen, home.Dir(ctx))
		m.rows = append(m.rows, Mark{mark, data})
	}
	var callerLock sync.Mutex
	callerLock.Lock()
	RecordConfigSaved(owner, map[string]any{}, map[string]any{"memory": map[string]any{"memory_enabled": false}})
	m.mu.Lock()
	early := len(m.rows)
	m.mu.Unlock()
	callerLock.Unlock()
	if early != 0 {
		t.Fatalf("diffed on the caller %d", early)
	}
	close(gate)
	rows := m.settled()
	if !reflect.DeepEqual(seen, []string{owner}) || len(rows) != 1 || rows[0].Name != FeatureDisabledMark {
		t.Fatalf("seen %v rows %v", seen, rows)
	}
}

func TestRealConfigWritesReportTheMoveAwayFromDefault(t *testing.T) {
	m := captureDisabled(t)
	ctx := context.Background()
	if err := os.MkdirAll(m.home, 0o755); err != nil {
		t.Fatal(err)
	}
	setSurface("cli_config")
	if err := config.SaveConfig(ctx, m.home, map[string]any{"memory": map[string]any{"memory_enabled": false}}, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	m.settled()
	if err := config.SetConfigValue(ctx, m.home, "curator.enabled", "false", false, disabledCommandSources()); err != nil {
		t.Fatal(err)
	}
	var got []Transition
	for _, r := range m.settled() {
		got = append(got, Transition{r.Data["kind"], r.Data["name"], r.Data["event"]})
	}
	want := []Transition{{"memory", "memory.memory_enabled", "disabled"}, {"curator", "curator.enabled", "disabled"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows %v", got)
	}
}

func TestRawConfigWritesSnapshotOnlyWithASurfaceAndCollection(t *testing.T) {
	m := captureDisabled(t)
	ctx := context.Background()
	if err := os.MkdirAll(m.home, 0o755); err != nil {
		t.Fatal(err)
	}
	set := func(ctx context.Context, value string) {
		t.Helper()
		if err := config.SetConfigValue(ctx, m.home, "curator.enabled", value, false, disabledCommandSources()); err != nil {
			t.Fatal(err)
		}
	}
	collect := func(on bool) {
		m.mu.Lock()
		m.on = on
		m.mu.Unlock()
	}
	setSurface("")
	set(ctx, "false")
	if rows := m.settled(); len(rows) != 0 {
		t.Fatalf("no surface recorded %v", rows)
	}
	set(ctx, "true")
	setSurface("cli_config")
	collect(false)
	set(ctx, "false")
	if rows := m.settled(); len(rows) != 0 {
		t.Fatalf("collection off recorded %v", rows)
	}
	set(ctx, "true")
	collect(true)
	set(config.AppliedWrite(ctx), "false")
	if rows := m.settled(); len(rows) != 0 {
		t.Fatalf("applied raw write recorded %v", rows)
	}
	set(ctx, "true")
	want := []Mark{{FeatureDisabledMark, map[string]string{"event": "re_enabled", "kind": "curator", "name": "curator.enabled", "surface": "cli_config"}}}
	if rows := m.settled(); !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %v", rows)
	}
	refused := errors.New("disk refused the write")
	err := RecordingRawConfigWrite(ctx, m.home, home.ConfigPath(m.home), map[string]any{"curator": map[string]any{"enabled": false}},
		func(string, map[string]any) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("write error surfaced as %v", err)
	}
	if rows := m.settled(); !reflect.DeepEqual(rows, want) {
		t.Fatalf("refused write recorded %v", rows)
	}
}

// Command collaborators routing upper snake keys to the env file with no stale route to drop
func disabledCommandSources() config.ConfigCommandSources {
	return config.ConfigCommandSources{
		IsEnvSettingKey: func(key string) bool {
			return !strings.Contains(key, ".") && key == strings.ToUpper(key) && !config.IsEnvConfigKey(key)
		},
		SaveEnvSetting: func(_ context.Context, h, key, value string) error {
			return config.SaveEnvValue(h, strings.ToUpper(key), value)
		},
		ReadEnvSetting: func(h, key string) (string, bool) { v, ok := config.LoadEnv(h)[strings.ToUpper(key)]; return v, ok },
		RemoveEnvSetting: func(_ context.Context, h, key string) (bool, error) {
			return config.RemoveHomeEnvValue(context.Background(), h, key)
		},
		DropStaleModelRoute: func(map[string]any, string, map[string]any) (map[string]any, bool) { return nil, false },
		RuntimeKeyOwners: config.RuntimeKeyOwners{
			IsPresetFieldPath:   func([]string) bool { return false },
			SelectionConfigKeys: func() map[string]bool { return nil },
			ActiveManifests:     func() []config.PluginConfigKeys { return nil },
		},
	}
}

func TestMigrationsAndEnvTemplatesAreNotUserDisables(t *testing.T) {
	m := captureDisabled(t)
	ctx := context.Background()
	if err := os.MkdirAll(m.home, 0o755); err != nil {
		t.Fatal(err)
	}
	setSurface("cli_config")
	if err := config.SaveConfig(config.AppliedWrite(ctx), m.home, map[string]any{"memory": map[string]any{"memory_enabled": false}}, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEM_ON", "false")
	if err := os.WriteFile(filepath.Join(m.home, "config.yaml"), []byte("compression:\n  enabled: ${MEM_ON}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveConfig(ctx, m.home, map[string]any{"compression": map[string]any{"enabled": "false"}, "display": map[string]any{"compact": true}}, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if rows := m.settled(); len(rows) != 0 {
		t.Fatalf("hermes writes recorded %v", rows)
	}
}

func TestSkillRemovalCountsOnlyCatalogSkills(t *testing.T) {
	m := captureDisabled(t)
	setSurface("")
	RecordSkillRemoved("arxiv")
	setSurface("cli_config")
	RecordSkillRemoved("my-hub-skill")
	RecordSkillRemoved("arxiv")
	want := []Mark{{FeatureDisabledMark, map[string]string{"event": "disabled", "kind": "skill", "name": "arxiv", "surface": "cli_config"}}}
	if rows := m.settled(); !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %v", rows)
	}
}

func TestBundledPluginNamesReadManifestsOneAndTwoLevelsDown(t *testing.T) {
	root := filepath.Join(pm.RepoRoot(), "plugins")
	before, _ := bundledPluginNames()
	tag := filepath.Base(t.TempDir())
	created := []string{"Obs-Top-" + tag, "obs-group-" + tag, "obs-deep-" + tag, "obs-loose-" + tag}
	t.Cleanup(func() {
		for _, dir := range created {
			os.RemoveAll(filepath.Join(root, dir))
		}
	})
	for _, rel := range []string{created[0] + "/plugin.yaml", created[1] + "/Telegram-" + tag + "/plugin.yaml",
		created[2] + "/b/c/plugin.yaml", created[3] + "/readme.md"} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("name: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	names, err := bundledPluginNames()
	want := append(append([]string{}, before...), strings.ToLower(created[0]), "telegram-"+strings.ToLower(tag))
	sort.Strings(names)
	sort.Strings(want)
	if err != nil || !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v want %v %v", names, want, err)
	}
}
