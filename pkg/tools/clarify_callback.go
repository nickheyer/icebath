package tools

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// ClarifyCallback is the surface callback of hermes clarify_tool, shown questions in and an answers outcome notice dict out
type ClarifyCallback func(ctx context.Context, shown []any) (map[string]any, error)

// Clarify result JSON through the session asker, the registry handler of the tool
func ClarifyTool(ctx context.Context, env *Env, questions any) string {
	return clarifyTool(ctx, env, questions)
}

// Asks one to five questions through a surface callback and returns the result JSON, unavailable without one
func ClarifyToolWithCallback(ctx context.Context, env *Env, questions any, callback ClarifyCallback) string {
	normalized, errText := clarifyNormalizeQuestions(questions)
	if errText != "" {
		return toolErrorJSON(errText, nil)
	}
	if callback == nil {
		return toolErrorJSON(clarifyUnavailable, nil)
	}
	shown := make([]clarifyQuestion, len(normalized))
	dicts := make([]any, len(normalized))
	prompts := make([]string, 0, len(normalized))
	for i, q := range normalized {
		offered := clarifyProse(env, q.ChoicesOffered)
		s := q
		s.Question = clarifyProse(env, []string{q.Question})[0]
		s.ChoicesOffered = offered
		s.Choices = nil
		var choices, offeredValue any
		if offered != nil {
			offeredValue = offered
		}
		if len(offered) > 0 {
			s.Choices = MarkRecommended(append([]string(nil), offered...))
			choices = anyStringsOf(s.Choices)
		}
		shown[i] = s
		dicts[i] = map[string]any{"qid": s.QID, "question": s.Question, "choices_offered": offeredValue,
			"multi_select": s.MultiSelect, "choices": choices}
		prompts = append(prompts, s.Question)
	}
	human := humanInputRequest(env, "clarify", strings.Join(prompts, "\n"), "")
	reply, err := callback(ctx, dicts)
	outcome := ""
	if err == nil {
		if raw := reply["outcome"]; pyrepr.Truthy(raw) {
			outcome = pyrepr.Str(raw)
		}
		if _, present := reply["outcome"]; !present {
			err = errors.New("'outcome'")
		}
	}
	if err != nil {
		human.finish(env, "error")
		return toolErrorJSON("Failed to get user input: "+err.Error(), nil)
	}
	answers, _ := reply["answers"].(map[string]any)
	out := ClarifyReply{Answers: answers, Outcome: pyrepr.Str(reply["outcome"])}
	if notice := reply["notice"]; pyrepr.Truthy(notice) {
		out.Notice = pyrepr.Str(notice)
	}
	human.finish(env, outcome)
	return clarifyResult(normalized, out, shown)
}
