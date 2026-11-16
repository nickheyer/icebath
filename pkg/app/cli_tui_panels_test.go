package app

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Reports the given terminal size to every size read of the test
func withTermSize(t *testing.T, columns, rows int) {
	t.Helper()
	t.Setenv("COLUMNS", strconv.Itoa(columns))
	t.Setenv("LINES", strconv.Itoa(rows))
}

// Text of rendered fragments joined
func fragmentText(fragments []styledFragment) string {
	var b strings.Builder
	for _, f := range fragments {
		b.WriteString(f.Text)
	}
	return b.String()
}

// A CLI showing the clarify panel for the questions, the active one offering the choices
func clarifyPanelCLI(t *testing.T, questions []string, answers map[string]string, choices []string, active int) *CLI {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	state := &clarifyModalState{Answers: map[string]*string{}, AnswerMeta: map[string]clarifyAnswerMeta{}, Active: active, Choices: choices}
	for i, q := range questions {
		state.Questions = append(state.Questions, clarifyQuestion{QID: "q" + string(rune('0'+i)), Question: q})
	}
	for qid, answer := range answers {
		a := answer
		state.Answers[qid] = &a
	}
	return &CLI{ctx: context.Background(), clarifyState: state}
}

var clarifyLongQuestion = strings.TrimSpace(strings.Repeat("Storage boundary for the signed graph. ", 12))

// A CLI with an open approval panel for the command
func approvalPanelCLI(t *testing.T, command, description string, choices []string, showFull bool) *CLI {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &CLI{ctx: context.Background(), approvalDeadline: pythread.Monotonic() + 30,
		approvalState: &approvalModalState{Command: command, Description: description, Choices: choices, ShowFull: showFull, response: make(chan string, 1)}}
}
