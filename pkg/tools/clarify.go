package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	clarifyMaxChoices     = 4
	clarifyMaxQuestions   = 5
	clarifyMaxChoiceChars = 8000
	clarifyRecommended    = "(Recommended)"
	clarifyUnavailable    = "Clarify tool is not available in this execution context."
	clarifyShape          = "Pass questions=[{question, choices?, multi_select?}]; a single question is a one-entry array."
)

// clarifyQuestion is one normalized question with its stable wire id and decorated choices
type clarifyQuestion struct {
	QID            string
	Question       string
	Choices        []string
	ChoicesOffered []string
	MultiSelect    bool
}

// MarkRecommended suffixes the first of two or more choices with the label, idempotently
func MarkRecommended(choices []string) []string {
	if len(choices) < 2 {
		return choices
	}
	first := strings.TrimSpace(choices[0])
	if first != StripRecommended(first) {
		return choices
	}
	out := append([]string{first + " " + clarifyRecommended}, choices[1:]...)
	return out
}

// StripRecommended removes the recommendation label so presentation never leaks into user_response
func StripRecommended(text string) string {
	stripped := strings.TrimSpace(text)
	if strings.HasSuffix(strings.ToLower(stripped), strings.ToLower(clarifyRecommended)) {
		return strings.TrimSpace(stripped[:len(stripped)-len(clarifyRecommended)])
	}
	return stripped
}

// Strips presentation from a locked answer, a multi select answer being a list, a JSON array string, or one typed answer
func clarifyCleanAnswer(raw any, multi bool) any {
	if !multi {
		return StripRecommended(fmt.Sprint(raw))
	}
	var items []any
	switch v := raw.(type) {
	case string:
		decoded, err := pyjson.DecodeWTF8([]byte(v))
		if parsed, isList := decoded.([]any); err == nil && isList {
			items = parsed
		} else {
			items = []any{v}
		}
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		items = []any{raw}
	}
	out := []string{}
	for _, item := range items {
		if strings.TrimSpace(fmt.Sprint(item)) != "" {
			out = append(out, StripRecommended(fmt.Sprint(item)))
		}
	}
	return out
}

// Validates the questions argument into normalized entries, the error text when invalid
func clarifyNormalizeQuestions(questions any) ([]clarifyQuestion, string) {
	list, ok := questions.([]any)
	if !ok || len(list) == 0 {
		return nil, "questions must be a non-empty array. " + clarifyShape
	}
	if len(list) > clarifyMaxQuestions {
		return nil, fmt.Sprintf("questions supports at most %d items.", clarifyMaxQuestions)
	}
	out := make([]clarifyQuestion, 0, len(list))
	for index, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("questions[%d] must be an object. %s", index, clarifyShape)
		}
		text := ""
		if truthy(m["question"]) {
			text = strings.TrimSpace(fmt.Sprint(m["question"]))
		}
		if text == "" {
			return nil, fmt.Sprintf("questions[%d].question must be non-empty text.", index)
		}
		var cleaned []string
		if rawChoices, present := m["choices"]; present && rawChoices != nil {
			choices, ok := rawChoices.([]any)
			if !ok {
				return nil, fmt.Sprintf("questions[%d].choices must be a list of strings.", index)
			}
			strs := make([]string, 0, len(choices))
			for _, c := range choices {
				s, ok := c.(string)
				if !ok {
					return nil, fmt.Sprintf("questions[%d].choices must be a list of strings.", index)
				}
				strs = append(strs, s)
			}
			for pos, choice := range strs {
				if n := len([]rune(strings.TrimSpace(choice))); n > clarifyMaxChoiceChars {
					return nil, fmt.Sprintf("questions[%d].choices[%d] is %d characters; the limit is %d. Move detail into the question text and resend.", index, pos, n, clarifyMaxChoiceChars)
				}
			}
			for _, c := range strs {
				if t := strings.TrimSpace(c); t != "" {
					cleaned = append(cleaned, t)
				}
			}
			if len(cleaned) > clarifyMaxChoices {
				cleaned = cleaned[:clarifyMaxChoices]
			}
			if len(strs) > 0 && len(cleaned) == 0 {
				return nil, fmt.Sprintf("questions[%d].choices has %d entries but all are blank. Send real labels, or omit choices to ask open-ended.", index, len(strs))
			}
		}
		q := clarifyQuestion{QID: fmt.Sprintf("q%d", index), Question: text, MultiSelect: truthy(m["multi_select"]) && len(cleaned) > 0}
		if len(cleaned) > 0 {
			q.ChoicesOffered = append([]string(nil), cleaned...)
			q.Choices = MarkRecommended(append([]string(nil), cleaned...))
		}
		out = append(out, q)
	}
	return out, ""
}

// Status and cleaned value of one answer keyed by qid
func clarifyResponseStatus(qid string, answers map[string]any, multi bool) (string, any) {
	raw, present := answers[qid]
	var cleaned any
	if present && raw != nil && raw != "" {
		cleaned = clarifyCleanAnswer(raw, multi)
	}
	switch v := cleaned.(type) {
	case string:
		if v != "" {
			return "answered", v
		}
	case []string:
		if len(v) > 0 {
			return "answered", v
		}
	}
	if present {
		return "skipped", nil
	}
	return "unanswered", nil
}

// ClarifyReply is what a surface returns for one clarify call
type ClarifyReply struct {
	Answers map[string]any
	Outcome string
	Notice  string
}

// Result JSON from a reply, mapping picked display copy choices back to the raw choice by position
func clarifyResult(normalized []clarifyQuestion, reply ClarifyReply, shown []clarifyQuestion) string {
	if shown == nil {
		shown = normalized
	}
	answers := reply.Answers
	if answers == nil {
		answers = map[string]any{}
	}
	responses := make([]any, 0, len(normalized))
	for i, entry := range normalized {
		seen := shown[i]
		status, value := clarifyResponseStatus(entry.QID, answers, entry.MultiSelect)
		back := map[string]string{}
		for j, s := range seen.ChoicesOffered {
			if j < len(entry.ChoicesOffered) {
				back[s] = entry.ChoicesOffered[j]
			}
		}
		if len(back) > 0 && value != nil {
			switch v := value.(type) {
			case []string:
				mapped := make([]string, len(v))
				for j, s := range v {
					mapped[j] = mapBack(back, s)
				}
				value = mapped
			case string:
				value = mapBack(back, v)
			}
		}
		var offered any
		if entry.ChoicesOffered != nil {
			offered = entry.ChoicesOffered
		}
		responses = append(responses, map[string]any{"question": entry.Question, "choices_offered": offered, "status": status, "user_response": value})
	}
	result := map[string]any{"responses": responses, "outcome": reply.Outcome}
	if reply.Notice != "" {
		result["notice"] = reply.Notice
	}
	return jsonCompact(result)
}

// Raw choice for a display copy, the value itself when unmapped
func mapBack(back map[string]string, v string) string {
	if raw, ok := back[v]; ok {
		return raw
	}
	return v
}

// Prose display copies of texts, redacted and kept distinct when two redact alike
func clarifyProse(env *Env, texts []string) []string {
	if texts == nil {
		return nil
	}
	out := make([]string, 0, len(texts))
	for _, t := range texts {
		text := redactApprovalDisplay(env, t, true)
		label, n := text, 1
		for containsString(out, label) {
			n++
			label = fmt.Sprintf("%s (%d)", text, n)
		}
		out = append(out, label)
	}
	return out
}

// Asks one to five questions through the session Asker and returns the result JSON
func clarifyTool(ctx context.Context, env *Env, questions any) string {
	normalized, errText := clarifyNormalizeQuestions(questions)
	if errText != "" {
		return toolErrorJSON(errText, nil)
	}
	if env == nil || env.Ask == nil {
		return toolErrorJSON(clarifyUnavailable, nil)
	}
	shown := make([]clarifyQuestion, len(normalized))
	prompts := make([]string, 0, len(normalized))
	for i, q := range normalized {
		offered := clarifyProse(env, q.ChoicesOffered)
		s := q
		s.Question = clarifyProse(env, []string{q.Question})[0]
		s.ChoicesOffered = offered
		s.Choices = nil
		if len(offered) > 0 {
			s.Choices = MarkRecommended(append([]string(nil), offered...))
		}
		shown[i] = s
		prompts = append(prompts, s.Question)
	}
	human := humanInputRequest(env, "clarify", strings.Join(prompts, "\n"), "")
	form := make([]Question, len(shown))
	for i, s := range shown {
		form[i] = Question{Header: s.QID, Text: s.Question, Choices: s.Choices, MultiSelect: s.MultiSelect}
	}
	result, err := env.Ask.Ask(ctx, form)
	if err != nil {
		human.finish(env, "error")
		return toolErrorJSON("Failed to get user input: "+err.Error(), nil)
	}
	reply := clarifyReplyFromForm(shown, result)
	human.finish(env, reply.Outcome)
	return clarifyResult(normalized, reply, shown)
}

// Converts a form result into the answers keyed by qid that the result builder reads
func clarifyReplyFromForm(shown []clarifyQuestion, result FormResult) ClarifyReply {
	answers := map[string]any{}
	for i, q := range shown {
		if i >= len(result.Answers) {
			continue
		}
		a := result.Answers[i]
		switch a.Status {
		case "answered":
			if q.MultiSelect {
				answers[q.QID] = anyStringsOf(a.Selected)
			} else if a.Text != "" {
				answers[q.QID] = a.Text
			} else if len(a.Selected) > 0 {
				answers[q.QID] = a.Selected[0]
			} else {
				answers[q.QID] = nil
			}
		case "skipped":
			answers[q.QID] = nil
		}
	}
	return ClarifyReply{Answers: answers, Outcome: result.Outcome, Notice: result.Notice}
}

// Strings as a generic list the answer cleaner accepts
func anyStringsOf(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

var clarifySchema = map[string]any{
	"name": "clarify",
	"description": "Ask the user one or more questions when you need a decision or feedback. " +
		"Pass every question " +
		fmt.Sprintf("in `questions` (1-%d entries) — a single question is a ", clarifyMaxQuestions) +
		"one-entry array, and several INDEPENDENT questions belong in ONE " +
		"call (one form beats a chain of clarify calls; if one answer would " +
		"change another question, ask separately). Per question: " +
		fmt.Sprintf("single-select (up to %d choices — put your recommended ", clarifyMaxChoices) +
		"option FIRST, the UI marks it '(Recommended)' and auto-appends an " +
		"'Other' free-text row), multi-select (multi_select=true), or " +
		"open-ended (omit choices). Options go ONLY in `choices`, never " +
		"enumerated inside the question text (choices render as pickable " +
		"rows; options written into the question are dead prose the user " +
		"can't click). Result: {responses: [...], outcome} in question order. " +
		"Each response has status answered, skipped or unanswered " +
		"(user_response is null unless answered); outcome is submitted, " +
		"cancelled, timed_out or undelivered, with a notice saying why " +
		"when the wait ended without a submit. Prefer deciding " +
		"low-stakes questions yourself; don't use this for dangerous-command " +
		"confirmation (the terminal tool handles that).",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"questions": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": clarifyMaxQuestions,
				"description": "The question(s). Each: question text (options excluded), " +
					"optional choices (recommended first; omit for free-text), " +
					"optional multi_select. Responses come back in question " +
					"order with the question text echoed.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question":     map[string]any{"type": "string"},
						"choices":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": clarifyMaxChoices},
						"multi_select": map[string]any{"type": "boolean"},
					},
					"required": []any{"question"},
				},
			},
		},
		"required": []any{"questions"},
	},
}

// Registers the clarify tool, always available
func registerClarifyTool() {
	Register(Def{Name: "clarify", Toolset: "clarify", Schema: clarifySchema, Emoji: "❓",
		Check: func(*Env) bool { return true },
		Handler: func(ctx context.Context, call *Call) string {
			return clarifyTool(ctx, call.Env, call.Args["questions"])
		}})
}
