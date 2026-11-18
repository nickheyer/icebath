package tui

import (
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Lines of flow output the wait dialog shows
const hostedWaitLines = 12

// Hosted setup flow on screen, its job, printed lines, progress line, last question and wait dialog
type hostedFlow struct {
	job      string
	after    int
	asked    int
	recent   []string
	progress string
	wait     *dialogCard
}

// Runs the model section on the gateway with its questions asked here, opening at the named row
func (a *app) openHostedModel(entry string) tea.Cmd { return a.openHostedFlow("model", entry) }

// Runs a setup section or first run mode on the gateway with its questions asked here
func (a *app) openHostedFlow(flow, entry string) tea.Cmd {
	a.store.ui.Status = tr("setupFlow.flow.starting")
	return a.events.rpc("setup.hosted.start", map[string]any{"flow": flow, "entry": entry}, func(res map[string]any) tea.Cmd {
		job, _ := res["job"].(string)
		if job == "" {
			a.store.ui.Status = a.store.statusFromBusy()
			return nil
		}
		return a.pollHostedFlow(&hostedFlow{job: job})
	})
}

// Lines a wait dialog shows, the newest output and the progress line under it
func (f *hostedFlow) waitBody() []string {
	body := append([]string{}, f.recent...)
	if trimJS(f.progress) != "" {
		body = append(body, f.progress)
	}
	return body
}

// Keeps the newest lines a wait dialog shows
func (f *hostedFlow) remember(lines []string) {
	f.recent = append(f.recent, lines...)
	if len(f.recent) > hostedWaitLines {
		f.recent = f.recent[len(f.recent)-hostedWaitLines:]
	}
}

// Closes the wait dialog without stopping the flow
func (f *hostedFlow) endWait(a *app) tea.Cmd {
	d := f.wait
	if d == nil {
		return nil
	}
	f.wait = nil
	d.settled = true
	if a.wm.Overlay() == wm.Card(d) {
		return a.wm.Dismiss()
	}
	return nil
}

// Follows a hosted flow, printing lines, asking questions, waiting in a dialog and settling at its end
func (a *app) pollHostedFlow(f *hostedFlow) tea.Cmd {
	return a.events.rpc("setup.hosted.poll", map[string]any{"job": f.job, "after": f.after}, func(res map[string]any) tea.Cmd {
		if res == nil {
			a.store.ui.Status = a.store.statusFromBusy()
			return f.endWait(a)
		}
		lines := stringList(res["lines"])
		f.after += len(lines)
		if len(lines) > 0 {
			a.sys(strings.Join(lines, "\n"))
			f.remember(lines)
		}
		if res["done"] == true {
			return tea.Batch(f.endWait(a), a.hostedFlowDone(res))
		}
		if q, _ := res["question"].(map[string]any); q != nil {
			id := int(jsNumberOf(q["id"]))
			if id == f.asked {
				return nil
			}
			f.asked = id
			return tea.Batch(f.endWait(a), a.askHosted(f, q))
		}
		a.store.ui.Status = tr("setupFlow.flow.working")
		f.progress, _ = res["progress"].(string)
		var shown tea.Cmd
		switch {
		case f.wait != nil:
			f.wait.body = f.waitBody()
		case len(lines) == 0:
			shown = a.showHostedWait(f)
		}
		return tea.Batch(shown, a.pollHostedFlow(f))
	})
}

// Dialog shown while the flow works on its own, its stop or escape ending the flow
func (a *app) showHostedWait(f *hostedFlow) tea.Cmd {
	d := &dialogCard{app: a, kind: "setupWait", requestID: f.job, focus: -1, marks: map[int]bool{}, title: tr("setupFlow.flow.waitTitle"),
		body: f.waitBody(), choices: []dialogChoice{{label: tr("setupFlow.flow.keepWaiting"), value: "wait"},
			{label: tr("setupFlow.flow.stop"), value: "stop"}}}
	stop := func() tea.Cmd {
		f.wait = nil
		a.sys(tr("setupFlow.flow.stopping"))
		return a.events.rpc("setup.hosted.cancel", map[string]any{"job": f.job}, nil)
	}
	d.onAnswer = func(choice *dialogChoice, _ []string, _ []string) tea.Cmd {
		if choice != nil && choice.value == "wait" && f.wait == d {
			return a.showHostedWait(f)
		}
		return stop()
	}
	d.onCancel = stop
	f.wait = d
	return a.wm.ShowOverlay(d)
}

// Sends the answer to the question on screen and goes back to following the flow
func (a *app) answerHosted(f *hostedFlow, id int, answer map[string]any) tea.Cmd {
	answer["job"], answer["question"] = f.job, id
	return a.events.rpc("setup.hosted.answer", answer, func(res map[string]any) tea.Cmd {
		if res == nil {
			a.store.ui.Status = a.store.statusFromBusy()
			return nil
		}
		return a.pollHostedFlow(f)
	})
}

// Run of spaces a terminal list pads its columns with
var hostedColumnGap = regexp.MustCompile(` {2,}`)

// Choice rows split into the first column and a detail with the heading, the default row first
func hostedRows(q map[string]any) ([]string, []string, []int) {
	rows := stringList(q["rows"])
	headings := map[int]string{}
	list, _ := q["headings"].([]any)
	for _, raw := range list {
		h, _ := raw.(map[string]any)
		text, _ := h["text"].(string)
		headings[int(jsNumberOf(h["at"]))] = text
	}
	titles, details := make([]string, len(rows)), make([]string, len(rows))
	group := ""
	for i, row := range rows {
		if text, starts := headings[i]; starts {
			group = text
		}
		columns := hostedColumnGap.Split(strings.TrimSpace(row), -1)
		titles[i] = columns[0]
		if group != "" {
			columns = append(columns[:1], append([]string{group}, columns[1:]...)...)
		}
		details[i] = strings.Join(columns[1:], " · ")
	}
	order := make([]int, 0, len(rows))
	def := int(jsNumberOf(q["default"]))
	if def >= 0 && def < len(rows) {
		order = append(order, def)
	}
	for i := range rows {
		if i != def {
			order = append(order, i)
		}
	}
	return titles, details, order
}

// Puts one question on screen in the form its kind takes, backing out answering as cancelled
func (a *app) askHosted(f *hostedFlow, q map[string]any) tea.Cmd {
	id := int(jsNumberOf(q["id"]))
	kind, _ := q["kind"].(string)
	title, _ := q["title"].(string)
	title = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(title), ":"))
	if description, _ := q["description"].(string); trimJS(description) != "" {
		a.sys(description)
	}
	a.store.ui.Status = a.store.statusFromBusy()
	settled := false
	cancel := func() tea.Cmd {
		if settled {
			return nil
		}
		settled = true
		return a.answerHosted(f, id, map[string]any{"cancelled": true})
	}
	switch kind {
	case "choice":
		rows, details, order := hostedRows(q)
		items := make([]wm.Item, len(order))
		for n, i := range order {
			items[n] = wm.Item{ID: "setup:flow:" + strconv.Itoa(id) + ":" + strconv.Itoa(i), Title: rows[i], Detail: details[i], Run: func() tea.Cmd {
				settled = true
				return a.answerHosted(f, id, map[string]any{"index": i})
			}}
		}
		p := wm.NewPalette(a.toolkit, items)
		p.Placeholder, p.OnClose = title, cancel
		return a.wm.ShowOverlay(p)
	case "checklist":
		rows := stringList(q["rows"])
		d := &dialogCard{app: a, kind: "setupChecklist", requestID: f.job, focus: -1, marks: map[int]bool{}, title: title, multi: true,
			body: []string{tr("setupFlow.flow.checklistHint")}}
		for i, row := range rows {
			d.choices = append(d.choices, dialogChoice{label: row, value: strconv.Itoa(i)})
		}
		picked, _ := q["picked"].([]any)
		for _, raw := range picked {
			d.marks[int(jsNumberOf(raw))] = true
		}
		d.onAnswer = func(_ *dialogChoice, marked []string, _ []string) tea.Cmd {
			settled = true
			chosen := []any{}
			for _, value := range marked {
				n, _ := strconv.Atoi(value)
				chosen = append(chosen, n)
			}
			return a.answerHosted(f, id, map[string]any{"picked": chosen})
		}
		d.onCancel = cancel
		return a.wm.ShowOverlay(d)
	case "secret":
		d := &dialogCard{app: a, kind: "setupSecret", requestID: f.job, focus: 0, marks: map[int]bool{}, title: title,
			fields: []dialogField{{label: title, masked: true}}}
		d.onAnswer = func(_ *dialogChoice, _ []string, fields []string) tea.Cmd {
			settled = true
			return a.answerHosted(f, id, map[string]any{"text": fields[0]})
		}
		d.onCancel = cancel
		return a.wm.ShowOverlay(d)
	case "command":
		argv := stringList(q["argv"])
		if len(argv) == 0 {
			return cancel()
		}
		settled = true
		a.sys(tr("setupFlow.flow.running", strings.Join(argv, " ")))
		return tty.Exec(exec.Command(argv[0], argv[1:]...), func(err error) tea.Msg {
			return asyncDoneMsg{run: func() tea.Cmd { return a.answerHosted(f, id, commandAnswer(err)) }}
		})
	}
	p := wm.NewPrompt(a.toolkit, title, "", func(text string) tea.Cmd {
		settled = true
		return a.answerHosted(f, id, map[string]any{"text": text})
	})
	p.OnClose = cancel
	return a.wm.ShowOverlay(p)
}

// Answer of a terminal program run, ran when it exited on its own, else the start error
func commandAnswer(err error) map[string]any {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return map[string]any{"ran": true}
	case errors.As(err, &exit):
		status, waited := exit.Sys().(syscall.WaitStatus)
		return map[string]any{"ran": !waited || !status.Signaled()}
	}
	return map[string]any{"error": err.Error()}
}

// Settles a finished flow, naming a changed default model and starting the first session once one is set
func (a *app) hostedFlowDone(res map[string]any) tea.Cmd {
	a.store.ui.Status = a.store.statusFromBusy()
	model, _ := res["model"].(string)
	provider, _ := res["provider"].(string)
	switch {
	case res["ok"] != true:
		a.sys(tr("setupFlow.flow.stopped"))
		return nil
	case model == "":
		return nil
	case a.store.ui.Sid != "":
		if res["changed"] == true {
			a.sys(tr("setupFlow.model.savedLive", model, provider))
		}
		return nil
	}
	a.sys(tr("setupFlow.model.saved", model, provider))
	return a.newSession(nil)
}
