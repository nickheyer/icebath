package plugins

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
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
	if _, ok, _ := AskChoice(WithRunningLoop(typed("default", true)), "q", testChoices); ok {
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

const pickPlugin = `
import json
def register(ctx):
    choices = %s
    ctx.register_command('delegation', lambda arg: 'MODE=%%s' %% ctx.ask_choice('Delegation mode', choices))
    ctx.register_command('gwdelegation', lambda arg: 'MODE=' + str(ctx.ask_choice('Delegation mode', choices)), description='Pick the delegation mode')
    def legacy(raw_args):
        return json.dumps([raw_args])
    def named(raw_args, *, session_id, platform):
        return json.dumps([raw_args, session_id, platform])
    def catch_all(args, **kwargs):
        return json.dumps([args, kwargs])
    def positional_only(raw_args, session_id=None, /):
        return json.dumps([raw_args, session_id])
    def first_named_source(source, **kwargs):
        return json.dumps([source, sorted(kwargs)])
    def first_positional_only(source, /, **kwargs):
        return json.dumps([source, kwargs["source"]])
    def switch(raw_args, session_key=""):
        return raw_args.strip() + "|" + session_key
    for fn in (legacy, named, catch_all, positional_only, first_named_source, first_positional_only, switch):
        ctx.register_command(fn.__name__.replace("_", "-"), fn)
`

func jsonEqual(t *testing.T, got any, want string) bool {
	t.Helper()
	s, ok := got.(string)
	if !ok {
		return false
	}
	var a, b any
	if json.Unmarshal([]byte(s), &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}
