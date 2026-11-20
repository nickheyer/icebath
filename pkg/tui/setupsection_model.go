package tui

import (
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Lines of flow output the wait dialog shows
const hostedWaitLines = 12

// Hosted setup flow on screen, its job, printed lines, progress line, last question and wait dialog
type hostedFlow struct {
	job      string
	after    int
	asked    int64
	recent   []string
	progress string
	wait     *dialogCard
}

// Runs the model section on the gateway with its questions asked here, opening at the named row
func (a *app) openHostedModel(entry string) tea.Cmd { return a.openHostedFlow("model", entry) }

// Runs a setup section or first run mode on the gateway with its questions asked here
func (a *app) openHostedFlow(flow, entry string) tea.Cmd {
	a.store.ui.Status = tr(txt.SetupFlowFlowStarting)
	params := tuigateway.SetupHostedStartParams{Flow: tuigateway.SetupHostedFlow(flow), Entry: entry}
	return ask(a.events.rpc, a.sys, tuigateway.SetupHostedStart, params, func(res *tuigateway.SetupHostedStartResult) tea.Cmd {
		if res == nil || res.Job == "" {
			a.store.ui.Status = a.store.statusFromBusy()
			return nil
		}
		return a.pollHostedFlow(&hostedFlow{job: res.Job})
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
	return ask(a.events.rpc, a.sys, tuigateway.SetupHostedPoll, tuigateway.SetupHostedPollParams{Job: f.job, After: int64(f.after)}, func(res *tuigateway.SetupHostedPollResult) tea.Cmd {
		if res == nil {
			a.store.ui.Status = a.store.statusFromBusy()
			return f.endWait(a)
		}
		lines := res.Lines
		f.after += len(lines)
		if len(lines) > 0 {
			a.sys(strings.Join(lines, "\n"))
			f.remember(lines)
		}
		if res.Done {
			return tea.Batch(f.endWait(a), a.hostedFlowDone(res))
		}
		if q := res.Question; q != nil {
			if q.ID == f.asked {
				return nil
			}
			f.asked = q.ID
			return tea.Batch(f.endWait(a), a.askHosted(f, q))
		}
		a.store.ui.Status = tr(txt.SetupFlowFlowWorking)
		f.progress = res.Progress
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
	d := &dialogCard{app: a, kind: "setupWait", requestID: f.job, focus: -1, marks: map[int]bool{}, title: tr(txt.SetupFlowFlowWaitTitle),
		body: f.waitBody(), choices: []dialogChoice{{label: tr(txt.SetupFlowFlowKeepWaiting), value: "wait"},
			{label: tr(txt.SetupFlowFlowStop), value: "stop"}}}
	stop := func() tea.Cmd {
		f.wait = nil
		a.sys(tr(txt.SetupFlowFlowStopping))
		return ask(a.events.rpc, a.sys, tuigateway.SetupHostedCancel, tuigateway.SetupHostedCancelParams{Job: f.job},
			func(*tuigateway.SetupHostedCancelResult) tea.Cmd { return nil })
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
func (a *app) answerHosted(f *hostedFlow, id int64, answer tuigateway.SetupHostedAnswerParams) tea.Cmd {
	answer.Job, answer.Question = f.job, id
	return ask(a.events.rpc, a.sys, tuigateway.SetupHostedAnswer, answer, func(res *tuigateway.SetupHostedAnswerResult) tea.Cmd {
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
func hostedRows(q *tuigateway.SetupHostedQuestion) ([]string, []string, []int) {
	rows := q.Rows
	headings := map[int]string{}
	for _, h := range q.Headings {
		headings[int(h.At)] = h.Text
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
	def := int(q.Default)
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
func (a *app) askHosted(f *hostedFlow, q *tuigateway.SetupHostedQuestion) tea.Cmd {
	id := q.ID
	title := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(q.Title), ":"))
	if trimJS(q.Description) != "" {
		a.sys(q.Description)
	}
	a.store.ui.Status = a.store.statusFromBusy()
	settled := false
	cancel := func() tea.Cmd {
		if settled {
			return nil
		}
		settled = true
		return a.answerHosted(f, id, tuigateway.SetupHostedAnswerParams{Cancelled: true})
	}
	switch q.Kind {
	case "choice":
		rows, details, order := hostedRows(q)
		items := make([]wm.Item, len(order))
		for n, i := range order {
			items[n] = wm.Item{ID: "setup:flow:" + strconv.FormatInt(id, 10) + ":" + strconv.Itoa(i), Title: rows[i], Detail: details[i], Run: func() tea.Cmd {
				settled = true
				return a.answerHosted(f, id, tuigateway.SetupHostedAnswerParams{Index: int64(i)})
			}}
		}
		p := wm.NewPalette(a.toolkit, items)
		p.Placeholder, p.OnClose = title, cancel
		return a.wm.ShowOverlay(p)
	case "checklist":
		rows := q.Rows
		d := &dialogCard{app: a, kind: "setupChecklist", requestID: f.job, focus: -1, marks: map[int]bool{}, title: title, multi: true,
			body: []string{tr(txt.SetupFlowFlowChecklistHint)}}
		for i, row := range rows {
			d.choices = append(d.choices, dialogChoice{label: row, value: strconv.Itoa(i)})
		}
		for _, i := range q.Picked {
			d.marks[int(i)] = true
		}
		d.onAnswer = func(_ *dialogChoice, marked []string, _ []string) tea.Cmd {
			settled = true
			chosen := []int64{}
			for _, value := range marked {
				n, _ := strconv.ParseInt(value, 10, 64)
				chosen = append(chosen, n)
			}
			return a.answerHosted(f, id, tuigateway.SetupHostedAnswerParams{Picked: chosen})
		}
		d.onCancel = cancel
		return a.wm.ShowOverlay(d)
	case "secret":
		d := &dialogCard{app: a, kind: "setupSecret", requestID: f.job, focus: 0, marks: map[int]bool{}, title: title,
			fields: []dialogField{{label: title, masked: true}}}
		d.onAnswer = func(_ *dialogChoice, _ []string, fields []string) tea.Cmd {
			settled = true
			return a.answerHosted(f, id, tuigateway.SetupHostedAnswerParams{Text: fields[0]})
		}
		d.onCancel = cancel
		return a.wm.ShowOverlay(d)
	case "command":
		argv := q.Argv
		if len(argv) == 0 {
			return cancel()
		}
		settled = true
		a.sys(tr(txt.SetupFlowFlowRunning, strings.Join(argv, " ")))
		return tty.Exec(exec.Command(argv[0], argv[1:]...), func(err error) tea.Msg {
			return asyncDoneMsg{run: func() tea.Cmd { return a.answerHosted(f, id, commandAnswer(err)) }}
		})
	}
	p := wm.NewPrompt(a.toolkit, title, "", func(text string) tea.Cmd {
		settled = true
		return a.answerHosted(f, id, tuigateway.SetupHostedAnswerParams{Text: text})
	})
	p.OnClose = cancel
	return a.wm.ShowOverlay(p)
}

// Answer of a terminal program run, ran when it exited on its own, else the start error
func commandAnswer(err error) tuigateway.SetupHostedAnswerParams {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return tuigateway.SetupHostedAnswerParams{Ran: true}
	case errors.As(err, &exit):
		status, waited := exit.Sys().(syscall.WaitStatus)
		return tuigateway.SetupHostedAnswerParams{Ran: !waited || !status.Signaled()}
	}
	return tuigateway.SetupHostedAnswerParams{Error: err.Error()}
}

// Settles a finished flow, naming a changed default model and starting the first session once one is set
func (a *app) hostedFlowDone(res *tuigateway.SetupHostedPollResult) tea.Cmd {
	a.store.ui.Status = a.store.statusFromBusy()
	model, provider := res.Model, res.Provider
	switch {
	case !res.Ok:
		a.sys(tr(txt.SetupFlowFlowStopped))
		return nil
	case model == "":
		return nil
	case a.store.ui.Sid != "":
		if res.Changed {
			a.sys(tr(txt.SetupFlowModelSavedLive, model, provider))
		}
		return nil
	}
	a.sys(tr(txt.SetupFlowModelSaved, model, provider))
	return a.newSession(nil)
}
