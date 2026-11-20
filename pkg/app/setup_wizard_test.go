package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// A wizard run's home, its console streams and the recorded calls of the doubled sections
type wizardRun struct {
	tmp, home string
	ctx       context.Context
	out, err  *bytes.Buffer
	calls     map[string]int
	asked     []string
	answers   map[string]int
	service   []gateway.ServiceSetupOptions
}

// Prints a section marker on the setup console and counts the call
func (w *wizardRun) mark(name string, extra string) {
	w.calls[name]++
	fmt.Fprintf(provider.SetupOutput(), "[%s%s]\n", name, extra)
}

// Sets up a temp home, an interactive console and markers for every section the wizard reaches
func newWizardRun(t *testing.T, stdin string, tty bool) *wizardRun {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", h)
	for _, key := range []string{"ICEBATH_MAX_ITERATIONS", "OPENROUTER_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL", "ICEBATH_INFERENCE_PROVIDER",
		"ICEBATH_NONINTERACTIVE", "ICEBATH_MANAGED", "GITHUB_TOKEN", "ELEVENLABS_API_KEY", "BROWSERBASE_API_KEY", "FIRECRAWL_API_KEY"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	w := &wizardRun{tmp: tmp, home: h, ctx: home.WithOverride(context.Background(), h), out: &bytes.Buffer{}, err: &bytes.Buffer{},
		calls: map[string]int{}, answers: map[string]int{}}
	con := &cliui.Console{In: strings.NewReader(stdin), Out: w.out, StdinTTY: func() bool { return tty }, StdoutTTY: func() bool { return false }}
	con.MaskedSecretPrompt = func(prompt string) (string, error) { return con.Input(prompt) }
	t.Cleanup(SetStreams(con, w.err))
	t.Cleanup(provider.SwapSetupRadiolist(func(question string, choices []string, def int, description string) int {
		w.asked = append(w.asked, question)
		if idx, ok := w.answers[question]; ok {
			return idx
		}
		return def
	}))
	swapSeam(t, &setupModelProviderFn, func(context.Context, config.Tree) error { w.mark("model", ""); return nil })
	swapSeam(t, &setupTerminalBackendFn, func(context.Context, config.Tree) error { w.mark("terminal", ""); return nil })
	swapSeam(t, &setupGatewayFn, func(context.Context, config.Tree) error { w.mark("gateway", ""); return nil })
	swapSeam(t, &setupOfferAgentImportFn, func(context.Context, string, bool) (string, error) { return "", nil })
	swapSeam(t, &setupPrintSetupSummaryFn, func(context.Context, config.Tree, string) error { w.mark("summary", ""); return nil })
	swapSeam(t, &setupRunQuickSetupFn, func(context.Context, config.Tree, string) error { w.mark("quick", ""); return nil })
	swapSeam(t, &setupEnsureGatewayService, func(_ context.Context, _ io.Writer, o gateway.ServiceSetupOptions) (bool, error) {
		w.service = append(w.service, o)
		w.mark("service", "")
		return false, nil
	})
	swapSeam(t, &setupRunFirstTimeQuickSetupFn, func(context.Context, config.Tree, string, string) error { w.mark("first-quick", ""); return nil })
	swapSeam(t, &setupRunBlankSlateSetupFn, func(context.Context, config.Tree, string) error { w.mark("blank", ""); return nil })
	return w
}

// Replaces a seam for the test, restoring it at cleanup
func swapSeam[T any](t *testing.T, target *T, value T) {
	t.Helper()
	prev := *target
	*target = value
	t.Cleanup(func() { *target = prev })
}

// Python spelling of a bool
func pyBoolText(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// Python config.yaml of a home as a map
func loadHomeConfig(t *testing.T, ctx context.Context) config.Tree {
	t.Helper()
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// JSON text of a config value for comparing YAML loaded and default values
func normalizedConfigJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestQuickSetupKeepsImportedModel(t *testing.T) {
	w := newWizardRun(t, "", true)
	swapSeam(t, &setupRunFirstTimeQuickSetupFn, runFirstTimeQuickSetup)
	t.Cleanup(provider.SwapSetupRadiolist(func(question string, _ []string, def int, _ string) int {
		w.asked = append(w.asked, question)
		return def
	}))
	swapSeam(t, &setupOfferAgentImportFn, func(ctx context.Context, h string, _ bool) (string, error) {
		cfg, err := config.LoadContext(ctx, h, nil)
		if err != nil {
			return "", err
		}
		cfg["model"] = map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.5"}
		if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
			return "", err
		}
		return "OpenClaw", config.SaveEnvValueContext(ctx, h, "OPENROUTER_API_KEY", "sk-or-demo-not-a-real-key")
	})
	if err := hostedQuickSetup(w.ctx, nil); err != nil {
		t.Fatal(err)
	}
	model, _ := loadHomeConfig(t, w.ctx)["model"].(map[string]any)
	if model["provider"] != "openrouter" || model["default"] != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("model %v", model)
	}
	if v, _, _ := config.GetEnvValue(w.ctx, w.home, "OPENROUTER_API_KEY"); v != "sk-or-demo-not-a-real-key" {
		t.Fatalf("imported key %q", v)
	}
	if w.calls["gateway"] != 1 {
		t.Fatalf("gateway ran %d times", w.calls["gateway"])
	}
}
