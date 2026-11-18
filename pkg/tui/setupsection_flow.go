package tui

import (
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// One setup flow running on the gateway and its session
type flowRun struct {
	session string
	title   string
}

// Starts a gateway setup flow and walks its questions until it ends
func (a *app) runSetupFlow(flow, title string, params map[string]any) tea.Cmd {
	a.store.ui.Status = tr("setupFlow.section.loading")
	start := map[string]any{"flow": flow}
	if params != nil {
		start["params"] = params
	}
	return a.events.rpc("setup.flow.start", start, func(res map[string]any) tea.Cmd {
		if res == nil {
			a.store.ui.Status = a.store.statusFromBusy()
			return nil
		}
		session, _ := res["session"].(string)
		return a.stepSetupFlow(&flowRun{session: session, title: title}, nil)
	})
}

// Sends an answer, prints the output that follows and opens the next question, ending on done
func (a *app) stepSetupFlow(run *flowRun, answer map[string]any) tea.Cmd {
	params := map[string]any{"session": run.session}
	if answer != nil {
		params["answer"] = answer
	}
	return a.events.rpc("setup.flow.step", params, func(res map[string]any) tea.Cmd {
		if res == nil {
			a.store.ui.Status = a.store.statusFromBusy()
			return nil
		}
		if lines := stringList(res["lines"]); len(lines) > 0 {
			a.sys(strings.Join(lines, "\n"))
		}
		if res["done"] == true {
			a.store.ui.Status = a.store.statusFromBusy()
			switch failure, _ := res["error"].(string); {
			case res["ok"] == true:
				a.sys(tr("setupFlow.flow.done", run.title))
			case failure != "":
				a.sys(tr("setupFlow.flow.failed", run.title, failure))
			}
			return nil
		}
		question, _ := res["question"].(map[string]any)
		if question == nil {
			return a.stepSetupFlow(run, nil)
		}
		a.store.ui.Status = a.store.statusFromBusy()
		return a.askFlowQuestion(run, question)
	})
}

// Whole number of a wire value, the fallback for anything else
func wireInt(v any, fallback int) int {
	if n, isNumber := sectionFloat(v); isNumber {
		return int(n)
	}
	return fallback
}

// Rows of a wire list of objects
func wireRows(v any) []map[string]any {
	list, _ := v.([]any)
	rows := make([]map[string]any, 0, len(list))
	for _, raw := range list {
		row, _ := raw.(map[string]any)
		rows = append(rows, row)
	}
	return rows
}

// Answer to a question, sent once per question, a closed overlay without an answer cancelling it
func (a *app) answerFlow(run *flowRun, question map[string]any, sent *bool, answer map[string]any) tea.Cmd {
	if *sent {
		return nil
	}
	*sent = true
	answer["question"] = question["id"]
	return a.stepSetupFlow(run, answer)
}

// Opens the overlay of one question, a choice palette, a toggle list or a text prompt
func (a *app) askFlowQuestion(run *flowRun, question map[string]any) tea.Cmd {
	sent := new(bool)
	text, _ := question["question"].(string)
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ":"))
	cancel := func() tea.Cmd { return a.answerFlow(run, question, sent, map[string]any{"cancel": true}) }
	switch question["kind"] {
	case "choose":
		def := wireInt(question["default"], 0)
		options := wireRows(question["options"])
		items := make([]wm.Item, len(options))
		for i, option := range options {
			title, _ := option["title"].(string)
			detail, _ := option["detail"].(string)
			mark := "○ "
			if i == def {
				mark = "● "
			}
			index := i
			items[i] = wm.Item{ID: "flow:" + run.session + ":" + strconv.Itoa(i), Title: mark + strings.TrimSpace(title), Detail: detail,
				Run: func() tea.Cmd { return a.answerFlow(run, question, sent, map[string]any{"index": index}) }}
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
		for _, raw := range wireAny(question["selected"]) {
			chosen[wireInt(raw, -1)] = true
		}
		return a.askFlowChecklist(run, question, text, chosen, sent)
	}
	if question["secret"] == true {
		d := &dialogCard{app: a, kind: "setupSecret", requestID: run.session, marks: map[int]bool{}, title: text,
			fields: []dialogField{{label: text, masked: true}}}
		d.onAnswer = func(_ *dialogChoice, _ []string, fields []string) tea.Cmd {
			return a.answerFlow(run, question, sent, map[string]any{"text": trimJS(fields[0])})
		}
		d.onCancel = cancel
		return a.wm.ShowOverlay(d)
	}
	def, _ := question["default"].(string)
	placeholder := text
	if def != "" {
		placeholder = tr("setupFlow.section.keep", text, def)
	}
	p := wm.NewPrompt(a.toolkit, placeholder, "", func(entered string) tea.Cmd {
		return a.answerFlow(run, question, sent, map[string]any{"text": trimJS(entered)})
	})
	p.OnClose = cancel
	return a.wm.ShowOverlay(p)
}

// Estimated context of the tools the chosen rows bring, empty when the flow sent no weights
func flowTokenStatus(question map[string]any, chosen map[int]bool) string {
	weights, _ := question["tokens"].(map[string]any)
	if len(weights) == 0 {
		return ""
	}
	rows := wireRows(question["items"])
	tools := map[string]bool{}
	for i, row := range rows {
		if chosen[i] {
			for _, name := range stringList(row["tools"]) {
				tools[name] = true
			}
		}
	}
	total := 0
	for name := range tools {
		total += wireInt(weights[name], 0)
	}
	if total >= 1000 {
		return tr("setupFlow.flow.tokensK", strconv.FormatFloat(float64(total)/1000, 'f', 1, 64))
	}
	return tr("setupFlow.flow.tokens", total)
}

// Toggle list of a multi choice, a row flipping its mark and the save row answering with the marked rows
func (a *app) askFlowChecklist(run *flowRun, question map[string]any, text string, chosen map[int]bool, sent *bool) tea.Cmd {
	rows := wireRows(question["items"])
	toggled := false
	items := []wm.Item{{ID: "flow:" + run.session + ":save", Title: tr("setupFlow.flow.save"), Detail: flowTokenStatus(question, chosen), Run: func() tea.Cmd {
		indices := []any{}
		for i := range rows {
			if chosen[i] {
				indices = append(indices, i)
			}
		}
		return a.answerFlow(run, question, sent, map[string]any{"indices": indices})
	}}}
	for i, row := range rows {
		title, _ := row["title"].(string)
		detail, _ := row["detail"].(string)
		mark := "[ ] "
		if chosen[i] {
			mark = "[x] "
		}
		index := i
		items = append(items, wm.Item{ID: "flow:" + run.session + ":" + strconv.Itoa(i), Title: mark + title, Detail: detail, Run: func() tea.Cmd {
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
		return a.answerFlow(run, question, sent, map[string]any{"cancel": true})
	}
	return a.wm.ShowOverlay(p)
}

// Toolsets per platform with the provider, key and model of each tool, run as the tools flow on the gateway
func toolsSetupSection() setupSection {
	title := tr("setupFlow.tools.title")
	return setupSection{"tools", title, tr("setupFlow.tools.detail"), func(a *app) tea.Cmd { return a.runSetupFlow("tools", title, nil) }}
}
