package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Reports the given terminal size to every size read of the test
func withTermSize(t *testing.T, columns, rows int) {
	t.Helper()
	t.Setenv("COLUMNS", strconv.Itoa(columns))
	t.Setenv("LINES", strconv.Itoa(rows))
}

// Text of rendered fragments joined
func fragmentText(fragments []styledFragment) string {
	var b strings.Builder
	for _, f := range fragments {
		b.WriteString(f.Text)
	}
	return b.String()
}

// A CLI showing the clarify panel for the questions, the active one offering the choices
func clarifyPanelCLI(t *testing.T, questions []string, answers map[string]string, choices []string, active int) *CLI {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	state := &clarifyModalState{Answers: map[string]*string{}, AnswerMeta: map[string]clarifyAnswerMeta{}, Active: active, Choices: choices}
	for i, q := range questions {
		state.Questions = append(state.Questions, clarifyQuestion{QID: "q" + string(rune('0'+i)), Question: q})
	}
	for qid, answer := range answers {
		a := answer
		state.Answers[qid] = &a
	}
	return &CLI{ctx: context.Background(), clarifyState: state}
}

var clarifyLongQuestion = strings.TrimSpace(strings.Repeat("Storage boundary for the signed graph. ", 12))

func TestClarifyBatchPanelHeight(t *testing.T) {
	t.Run("test_short_terminal_keeps_active_choices_inside_the_viewport", func(t *testing.T) {
		c := clarifyPanelCLI(t, []string{clarifyLongQuestion, clarifyLongQuestion, clarifyLongQuestion + " ACTIVE_QUESTION_TAIL"},
			map[string]string{"q0": strings.Repeat("locked answer ", 20), "q1": strings.Repeat("locked answer ", 20)},
			[]string{"CHOICE_ALPHA_TOKEN keep the graph", "CHOICE_BETA_TOKEN replace it"}, 2)
		withTermSize(t, 100, 24)
		rendered := fragmentText(c.getClarifyDisplayFragments())
		if strings.Count(rendered, "\n") > 24-panelReservedBelow || !strings.Contains(rendered, "CHOICE_ALPHA_TOKEN") || !strings.Contains(rendered, "CHOICE_BETA_TOKEN") {
			t.Fatalf("%d rows:\n%s", strings.Count(rendered, "\n"), rendered)
		}
	})
	t.Run("test_tall_terminal_keeps_the_full_active_question", func(t *testing.T) {
		c := clarifyPanelCLI(t, []string{clarifyLongQuestion, clarifyLongQuestion, clarifyLongQuestion + " ACTIVE_QUESTION_TAIL"},
			map[string]string{"q0": "short", "q1": "short"}, []string{"CHOICE_ALPHA_TOKEN keep the graph"}, 2)
		withTermSize(t, 100, 80)
		rendered := fragmentText(c.getClarifyDisplayFragments())
		if !strings.Contains(rendered, "ACTIVE_QUESTION_TAIL") || !strings.Contains(rendered, "CHOICE_ALPHA_TOKEN") {
			t.Fatalf("rendered:\n%s", rendered)
		}
	})
	t.Run("test_selected_choice_stays_visible_when_choices_overflow", func(t *testing.T) {
		var choices []string
		for i := 0; i < 4; i++ {
			choices = append(choices, "CHOICE_"+string(rune('0'+i))+"_TOKEN "+strings.Repeat("long label ", 12))
		}
		c := clarifyPanelCLI(t, []string{clarifyLongQuestion, clarifyLongQuestion}, nil, choices, 1)
		c.clarifyState.Selected = 3
		withTermSize(t, 100, 16)
		rendered := fragmentText(c.getClarifyDisplayFragments())
		if strings.Count(rendered, "\n") > 16-panelReservedBelow || !strings.Contains(rendered, "CHOICE_3_TOKEN") {
			t.Fatalf("%d rows:\n%s", strings.Count(rendered, "\n"), rendered)
		}
	})
}

// A CLI with an open approval panel for the command
func approvalPanelCLI(t *testing.T, command, description string, choices []string, showFull bool) *CLI {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &CLI{ctx: context.Background(), approvalDeadline: pythread.Monotonic() + 30,
		approvalState: &approvalModalState{Command: command, Description: description, Choices: choices, ShowFull: showFull, response: make(chan string, 1)}}
}

func TestApprovalPanelAndHintFollowTheLiveCatalog(t *testing.T) {
	locales := t.TempDir()
	catalog := "cli:\n  tui:\n    approval_once: 'ONCE-SWAPPED'\n    approval_deny: 'DENY-SWAPPED'\n    approval_title: 'TITLE-SWAPPED'\n    hint_approval: 'HINT-SWAPPED'\n"
	if err := os.WriteFile(filepath.Join(locales, "en.yaml"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_BUNDLED_LOCALES", locales)
	i18n.ResetLanguageCache()
	t.Cleanup(i18n.ResetLanguageCache)
	withTermSize(t, 100, 40)
	c := approvalPanelCLI(t, "rm -rf build", "recursive delete", []string{"once", "deny"}, false)
	rendered := fragmentText(c.getApprovalDisplayFragments())
	if !strings.Contains(rendered, "TITLE-SWAPPED") || !strings.Contains(rendered, "1. ONCE-SWAPPED") || !strings.Contains(rendered, "2. DENY-SWAPPED") || strings.Contains(rendered, "Allow once") {
		t.Fatalf("rendered:\n%s", rendered)
	}
	hint := fragmentText(c.tuiHintText())
	if !strings.HasPrefix(hint, "  HINT-SWAPPED") || !strings.HasSuffix(strings.TrimRight(hint, " "), "s)") {
		t.Fatalf("hint %q", hint)
	}
}

func TestUnknownChoiceIdRendersVerbatim(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	if got := approvalChoiceLabel(ctx, "custom-id"); got != "custom-id" {
		t.Fatalf("got %q", got)
	}
	if got := approvalChoiceLabel(ctx, "deny"); got != i18n.T(ctx, "cli.tui.approval_deny", "", nil) {
		t.Fatalf("got %q", got)
	}
}

func TestApprovalDisplayTruncatesGiantCommandInViewMode(t *testing.T) {
	c := approvalPanelCLI(t, "bash -c 'echo "+strings.Repeat("x", 3000)+"'", "shell command via -c/-lc flag", []string{"once", "session", "always", "deny"}, true)
	withTermSize(t, 100, 24)
	rendered := fragmentText(c.getApprovalDisplayFragments())
	for _, key := range []string{"approval_once", "approval_session", "approval_always", "approval_deny"} {
		if label := c.t("cli.tui."+key, nil); !strings.Contains(rendered, label) {
			t.Fatalf("choice %q missing:\n%s", label, rendered)
		}
	}
	if !strings.Contains(rendered, c.t("cli.tui.approval_command_truncated", nil)) {
		t.Fatalf("no truncation marker:\n%s", rendered)
	}
}

func TestExtensionHookSubclass(t *testing.T) {
	t.Run("test_extra_widgets_inserted_before_status_bar", func(t *testing.T) {
		named := func(name string) *ptWindow { return &ptWindow{name: name} }
		c := &CLI{ctx: context.Background()}
		c.tui.extraWidgets = func() []*ptWindow { return []*ptWindow{named("radio-menu"), named("mini-player")} }
		children := c.buildTUILayoutChildren(tuiLayoutChildren{sudo: named("sudo"), secret: named("secret"), approval: named("approval"),
			clarify: named("clarify"), spinner: named("spinner"), spacer: named("spacer"), statusBar: named("status"), inputRuleTop: named("top-rule"),
			imageBar: named("image-bar"), inputArea: named("input-area"), inputRuleBot: named("bottom-rule"), voiceStatusBar: named("voice-status"),
			completionsMenu: named("completions-menu")})
		index := map[string]int{}
		for i, child := range children {
			index[child.name] = i
		}
		spacer := index["spacer"]
		if children[spacer+1].name != "radio-menu" || children[spacer+2].name != "mini-player" || children[spacer+3].name != "status" || index["status"] != spacer+3 {
			t.Fatalf("order %v", index)
		}
	})
}

func TestStartupMaintenanceReturnsWhileCuratorStillRunning(t *testing.T) {
	release, entered := make(chan struct{}), make(chan uint64, 1)
	prev := maybeRunCurator
	maybeRunCurator = func(*CLI, *float64, func(string)) {
		entered <- pythread.GoroutineID()
		<-release
	}
	t.Cleanup(func() { maybeRunCurator = prev })
	started := time.Now()
	(&CLI{ctx: context.Background()}).tuiStartupBackgroundMaintenance()
	elapsed := time.Since(started)
	defer close(release)
	select {
	case worker := <-entered:
		if worker == pythread.GoroutineID() {
			t.Fatal("the curator pass ran on the caller's goroutine")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("curator pass never started")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("startup maintenance held the caller for %s", elapsed)
	}
}

// Names of the layout children built around an optional stash panel widget
func layoutChildNames(stashPanel *ptWindow) []string {
	named := func(name string) *ptWindow { return &ptWindow{name: name} }
	c := &CLI{ctx: context.Background()}
	c.tui.stashPanelWidget = stashPanel
	var names []string
	for _, child := range c.buildTUILayoutChildren(tuiLayoutChildren{sudo: named("sudo"), secret: named("secret"), approval: named("approval"),
		clarify: named("clarify"), spinner: named("spinner"), spacer: named("spacer"), statusBar: named("status"), inputRuleTop: named("top-rule"),
		imageBar: named("image-bar"), inputArea: named("input-area"), inputRuleBot: named("bottom-rule"), voiceStatusBar: named("voice-status"),
		completionsMenu: named("completions-menu")}) {
		names = append(names, child.name)
	}
	return names
}

func TestLayoutSlot(t *testing.T) {
	t.Run("test_layout_includes_stash_panel_when_present", func(t *testing.T) {
		names := layoutChildNames(&ptWindow{name: "stash-panel"})
		if stash, status := indexOf(names, "stash-panel"), indexOf(names, "status"); stash < 0 || stash > status {
			t.Fatalf("children %v", names)
		}
	})
	t.Run("test_layout_omits_stash_panel_when_absent", func(t *testing.T) {
		names := layoutChildNames(nil)
		if indexOf(names, "stash-panel") >= 0 || indexOf(names, "") >= 0 || len(names) != 14 {
			t.Fatalf("children %v", names)
		}
	})
}
