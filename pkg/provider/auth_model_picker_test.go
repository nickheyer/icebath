package provider

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Replaces the picker terminal seams for the test and restores them at cleanup
func scriptPickerTerminal(t *testing.T, radiolist func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error), answers []string, line func(string) (string, error), warnings []SelectionWarning) *[]string {
	t.Helper()
	prevRadio, prevInput, prevLine, prevWarn := pickerRadiolistFn, pickerInputFn, pickerLineInputFn, pickerSelectionWarningsFn
	prompts := &[]string{}
	pickerRadiolistFn = radiolist
	pickerInputFn = func(out io.Writer, prompt string) (string, error) {
		*prompts = append(*prompts, prompt)
		if len(answers) == 0 {
			return "", io.EOF
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
	pickerLineInputFn = line
	pickerSelectionWarningsFn = func(env PoolEnv, modelID, provider, baseURL, apiKey string, includeKinds []string) ([]SelectionWarning, error) {
		return warnings, nil
	}
	t.Cleanup(func() {
		pickerRadiolistFn, pickerInputFn, pickerLineInputFn, pickerSelectionWarningsFn = prevRadio, prevInput, prevLine, prevWarn
	})
	return prompts
}

var errPickerNoMenu = errors.New("curses menu unavailable")

func pickerNoMenu(string, [][]PickerSegment, PickerRadiolistOptions) (int, error) {
	return 0, errPickerNoMenu
}

func editedModelLine(prompt string) (string, error) {
	if prompt == "Enter model name: " {
		return "vendor/edited-model", nil
	}
	return "", nil
}

func TestPickerCustomEntryIsAlwaysOffered(t *testing.T) {
	var captured [][]PickerSegment
	scriptPickerTerminal(t, func(title string, choices [][]PickerSegment, opts PickerRadiolistOptions) (int, error) {
		captured = choices
		return -1, nil
	}, nil, editedModelLine, nil)
	if got, ok := promptModelSelection(PoolEnv{}, io.Discard, ModelSelectionPrompt{ModelIDs: []string{"a/m1", "b/m2"}}); ok || got != "" {
		t.Fatalf("cancel returned %q", got)
	}
	found := false
	for _, c := range captured {
		found = found || (len(c) == 1 && c[0].Text == pickerCustomLabel)
	}
	if !found {
		t.Fatalf("choices %v", captured)
	}
}

func TestPickerPromptUsesLineEditorForCustomModel(t *testing.T) {
	scriptPickerTerminal(t, func(_ string, choices [][]PickerSegment, _ PickerRadiolistOptions) (int, error) {
		return len(choices) - 2, nil
	}, nil, editedModelLine, nil)
	if got, ok := promptModelSelection(PoolEnv{}, io.Discard, ModelSelectionPrompt{ModelIDs: []string{"vendor/default-model"}}); !ok || got != "vendor/edited-model" {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestPromptModelSelectionRequiresExpensiveConfirmation(t *testing.T) {
	scriptPickerTerminal(t, pickerNoMenu, []string{"1", "n"}, editedModelLine,
		[]SelectionWarning{{Kind: "cost", Message: "EXPENSIVE MODEL WARNING"}})
	var out bytes.Buffer
	if got, ok := promptModelSelection(PoolEnv{}, &out, ModelSelectionPrompt{ModelIDs: []string{"openai/gpt-5.5-pro"}, ConfirmProvider: "openrouter"}); ok || got != "" {
		t.Fatalf("declined guard returned %q", got)
	}
	if !strings.Contains(out.String(), "EXPENSIVE MODEL WARNING") {
		t.Fatalf("output %q", out.String())
	}
}

func TestPromptModelSelectionFallbackUsesLineEditorForCustomModel(t *testing.T) {
	scriptPickerTerminal(t, pickerNoMenu, []string{"2"}, editedModelLine, nil)
	if got, ok := promptModelSelection(PoolEnv{}, io.Discard, ModelSelectionPrompt{ModelIDs: []string{"vendor/default-model"}}); !ok || got != "vendor/edited-model" {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestSaveModelChoiceStringModelBecomesDict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model: some-old-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := ModelChoiceSaveCount()
	if err := saveModelChoice(PoolEnv{}, "kimi-k2.5"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ReadUser(dir)
	if err != nil {
		t.Fatal(err)
	}
	model, ok := cfg["model"].(map[string]any)
	if !ok || model["default"] != "kimi-k2.5" || ModelChoiceSaveCount() != before+1 {
		t.Fatalf("model %#v", cfg["model"])
	}
}
