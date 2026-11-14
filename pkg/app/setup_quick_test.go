package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

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
