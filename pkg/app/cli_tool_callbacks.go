package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// singleQueryAsker answers every tool question form at once as undelivered, a -q turn having no user to ask
type singleQueryAsker struct{}

// Skips every question of the form and hands the model the single query notice
func (singleQueryAsker) Ask(ctx context.Context, questions []tools.Question) (tools.FormResult, error) {
	reply := singleQueryClarifyCallback(nil)
	result := tools.FormResult{Outcome: cliStr(reply["outcome"], ""), Notice: cliStr(reply["notice"], ""), Answers: make([]tools.Answer, len(questions))}
	for i := range result.Answers {
		result.Answers[i] = tools.Answer{Status: "skipped"}
	}
	return result, nil
}
