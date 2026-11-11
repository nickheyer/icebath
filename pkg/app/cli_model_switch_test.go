package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

var pickerModels = []string{
	"anthropic/claude-opus-4.8",
	"anthropic/claude-sonnet-4.6",
	"anthropic/claude-haiku-4.5",
	"openai/gpt-5.5",
	"x-ai/grok-4.6",
	"deepseek/deepseek-v4-flash",
}

// Every model paired with its own index
func allPickerPairs() []pickerEntry {
	out := make([]pickerEntry, len(pickerModels))
	for i, m := range pickerModels {
		out[i] = pickerEntry{Index: i, Model: m}
	}
	return out
}

func TestEmptyQueryReturnsAllWithOriginalIndices(t *testing.T) {
	if got := bareCLI().filterModelPickerEntries(pickerModels, ""); !reflect.DeepEqual(got, allPickerPairs()) {
		t.Fatalf("pairs %v", got)
	}
}

func TestFilterNarrowsAndPreservesOriginalIndex(t *testing.T) {
	got := bareCLI().filterModelPickerEntries(pickerModels, "grok")
	if !reflect.DeepEqual(got, []pickerEntry{{Index: 4, Model: "x-ai/grok-4.6"}}) || pickerModels[got[0].Index] != got[0].Model {
		t.Fatalf("pairs %v", got)
	}
}

func TestSubsequenceMatchCaseInsensitive(t *testing.T) {
	for _, p := range bareCLI().filterModelPickerEntries(pickerModels, "CS46") {
		if p.Model == "anthropic/claude-sonnet-4.6" {
			return
		}
	}
	t.Fatal("subsequence missed")
}

func TestNoMatchReturnsEmpty(t *testing.T) {
	if got := bareCLI().filterModelPickerEntries(pickerModels, "zzzznope"); len(got) != 0 {
		t.Fatalf("pairs %v", got)
	}
}

func TestFilterDoesNotReorderOrPickADefault(t *testing.T) {
	got := bareCLI().filterModelPickerEntries(pickerModels, "claude")
	want := []pickerEntry{{0, "anthropic/claude-opus-4.8"}, {1, "anthropic/claude-sonnet-4.6"}, {2, "anthropic/claude-haiku-4.5"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs %v", got)
	}
}

func TestWhitespaceQueryIsTreatedAsEmpty(t *testing.T) {
	if got := bareCLI().filterModelPickerEntries(pickerModels, "   "); !reflect.DeepEqual(got, allPickerPairs()) {
		t.Fatalf("pairs %v", got)
	}
}

// Viewport with the default panel chrome of the picker helper
func pickerViewport(selected, offset, n, rows int) (int, int) {
	return computeModelPickerViewport(selected, offset, n, rows, 6)
}

func TestShortListNoScroll(t *testing.T) {
	if offset, visible := pickerViewport(0, 0, 5, 30); offset != 0 || visible != 5 {
		t.Fatalf("offset %d visible %d", offset, visible)
	}
}

func TestOffsetRecoversAfterStageSwitch(t *testing.T) {
	offset, visible := pickerViewport(0, 25, 36, 30)
	if offset != 0 || visible <= 0 {
		t.Fatalf("offset %d visible %d", offset, visible)
	}
}

func TestFullNavigationKeepsCursorVisible(t *testing.T) {
	offset := 0
	var cursors []int
	for i := 0; i < 36; i++ {
		cursors = append(cursors, i)
	}
	for i := 35; i >= 0; i-- {
		cursors = append(cursors, i)
	}
	for _, cursor := range cursors {
		var visible int
		offset, visible = pickerViewport(cursor, offset, 36, 30)
		if cursor < offset || cursor >= offset+visible {
			t.Fatalf("cursor=%d out of view: offset=%d visible=%d", cursor, offset, visible)
		}
	}
}

func TestPromptToolkitModelPickerDefersConfirmationOffKeyHandler(t *testing.T) {
	recordPrints(t)
	result := provider.ModelSwitchResult{Success: true, NewModel: "openai/gpt-5.5-pro", TargetProvider: "nous"}
	savedResolve, savedStart := switchModelResolve, startSwitchWorker
	t.Cleanup(func() { switchModelResolve, startSwitchWorker = savedResolve, savedStart })
	switchModelResolve = func(context.Context, provider.SwitchModelOptions) (provider.ModelSwitchResult, error) {
		return result, nil
	}
	var started func()
	startSwitchWorker = func(target func()) { started = target }
	c := bareCLI()
	c.app = &fakeTUIApp{}
	c.provider, c.model = "nous", "openai/gpt-5.5"
	c.modelPickerState = map[string]any{"stage": "model", "provider_data": map[string]any{"slug": "nous"},
		"model_list": []string{"openai/gpt-5.5-pro"}, "selected": 0, "user_provs": nil, "custom_provs": nil}
	if err := c.handleModelPickerSelection(true); err != nil {
		t.Fatal(err)
	}
	if c.modelPicker()["stage"] != "reasoning" || started != nil {
		t.Fatalf("state %v started %v", c.modelPicker(), started != nil)
	}
	c.modelPicker()["selected"] = len(pickerReasoningRows()) - 1
	if err := c.handleModelPickerSelection(true); err != nil {
		t.Fatal(err)
	}
	if c.modelPickerState != nil || started == nil {
		t.Fatalf("picker %v started %v", c.modelPickerState, started != nil)
	}
}
