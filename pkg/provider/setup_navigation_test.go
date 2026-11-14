package provider

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
)

func stubSetupRadiolist(t *testing.T, fn func(question string, choices []string, def int, description string) int) *[]string {
	t.Helper()
	prev := setupRadiolist
	t.Cleanup(func() { setupRadiolist = prev })
	titles := &[]string{}
	setupRadiolist = func(question string, choices []string, def int, description string) int {
		*titles = append(*titles, question)
		return fn(question, choices, def, description)
	}
	return titles
}

func captureSetupOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	restore := WithSetupOutput(&out)
	t.Cleanup(restore)
	return &out
}

func TestPromptYesNoHeadlessAnswersTheDefault(t *testing.T) {
	t.Setenv("ICEBATH_NONINTERACTIVE", "1")
	if !PromptYesNo("Go?", true, "") || PromptYesNo("Go?", false, "") {
		t.Fatal("headless prompt did not answer the default")
	}
}

func TestPromptYesNoInsideSetupUsesTheMenu(t *testing.T) {
	var seen []string
	stubSetupRadiolist(t, func(question string, choices []string, def int, description string) int {
		seen = append(seen, question, strings.Join(choices, ","), description)
		if def != 1 {
			t.Fatalf("default index %d for a no default", def)
		}
		return 0
	})
	t.Setenv("ICEBATH_NONINTERACTIVE", "")
	answer := false
	setupNavigationScope(func() { answer = PromptYesNo("Import?", false, "  Found: X") })
	if !answer || len(seen) != 3 || seen[0] != "Import?" || seen[1] != "Yes,No" || seen[2] != "  Found: X" {
		t.Fatalf("answer %v seen %v", answer, seen)
	}
}

func TestPromptYesNoLinePromptRetriesAndReadsEOFAsDefault(t *testing.T) {
	t.Setenv("ICEBATH_NONINTERACTIVE", "")
	var out bytes.Buffer
	restore := WithSetupOutput(&out)
	defer restore()
	setupConsoleMu.Lock()
	setupConsole.In = strings.NewReader("maybe\nYES\n")
	setupConsole.StdinTTY = func() bool { return false }
	setupConsoleMu.Unlock()
	if !PromptYesNo("Continue?", false, "") {
		t.Fatal("yes not read")
	}
	if !strings.Contains(out.String(), "Please enter 'y' or 'n'") || !strings.Contains(out.String(), "Continue? [y/N]: ") {
		t.Fatalf("output %q", out.String())
	}
	setupConsoleMu.Lock()
	setupConsole.In = strings.NewReader("")
	setupConsoleMu.Unlock()
	if !PromptYesNo("Continue?", true, "") {
		t.Fatal("EOF did not answer the default")
	}
}

func TestPromptChoiceKeepsTheDefaultOnCancelAndSkip(t *testing.T) {
	out := captureSetupOutput(t)
	stubSetupRadiolist(t, func(string, []string, int, string) int { return -1 })
	if PromptChoice("Pick", []string{"a", "b"}, 1, "") != 1 {
		t.Fatal("cancel changed the default")
	}
	stubSetupRadiolist(t, func(string, []string, int, string) int { return 1 })
	if PromptChoice("Pick", []string{"a", "b"}, 1, "") != 1 || !strings.Contains(out.String(), "Skipped (keeping current)") {
		t.Fatalf("skip output %q", out.String())
	}
	stubSetupRadiolist(t, func(string, []string, int, string) int { return 0 })
	if PromptChoice("Pick", []string{"a", "b"}, 1, "") != 0 {
		t.Fatal("pick not returned")
	}
}

func TestRunSetupActionWithNavigationCancelPrintsTheMessage(t *testing.T) {
	out := captureSetupOutput(t)
	stubSetupRadiolist(t, func(string, []string, int, string) int { panic(setupCancelled{}) })
	err := RunSetupActionWithNavigation(context.Background(), "Model", func(context.Context) error {
		PromptChoice("Pick", []string{"a"}, 0, "")
		return nil
	}, "No change.")
	if err != nil || !strings.Contains(out.String(), "No change.") {
		t.Fatalf("err %v output %q", err, out.String())
	}
	if currentSetupNavigation() != nil {
		t.Fatal("navigation state leaked past the scope")
	}
}

func TestRunSetupStepsLeftArrowReplaysEarlierChoices(t *testing.T) {
	out := captureSetupOutput(t)
	var visits []string
	stubSetupRadiolist(t, func(question string, choices []string, def int, description string) int {
		start, err := handleSetupMenuNavigation(cliui.NavEventBegin)
		if err != nil {
			t.Fatal(err)
		}
		if start.Replay {
			_, _ = handleSetupMenuNavigation(cliui.NavEventResolve, start.ReplayValue)
			visits = append(visits, "replay:"+question)
			return start.ReplayValue.(int)
		}
		visits = append(visits, question)
		if question == "third" && len(visits) == 3 {
			panic(setupGoBack{promptIndex: 2})
		}
		idx := len(visits) % 2
		_, _ = handleSetupMenuNavigation(cliui.NavEventResolve, idx)
		return idx
	})
	err := runSetupStepsWithNavigation([]SetupStep{{Label: "Provider", Action: func() error {
		setupRadiolist("first", []string{"a", "b"}, 0, "")
		setupRadiolist("second", []string{"a", "b"}, 0, "")
		setupRadiolist("third", []string{"a", "b"}, 0, "")
		return nil
	}}}, "cancelled")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(visits, " ") != "first second third replay:first second third" {
		t.Fatalf("visits %v", visits)
	}
	if !strings.Contains(out.String(), "Returning to the previous choice in Provider...") {
		t.Fatalf("output %q", out.String())
	}
}

func TestRunSetupStepsBackFromTheFirstChoiceReturnsToThePreviousSection(t *testing.T) {
	out := captureSetupOutput(t)
	runs := map[string]int{}
	err := runSetupStepsWithNavigation([]SetupStep{
		{Label: "One", Action: func() error { runs["one"]++; return nil }},
		{Label: "Two", Action: func() error {
			runs["two"]++
			if runs["two"] == 1 {
				panic(setupGoBack{promptIndex: 0})
			}
			return nil
		}},
	}, "cancelled")
	if err != nil || runs["one"] != 2 || runs["two"] != 2 {
		t.Fatalf("err %v runs %v", err, runs)
	}
	if !strings.Contains(out.String(), "Returning to One...") {
		t.Fatalf("output %q", out.String())
	}
}

func TestRunSetupStepsPropagatesActionErrors(t *testing.T) {
	want := errors.New("boom")
	err := runSetupStepsWithNavigation([]SetupStep{{Label: "X", Action: func() error { return want }}}, "cancelled")
	if !errors.Is(err, want) {
		t.Fatalf("err %v", err)
	}
}

func TestSetupPrintHelpersFormatLikeHermes(t *testing.T) {
	out := captureSetupOutput(t)
	PrintSetupHeader("Models", true)
	SetupInfo("first", nil, "second")
	SetupSectionRule("Keys")
	got := out.String()
	for _, want := range []string{"\n\n◆ Models\n", "  first\n\n  second\n", "\n  ─── Keys ───\n\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q missing from %q", want, got)
		}
	}
}

func TestSanitizePastedInputStripsBracketedPasteMarkers(t *testing.T) {
	if got := sanitizePastedInput("\x1b[200~sk-abc\x1b[201~"); got != "sk-abc" {
		t.Fatalf("got %q", got)
	}
}
