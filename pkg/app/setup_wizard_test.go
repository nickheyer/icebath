package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Recorded hermes output of one wizard scenario
type wizardRef struct {
	Out    string   `json:"out"`
	Stderr string   `json:"stderr"`
	Err    *string  `json:"err"`
	Asked  []string `json:"asked"`
}

func loadWizardRefs(t *testing.T) map[string]wizardRef {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "setup_wizard.json"))
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]wizardRef{}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

// Hermes wizard text with the icebath renames applied, box lines keeping their width
var wizardRenames = strings.NewReplacer(
	"☤ Hermes Agent Setup Wizard                │", "☤ Icebath Agent Setup Wizard               │",
	"Let's configure your Hermes Agent installation.       │", "Let's configure your Icebath Agent installation.      │",
	"☤ Hermes Setup — Nous Portal (one-shot)             │", "☤ Icebath Setup — Nous Portal (one-shot)            │",
	"☤ Hermes Setup — Agent Settings                     │", "☤ Icebath Setup — Agent Settings                    │",
	"☤ Hermes Setup — Model & Provider                   │", "☤ Icebath Setup — Model & Provider                  │",
	"`hermes-agent`", "`icebath-agent`", "Hermes", "Icebath",
	"`hermes`", "`icebath`", "`hermes ", "`icebath ", "'hermes ", "'icebath ", " hermes ", " icebath ",
	"/.hermes", "/.icebath",
)

var setupStampRe = regexp.MustCompile(`\d{8}-\d{6}`)

// A wizard run's home, its console streams and the recorded calls of the doubled sections
type wizardRun struct {
	tmp, home string
	ctx       context.Context
	out, err  *bytes.Buffer
	calls     map[string]int
	sections  map[string][]int
	asked     []string
	answers   map[string]int
	service   []gateway.ServiceSetupOptions
}

// Clean output with the temp dir, install root and backup stamps replaced like the recording
func (w *wizardRun) screen() string {
	text := strings.ReplaceAll(w.out.String(), config.GetProjectRoot(), "<ROOT>")
	text = strings.ReplaceAll(text, w.tmp, "<TMP>")
	return setupStampRe.ReplaceAllString(text, "<STAMP>")
}

// Prints a section marker on the setup console and counts the call with its section index
func (w *wizardRun) mark(name string, extra string) {
	w.calls[name]++
	idx, ok := provider.SetupNavigationSectionIndex()
	if !ok {
		idx = -2
	}
	w.sections[name] = append(w.sections[name], idx)
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
		calls: map[string]int{}, sections: map[string][]int{}, answers: map[string]int{}}
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
	swapSeam(t, &setupToolsFn, func(_ context.Context, _ config.Tree, firstInstall bool) error {
		w.mark("tools", fmt.Sprintf(" first_install=%s", pyBoolText(firstInstall)))
		return nil
	})
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
	t.Cleanup(provider.SwapSetupRecordCompleted(func(context.Context, map[string]any) error { w.mark("recorded", ""); return nil }))
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

// Writes config.yaml, auth.json and .env of the run home
func (w *wizardRun) seed(t *testing.T, configText string, auth map[string]any, env map[string]string) {
	t.Helper()
	if configText != "" {
		if err := os.WriteFile(home.ConfigPath(w.home), []byte(configText), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if auth != nil {
		data, _ := json.Marshal(auth)
		if err := os.WriteFile(filepath.Join(w.home, "auth.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	for k, v := range env {
		lines = append(lines, k+"="+v)
	}
	if len(lines) > 0 {
		if err := os.WriteFile(home.EnvPath(w.home), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Runs the wizard with an argparse namespace, the flags absent from it reading false
func (w *wizardRun) run(t *testing.T, ns map[string]any) (int, error) {
	t.Helper()
	return RunSetupWizard(w.ctx, ns)
}

const existingConfig = "model:\n  provider: openrouter\n  default: anthropic/claude-sonnet-4.5\n"

var existingAuth = map[string]any{"version": 1, "providers": map[string]any{}, "active_provider": "openrouter"}

// Compares a run's screen and menus with the recorded hermes scenario
func (w *wizardRun) assertMatches(t *testing.T, ref wizardRef) {
	t.Helper()
	if got, want := w.screen(), wizardRenames.Replace(ref.Out); got != want {
		t.Fatalf("screen\n%s\nwant\n%s", got, want)
	}
	want := make([]string, len(ref.Asked))
	for i, q := range ref.Asked {
		want[i] = wizardRenames.Replace(q)
	}
	if !slices.Equal(w.asked, want) && !(len(w.asked) == 0 && len(want) == 0) {
		t.Fatalf("menus %q want %q", w.asked, want)
	}
}

func TestExistingInstallDefaultBareSetupRunsFullReconfigureWithoutMenu(t *testing.T) {
	w := newWizardRun(t, "", true)
	w.seed(t, existingConfig, existingAuth, nil)
	if code, err := w.run(t, map[string]any{}); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if len(w.asked) != 0 || w.calls["quick"] != 0 {
		t.Fatalf("menus %v quick %d", w.asked, w.calls["quick"])
	}
	for _, name := range []string{"model", "terminal", "gateway", "tools"} {
		if w.calls[name] != 1 {
			t.Fatalf("%s ran %d times", name, w.calls[name])
		}
	}
	if strings.Contains(w.out.String(), "◆ Agent Settings") {
		t.Fatal("agent settings are not prompted on an existing install")
	}
	w.assertMatches(t, loadWizardRefs(t)["existing_bare"])
}

func TestQuickFlagQuickFlagRunsQuickSetupOnly(t *testing.T) {
	w := newWizardRun(t, "", true)
	w.seed(t, existingConfig, existingAuth, nil)
	if code, err := w.run(t, map[string]any{"quick": true}); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if w.calls["quick"] != 1 || !slices.Equal(w.sections["quick"], []int{0}) {
		t.Fatalf("quick %d sections %v", w.calls["quick"], w.sections["quick"])
	}
	for _, name := range []string{"model", "terminal", "gateway", "tools"} {
		if w.calls[name] != 0 {
			t.Fatalf("%s ran", name)
		}
	}
	w.assertMatches(t, loadWizardRefs(t)["existing_quick"])
}

func TestFreshInstallReconfigureOnFreshInstallFallsThrough(t *testing.T) {
	w := newWizardRun(t, "", true)
	if code, err := w.run(t, map[string]any{"reconfigure": true}); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if len(w.asked) != 1 || w.calls["first-quick"] != 1 || !slices.Equal(w.sections["first-quick"], []int{0}) {
		t.Fatalf("menus %v first %d sections %v", w.asked, w.calls["first-quick"], w.sections["first-quick"])
	}
	w.assertMatches(t, loadWizardRefs(t)["fresh_reconfigure_quick_mode"])
}

func TestFreshInstallBlankSlateRunsInsideNavigationStep(t *testing.T) {
	w := newWizardRun(t, "", true)
	w.answers["How would you like to set up Icebath?"] = 2
	if code, err := w.run(t, map[string]any{"quick": true}); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if w.calls["blank"] != 1 || !slices.Equal(w.sections["blank"], []int{0}) {
		t.Fatalf("blank %d sections %v", w.calls["blank"], w.sections["blank"])
	}
	w.assertMatches(t, loadWizardRefs(t)["fresh_blank"])
}

func TestSetupWizardScreensMatchHermes(t *testing.T) {
	refs := loadWizardRefs(t)
	t.Run("fresh_full", func(t *testing.T) {
		w := newWizardRun(t, "", true)
		w.answers["How would you like to set up Icebath?"] = 1
		if _, err := w.run(t, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		w.assertMatches(t, refs["fresh_full"])
	})
	t.Run("unknown_section", func(t *testing.T) {
		w := newWizardRun(t, "", true)
		if _, err := w.run(t, map[string]any{"section": "nope"}); err != nil {
			t.Fatal(err)
		}
		w.assertMatches(t, refs["unknown_section"])
	})
	t.Run("section_agent", func(t *testing.T) {
		w := newWizardRun(t, "\n\n\n", true)
		w.seed(t, existingConfig, nil, nil)
		if _, err := w.run(t, map[string]any{"section": "agent"}); err != nil {
			t.Fatal(err)
		}
		w.assertMatches(t, refs["section_agent"])
	})
	t.Run("fresh_escape", func(t *testing.T) {
		w := newWizardRun(t, "", true)
		t.Cleanup(provider.SwapSetupRadiolist(func(question string, _ []string, _ int, _ string) int {
			w.asked = append(w.asked, question)
			provider.HandleSetupMenuNavigation(cliui.NavEventCancel)
			return 0
		}))
		if code, err := w.run(t, map[string]any{}); code != 0 || err != nil {
			t.Fatalf("code %d err %v", code, err)
		}
		w.assertMatches(t, refs["fresh_escape"])
	})
	t.Run("fresh_full_imported", func(t *testing.T) {
		w := newWizardRun(t, "", true)
		w.answers["How would you like to set up Icebath?"] = 1
		w.answers["  Reconfigure model & provider?"] = 0
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
		swapSeam(t, &setupAllPlatforms, func(context.Context) []gateway.SetupPlatform { return []gateway.SetupPlatform{{Label: "Telegram"}} })
		swapSeam(t, &setupPlatformStatus, func(context.Context, gateway.SetupPlatform) (string, error) { return "not configured", nil })
		if _, err := w.run(t, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		w.assertMatches(t, refs["fresh_full_imported"])
	})
	for _, effect := range []string{"ok", "interrupt", "exit", "error"} {
		t.Run("portal_"+effect, func(t *testing.T) {
			w := newWizardRun(t, "", true)
			t.Cleanup(provider.SwapSetupModelFlowNous(func(ctx context.Context, cfg map[string]any) error {
				switch effect {
				case "interrupt":
					return cliui.ErrInterrupt
				case "exit":
					panic(provider.SetupExit{Code: 1})
				case "error":
					return fmt.Errorf("portal down")
				}
				fmt.Fprintln(provider.SetupOutput(), "[nous]")
				fresh, err := config.LoadContext(ctx, home.Dir(ctx), nil)
				if err != nil {
					return err
				}
				fresh["model"] = map[string]any{"provider": "nous", "default": "Hermes-4-405B"}
				return config.SaveConfig(ctx, home.Dir(ctx), fresh, config.SaveOptions{})
			}))
			if code, err := w.run(t, map[string]any{"portal": true}); code != 0 || err != nil {
				t.Fatalf("code %d err %v", code, err)
			}
			w.assertMatches(t, refs["portal_"+effect])
		})
	}
	t.Run("managed", func(t *testing.T) {
		w := newWizardRun(t, "", true)
		t.Setenv("ICEBATH_MANAGED", "true")
		var notice bytes.Buffer
		swapSeam(t, &config.FacadeNoticeOut, io.Writer(&notice))
		if code, err := w.run(t, map[string]any{}); code != 0 || err != nil {
			t.Fatalf("code %d err %v", code, err)
		}
		if got, want := notice.String(), wizardRenames.Replace(refs["managed"].Stderr); got != want || w.out.Len() != 0 {
			t.Fatalf("notice %q want %q screen %q", got, want, w.out.String())
		}
	})
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

// Writes a config.yaml with a distinctive model and max turns the user tuned
func writeTunedSetupConfig(t *testing.T, w *wizardRun, maxTurns int, model string) {
	t.Helper()
	cfg := loadHomeConfig(t, w.ctx)
	cfg["model"] = map[string]any{"provider": "custom", "base_url": "http://localhost:8080/v1", "default": model}
	agent, _ := provider.SetdefaultDict(cfg, "agent")
	agent["max_turns"] = maxTurns
	if err := config.SaveConfig(w.ctx, w.home, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(home.ConfigPath(w.home))
	if !strings.Contains(string(text), model) {
		t.Fatal("fixture failed to persist the user value")
	}
}

// Backups the wizard took of config.yaml
func setupBackups(t *testing.T, h string) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(h, "backups", "config", "config.yaml.pre-setup.*"))
	slices.Sort(matches)
	return matches
}

func TestNonInteractiveSetupResetFlagRewritesConfigBeforeNoninteractiveExit(t *testing.T) {
	w := newWizardRun(t, "", false)
	writeTunedSetupConfig(t, w, 12, "llama3")
	if code, err := w.run(t, map[string]any{"non_interactive": true, "reset": true}); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	reloaded := loadHomeConfig(t, w.ctx)
	defaults := config.Defaults()
	if normalizedConfigJSON(t, reloaded["model"]) != normalizedConfigJSON(t, defaults["model"]) {
		t.Fatalf("model %v", reloaded["model"])
	}
	got, _ := config.CfgGet(reloaded, "agent", "max_turns")
	want, _ := config.CfgGet(defaults, "agent", "max_turns")
	if normalizedConfigJSON(t, got) != normalizedConfigJSON(t, want) {
		t.Fatalf("max_turns %v want %v", got, want)
	}
	if !strings.Contains(w.out.String(), "Configuration reset to defaults.") {
		t.Fatalf("screen %q", w.out.String())
	}
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

func TestResetBackupOrderingResetBacksUpUserConfigNotTheResetDefaults(t *testing.T) {
	const sentinel = "hand-tuned-local-model-xyz"
	w := newWizardRun(t, "", false)
	writeTunedSetupConfig(t, w, 47, sentinel)
	if _, err := w.run(t, map[string]any{"non_interactive": true, "reset": true}); err != nil {
		t.Fatal(err)
	}
	backups := setupBackups(t, w.home)
	if len(backups) != 1 {
		t.Fatalf("expected exactly one backup, got %v", backups)
	}
	backupText, _ := os.ReadFile(backups[0])
	if !strings.Contains(string(backupText), sentinel) || !strings.Contains(string(backupText), "47") {
		t.Fatalf("backup captured the post-reset defaults instead of the user's config: %s", backupText)
	}
	current, _ := os.ReadFile(home.ConfigPath(w.home))
	if strings.Contains(string(current), sentinel) {
		t.Fatal("the reset did not take effect on config.yaml")
	}
	if normalizedConfigJSON(t, loadHomeConfig(t, w.ctx)["model"]) != normalizedConfigJSON(t, config.Defaults()["model"]) {
		t.Fatal("model is not the default after the reset")
	}
}

func TestResetBackupOrderingResetReportsWhereTheBackupLanded(t *testing.T) {
	w := newWizardRun(t, "", false)
	writeTunedSetupConfig(t, w, 47, "hand-tuned-local-model-xyz")
	if _, err := w.run(t, map[string]any{"non_interactive": true, "reset": true}); err != nil {
		t.Fatal(err)
	}
	backups := setupBackups(t, w.home)
	out := w.out.String()
	if len(backups) != 1 || !strings.Contains(out, "Configuration reset to defaults.") || !strings.Contains(out, "Previous config backed up to:") ||
		!strings.Contains(out, filepath.Base(backups[0])) {
		t.Fatalf("backups %v screen %q", backups, out)
	}
	refs := loadWizardRefs(t)
	if got, want := w.screen(), wizardRenames.Replace(refs["noninteractive_reset"].Out); got != want {
		t.Fatalf("screen\n%s\nwant\n%s", got, want)
	}
}

func TestQuickSetupKeepsImportedModelUnlessUserSwitches(t *testing.T) {
	for _, c := range []struct {
		switchModel bool
		nousRuns    int
		expected    map[string]any
	}{
		{false, 0, map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.5"}},
		{true, 1, map[string]any{"provider": "nous", "default": "Hermes-4-405B"}},
	} {
		t.Run(fmt.Sprintf("switch=%v", c.switchModel), func(t *testing.T) {
			w := newWizardRun(t, "", true)
			swapSeam(t, &setupRunFirstTimeQuickSetupFn, runFirstTimeQuickSetup)
			t.Cleanup(provider.SwapSetupRadiolist(func(question string, _ []string, def int, _ string) int {
				w.asked = append(w.asked, question)
				if c.switchModel && strings.Contains(question, "switch to Nous Portal") {
					return 1
				}
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
			nousRuns := 0
			t.Cleanup(provider.SwapSetupModelFlowNous(func(ctx context.Context, cfg map[string]any) error {
				nousRuns++
				cfg["model"] = map[string]any{"provider": "nous", "default": "Hermes-4-405B"}
				return config.SaveConfig(ctx, home.Dir(ctx), cfg, config.SaveOptions{})
			}))
			if _, err := w.run(t, map[string]any{"non_interactive": false, "section": nil, "reset": false, "reconfigure": false, "quick": false}); err != nil {
				t.Fatal(err)
			}
			if nousRuns != c.nousRuns {
				t.Fatalf("nous ran %d times", nousRuns)
			}
			model, _ := loadHomeConfig(t, w.ctx)["model"].(map[string]any)
			for k, v := range c.expected {
				if model[k] != v {
					t.Fatalf("model %v want %v", model, c.expected)
				}
			}
			if v, _, _ := config.GetEnvValue(w.ctx, w.home, "OPENROUTER_API_KEY"); v != "sk-or-demo-not-a-real-key" {
				t.Fatalf("imported key %q", v)
			}
			if w.calls["gateway"] != 1 {
				t.Fatalf("gateway ran %d times", w.calls["gateway"])
			}
			if !slices.ContainsFunc(w.asked, func(q string) bool {
				return strings.Contains(q, "Keep your imported model (OpenRouter · anthropic/claude-sonnet-4.5)")
			}) {
				t.Fatalf("menus %q", w.asked)
			}
		})
	}
}

func TestSectionSpecificModelSetupCanGoBackFromModelToProvider(t *testing.T) {
	w := newWizardRun(t, "", true)
	var shown []string
	attempts := 0
	modelFlow := func(context.Context, config.Tree) error {
		attempts++
		for _, label := range []string{"provider", "model"} {
			start, _ := provider.HandleSetupMenuNavigation(cliui.NavEventBegin)
			if start.Replay {
				provider.HandleSetupMenuNavigation(cliui.NavEventResolve, start.ReplayValue)
				continue
			}
			shown = append(shown, label)
			if label == "model" && attempts == 1 {
				if start.AllowBack {
					provider.HandleSetupMenuNavigation(cliui.NavEventBack)
				}
				return nil
			}
			provider.HandleSetupMenuNavigation(cliui.NavEventResolve, label)
		}
		return nil
	}
	swapSeam(t, &setupSections, []setupSection{{"model", "Model & Provider", modelFlow}})
	if _, err := w.run(t, map[string]any{"section": "model", "reset": false, "reconfigure": false, "quick": false, "portal": false, "non_interactive": false}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"provider", "model", "provider", "model"}; !slices.Equal(shown, want) {
		t.Fatalf("shown %v", shown)
	}
}
