package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func bridgeContext(name, key, namespace string) *Context {
	return newContext(&Manifest{Name: name, Key: key, DataNamespace: namespace}, NewManager(""))
}

func homeCtx(h string) context.Context { return home.WithOverride(context.Background(), h) }

func captureLogs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	saved := Logger
	Logger = slog.New(slog.NewTextHandler(&syncWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { Logger = saved })
	return &buf
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func mustGet(t *testing.T, c *Context, ctx context.Context, key string, def any) any {
	t.Helper()
	v, err := c.GetConfig(ctx, key, def)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestConfigRoundTripUsesCanonicalSettingsNamespace(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	ctx := homeCtx(h)
	c := bridgeContext("fixture-plugin", "category/fixture-plugin", "")
	if v := mustGet(t, c, ctx, "api_url", "unset"); v != "unset" {
		t.Fatal(v)
	}
	path := `C:\Users\Owner\Hermes 🚀`
	if err := c.SetConfig(ctx, "api_url", path); err != nil {
		t.Fatal(err)
	}
	if err := c.SetConfig(ctx, "retry.policy", map[string]any{"attempts": 3, "enabled": true}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	settings := doc["plugins"].(map[string]any)["entries"].(map[string]any)["category/fixture-plugin"].(map[string]any)["settings"]
	want := map[string]any{"api_url": path, "retry": map[string]any{"policy": map[string]any{"attempts": 3, "enabled": true}}}
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("settings %#v", settings)
	}
	if v := mustGet(t, c, ctx, "api_url", nil); v != path {
		t.Fatal(v)
	}
	if v := mustGet(t, c, ctx, "retry.policy", nil); !reflect.DeepEqual(v, map[string]any{"attempts": 3, "enabled": true}) {
		t.Fatalf("%#v", v)
	}
}

func TestConfigReadsLegacyConfigNamespaceUntilCanonicalValueIsSet(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	ctx := homeCtx(h)
	_ = os.MkdirAll(h, 0o755)
	doc := "plugins:\n  entries:\n    fixture-plugin:\n      config:\n        endpoint: legacy\n        legacy_only: 7\n      settings:\n        endpoint: canonical\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c := bridgeContext("fixture-plugin", "", "")
	if v := mustGet(t, c, ctx, "endpoint", nil); v != "canonical" {
		t.Fatal(v)
	}
	if v := mustGet(t, c, ctx, "legacy_only", nil); v != 7 {
		t.Fatal(v)
	}
	if err := c.SetConfig(ctx, "legacy_only", 8); err != nil {
		t.Fatal(err)
	}
	if v := mustGet(t, c, ctx, "legacy_only", nil); v != 8 {
		t.Fatal(v)
	}
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	var out map[string]any
	_ = yaml.Unmarshal(raw, &out)
	entry := out["plugins"].(map[string]any)["entries"].(map[string]any)["fixture-plugin"].(map[string]any)
	if entry["config"].(map[string]any)["legacy_only"] != 7 || entry["settings"].(map[string]any)["legacy_only"] != 8 {
		t.Fatalf("%#v", entry)
	}
}

func TestConfigRejectsGlobalCrossPluginAndTraversalPaths(t *testing.T) {
	for _, key := range []string{"../../security.approval_mode", `..\..\model.provider`, "security.approval_mode",
		"model.provider", "plugins.entries.other.settings.token", "settings.endpoint", ""} {
		h := filepath.Join(t.TempDir(), "profile")
		logs := captureLogs(t)
		err := bridgeContext("fixture-plugin", "", "").SetConfig(homeCtx(h), key, "blocked")
		if err == nil || !strings.Contains(err.Error(), "plugin-relative config key") {
			t.Fatalf("%q: %v", key, err)
		}
		if _, err := os.Stat(filepath.Join(h, "config.yaml")); err == nil {
			t.Fatalf("%q wrote config", key)
		}
		if !strings.Contains(logs.String(), "Rejected config path") {
			t.Fatalf("%q not logged", key)
		}
	}
}

func TestConfigReadRejectsEscapeInsteadOfExposingGlobalConfig(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	_ = os.MkdirAll(h, 0o755)
	_ = os.WriteFile(filepath.Join(h, "config.yaml"), []byte("security:\n  approval_mode: always_allow\n"), 0o644)
	_, err := bridgeContext("fixture-plugin", "", "").GetConfig(homeCtx(h), "security.approval_mode", nil)
	if err == nil || !strings.Contains(err.Error(), "plugin-relative config key") {
		t.Fatal(err)
	}
}

func TestConfigParseFailureIsNotSilentlyOverwritten(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	_ = os.MkdirAll(h, 0o755)
	broken := "plugins:\n  entries: [unterminated\n"
	path := filepath.Join(h, "config.yaml")
	_ = os.WriteFile(path, []byte(broken), 0o644)
	err := bridgeContext("fixture-plugin", "", "").SetConfig(homeCtx(h), "endpoint", "safe")
	if err == nil || !(strings.Contains(err.Error(), "while parsing") || strings.Contains(err.Error(), "expected")) {
		t.Fatalf("parse failure not raised: %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != broken {
		t.Fatalf("config rewritten: %q", raw)
	}
}

func TestConcurrentConfigWritesDoNotDropSiblingSettings(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	ctx := homeCtx(h)
	c := bridgeContext("fixture-plugin", "", "")
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- c.SetConfig(ctx, fmt.Sprintf("worker_%d", i), i)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 24; i++ {
		if v := mustGet(t, c, ctx, fmt.Sprintf("worker_%d", i), nil); v != i {
			t.Fatalf("worker_%d = %v", i, v)
		}
	}
}

const bridgeChildEnv = "ICEBATH_PLUGINS_BRIDGE_CHILD"

func TestBridgeChildProcess(t *testing.T) {
	mode := os.Getenv(bridgeChildEnv)
	if mode == "" {
		return
	}
	h := os.Getenv("ICEBATH_HOME")
	start, _ := strconv.Atoi(os.Getenv("BRIDGE_START"))
	c := bridgeContext("fixture-plugin", "", "")
	for i := start; i < start+20; i++ {
		var err error
		if mode == "config" {
			err = c.SetConfig(homeCtx(h), fmt.Sprintf("process_%d", i), i)
		} else {
			err = c.State(homeCtx(h)).Set(fmt.Sprintf("process_%d", i), i)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}

func runBridgeChildren(t *testing.T, h, mode string) {
	var cmds []*exec.Cmd
	for _, start := range []int{0, 20} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBridgeChildProcess$")
		cmd.Env = append(os.Environ(), bridgeChildEnv+"="+mode, "ICEBATH_HOME="+h, "BRIDGE_START="+strconv.Itoa(start))
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigCrossProcessLockPreservesEverySetting(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	runBridgeChildren(t, h, "config")
	c := bridgeContext("fixture-plugin", "", "")
	for i := 0; i < 40; i++ {
		if v := mustGet(t, c, homeCtx(h), fmt.Sprintf("process_%d", i), nil); v != i {
			t.Fatalf("process_%d = %v", i, v)
		}
	}
}

func readStateFile(t *testing.T, s *PluginState) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStateRoundTripIsAtomicAndAlignedWithPluginData(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	s := bridgeContext("fixture-plugin", "", "agent-plugin-fixture-a1b2c3d4").State(homeCtx(h))
	if v, err := s.Get("cursor", "start"); err != nil || v != "start" {
		t.Fatal(v, err)
	}
	value := map[string]any{"page": 2, "path": `C:\Users\Owner`}
	if err := s.Set("cursor", value); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(h, "plugin-data", "agent-plugin-fixture-a1b2c3d4", "state.json")
	if s.DataDir() != filepath.Dir(statePath) {
		t.Fatal(s.DataDir())
	}
	if got := readStateFile(t, s); !reflect.DeepEqual(got, map[string]any{"cursor": map[string]any{"page": 2.0, "path": `C:\Users\Owner`}}) {
		t.Fatalf("%#v", got)
	}
	got, _ := s.Get("cursor", nil)
	if m := got.(map[string]any); m["page"] != 2 || m["path"] != `C:\Users\Owner` {
		t.Fatalf("%#v", got)
	}
	if tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(statePath), "*.tmp")); len(tmps) != 0 {
		t.Fatal(tmps)
	}
}

func TestNativeStateNamespaceIsWindowsSafeAndCannotTraverse(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	for index, c := range []*Context{bridgeContext("CON", "", ""), bridgeContext("x", "../../other-plugin", "")} {
		s := c.State(homeCtx(h))
		if err := s.Set("value", index); err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(s.DataDir())
		if filepath.Dir(s.DataDir()) != filepath.Join(h, "plugin-data") || !strings.HasPrefix(name, "agent-plugin-") {
			t.Fatal(s.DataDir())
		}
		switch strings.ToUpper(name) {
		case "CON", "NUL", "COM1", "LPT1":
			t.Fatal(name)
		}
	}
}

func TestConcurrentStateUpdatesDoNotDropKeys(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	c := bridgeContext("fixture-plugin", "", "")
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := c.State(homeCtx(h)).Set(fmt.Sprintf("cursor_%d", i), i); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got := readStateFile(t, c.State(homeCtx(h)))
	if len(got) != 40 {
		t.Fatalf("%d keys", len(got))
	}
	for i := 0; i < 40; i++ {
		if got[fmt.Sprintf("cursor_%d", i)] != float64(i) {
			t.Fatalf("cursor_%d", i)
		}
	}
}

func TestStateCrossProcessLockPreservesEveryUpdate(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	runBridgeChildren(t, h, "state")
	got := readStateFile(t, bridgeContext("fixture-plugin", "", "").State(homeCtx(h)))
	for i := 0; i < 40; i++ {
		if got[fmt.Sprintf("process_%d", i)] != float64(i) {
			t.Fatalf("process_%d missing in %v", i, got)
		}
	}
}

func TestStateQuotaFailurePreservesPreviousFile(t *testing.T) {
	s := bridgeContext("fixture-plugin", "", "").State(homeCtx(filepath.Join(t.TempDir(), "profile")))
	if err := s.Set("cursor", "safe"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.Path())
	err := s.Set("oversized", strings.Repeat("x", s.QuotaBytes()+1))
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(s.Path()); !bytes.Equal(before, after) {
		t.Fatal("state rewritten")
	}
	if v, _ := s.Get("cursor", nil); v != "safe" {
		t.Fatal(v)
	}
}

func TestCorruptStateIsNotSilentlyOverwritten(t *testing.T) {
	s := bridgeContext("fixture-plugin", "", "").State(homeCtx(filepath.Join(t.TempDir(), "profile")))
	_ = os.MkdirAll(s.DataDir(), 0o755)
	_ = os.WriteFile(s.Path(), []byte(`{"cursor":`), 0o644)
	if err := s.Set("cursor", "replacement"); err == nil || !strings.Contains(err.Error(), "Cannot parse plugin state") {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(s.Path()); string(raw) != `{"cursor":` {
		t.Fatal(string(raw))
	}
}

func TestConfigAndStateFollowContextLocalProfileScope(t *testing.T) {
	root := t.TempDir()
	homes := []string{filepath.Join(root, "profiles", "alpha"), filepath.Join(root, "profiles", "beta")}
	c := bridgeContext("fixture-plugin", "", "fixture-plugin-data")
	for i, h := range homes {
		_ = os.MkdirAll(h, 0o755)
		if err := c.SetConfig(homeCtx(h), "profile_value", i); err != nil {
			t.Fatal(err)
		}
		if err := c.State(homeCtx(h)).Set("profile_value", i); err != nil {
			t.Fatal(err)
		}
	}
	for i, h := range homes {
		if v := mustGet(t, c, homeCtx(h), "profile_value", nil); v != i {
			t.Fatal(v)
		}
		if v, _ := c.State(homeCtx(h)).Get("profile_value", nil); v != i {
			t.Fatal(v)
		}
	}
}

func TestFixturePluginRoundTripsBridgeDuringRealDiscovery(t *testing.T) {
	installLiveServices(t)
	h, m := liveHome(t, liveInterpreters(t)["system"], nil)
	dir := filepath.Join(h, "plugins", "bridge-fixture")
	writePluginDir(t, dir, "name: bridge-fixture\nversion: 1.0.0\n")
	writeFile(t, filepath.Join(dir, "__init__.py"), "def register(ctx):\n"+
		"    old = ctx.get_config('loads', default=0)\n"+
		"    ctx.set_config('loads', old + 1)\n"+
		"    ctx.state.set('registered', {'profile': ctx.profile_name})\n")
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: host\n  enabled:\n    - bridge-fixture\n")
	config.ResetCache()
	ctx := homeCtx(h)
	m.DiscoverAndLoad(ctx, true)
	m.mu.RLock()
	loaded := m.plugins["bridge-fixture"]
	m.mu.RUnlock()
	if loaded == nil || !loaded.Enabled {
		t.Fatalf("bridge-fixture did not load (error %q)", loadedError(m, "bridge-fixture"))
	}
	c := bridgeContext("bridge-fixture", "", "")
	config.ResetCache()
	if loads, err := c.GetConfig(ctx, "loads", nil); err != nil || config.PyRepr(loads) != "1" {
		t.Fatalf("loads = %#v %v", loads, err)
	}
	registered, err := c.State(ctx).Get("registered", nil)
	row, _ := registered.(map[string]any)
	if err != nil || (row["profile"] != "custom" && row["profile"] != "default") {
		t.Fatalf("registered = %#v %v", registered, err)
	}
}
