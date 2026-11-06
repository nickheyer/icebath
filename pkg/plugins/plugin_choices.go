package plugins

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Surface prompt taking a question and shown labels, returning the picked label or false
type ChoiceAsker func(question string, labels []string) (string, bool)

type choiceAskerKey struct{}

// Context whose AskChoice calls the asker binds, the surface that dispatches the command wraps its handler call
func BindAsker(ctx context.Context, asker ChoiceAsker) context.Context {
	return context.WithValue(ctx, choiceAskerKey{}, asker)
}

// Reports whether a surface bound an asker for this context
func AskerBound(ctx context.Context) bool { return boundAsker(ctx) != nil }

func boundAsker(ctx context.Context) ChoiceAsker {
	asker, _ := ctx.Value(choiceAskerKey{}).(ChoiceAsker)
	return asker
}

// Asker over a clarify tool callback taking questions and returning answers keyed by qid
func ClarifyAsker(callback func(questions []any) map[string]any) ChoiceAsker {
	return func(question string, labels []string) (string, bool) {
		shown := make([]any, len(labels))
		for i, label := range labels {
			shown[i] = label
		}
		reply := callback([]any{map[string]any{"qid": "q0", "question": question, "choices": shown, "multi_select": false}})
		answers, _ := reply["answers"].(map[string]any)
		answer, ok := answers["q0"].(string)
		return answer, ok
	}
}

type choiceOption struct{ value, label, shown string }

// Value, plain label and shown label per choice, a description riding on the shown label only
func choiceOptions(choices []any) ([]choiceOption, error) {
	options := make([]choiceOption, 0, len(choices))
	for _, choice := range choices {
		if s, ok := choice.(string); ok {
			options = append(options, choiceOption{s, s, s})
			continue
		}
		get, err := choiceGetter(choice)
		if err != nil {
			return nil, err
		}
		raw, ok := get("value")
		if !ok {
			return nil, &HostBuiltinError{Type: "KeyError", Message: "'value'"}
		}
		value := config.PyStr(raw)
		label := value
		if l, _ := get("label"); config.PyTruthy(l) {
			label = config.PyStr(l)
		}
		description := ""
		if d, _ := get("description"); config.PyTruthy(d) {
			description = pystr.Strip(config.PyStr(d))
		}
		shown := label
		if description != "" {
			shown = label + " — " + description
		}
		options = append(options, choiceOption{value, label, shown})
	}
	return options, nil
}

// Key lookup over the mapping shapes a choice arrives in, a TypeError for anything else
func choiceGetter(choice any) (func(string) (any, bool), error) {
	switch m := choice.(type) {
	case map[string]any:
		return func(k string) (any, bool) { v, ok := m[k]; return v, ok }, nil
	case interface{ Get(string) (any, bool) }:
		return m.Get, nil
	}
	return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s indices must be integers or slices, not str", pyrepr.TypeName(choice))}
}

// Picked choice value from the bound asker, false with no asker, on an event loop, on cancel or an unknown answer
func askChoice(ctx context.Context, question any, choices []any) (string, bool, error) {
	asker := boundAsker(ctx)
	options, err := choiceOptions(choices)
	if err != nil {
		return "", false, err
	}
	if asker == nil || len(options) == 0 {
		return "", false, nil
	}
	if runningLoop(ctx) {
		logWarn("ask_choice needs a sync slash-command handler: an async one would block the event loop that delivers the answer; returning None")
		return "", false, nil
	}
	labels := make([]string, len(options))
	for i, o := range options {
		labels[i] = o.shown
	}
	picked, _ := asker(config.PyStr(question), labels)
	typed := pystr.Casefold(pystr.Strip(picked))
	if typed == "" {
		return "", false, nil
	}
	for _, o := range options {
		if typed == pystr.Casefold(o.shown) || typed == pystr.Casefold(o.label) || typed == pystr.Casefold(o.value) {
			return o.value, true, nil
		}
	}
	return "", false, nil
}

// Public entry of plugin_choices ask_choice for surfaces and tests outside a plugin context
func AskChoice(ctx context.Context, question any, choices []any) (string, bool, error) {
	return askChoice(ctx, question, choices)
}
