package plugins

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
)

var testChoices = []any{
	map[string]any{"value": "default", "label": "Default", "description": "delegation.model / parent model"},
	map[string]any{"value": "model_map", "label": "Model routes"}, "profile_routes",
}

var testChoiceLabels = []any{"Default — delegation.model / parent model", "Model routes", "profile_routes"}

// Clarify panel answering with the label at pick and recording what it showed
func choicePanel(pick int) (func([]any) map[string]any, *[][]any) {
	shown := &[][]any{}
	return func(questions []any) map[string]any {
		*shown = append(*shown, questions)
		q := questions[0].(map[string]any)
		return map[string]any{"answers": map[string]any{q["qid"].(string): q["choices"].([]any)[pick]}, "outcome": "submitted"}
	}, shown
}

// test_a_described_choice_still_accepts_its_plain_label
func TestAskChoiceDescribedChoiceAcceptsPlainLabel(t *testing.T) {
	ctx := BindAsker(context.Background(), func(string, []string) (string, bool) { return "model routes", true })
	got, ok, err := AskChoice(ctx, "Delegation mode", []any{map[string]any{"value": "model_map", "label": "Model routes", "description": "a model per task"}})
	if err != nil || !ok || got != "model_map" {
		t.Fatalf("got %q %v %v", got, ok, err)
	}
}

// Panel questions, typed values, cancels, the event loop refusal and malformed choices of plugin_choices
func TestAskChoiceThroughTheClarifyPanel(t *testing.T) {
	panel, shown := choicePanel(1)
	ctx := BindAsker(context.Background(), ClarifyAsker(panel))
	if !AskerBound(ctx) || AskerBound(context.Background()) {
		t.Fatal("asker binding")
	}
	got, ok, err := AskChoice(ctx, "Delegation mode", testChoices)
	want := [][]any{{map[string]any{"qid": "q0", "question": "Delegation mode", "choices": testChoiceLabels, "multi_select": false}}}
	if err != nil || !ok || got != "model_map" || !reflect.DeepEqual(*shown, want) {
		t.Fatalf("got %q %v %v shown %v", got, ok, err, *shown)
	}
	if _, ok, _ := AskChoice(context.Background(), "q", testChoices); ok {
		t.Fatal("no asker must answer None")
	}
	typed := func(answer string, ok bool) context.Context {
		return BindAsker(context.Background(), func(string, []string) (string, bool) { return answer, ok })
	}
	for answer, want := range map[string]string{" PROFILE_ROUTES ": "profile_routes", "default": "default", "Default — delegation.model / parent model": "default", "nope": "", "": ""} {
		got, ok, _ := AskChoice(typed(answer, true), "q", testChoices)
		if got != want || ok != (want != "") {
			t.Fatalf("typed %q got %q %v", answer, got, ok)
		}
	}
	if _, ok, _ := AskChoice(typed("", false), "q", testChoices); ok {
		t.Fatal("a cancelled panel must answer None")
	}
	if _, ok, _ := AskChoice(pyctx.WithRunningLoop(typed("default", true)), "q", testChoices); ok {
		t.Fatal("an event loop handler must answer None")
	}
	if _, ok, _ := AskChoice(typed("default", true), "q", nil); ok {
		t.Fatal("no options must answer None")
	}
	if _, _, err := AskChoice(typed("x", true), "q", []any{map[string]any{"label": "x"}}); err == nil || err.(*HostBuiltinError).Type != "KeyError" || err.Error() != "'value'" {
		t.Fatalf("missing value %v", err)
	}
	refused := ClarifyAsker(func([]any) map[string]any { return map[string]any{"answers": map[string]any{"q0": 3}} })
	if _, ok := refused("q", []string{"a"}); ok {
		t.Fatal("a non string answer is None")
	}
}

// Public entry of plugin_choices ask_choice for surfaces and tests outside a plugin context
func AskChoice(ctx context.Context, question any, choices []any) (string, bool, error) {
	return askChoice(ctx, question, choices)
}
