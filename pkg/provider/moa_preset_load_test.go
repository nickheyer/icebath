package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Env naming the home whose MoA preset names a re-executed test binary prints as JSON
const moaPresetProbeEnv = "ICEBATH_PROVIDER_MOA_PRESET_PROBE"

// Prints the sorted MoA preset names a fresh process loads for the named home
func dispatchMoAPresetProbeEntry() {
	h := os.Getenv(moaPresetProbeEnv)
	if h == "" {
		return
	}
	cfg, err := config.Load(h, config.EnvRefLookup)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	names := []string{}
	for name := range cfg.Map("moa.presets") {
		names = append(names, name)
	}
	sort.Strings(names)
	out, _ := json.Marshal(names)
	fmt.Println(string(out))
	os.Exit(0)
}

const threeMoAPresets = "moa:\n  default_preset: keep_a\n  presets:\n" +
	"    keep_a:\n      reference_models:\n      - provider: openai-codex\n        model: gpt-5.5\n      aggregator:\n        provider: anthropic\n        model: claude-opus-5\n      enabled: true\n" +
	"    doomed:\n      reference_models:\n      - provider: openai-codex\n        model: gpt-5.6-luna\n      aggregator:\n        provider: anthropic\n        model: claude-opus-5\n      enabled: true\n" +
	"    keep_b:\n      reference_models:\n      - provider: openai-codex\n        model: gpt-5.7\n      aggregator:\n        provider: anthropic\n        model: claude-opus-5\n      enabled: true\n"

// Home named .icebath seeded with a config and cold caches
func moaPresetHome(t *testing.T, text string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	config.InvalidateManagedCache()
	config.ResetCache()
	config.ResetConfigParseFailures()
	t.Cleanup(func() { config.ResetCache(); config.ResetConfigParseFailures() })
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func loadedPresetNames(t *testing.T, h string) []string {
	t.Helper()
	cfg, err := config.LoadContext(t.Context(), h, config.EnvRefLookup)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for name := range cfg.Map("moa.presets") {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestNamedMapDoesNotReceiveSchemaDefaultOnLoad(t *testing.T) {
	h := moaPresetHome(t, threeMoAPresets)
	if got := loadedPresetNames(t, h); !reflect.DeepEqual(got, []string{"doomed", "keep_a", "keep_b"}) {
		t.Fatalf("presets %v", got)
	}
}

func TestNamedMapStaysAuthoritativeInAFreshProcessAndBackupFallback(t *testing.T) {
	h := moaPresetHome(t, threeMoAPresets)
	if got := loadedPresetNames(t, h); !reflect.DeepEqual(got, []string{"doomed", "keep_a", "keep_b"}) {
		t.Fatalf("presets %v", got)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("moa: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := exec.Command(os.Args[0])
	probe.Env = append(os.Environ(), "ICEBATH_HOME="+h, moaPresetProbeEnv+"="+h)
	out, err := probe.Output()
	if err != nil {
		t.Fatalf("fresh process: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var names []string
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &names); err != nil {
		t.Fatalf("probe output %q: %v", out, err)
	}
	if !reflect.DeepEqual(names, []string{"doomed", "keep_a", "keep_b"}) {
		t.Fatalf("fresh process presets %v", names)
	}
}

func TestMissingMoaMapStillReceivesSchemaDefaults(t *testing.T) {
	h := moaPresetHome(t, "model: {}\n")
	cfg, err := config.LoadContext(t.Context(), h, config.EnvRefLookup)
	if err != nil {
		t.Fatal(err)
	}
	actual := NormalizeMoAConfig(cfg["moa"])
	expected := NormalizeMoAConfig(config.Defaults()["moa"])
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("moa %v, want %v", actual, expected)
	}
}
