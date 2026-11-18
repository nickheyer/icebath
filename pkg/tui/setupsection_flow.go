package tui

import (
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// One setup flow running on the gateway and its session
type flowRun struct {
	session string
	title   string
}

// Starts a gateway setup flow and walks its questions until it ends
func (a *app) runSetupFlow(flow, title string, params map[string]string) tea.Cmd {
	a.store.ui.Status = tr("setupFlow.section.loading")
	start := tuigateway.SetupFlowStartParams{Flow: flow, Params: params}
	return ask(a.events.rpc, a.sys, tuigateway.SetupFlowStart, start, func(res *tuigateway.SetupFlowStartResult) tea.Cmd {
		if res == nil {
			a.store.ui.Status = a.store.statusFromBusy()
			return nil
		}
		return a.stepSetupFlow(&flowRun{session: res.Session, title: title}, nil)
	})
}

// Sends an answer, prints the output that follows and opens the next question, ending on done
func (a *app) stepSetupFlow(run *flowRun, answer *tuigateway.SetupFlowAnswer) tea.Cmd {
	params := tuigateway.SetupFlowStepParams{Session: run.session, Answer: answer}
	return ask(a.events.rpc, a.sys, tuigateway.SetupFlowStep, params, func(res *tuigateway.SetupFlowStepResult) tea.Cmd {
		if res == nil {
			a.store.ui.Status = a.store.statusFromBusy()
			return nil
		}
		if len(res.Lines) > 0 {
			a.sys(strings.Join(res.Lines, "\n"))
		}
		if res.Done {
			a.store.ui.Status = a.store.statusFromBusy()
			switch {
			case res.Ok:
				a.sys(tr("setupFlow.flow.done", run.title))
			case res.Error != "":
				a.sys(tr("setupFlow.flow.failed", run.title, res.Error))
			}
			return nil
		}
		if res.Question == nil {
			return a.stepSetupFlow(run, nil)
		}
		a.store.ui.Status = a.store.statusFromBusy()
		return a.askFlowQuestion(run, res.Question)
	})
}

// Answer to a question, sent once per question, a closed overlay without an answer cancelling it
func (a *app) answerFlow(run *flowRun, question *tuigateway.SetupFlowQuestion, sent *bool, answer tuigateway.SetupFlowAnswer) tea.Cmd {
	if *sent {
		return nil
	}
	*sent = true
	answer.Question = question.ID
	return a.stepSetupFlow(run, &answer)
}

// Opens the overlay of one question, a choice palette, a toggle list or a text prompt
func (a *app) askFlowQuestion(run *flowRun, question *tuigateway.SetupFlowQuestion) tea.Cmd {
	sent := new(bool)
	text := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(question.Question), ":"))
	cancel := func() tea.Cmd { return a.answerFlow(run, question, sent, tuigateway.SetupFlowAnswer{Cancel: true}) }
	switch question.Kind {
	case "choose":
		items := make([]wm.Item, len(question.Options))
		for i, option := range question.Options {
			mark := "○ "
			if int64(i) == question.Default {
				mark = "● "
			}
			index := int64(i)
			items[i] = wm.Item{ID: "flow:" + run.session + ":" + strconv.Itoa(i), Title: mark + strings.TrimSpace(option.Title), Detail: option.Detail,
				Run: func() tea.Cmd { return a.answerFlow(run, question, sent, tuigateway.SetupFlowAnswer{Index: &index}) }}
		}
		sort.SliceStable(items, func(i, j int) bool {
			return strings.HasPrefix(items[i].Title, "● ") && !strings.HasPrefix(items[j].Title, "● ")
		})
		p := wm.NewPalette(a.toolkit, items)
		p.Placeholder = text
		p.OnClose = cancel
		return a.wm.ShowOverlay(p)
	case "check":
		chosen := map[int]bool{}
		for _, i := range question.Selected {
			chosen[int(i)] = true
		}
		return a.askFlowChecklist(run, question, text, chosen, sent)
	}
	if question.Secret {
		d := &dialogCard{app: a, kind: "setupSecret", requestID: run.session, marks: map[int]bool{}, title: text,
			fields: []dialogField{{label: text, masked: true}}}
		d.onAnswer = func(_ *dialogChoice, _ []string, fields []string) tea.Cmd {
			return a.answerFlow(run, question, sent, tuigateway.SetupFlowAnswer{Text: trimJS(fields[0])})
		}
		d.onCancel = cancel
		return a.wm.ShowOverlay(d)
	}
	placeholder := text
	if question.DefaultText != "" {
		placeholder = tr("setupFlow.section.keep", text, question.DefaultText)
	}
	p := wm.NewPrompt(a.toolkit, placeholder, "", func(entered string) tea.Cmd {
		return a.answerFlow(run, question, sent, tuigateway.SetupFlowAnswer{Text: trimJS(entered)})
	})
	p.OnClose = cancel
	return a.wm.ShowOverlay(p)
}

// Estimated context of the tools the chosen rows bring, empty when the flow sent no weights
func flowTokenStatus(question *tuigateway.SetupFlowQuestion, chosen map[int]bool) string {
	if len(question.Tokens) == 0 {
		return ""
	}
	tools := map[string]bool{}
	for i, row := range question.Items {
		if chosen[i] {
			for _, name := range row.Tools {
				tools[name] = true
			}
		}
	}
	total := int64(0)
	for name := range tools {
		total += question.Tokens[name]
	}
	if total >= 1000 {
		return tr("setupFlow.flow.tokensK", strconv.FormatFloat(float64(total)/1000, 'f', 1, 64))
	}
	return tr("setupFlow.flow.tokens", total)
}

// Toggle list of a multi choice, a row flipping its mark and the save row answering with the marked rows
func (a *app) askFlowChecklist(run *flowRun, question *tuigateway.SetupFlowQuestion, text string, chosen map[int]bool, sent *bool) tea.Cmd {
	toggled := false
	items := []wm.Item{{ID: "flow:" + run.session + ":save", Title: tr("setupFlow.flow.save"), Detail: flowTokenStatus(question, chosen), Run: func() tea.Cmd {
		indices := []int64{}
		for i := range question.Items {
			if chosen[i] {
				indices = append(indices, int64(i))
			}
		}
		return a.answerFlow(run, question, sent, tuigateway.SetupFlowAnswer{Indices: indices})
	}}}
	for i, row := range question.Items {
		mark := "[ ] "
		if chosen[i] {
			mark = "[x] "
		}
		index := i
		items = append(items, wm.Item{ID: "flow:" + run.session + ":" + strconv.Itoa(i), Title: mark + row.Title, Detail: row.Detail, Run: func() tea.Cmd {
			chosen[index] = !chosen[index]
			toggled = true
			return a.askFlowChecklist(run, question, text, chosen, sent)
		}})
	}
	p := wm.NewPalette(a.toolkit, items)
	p.Placeholder = text
	p.OnClose = func() tea.Cmd {
		if toggled {
			return nil
		}
		return a.answerFlow(run, question, sent, tuigateway.SetupFlowAnswer{Cancel: true})
	}
	return a.wm.ShowOverlay(p)
}

// Toolsets per platform with the provider, key and model of each tool, run as the tools flow on the gateway
func toolsSetupSection() setupSection {
	title := tr("setupFlow.tools.title")
	return setupSection{"tools", title, tr("setupFlow.tools.detail"), func(a *app) tea.Cmd { return a.runSetupFlow("tools", title, nil) }}
}
