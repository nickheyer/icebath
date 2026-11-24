package tools

import (
	"context"
	"errors"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// The surface callback of the clarify tool, shown questions in and an answers outcome notice dict out
type ClarifyCallback func(ctx context.Context, shown []any) (map[string]any, error)

type clarifyCallbackKey struct{}

// Binds the session's clarify surface to the tool calls of a turn
func WithClarifyCallback(ctx context.Context, cb ClarifyCallback) context.Context {
	return context.WithValue(ctx, clarifyCallbackKey{}, cb)
}

// Clarify surface bound to the turn, nil when the session has none
func ClarifyCallbackFrom(ctx context.Context) ClarifyCallback {
	if ctx == nil {
		return nil
	}
	cb, _ := ctx.Value(clarifyCallbackKey{}).(ClarifyCallback)
	return cb
}

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
	human := humanInputRequest(ctx, env, "clarify", strings.Join(prompts, "\n"), "")
	reply, err := callback(ctx, dicts)
	outcome := ""
	if err == nil {
		if raw := reply["outcome"]; anyutil.IsTrue(raw) {
			outcome = anyutil.String(raw)
		}
		if _, present := reply["outcome"]; !present {
			err = errors.New("'outcome'")
		}
	}
	if err != nil {
		human.finish(ctx, env, "error")
		return toolErrorJSON("Failed to get user input: "+err.Error(), nil)
	}
	answers, _ := reply["answers"].(map[string]any)
	out := ClarifyReply{Answers: answers, Outcome: anyutil.String(reply["outcome"])}
	if notice := reply["notice"]; anyutil.IsTrue(notice) {
		out.Notice = anyutil.String(notice)
	}
	human.finish(ctx, env, outcome)
	return clarifyResult(normalized, out, shown)
}
