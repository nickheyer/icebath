package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tools"
)

const (
	maxQuestions   = 5
	maxChoices     = 4
	maxChoiceChars = 8000
	recommended    = "(Recommended)"
	shape          = "Pass questions=[{question, choices?, multi_select?}]; a single question is a one-entry array."
)

const description = "Ask the user one or more questions when you need a decision or feedback. Pass every question in `questions` (1-5 entries) — a " +
	"single question is a one-entry array, and several INDEPENDENT questions belong in ONE call (one form beats a chain of clarify calls; if one " +
	"answer would change another question, ask separately). Per question: single-select (up to 4 choices — put your recommended option FIRST, the " +
	"UI marks it '(Recommended)' and auto-appends an 'Other' free-text row), multi-select (multi_select=true), or open-ended (omit choices). " +
	"Options go ONLY in `choices`, never enumerated inside the question text (choices render as pickable rows; options written into the question " +
	"are dead prose the user can't click). Result: {responses: [...], outcome} in question order. Each response has status answered, skipped or " +
	"unanswered (user_response is null unless answered); outcome is submitted, cancelled, timed_out or undelivered, with a notice saying why when " +
	"the wait ended without a submit. Prefer deciding low-stakes questions yourself; don't use this for dangerous-command confirmation (the " +
	"terminal tool handles that)."

type registrant struct{}

func Register(registry *tools.Registry) {
	registry.Register(registrant{})
}

func (registrant) Tools() []tools.Def {
	return []tools.Def{{
		Name:    "clarify",
		Toolset: "clarify",
		Title:   "Ask",
		Icon:    tools.IconAsk,
		Schema: tools.Schema(description, []string{"questions"}, map[string]any{
			"questions": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": maxQuestions,
				"description": "The question(s). Each: question text (options excluded), optional choices (recommended first; omit for free-text), " +
					"optional multi_select. Responses come back in question order with the question text echoed.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question":     map[string]any{"type": "string"},
						"choices":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": maxChoices},
						"multi_select": map[string]any{"type": "boolean"},
					},
					"required": []string{"question"},
				},
			},
		}),
		Handler: tools.Bind(clarify),
	}}
}

// One question as the model asked it, its choices cleaned
type question struct {
	id    string
	text  string
	multi bool
	// Choices as the model sent them, trimmed, blank ones dropped
	offered []string
}

func clarify(ctx context.Context, call *tools.Call, args struct {
	Questions any `json:"questions"`
}) (string, error) {
	asked, err := normalize(args.Questions)
	if err != nil {
		return "", err
	}
	if call.Env.Clarify == nil {
		return "", errors.New("Clarify tool is not available in this execution context.")
	}
	form := make([]tools.Question, len(asked))
	for i, q := range asked {
		form[i] = tools.Question{ID: q.id, Text: tools.Visible(q.text), MultiSelect: q.multi, Choices: labels(q.offered)}
	}
	reply, err := call.Env.Clarify(ctx, form)
	if err != nil {
		return "", fmt.Errorf("Failed to get user input: %w", err)
	}
	return result(asked, form, reply), nil
}

// Validates the questions argument, the error telling the model how to fix it
func normalize(raw any) ([]question, error) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, errors.New("questions must be a non-empty array. " + shape)
	}
	if len(list) > maxQuestions {
		return nil, fmt.Errorf("questions supports at most %d items.", maxQuestions)
	}
	out := make([]question, 0, len(list))
	for index, item := range list {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("questions[%d] must be an object. %s", index, shape)
		}
		text := strings.TrimSpace(textOf(fields["question"]))
		if text == "" {
			return nil, fmt.Errorf("questions[%d].question must be non-empty text.", index)
		}
		q := question{id: fmt.Sprintf("q%d", index), text: text}
		if rawChoices := fields["choices"]; rawChoices != nil {
			items, ok := rawChoices.([]any)
			if !ok {
				return nil, fmt.Errorf("questions[%d].choices must be a list of strings.", index)
			}
			choices := make([]string, 0, len(items))
			for _, item := range items {
				choice, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("questions[%d].choices must be a list of strings.", index)
				}
				choices = append(choices, choice)
			}
			for at, choice := range choices {
				if n := utf8.RuneCountInString(strings.TrimSpace(choice)); n > maxChoiceChars {
					return nil, fmt.Errorf("questions[%d].choices[%d] is %d characters; the limit is %d. Move detail into the question text and resend.", index, at, n, maxChoiceChars)
				}
			}
			for _, choice := range choices {
				if choice = strings.TrimSpace(choice); choice != "" && len(q.offered) < maxChoices {
					q.offered = append(q.offered, choice)
				}
			}
			if len(choices) > 0 && len(q.offered) == 0 {
				return nil, fmt.Errorf("questions[%d].choices has %d entries but all are blank. Send real labels, or omit choices to ask open-ended.", index, len(choices))
			}
		}
		multi, _ := fields["multi_select"].(bool)
		q.multi = multi && len(q.offered) > 0
		out = append(out, q)
	}
	return out, nil
}

// Text of a question: a string as it is, anything else but null as its JSON
func textOf(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	}
	text, _ := json.Marshal(value)
	return string(text)
}

// Choices as the user sees them: made visible, kept distinct when two read alike, the first of several marked as recommended
func labels(offered []string) []string {
	if offered == nil {
		return nil
	}
	shown := make([]string, 0, len(offered))
	for _, choice := range offered {
		text := tools.Visible(choice)
		label := text
		for n := 2; slices.Contains(shown, label); n++ {
			label = fmt.Sprintf("%s (%d)", text, n)
		}
		shown = append(shown, label)
	}
	if len(shown) >= 2 && strip(shown[0]) == strings.TrimSpace(shown[0]) {
		shown[0] = strings.TrimSpace(shown[0]) + " " + recommended
	}
	return shown
}

// Choice without the recommendation label, so presentation never reaches the model
func strip(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasSuffix(strings.ToLower(text), strings.ToLower(recommended)) {
		return strings.TrimSpace(text[:len(text)-len(recommended)])
	}
	return text
}

type response struct {
	Question       string   `json:"question"`
	ChoicesOffered []string `json:"choices_offered"`
	Status         string   `json:"status"`
	UserResponse   any      `json:"user_response"`
}

// Result the model reads: every question in order with its status and answer, a picked label mapped back to the choice the model sent
func result(asked []question, form []tools.Question, reply tools.Reply) string {
	responses := make([]response, len(asked))
	for i, q := range asked {
		back := map[string]string{}
		for at, label := range form[i].Choices {
			back[strip(label)] = q.offered[at]
		}
		pick := func(value string) string {
			value = strip(value)
			if choice, ok := back[value]; ok {
				return choice
			}
			return value
		}
		entry := response{Question: q.text, ChoicesOffered: q.offered, Status: "unanswered"}
		values, present := reply.Answers[q.id]
		var picked []string
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				picked = append(picked, pick(value))
			}
		}
		switch {
		case len(picked) > 0 && q.multi:
			entry.Status, entry.UserResponse = "answered", picked
		case len(picked) > 0:
			entry.Status, entry.UserResponse = "answered", picked[0]
		case present:
			entry.Status = "skipped"
		}
		responses[i] = entry
	}
	body, _ := json.Marshal(struct {
		Responses []response `json:"responses"`
		Outcome   string     `json:"outcome"`
		Notice    string     `json:"notice,omitempty"`
	}{responses, reply.Outcome, reply.Notice})
	return string(body)
}
