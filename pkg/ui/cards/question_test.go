package cards

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Messages a command yields, batches flattened
func collect(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, collect(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// Reply among the messages, false when none is
func reply(msgs []tea.Msg) (ReplyMsg, bool) {
	for _, m := range msgs {
		if r, ok := m.(ReplyMsg); ok {
			return r, true
		}
	}
	return ReplyMsg{}, false
}

func askingComposer(t *testing.T) *Composer {
	t.Helper()
	c := NewComposer(theme.Blackwall(), keys.Default(), "", nil)
	c.SetFocus(true)
	c.SetQuestion(&harness.Asking{ID: 3, Questions: []harness.Question{
		{Text: "Which store?", Header: "storage", Choices: []harness.Choice{{Label: "Postgres", Description: "already in the stack"}, {Label: "SQLite"}}},
		{Text: "Which parts?", Choices: []harness.Choice{{Label: "api"}, {Label: "cli"}, {Label: "docs"}}, Multi: true},
	}})
	return c
}

func TestComposerAnswersByChoiceAndText(t *testing.T) {
	c := askingComposer(t)
	if !c.Asking() || c.AskingMulti() || c.Height() < minInputRows+4 {
		t.Fatalf("asking %v multi %v height %d", c.Asking(), c.AskingMulti(), c.Height())
	}
	view := c.View(60, c.Height())
	for _, want := range []string{"Which store?", "storage", "1 Postgres", "already in the stack", "2 SQLite", "3 Other", "1/2"} {
		if !strings.Contains(ansi.Strip(view), want) {
			t.Fatalf("view lacks %q:\n%s", want, ansi.Strip(view))
		}
	}
	if cmd, ok := c.questionAction("question.down"); !ok || cmd != nil || c.ask.idx != 1 {
		t.Fatalf("down: %v %d", ok, c.ask.idx)
	}
	if _, ok := reply(collect(c.Action("question.pick"))); ok {
		t.Fatal("replied after the first of two questions")
	}
	if !c.AskingMulti() || c.ask.at != 1 || len(c.ask.answers) != 1 || c.ask.answers[0].Picked[0] != "SQLite" {
		t.Fatalf("after the first answer: %+v", c.ask)
	}
	c.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	c.Update(tea.KeyPressMsg{Code: '3', Text: "3"})
	if !c.ask.marked[0] || c.ask.marked[1] || !c.ask.marked[2] || c.Value() != "" {
		t.Fatalf("digits did not mark: %+v %q", c.ask.marked, c.Value())
	}
	r, ok := reply(collect(c.Action("question.pick")))
	if !ok || r.ID != 3 || len(r.Answers) != 2 || strings.Join(r.Answers[1].Picked, ",") != "api,docs" || c.Asking() {
		t.Fatalf("reply %+v %v, asking %v", r, ok, c.Asking())
	}
	c = askingComposer(t)
	for _, r := range "redis" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if c.ask.idx != c.ask.other() {
		t.Fatalf("typing did not move to other: %d", c.ask.idx)
	}
	c.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	if c.Value() != "redis2" {
		t.Fatalf("digit after text was taken as a choice: %q", c.Value())
	}
	if _, ok := reply(collect(c.Action("question.pick"))); ok {
		t.Fatal("replied early")
	}
	if c.ask.answers[0].Text != "redis2" || c.Value() != "" {
		t.Fatalf("typed answer: %+v %q", c.ask.answers[0], c.Value())
	}
	r, ok = reply(collect(c.Action("question.skip")))
	if !ok || !r.Answers[1].Declined || r.Answers[0].Text != "redis2" {
		t.Fatalf("skip: %+v %v", r, ok)
	}
	if c.Asking() || c.Height() != minInputRows {
		t.Fatalf("after the reply: asking %v height %d", c.Asking(), c.Height())
	}
}

func TestComposerQuestionClicksAndRebinds(t *testing.T) {
	c := askingComposer(t)
	if _, ok := c.Click(3, 2); !ok || c.ask.at != 1 || c.ask.answers[0].Picked[0] != "SQLite" {
		t.Fatalf("click on a choice: %v at %d %+v", ok, c.ask.at, c.ask.answers)
	}
	if _, ok := c.Click(3, 0); ok {
		t.Fatal("click on the question line hit a choice")
	}
	if cmd, ok := c.Click(3, 2); !ok || cmd != nil || !c.ask.marked[1] {
		t.Fatalf("click on a multi choice: %v %v marked %+v", cmd, ok, c.ask.marked)
	}
	same := &harness.Asking{ID: 3, Questions: []harness.Question{{Text: "x", Choices: []harness.Choice{{Label: "a"}, {Label: "b"}}}}}
	c.SetQuestion(same)
	if c.ask.at != 1 {
		t.Fatal("the same ask reset the progress")
	}
	c.SetQuestion(&harness.Asking{ID: 4, Questions: same.Questions})
	if c.ask.id != 4 || c.ask.at != 0 {
		t.Fatalf("new ask: %+v", c.ask)
	}
	c.SetQuestion(nil)
	if c.Asking() {
		t.Fatal("nil did not clear the question")
	}
	if cmd, ok := c.questionAction("question.pick"); ok || cmd != nil {
		t.Fatal("question action handled with no question")
	}
}
