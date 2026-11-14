package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Pins the display context resolver to one answer for the case
func pinDisplayContextLength(t *testing.T, n int) {
	t.Helper()
	prev := provider.GetModelContextLength
	provider.GetModelContextLength = func(context.Context, provider.ContextLengthRequest) int { return n }
	t.Cleanup(func() { provider.GetModelContextLength = prev })
}

// Picker path commit on a bare CLI, the printed lines captured
func runSwitchDisplay(t *testing.T, result *provider.ModelSwitchResult) []string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := &bytes.Buffer{}
	t.Cleanup(SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out}, out))
	if err := bareCLI().applyModelSwitchResult(result, false, nil, ""); err != nil {
		t.Fatal(err)
	}
	return strings.Split(out.String(), "\n")
}

// First printed line naming the context window
func contextLine(lines []string) string {
	for _, l := range lines {
		if strings.Contains(l, "Context:") {
			return l
		}
	}
	return ""
}

func TestPickerPathUsesProviderAwareContextOnCodex(t *testing.T) {
	pinDisplayContextLength(t, 272_000)
	lines := runSwitchDisplay(t, &provider.ModelSwitchResult{Success: true, NewModel: "gpt-5.5", TargetProvider: "openai-codex",
		ProviderChanged: true, BaseURL: "https://chatgpt.com/backend-api/codex", APIMode: "codex_responses", ProviderLabel: "ChatGPT Codex",
		ModelInfo: &provider.ModelsDevEntry{ContextWindow: 1_050_000}})
	line := contextLine(lines)
	if !strings.Contains(line, "272,000") {
		t.Fatalf("picker-path display must show Codex's 272K cap, got: %q", line)
	}
	if strings.Contains(line, "1,050,000") {
		t.Fatalf("picker-path display leaked models.dev's 1.05M for Codex: %q", line)
	}
}

func TestPickerPathFallsBackToModelInfoWhenResolverEmpty(t *testing.T) {
	pinDisplayContextLength(t, 0)
	lines := runSwitchDisplay(t, &provider.ModelSwitchResult{Success: true, NewModel: "some-model", TargetProvider: "some-provider",
		ProviderChanged: true, APIMode: "chat_completions", ProviderLabel: "Some Provider",
		ModelInfo: &provider.ModelsDevEntry{ContextWindow: 1_050_000}})
	if line := contextLine(lines); !strings.Contains(line, "1,050,000") {
		t.Fatalf("resolver-empty path should fall back to ModelInfo, got: %q", line)
	}
}

func TestGlobalSwitchClearsContextPinOwnedByPreviousRoute(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	out := &bytes.Buffer{}
	t.Cleanup(SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out}, out))
	c := bareCLI()
	c.model, c.provider = "shared-model", "custom"
	c.baseURL = "https://small.example/v1"
	result := &provider.ModelSwitchResult{Success: true, NewModel: "shared-model", TargetProvider: "custom",
		BaseURL: "https://small.example/v1", APIMode: "chat_completions", ProviderLabel: "Custom",
		ModelInfo: &provider.ModelsDevEntry{ContextWindow: 1_050_000}, IsGlobal: true}
	configPath := filepath.Join(h, "config.yaml")
	configured := "model:\n  base_url: https://large.example/v1\n  context_length: 1048576\n  default: shared-model\n  provider: custom\n"
	if err := os.WriteFile(configPath, []byte(configured), 0o644); err != nil {
		t.Fatal(err)
	}
	pinDisplayContextLength(t, 256_000)
	if err := c.applyModelSwitchResult(result, true, nil, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := config.ReadUserConfigRaw(context.Background(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := raw["model"].(map[string]any)
	if _, pinned := model["context_length"]; pinned {
		t.Fatalf("context pin of the previous route survived %v", model)
	}
	if model["base_url"] != result.BaseURL {
		t.Fatalf("base_url %v", model["base_url"])
	}
}
