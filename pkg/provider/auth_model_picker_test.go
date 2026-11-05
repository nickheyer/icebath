package provider

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

func TestPickerWindowPlanMenuSectionsAndUnavailableFooter(t *testing.T) {
	installPickerEnglish(t)
	var title string
	var opts PickerRadiolistOptions
	scriptPickerTerminal(t, func(tl string, _ [][]PickerSegment, o PickerRadiolistOptions) (int, error) {
		title, opts = tl, o
		return 0, nil
	}, nil, editedModelLine, nil)
	usage := pickerWindowUsage(pickerKW{"featured_model_ids": []string{"example/model-a"}})
	got, ok := promptModelSelection(PoolEnv{}, io.Discard, ModelSelectionPrompt{ModelIDs: []string{"example/model-b", "example/model-a"},
		UnavailableModels: []string{"paid/x"}, ConfirmProvider: "nous", Usage: usage, CurrentModel: "example/model-b"})
	if !ok || got != "example/model-a" || title != pickerDefaultTitle {
		t.Fatalf("got %q %v title %q", got, ok, title)
	}
	if !reflect.DeepEqual(opts.Headings, map[int]string{0: SectionRecommended, 1: SectionOther}) || opts.Selected != 1 || opts.CancelReturns != -1 || !opts.Searchable {
		t.Fatalf("opts %+v", opts)
	}
	if !strings.Contains(opts.Description, "  ── Upgrade at "+DefaultNousPortalURL+" for paid models ──") || !strings.Contains(opts.Description, "Window Plan — every model below is included") {
		t.Fatalf("description %q", opts.Description)
	}
	if opts.SearchLabels[len(opts.SearchLabels)-2] != pickerCustomLabel || opts.SearchLabels[len(opts.SearchLabels)-1] != pickerSkipLabel {
		t.Fatalf("search labels %q", opts.SearchLabels)
	}
}

func TestPickerSaleRowsCarryStarPercentAndWasPrices(t *testing.T) {
	pricing := PricingTable{
		"sale/m": {"prompt": "0.000001", "completion": "0.000002", "original": map[string]any{"prompt": "0.000002", "completion": "0.000004"}},
		"full/m": {"prompt": "0.000003", "completion": "0.000006", "input_cache_read": "0.0000003"},
	}
	rows := newModelPickerRows([]string{"sale/m", "full/m"}, pricing, "full/m", true, nil, map[string]string{"sale/m": "usage credits"})
	segs := rows.segments("sale/m")
	if segs[0] != (PickerSegment{"★ ", "yellow"}) || segs[len(segs)-1] != (PickerSegment{"  · usage credits", "dim"}) {
		t.Fatalf("sale segments %v", segs)
	}
	label := rows.label("sale/m")
	if !strings.Contains(label, "  -50%") || !strings.Contains(label, "  was $2.00/$4.00") {
		t.Fatalf("sale label %q", label)
	}
	if !strings.HasSuffix(rows.label("full/m"), pickerCurrentSuffix) {
		t.Fatalf("current row %q", rows.label("full/m"))
	}
	header := rows.descriptionLines()[0]
	if !strings.Contains(header, "Cache  $/Mtok  ★ = on sale") {
		t.Fatalf("header %q", header)
	}
	ordered, sections, start := pickerOrder([]string{"a", "full/m", "a"}, "full/m", nil)
	if !reflect.DeepEqual(ordered, []string{"full/m", "a"}) || len(sections) != 0 || start != 0 {
		t.Fatalf("order %v %v %d", ordered, sections, start)
	}
}

func TestPromptModelSelectionRequiresExpensiveConfirmation(t *testing.T) {
	scriptPickerTerminal(t, pickerNoMenu, []string{"1", "n"}, editedModelLine,
		[]SelectionWarning{{Kind: "cost", Message: "EXPENSIVE MODEL WARNING"}})
	var out bytes.Buffer
	if got, ok := promptModelSelection(PoolEnv{}, &out, ModelSelectionPrompt{ModelIDs: []string{"openai/gpt-5.5-pro"}, ConfirmProvider: "nous"}); ok || got != "" {
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
