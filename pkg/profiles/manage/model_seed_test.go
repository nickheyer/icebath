package manage

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Parsed config.yaml of a created profile
func profileModelCfg(t *testing.T, dir string) map[string]any {
	t.Helper()
	cfg := loadYAML(t, filepath.Join(dir, "config.yaml"))
	return cfg
}

func TestFreshProfileInheritsAUsableModel(t *testing.T) {
	tmp := manageEnv(t)
	writeFile(t, filepath.Join(tmp, ".icebath", "config.yaml"), "model:\n  provider: nous\n  default: some/model\n")
	cfg := profileModelCfg(t, mustCreate(t, "coder", CreateOptions{}))
	if m := cfg["model"].(map[string]any); m["provider"] != "nous" || m["default"] != "some/model" {
		t.Fatalf("model = %v", m)
	}
}

func TestFreshProfileInheritsItsCustomProviderGateway(t *testing.T) {
	tmp := manageEnv(t)
	writeFile(t, filepath.Join(tmp, ".icebath", "config.yaml"), "model:\n  provider: my-gateway\n  default: my-finetune\n"+
		"providers:\n  my-gateway:\n    api: https://llm.internal.example.com/v1\n    key_env: GW_KEY\n"+
		"  unrelated:\n    api: https://other.example.com/v1\n")
	cfg := profileModelCfg(t, mustCreate(t, "coder", CreateOptions{}))
	if !reflect.DeepEqual(cfg["model"], map[string]any{"provider": "my-gateway", "default": "my-finetune"}) {
		t.Fatalf("model = %v", cfg["model"])
	}
	want := map[string]any{"my-gateway": map[string]any{"api": "https://llm.internal.example.com/v1", "key_env": "GW_KEY"}}
	if !reflect.DeepEqual(cfg["providers"], want) {
		t.Fatalf("providers = %v", cfg["providers"])
	}
}

func TestFreshProfileModelIsCopiedNotLinked(t *testing.T) {
	tmp := manageEnv(t)
	source := filepath.Join(tmp, ".icebath", "config.yaml")
	writeFile(t, source, "model:\n  provider: nous\n  default: some/model\n")
	dir := mustCreate(t, "coder", CreateOptions{})
	writeFile(t, source, "model:\n  provider: other\n  default: changed/model\n")
	if m := profileModelCfg(t, dir)["model"].(map[string]any); m["provider"] != "nous" || m["default"] != "some/model" {
		t.Fatalf("model = %v", m)
	}
}

func TestFreshProfileRetainsLaunchCustomEndpoint(t *testing.T) {
	tmp := manageEnv(t)
	writeFile(t, filepath.Join(tmp, ".icebath", "config.yaml"), "model: {provider: lab, default: synthetic-model}\n"+
		"providers:\n  lab: {name: Display Name, base_url: \"http://127.0.0.1:11434/v1\", api_key: \"${LAB_TOKEN}\"}\n"+
		"  other: {name: Unrelated, base_url: \"http://127.0.0.1:11435/v1\"}\n")
	cfg := profileModelCfg(t, mustCreate(t, "sample-bot", CreateOptions{NoSkills: true}))
	providers := cfg["providers"].(map[string]any)
	if len(providers) != 1 || !strings.Contains(providers["lab"].(map[string]any)["api_key"].(string), "${LAB_TOKEN}") {
		t.Fatalf("providers = %v", providers)
	}
}

func TestCreateProfileDocker(t *testing.T) {
	tmp := manageEnv(t)
	docker := filepath.Join(tmp, "opt", "data")
	if err := os.MkdirAll(docker, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", docker)
	if got, want := mustCreate(t, "orchestrator", CreateOptions{}), filepath.Join(docker, "profiles", "orchestrator"); got != want || !isDir(want) {
		t.Fatalf("created %s, want %s", got, want)
	}
}
