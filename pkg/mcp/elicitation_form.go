package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fields one form question batch carries, the most a clarify form shows at once
const formBatchSize = 5

// Rounds a field may be asked again after an answer its schema refuses
const formFieldAttempts = 3

// One property of a requested schema with the choices its enum or titled one of offers
type formField struct {
	name     string
	spec     map[string]any
	required bool
	choices  []string
	consts   []any
	multi    bool
	pattern  *regexp2.Regexp
}

// Fields of a flat requested schema in the server's order, an invalid params error for a schema outside the primitive subset
func formFieldsOf(schema map[string]any, raw json.RawMessage) ([]*formField, *RPCError) {
	props, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	if list, ok := schema["required"].([]any); ok {
		for _, r := range list {
			if name, ok := r.(string); ok {
				required[name] = true
			}
		}
	}
	invalid := func(format string, args ...any) *RPCError {
		return &RPCError{Code: CodeInvalidParams, Message: "Invalid params: " + fmt.Sprintf(format, args...)}
	}
	var fields []*formField
	for _, name := range schemaPropertyOrder(props, raw) {
		spec, ok := props[name].(map[string]any)
		if !ok {
			return nil, invalid("property %q of requestedSchema is not an object", name)
		}
		f := &formField{name: name, spec: spec, required: required[name]}
		kind, _ := spec["type"].(string)
		switch kind {
		case "string", "number", "integer":
			if enum, ok := spec["enum"].([]any); ok {
				for _, v := range enum {
					f.choices, f.consts = append(f.choices, fmt.Sprint(v)), append(f.consts, v)
				}
			} else if one, ok := spec["oneOf"].([]any); ok {
				f.choices, f.consts = titledChoices(one)
			}
		case "boolean":
			f.choices, f.consts = []string{"yes", "no"}, []any{true, false}
		case "array":
			items, _ := spec["items"].(map[string]any)
			if enum, ok := items["enum"].([]any); ok {
				for _, v := range enum {
					f.choices, f.consts = append(f.choices, fmt.Sprint(v)), append(f.consts, v)
				}
			} else if options, ok := items["anyOf"].([]any); ok {
				f.choices, f.consts = titledChoices(options)
			}
			if len(f.choices) == 0 {
				return nil, invalid("array property %q of requestedSchema offers no enum items", name)
			}
			f.multi = true
		default:
			return nil, invalid("property %q of requestedSchema has type %q outside string, number, integer, boolean and enum array", name, kind)
		}
		if pattern, ok := spec["pattern"].(string); ok && pattern != "" {
			re, err := regexp2.Compile(pattern, regexp2.ECMAScript)
			if err != nil {
				return nil, invalid("pattern of property %q does not compile: %v", name, err)
			}
			f.pattern = re
		}
		fields = append(fields, f)
	}
	return fields, nil
}

// Titles and constants of a titled enum list
func titledChoices(options []any) ([]string, []any) {
	var titles []string
	var consts []any
	for _, o := range options {
		entry, _ := o.(map[string]any)
		if entry == nil {
			continue
		}
		title, _ := entry["title"].(string)
		if title == "" {
			title = fmt.Sprint(entry["const"])
		}
		titles, consts = append(titles, title), append(consts, entry["const"])
	}
	return titles, consts
}

// Question text of a field with its requirement, description, limits and default
func (f *formField) question(problem string) string {
	label := f.name
	if title, ok := f.spec["title"].(string); ok && title != "" {
		label = title + " (" + f.name + ")"
	}
	parts := []string{label}
	if f.required {
		parts[0] += ", required"
	}
	if desc, ok := f.spec["description"].(string); ok && desc != "" {
		parts = append(parts, desc)
	}
	var limits []string
	for _, key := range []string{"minLength", "maxLength", "minimum", "maximum", "minItems", "maxItems", "format"} {
		if v, ok := f.spec[key]; ok {
			limits = append(limits, fmt.Sprintf("%s %v", key, v))
		}
	}
	if pattern, ok := f.spec["pattern"].(string); ok && pattern != "" {
		limits = append(limits, "pattern "+pattern)
	}
	switch f.spec["type"] {
	case "integer":
		limits = append(limits, "a whole number")
	case "number":
		limits = append(limits, "a number")
	}
	if len(limits) > 0 {
		parts = append(parts, "("+strings.Join(limits, ", ")+")")
	}
	if def, ok := f.spec["default"]; ok {
		parts = append(parts, fmt.Sprintf("Default %v, used when you skip it.", def))
	}
	if problem != "" {
		parts = append(parts, "Your last answer was refused: "+problem)
	}
	return strings.Join(parts, " ")
}

// Value of one answer checked against the field schema, a refusal reason when it does not fit
func (f *formField) value(a tools.Answer) (any, string) {
	if f.multi {
		picked := a.Selected
		if len(picked) == 0 && strings.TrimSpace(a.Text) != "" {
			for _, part := range strings.Split(a.Text, ",") {
				picked = append(picked, strings.TrimSpace(part))
			}
		}
		out := []any{}
		for _, p := range picked {
			v, ok := f.constOf(p)
			if !ok {
				return nil, fmt.Sprintf("%q is not one of %s", p, strings.Join(f.choices, ", "))
			}
			out = append(out, v)
		}
		if n, ok := f.intLimit("minItems"); ok && len(out) < n {
			return nil, fmt.Sprintf("pick at least %d", n)
		}
		if n, ok := f.intLimit("maxItems"); ok && len(out) > n {
			return nil, fmt.Sprintf("pick at most %d", n)
		}
		return out, ""
	}
	text := strings.TrimSpace(a.Text)
	if text == "" && len(a.Selected) > 0 {
		text = a.Selected[0]
	}
	if len(f.choices) > 0 {
		v, ok := f.constOf(text)
		if !ok {
			return nil, fmt.Sprintf("%q is not one of %s", text, strings.Join(f.choices, ", "))
		}
		return v, ""
	}
	switch f.spec["type"] {
	case "integer":
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Sprintf("%q is not a whole number", text)
		}
		return n, f.rangeProblem(float64(n))
	case "number":
		n, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, fmt.Sprintf("%q is not a number", text)
		}
		return n, f.rangeProblem(n)
	}
	length := utf8.RuneCountInString(text)
	if n, ok := f.intLimit("minLength"); ok && length < n {
		return nil, fmt.Sprintf("it needs at least %d characters", n)
	}
	if n, ok := f.intLimit("maxLength"); ok && length > n {
		return nil, fmt.Sprintf("it allows at most %d characters", n)
	}
	if f.pattern != nil {
		if matched, err := f.pattern.MatchString(text); err != nil || !matched {
			return nil, "it does not match the pattern " + fmt.Sprint(f.spec["pattern"])
		}
	}
	if problem := formatProblem(fmt.Sprint(f.spec["format"]), text); problem != "" {
		return nil, problem
	}
	return text, ""
}

// Constant a shown choice stands for, case insensitive
func (f *formField) constOf(choice string) (any, bool) {
	for i, c := range f.choices {
		if strings.EqualFold(strings.TrimSpace(choice), c) {
			return f.consts[i], true
		}
	}
	if f.spec["type"] == "boolean" {
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "true", "y":
			return true, true
		case "false", "n":
			return false, true
		}
	}
	return nil, false
}

// Integer limit keyword of the field schema
func (f *formField) intLimit(key string) (int, bool) {
	switch v := f.spec[key].(type) {
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case int:
		return v, true
	}
	return 0, false
}

// Refusal of a number outside the minimum or maximum
func (f *formField) rangeProblem(n float64) string {
	if v, ok := f.spec["minimum"].(float64); ok && n < v {
		return fmt.Sprintf("it must be at least %v", v)
	}
	if v, ok := f.spec["maximum"].(float64); ok && n > v {
		return fmt.Sprintf("it must be at most %v", v)
	}
	return ""
}

// Refusal of a string outside its declared email, uri, date or date-time format
func formatProblem(format, text string) string {
	switch format {
	case "email":
		if addr, err := mail.ParseAddress(text); err != nil || addr.Address != text {
			return fmt.Sprintf("%q is not an email address", text)
		}
	case "uri":
		if u, err := url.Parse(text); err != nil || u.Scheme == "" {
			return fmt.Sprintf("%q is not an absolute URI", text)
		}
	case "date":
		if _, err := time.Parse("2006-01-02", text); err != nil {
			return fmt.Sprintf("%q is not a date like 2026-01-31", text)
		}
	case "date-time":
		if _, err := time.Parse(time.RFC3339, text); err != nil {
			return fmt.Sprintf("%q is not a date and time like 2026-01-31T09:30:00Z", text)
		}
	}
	return ""
}

// Collects every field of a form elicitation through the session's question surface, then asks the user to review and send them
func (h *elicitationHandler) elicitForm(ctx context.Context, message string, fields []*formField) (*ElicitResult, *RPCError) {
	base, env := h.callEnv(ctx)
	ask := formAsker(base, env)
	if ask == nil {
		pkgLog().Info(fmt.Sprintf("MCP server '%s' form elicitation declined: no user can answer questions in this session", h.serverName))
		h.count("declined")
		return &ElicitResult{Action: "decline"}, nil
	}
	content := map[string]any{}
	problems := map[string]string{}
	attempts := map[string]int{}
	open := append([]*formField(nil), fields...)
	for len(open) > 0 {
		batch := open[:min(formBatchSize, len(open))]
		open = open[len(batch):]
		questions := make([]tools.Question, len(batch))
		for i, f := range batch {
			text := f.question(problems[f.name])
			if i == 0 {
				text = fmt.Sprintf("MCP server '%s' asks: %s\n%s", h.serverName, message, text)
			}
			questions[i] = tools.Question{Header: f.name, Text: text, Choices: f.choices, MultiSelect: f.multi}
		}
		askCtx, cancel := context.WithTimeout(ctx, h.ElicitationWait())
		result, err := ask(askCtx, questions)
		cancel()
		if err != nil || result.Outcome == "cancelled" || result.Outcome == "timed_out" {
			pkgLog().Info(fmt.Sprintf("MCP server '%s' form elicitation ended without answers (%s)", h.serverName, result.Outcome))
			h.count("errors")
			return &ElicitResult{Action: "cancel"}, nil
		}
		for i, f := range batch {
			var a tools.Answer
			if i < len(result.Answers) {
				a = result.Answers[i]
			}
			if a.Status != "answered" || (strings.TrimSpace(a.Text) == "" && len(a.Selected) == 0) {
				if def, ok := f.spec["default"]; ok {
					content[f.name] = def
				} else if f.required {
					pkgLog().Info(fmt.Sprintf("MCP server '%s' form elicitation declined: required field %s skipped", h.serverName, f.name))
					h.count("declined")
					return &ElicitResult{Action: "decline"}, nil
				}
				continue
			}
			v, problem := f.value(a)
			if problem == "" {
				content[f.name] = v
				continue
			}
			attempts[f.name]++
			if attempts[f.name] >= formFieldAttempts {
				pkgLog().Info(fmt.Sprintf("MCP server '%s' form elicitation declined: field %s refused %d times", h.serverName, f.name, attempts[f.name]))
				h.count("declined")
				return &ElicitResult{Action: "decline"}, nil
			}
			problems[f.name] = problem
			open = append(open, f)
		}
	}
	review := []string{fmt.Sprintf("Answers for MCP server '%s':", h.serverName)}
	for _, f := range fields {
		if v, ok := content[f.name]; ok {
			shown, _ := json.Marshal(v)
			review = append(review, "  "+f.name+": "+string(shown))
		}
	}
	answer, metric := h.ask(ctx, "Send these answers?", strings.Join(review, "\n"), tools.ElicitationOptions{Title: "Send these answers?"})
	h.count(metric)
	if answer != "accept" {
		return &ElicitResult{Action: answer}, nil
	}
	return &ElicitResult{Action: "accept", Content: content}, nil
}

// Question surface of the session, its asker or the turn's clarify callback, nil when no user can answer
func formAsker(ctx context.Context, env *tools.Env) func(context.Context, []tools.Question) (tools.FormResult, error) {
	if env != nil && env.Ask != nil {
		return env.Ask.Ask
	}
	cb := tools.ClarifyCallbackFrom(ctx)
	if cb == nil {
		return nil
	}
	return func(ctx context.Context, questions []tools.Question) (tools.FormResult, error) {
		shown := make([]any, len(questions))
		for i, q := range questions {
			var choices any
			if len(q.Choices) > 0 {
				list := make([]any, len(q.Choices))
				for j, c := range q.Choices {
					list[j] = c
				}
				choices = list
			}
			shown[i] = map[string]any{"qid": q.Header, "question": q.Text, "choices_offered": choices, "multi_select": q.MultiSelect, "choices": choices}
		}
		reply, err := cb(ctx, shown)
		if err != nil {
			return tools.FormResult{}, err
		}
		answers, _ := reply["answers"].(map[string]any)
		result := tools.FormResult{Outcome: fmt.Sprint(reply["outcome"])}
		for _, q := range questions {
			a := tools.Answer{Status: "skipped"}
			switch v := answers[q.Header].(type) {
			case string:
				a = tools.Answer{Status: "answered", Text: v}
			case []string:
				a = tools.Answer{Status: "answered", Selected: v}
			case []any:
				a.Status = "answered"
				for _, item := range v {
					a.Selected = append(a.Selected, fmt.Sprint(item))
				}
			}
			result.Answers = append(result.Answers, a)
		}
		return result, nil
	}
}
