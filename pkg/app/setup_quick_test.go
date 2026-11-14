package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Recorded hermes output of one setup_quick scenario, with the checklists it showed
type quickRef struct {
	Out    string          `json:"out"`
	Err    *string         `json:"err"`
	Asked  []string        `json:"asked"`
	Checks [][]any         `json:"checks"`
	Env    string          `json:"env"`
	Cfg    json.RawMessage `json:"cfg"`
}

func loadQuickRefs(t *testing.T) map[string]quickRef {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "setup_quick.json"))
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]quickRef{}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

// A wizard run for calling the setup_quick flows directly, the setup console and checklist answers installed
func newQuickRun(t *testing.T, stdin string, checklist map[string][]int) (*wizardRun, *[][]any) {
	t.Helper()
	for _, kv := range os.Environ() {
		key := strings.SplitN(kv, "=", 2)[0]
		if strings.HasSuffix(key, "_API_KEY") || strings.HasSuffix(key, "_TOKEN") || strings.HasSuffix(key, "_SECRET") ||
			strings.HasPrefix(key, "TELEGRAM") || strings.HasPrefix(key, "DISCORD") || strings.HasPrefix(key, "SLACK") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
	w := newWizardRun(t, stdin, true)
	t.Cleanup(provider.WithSetupConsole(console()))
	checks := &[][]any{}
	t.Cleanup(provider.SwapSetupChecklist(func(title string, items []string, pre map[int]bool) (map[int]bool, error) {
		row := []any{title}
		labels := make([]any, len(items))
		for i, item := range items {
			labels[i] = item
		}
		*checks = append(*checks, append(row, labels))
		picked := map[int]bool{}
		for _, idx := range checklist[title] {
			picked[idx] = true
		}
		return picked, nil
	}))
	swapSeam(t, &setupSetBundledSkillsOptOut, func(_ context.Context, optOut bool) map[string]any {
		fmt.Fprintf(provider.SetupOutput(), "[opt-out %s]\n", pyBoolText(optOut))
		return map[string]any{"ok": true}
	})
	swapSeam(t, &setupSyncSkills, func(context.Context, bool) (map[string]any, error) {
		return map[string]any{"copied": []string{"a", "b", "c"}, "updated": []string{}, "skipped": 0}, nil
	})
	return w, checks
}

// Compares a quick flow's screen, menus and checklists with the recorded scenario
func (w *wizardRun) assertQuickMatches(t *testing.T, ref quickRef, checks [][]any) {
	t.Helper()
	w.assertMatches(t, wizardRef{Out: ref.Out, Asked: ref.Asked})
	if len(checks) != len(ref.Checks) {
		t.Fatalf("checklists %d want %d", len(checks), len(ref.Checks))
	}
	for i := range checks {
		got, _ := json.Marshal(checks[i])
		want, _ := json.Marshal(ref.Checks[i])
		if wizardRenames.Replace(string(got)) != wizardRenames.Replace(string(want)) {
			t.Fatalf("checklist %d\n%s\nwant\n%s", i, got, want)
		}
	}
}

func TestSetupQuickScreensMatchHermes(t *testing.T) {
	refs := loadQuickRefs(t)
	const toolTitle, messagingTitle = "Which tools would you like to configure?", "Which platforms would you like to set up?"
	t.Run("quick_full", func(t *testing.T) {
		w, checks := newQuickRun(t, strings.Repeat("\n", 40), map[string][]int{toolTitle: {0}, messagingTitle: {0}})
		cfg := loadHomeConfig(t, w.ctx)
		if err := runQuickSetup(w.ctx, cfg, w.home); err != nil {
			t.Fatal(err)
		}
		w.assertQuickMatches(t, refs["quick_full"], *checks)
	})
	t.Run("quick_key", func(t *testing.T) {
		w, checks := newQuickRun(t, "sk-test-key\n", map[string][]int{toolTitle: {0}})
		cfg := loadHomeConfig(t, w.ctx)
		if err := runQuickSetup(w.ctx, cfg, w.home); err != nil {
			t.Fatal(err)
		}
		w.assertQuickMatches(t, refs["quick_key"], *checks)
		env, _ := os.ReadFile(filepath.Join(w.home, ".env"))
		if string(env) != refs["quick_key"].Env {
			t.Fatalf(".env %q want %q", env, refs["quick_key"].Env)
		}
	})
	t.Run("blank_finish", func(t *testing.T) {
		w, checks := newQuickRun(t, "", nil)
		cfg := config.Tree{}
		var err error
		provider.SetupNavigationScope(func() {
			err = provider.RunSetupSteps([]provider.SetupStep{{Label: "Step", Action: func() error { return runBlankSlateSetup(w.ctx, cfg, w.home) }}})
		})
		if err != nil {
			t.Fatal(err)
		}
		w.assertQuickMatches(t, refs["blank_finish"], *checks)
		got := map[string]any{}
		for _, k := range []string{"platform_toolsets", "compression", "memory", "checkpoints", "smart_model_routing", "agent"} {
			got[k] = cfg[k]
		}
		var want map[string]any
		_ = json.Unmarshal(refs["blank_finish"].Cfg, &want)
		if normalizedConfigJSON(t, got) != normalizedConfigJSON(t, want) {
			t.Fatalf("config\n%s\nwant\n%s", normalizedConfigJSON(t, got), normalizedConfigJSON(t, want))
		}
	})
	walk := func(name string, answers map[string]int) {
		t.Run(name, func(t *testing.T) {
			w, checks := newQuickRun(t, "", nil)
			for k, v := range answers {
				w.answers[k] = v
			}
			cfg := config.Tree{}
			var err error
			provider.SetupNavigationScope(func() {
				err = provider.RunSetupSteps([]provider.SetupStep{{Label: "Step", Action: func() error { return runBlankSlateSetup(w.ctx, cfg, w.home) }}})
			})
			if err != nil {
				t.Fatal(err)
			}
			w.assertQuickMatches(t, refs[name], *checks)
		})
	}
	walk("blank_walk", map[string]int{"Your minimal agent is ready. What next?": 1, "Seed the full bundled skill catalog? (No = start with zero skills)": 0,
		"Review and enable built-in plugins now?": 0, "Connect a messaging platform (Telegram, Discord, …)?": 0})
	walk("blank_walk_no", map[string]int{"Your minimal agent is ready. What next?": 1})
	for _, effect := range []string{"ok", "interrupt", "error"} {
		t.Run("first_quick_"+effect, func(t *testing.T) {
			w, checks := newQuickRun(t, "", nil)
			t.Cleanup(provider.SwapSetupModelFlowNous(func(context.Context, map[string]any) error {
				switch effect {
				case "interrupt":
					return cliui.ErrInterrupt
				case "error":
					return errors.New("portal down")
				}
				fmt.Fprintln(provider.SetupOutput(), "[nous]")
				return nil
			}))
			swapSeam(t, &setupSysPlatform, "linux")
			cfg := loadHomeConfig(t, w.ctx)
			var err error
			provider.SetupNavigationScope(func() {
				err = provider.RunSetupSteps([]provider.SetupStep{{Label: "Step", Action: func() error { return runFirstTimeQuickSetup(w.ctx, cfg, w.home, "") }}})
			})
			if err != nil {
				t.Fatal(err)
			}
			w.assertQuickMatches(t, refs["first_quick_"+effect], *checks)
		})
	}
	t.Run("first_quick_skip_gateway", func(t *testing.T) {
		w, checks := newQuickRun(t, "", nil)
		w.answers["Connect a messaging platform? (Telegram, Discord, etc.)"] = 1
		t.Cleanup(provider.SwapSetupModelFlowNous(func(context.Context, map[string]any) error {
			fmt.Fprintln(provider.SetupOutput(), "[nous]")
			return nil
		}))
		swapSeam(t, &setupSysPlatform, "linux")
		cfg := loadHomeConfig(t, w.ctx)
		var err error
		provider.SetupNavigationScope(func() {
			err = provider.RunSetupSteps([]provider.SetupStep{{Label: "Step", Action: func() error { return runFirstTimeQuickSetup(w.ctx, cfg, w.home, "") }}})
		})
		if err != nil {
			t.Fatal(err)
		}
		w.assertQuickMatches(t, refs["first_quick_skip_gateway"], *checks)
		if len(w.service) != 1 || w.service[0].Context != "setup" || !w.service[0].Interactive || w.service[0].Config == nil {
			t.Fatalf("service offer %+v", w.service)
		}
	})
	t.Run("quick setup exits on a login sys.exit", func(t *testing.T) {
		w, _ := newQuickRun(t, "", nil)
		t.Cleanup(provider.SwapSetupModelFlowNous(func(context.Context, map[string]any) error { return &provider.NousLoginExitError{Code: 130} }))
		err := quickSetupNousPortal(w.ctx, loadHomeConfig(t, w.ctx))
		if code, _, ok := provider.SetupSystemExit(err); !ok || code != 130 {
			t.Fatalf("err %v", err)
		}
	})
}

func TestBlankSlateForkFinishNowSkipsWalkthrough(t *testing.T) {
	w, _ := newQuickRun(t, "", nil)
	swapSeam(t, &setupModelProviderFn, func(context.Context, config.Tree) error { return nil })
	swapSeam(t, &setupTerminalBackendFn, func(context.Context, config.Tree) error { return nil })
	swapSeam(t, &setupPrintSetupSummaryFn, func(context.Context, config.Tree, string) error { return nil })
	walked := false
	swapSeam(t, &setupWalkthroughFn, func(context.Context, config.Tree, string) error { walked = true; return nil })
	var optedOut []bool
	swapSeam(t, &setupSetBundledSkillsOptOut, func(_ context.Context, enabled bool) map[string]any {
		optedOut = append(optedOut, enabled)
		return map[string]any{"ok": true}
	})
	cfg := config.Tree{}
	if err := runBlankSlateSetup(w.ctx, cfg, w.tmp); err != nil {
		t.Fatal(err)
	}
	cli, _ := config.CfgGet(cfg, "platform_toolsets", "cli")
	if normalizedConfigJSON(t, cli) != `["file","skills","terminal","vision"]` || walked || !slices.Equal(optedOut, []bool{true}) {
		t.Fatalf("cli %v walked %v opted out %v", cli, walked, optedOut)
	}
}

// Installs a darwin host whose TCC directory read fails with err, nil when the read succeeds
func fakeMacosTCC(t *testing.T, readErr error) string {
	t.Helper()
	userHome := t.TempDir()
	tcc := filepath.Join(userHome, "Library", "Application Support", "com.apple.TCC")
	swapSeam(t, &setupUserHomeDir, func() (string, error) { return userHome, nil })
	swapSeam(t, &setupReadDirNames, func(path string) error {
		if path != tcc {
			t.Fatalf("probed %s", path)
		}
		return readErr
	})
	return tcc
}

func TestSetupTipIsSilentWhenAlreadyGranted(t *testing.T) {
	w, _ := newQuickRun(t, "", nil)
	swapSeam(t, &setupSysPlatform, "darwin")
	fakeMacosTCC(t, nil)
	if err := printMacosFDATip(); err != nil {
		t.Fatal(err)
	}
	if w.out.Len() != 0 || loadQuickRefs(t)["fda_granted"].Out != "" {
		t.Fatalf("screen %q", w.out.String())
	}
}

func TestSetupTipPrintedWhenDenied(t *testing.T) {
	w, _ := newQuickRun(t, "", nil)
	swapSeam(t, &setupSysPlatform, "darwin")
	fakeMacosTCC(t, &fs.PathError{Op: "open", Path: "tcc", Err: fs.ErrPermission})
	if err := printMacosFDATip(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.out.String(), "Privacy_AllFiles") {
		t.Fatalf("screen %q", w.out.String())
	}
	if got, want := w.screen(), wizardRenames.Replace(loadQuickRefs(t)["fda_denied"].Out); got != want {
		t.Fatalf("screen\n%s\nwant\n%s", got, want)
	}
}

func TestSetupTipIsSilentWhenProbeIsIndeterminate(t *testing.T) {
	w, _ := newQuickRun(t, "", nil)
	swapSeam(t, &setupSysPlatform, "darwin")
	fakeMacosTCC(t, &fs.PathError{Op: "open", Path: "tcc", Err: fs.ErrNotExist})
	if err := printMacosFDATip(); err != nil {
		t.Fatal(err)
	}
	if w.out.Len() != 0 || loadQuickRefs(t)["fda_missing"].Out != "" {
		t.Fatalf("screen %q", w.out.String())
	}
}
