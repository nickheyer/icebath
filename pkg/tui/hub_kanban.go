package tui

import (
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the kanban board
const kanbanContext = "tui.kanban"

// How often an open board reads its tasks again, so workers' moves show while it is open
const kanbanRefreshEvery = 3 * time.Second

// Keys of the kanban board beyond the shared hub keys
func registerKanbanKeys(km *keys.Map) {
	if keys.ContextDocs[kanbanContext] == "" {
		if err := keys.AddContext(kanbanContext, "kanban board"); err != nil {
			panic(err)
		}
	}
	for _, act := range []keys.Action{
		{ID: "tui.kanban.move", Context: kanbanContext, Label: "move", Doc: "put the highlighted task in another column", Default: []string{"m"}},
		{ID: "tui.kanban.comment", Context: kanbanContext, Label: "comment", Doc: "add a comment to the highlighted task", Default: []string{"c"}},
		{ID: "tui.kanban.assign", Context: kanbanContext, Label: "assign", Doc: "hand the highlighted task to a profile", Default: []string{"p"}},
		{ID: "tui.kanban.board", Context: kanbanContext, Label: "board", Doc: "switch to another board or make a new one", Default: []string{"b"}},
		{ID: "tui.kanban.archived", Context: kanbanContext, Label: "archived", Doc: "show or hide the archived tasks", Default: []string{"tab"}},
	} {
		km.Register(act)
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "kanban", order: 80, title: txt.HubsTitleKanban, short: txt.HubsShortKanban, slash: "kanban",
		usage: "[list|show|create|comment|complete|block|unblock|assign|boards …]", open: func(a *app, _ string) tea.Cmd { return a.openKanban() }, run: (*app).kanbanSlash,
		suggest: func(string) []string {
			return []string{"list", "show", "create", "comment", "complete", "block", "unblock", "assign", "stats", "boards", "runs", "log"}
		}, keys: registerKanbanKeys})
}

// /kanban: the board, or the words after it run by the background service as icebath kanban runs them
func (a *app) kanbanSlash(r *slashRun) tea.Cmd {
	if strings.TrimSpace(r.arg) == "" {
		return tea.Batch(r.ran(), a.openKanban())
	}
	return r.viaGateway(tr(txt.HubsTitleKanban), tr(txt.SlashCmdCoreNoOutputFor, "kanban"))
}

// One row of the board: a column heading or a task under it
type kanbanRow struct {
	column tuigateway.KanbanColumn
	count  int
	task   *tuigateway.KanbanTaskRow
}

// Kanban board: the tasks of one board grouped by column, kept current while it is open
type kanbanHub struct {
	a    *app
	h    *cards.Hub
	list *cards.List
	view tuigateway.KanbanBoardResult
	rows []kanbanRow
	// Whether archived tasks show, and whether the board was read once
	archived, loaded bool
}

// Opens the kanban board on the current board and reads it
func (a *app) openKanban() tea.Cmd {
	if line, absent := linktier.Refusal("/kanban", linktier.Extras); absent {
		return wm.Notify(line, wm.Warn)
	}
	k := &kanbanHub{a: a}
	task := func(i int, fn func(t tuigateway.KanbanTaskRow) tea.Cmd) tea.Cmd {
		if i < 0 || i >= len(k.rows) || k.rows[i].task == nil {
			return nil
		}
		return fn(*k.rows[i].task)
	}
	k.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleKanban), Glyph: a.toolkit.Glyphs.Plan, Key: "tui:kanban", Context: kanbanContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsKanbanColTask), Flex: 1}, {Title: tr(txt.HubsKanbanColWho), Max: 18}, {Title: tr(txt.HubsKanbanColAge), Right: true}},
		Yield:   []int{2},
		Rows:    k.cells,
		Detail:  func(i int) []string { return k.detail(i) },
		Status:  k.status,
		Empty: func() string {
			if !k.loaded {
				return tr(txt.HubsKanbanLoading)
			}
			return tr(txt.HubsKanbanEmpty, k.boardName(), a.keys.Display("hub.add"))
		},
		Hints: k.hints,
		Free:  map[string]bool{"hub.add": true, "tui.kanban.board": true, "tui.kanban.archived": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return task(i, k.open) },
			"hub.add": func(*cards.List, int) tea.Cmd { return k.form(nil) },
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				return task(i, func(t tuigateway.KanbanTaskRow) tea.Cmd { return k.form(&t) })
			},
			"hub.rename": func(_ *cards.List, i int) tea.Cmd { return task(i, k.rename) },
			"hub.toggle": func(_ *cards.List, i int) tea.Cmd { return task(i, k.toggle) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				return task(i, func(t tuigateway.KanbanTaskRow) tea.Cmd { return k.remove(l, t) })
			},
			"tui.kanban.move":    func(_ *cards.List, i int) tea.Cmd { return task(i, k.move) },
			"tui.kanban.comment": func(_ *cards.List, i int) tea.Cmd { return task(i, k.comment) },
			"tui.kanban.assign":  func(_ *cards.List, i int) tea.Cmd { return task(i, k.assign) },
			"tui.kanban.board":   func(*cards.List, int) tea.Cmd { return k.boards() },
			"tui.kanban.archived": func(*cards.List, int) tea.Cmd {
				k.archived = !k.archived
				return k.load(false)
			},
		},
	})
	k.h = a.newHub("tui:kanban", k.list)
	return tea.Batch(a.wm.ShowOverlay(k.h), k.load(true), k.schedule())
}

// Reads the board again, the first read saying so in the hub, the cursor kept on the task it was on
func (k *kanbanHub) load(first bool) tea.Cmd {
	busy := ""
	if first {
		busy = tr(txt.HubsKanbanLoading)
	}
	params := tuigateway.KanbanBoardParams{Board: k.view.Board, Archived: k.archived}
	return hubCall(k.a, k.h, busy, tuigateway.KanbanBoardView, params, func(r *tuigateway.KanbanBoardResult) tea.Cmd {
		anchor := ""
		if i := k.list.Index(); i >= 0 && i < len(k.rows) && k.rows[i].task != nil {
			anchor = k.rows[i].task.ID
		}
		k.view, k.loaded = *r, true
		k.group()
		k.list.Refresh()
		for i, row := range k.rows {
			if (anchor == "" && row.task != nil) || (row.task != nil && row.task.ID == anchor) {
				k.list.Select(i)
				if anchor != "" {
					break
				}
				anchor = row.task.ID
			}
		}
		return nil
	})
}

// Next quiet read of the board while it is open
func (k *kanbanHub) schedule() tea.Cmd {
	return tea.Tick(kanbanRefreshEvery, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			if !k.a.wm.ShowsOverlay(k.h) {
				return nil
			}
			return tea.Batch(k.load(false), k.schedule())
		}}
	})
}

// Builds the rows: each column holding tasks under its heading in board order, archived tasks last
func (k *kanbanHub) group() {
	columns := slices.Clone(k.view.Columns)
	if k.archived {
		columns = append(columns, tuigateway.KanbanColumn{Key: "archived", Label: tr(txt.HubsKanbanArchivedColumn), Icon: "▫"})
	}
	k.rows = nil
	for _, col := range columns {
		var under []kanbanRow
		for i := range k.view.Tasks {
			if t := &k.view.Tasks[i]; t.Status == col.Key {
				under = append(under, kanbanRow{column: col, task: t})
			}
		}
		if len(under) == 0 {
			continue
		}
		k.rows = append(k.rows, kanbanRow{column: col, count: len(under)})
		k.rows = append(k.rows, under...)
	}
}

// Label of a column key in the board's own words
func (k *kanbanHub) columnLabel(key string) string {
	for _, c := range k.view.Columns {
		if c.Key == key {
			return c.Label
		}
	}
	if key == "archived" {
		return tr(txt.HubsKanbanArchivedColumn)
	}
	return key
}

// Display name of the board on screen
func (k *kanbanHub) boardName() string {
	for _, b := range k.view.Boards {
		if b.Slug == k.view.Board && b.Name != "" {
			return b.Name
		}
	}
	return k.view.Board
}

// Short age of an epoch second: minutes, hours or days
func kanbanAge(epoch int64) string {
	if epoch <= 0 {
		return ""
	}
	d := time.Since(time.Unix(epoch, 0))
	switch {
	case d < time.Hour:
		return strconv.Itoa(max(int(d.Minutes()), 0)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

// Cells of every row, a heading bold with its count
func (k *kanbanHub) cells() [][]string {
	t := k.a.toolkit
	out := make([][]string, len(k.rows))
	for i, row := range k.rows {
		if row.task == nil {
			out[i] = []string{row.column.Icon, render.Heading(row.column.Label+" · "+strconv.Itoa(row.count), t), "", ""}
			continue
		}
		task := row.task
		age := kanbanAge(task.CreatedAt)
		if task.Status == "running" && task.StartedAt > 0 {
			age = kanbanAge(task.StartedAt)
		}
		title := "  " + task.Title
		if task.Status == "blocked" && task.BlockKind != "" {
			title += render.Dim(" · "+task.BlockKind, t)
		}
		out[i] = []string{"", title, task.Assignee, age}
	}
	return out
}

// Detail pane of the highlighted task
func (k *kanbanHub) detail(i int) []string {
	if i < 0 || i >= len(k.rows) || k.rows[i].task == nil {
		return nil
	}
	return k.taskLines(*k.rows[i].task, nil)
}

// Lines that describe a task, with its comments, attempts and links once they were read
func (k *kanbanHub) taskLines(task tuigateway.KanbanTaskRow, full *tuigateway.KanbanTaskResult) []string {
	t := k.a.toolkit
	field := func(label txt.Key, value string) string { return render.Dim(tr(label)+": ", t) + value }
	out := []string{render.Heading(task.Title, t) + "  " + render.Dim(k.columnLabel(task.Status)+" · "+task.ID, t)}
	if task.Assignee != "" {
		out = append(out, field(txt.HubsKanbanColWho, task.Assignee))
	}
	if task.Priority != 0 {
		out = append(out, field(txt.HubsKanbanFormPriority, strconv.FormatInt(task.Priority, 10)))
	}
	if len(task.Skills) > 0 {
		out = append(out, field(txt.HubsKanbanFormSkills, strings.Join(task.Skills, ", ")))
	}
	if task.Model != "" {
		out = append(out, field(txt.HubsKanbanDetailModel, task.Model))
	}
	if task.Body != "" {
		out = append(out, "", task.Body)
	}
	if task.Result != "" {
		out = append(out, "", field(txt.HubsKanbanDetailResult, task.Result))
	}
	if task.LastError != "" {
		out = append(out, render.Bad(tr(txt.HubsKanbanDetailError)+": "+task.LastError, t))
	}
	if full == nil {
		return out
	}
	if len(full.Parents) > 0 {
		out = append(out, field(txt.HubsKanbanDetailParents, strings.Join(full.Parents, ", ")))
	}
	if len(full.Children) > 0 {
		out = append(out, field(txt.HubsKanbanDetailChildren, strings.Join(full.Children, ", ")))
	}
	if len(full.Comments) > 0 {
		out = append(out, "", render.Heading(tr(txt.HubsKanbanDetailComments), t))
		for _, c := range full.Comments {
			out = append(out, render.Dim(c.Author+" · "+kanbanAge(c.CreatedAt), t)+"  "+c.Body)
		}
	}
	if len(full.Runs) > 0 {
		out = append(out, "", render.Heading(tr(txt.HubsKanbanDetailRuns), t))
		for _, r := range full.Runs {
			line := strings.TrimSpace(r.Profile + " · " + firstText(r.Outcome, r.Status) + " · " + kanbanAge(r.StartedAt))
			if s := firstText(r.Summary, r.Error); s != "" {
				line += "  " + strings.ReplaceAll(s, "\n", " ")
			}
			out = append(out, line)
		}
	}
	return out
}

// Trailer of the board: its name and how many tasks it holds
func (k *kanbanHub) status() string {
	if !k.loaded {
		return ""
	}
	text := k.boardName() + " · " + render.Count(len(k.view.Tasks), "task")
	if k.archived {
		text += " · " + tr(txt.HubsKanbanArchivedShown)
	}
	return text
}

// Legend of the highlighted row, the task keys only on a task
func (k *kanbanHub) hints(_ *cards.List, i int) []wm.Hint {
	out := []wm.Hint{}
	if i >= 0 && i < len(k.rows) && k.rows[i].task != nil {
		done := strings.ToLower(tr(txt.HubsKanbanActComplete))
		if s := k.rows[i].task.Status; s == "done" || s == "archived" {
			done = strings.ToLower(tr(txt.HubsKanbanActReopen))
		}
		out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "tui.kanban.move"}, wm.Hint{Action: "hub.toggle", Label: done},
			wm.Hint{Action: "tui.kanban.comment"}, wm.Hint{Action: "tui.kanban.assign"}, wm.Hint{Action: "hub.remove"})
	}
	return append(out, wm.Hint{Action: "hub.add"}, wm.Hint{Action: "tui.kanban.board"}, wm.Hint{Action: "tui.kanban.archived"})
}

// Runs a task command on the board, saying what it did or why it did not
func (k *kanbanHub) command(args []string, done string) tea.Cmd {
	params := tuigateway.KanbanCommandParams{Board: k.view.Board, Args: args}
	return hubCall(k.a, k.h, tr(txt.HubsKanbanSaving), tuigateway.KanbanTaskCommand, params, func(r *tuigateway.KanbanCommandResult) tea.Cmd {
		if !r.OK {
			return k.h.Fail(firstText(kanbanLastLine(r.Output), tr(txt.HubsKanbanFailed)))
		}
		return tea.Batch(k.h.Done(done), k.load(false))
	})
}

// Last line of a command's output that says something, where its refusal stands
func kanbanLastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// One move of a task to another column, the command that makes it and the columns it starts from
type kanbanMove struct {
	verb, to string
	from     []string
	does     txt.Key
	// Asks why, the answer kept as a comment
	reason bool
}

// Moves a person may make, each by the command that makes it
var kanbanMoves = []kanbanMove{
	{verb: "specify", to: "todo", from: []string{"triage"}, does: txt.HubsKanbanMoveSpecify},
	{verb: "promote", to: "ready", from: []string{"todo", "blocked"}, does: txt.HubsKanbanMovePromote},
	{verb: "unblock", to: "ready", from: []string{"blocked", "scheduled"}, does: txt.HubsKanbanMoveUnblock},
	{verb: "request-review", to: "review", from: []string{"ready", "running"}, does: txt.HubsKanbanMoveReview},
	{verb: "reopen-review", to: "ready", from: []string{"review"}, does: txt.HubsKanbanMoveChanges},
	{verb: "block", to: "blocked", from: []string{"triage", "todo", "scheduled", "ready", "running", "review"}, does: txt.HubsKanbanMoveBlock, reason: true},
	{verb: "schedule", to: "scheduled", from: []string{"triage", "todo", "ready", "blocked"}, does: txt.HubsKanbanMoveSchedule, reason: true},
	{verb: "complete", to: "done", from: []string{"triage", "todo", "scheduled", "ready", "running", "blocked", "review"}, does: txt.HubsKanbanMoveComplete},
	{verb: "reopen", to: "ready", from: []string{"done", "archived"}, does: txt.HubsKanbanMoveReopen},
	{verb: "archive", to: "archived", from: []string{"triage", "todo", "scheduled", "ready", "blocked", "review", "done"}, does: txt.HubsKanbanMoveArchive},
}

// Picks the column to move the task to among the moves its column allows, asking why where the move keeps a reason
func (k *kanbanHub) move(task tuigateway.KanbanTaskRow) tea.Cmd {
	var choices []hubChoice
	moves := map[string]kanbanMove{}
	for _, m := range kanbanMoves {
		if slices.Contains(m.from, task.Status) {
			choices = append(choices, hubChoice{id: m.verb, title: k.columnLabel(m.to), detail: tr(m.does)})
			moves[m.verb] = m
		}
	}
	return k.a.hubPick(k.h, tr(txt.HubsKanbanMoveTitle, task.Title), choices, "", func(c hubChoice) tea.Cmd {
		m := moves[c.id]
		done := tr(txt.HubsKanbanMoved, task.Title, k.columnLabel(m.to))
		if !m.reason {
			return k.command([]string{m.verb, task.ID}, done)
		}
		spec := cards.FormSpec{Title: k.columnLabel(m.to), Submit: k.columnLabel(m.to),
			Fields: []cards.Field{{ID: "reason", Label: tr(txt.HubsKanbanReason), Help: tr(txt.HubsKanbanReasonHelp)}}}
		return k.a.hubForm(k.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
			args := []string{m.verb, task.ID}
			if reason := strings.TrimSpace(v["reason"]); reason != "" {
				args = append(args, reason)
			}
			return k.command(args, done)
		})
	})
}

// Completes a task, or reopens a done or archived one
func (k *kanbanHub) toggle(task tuigateway.KanbanTaskRow) tea.Cmd {
	if task.Status == "done" || task.Status == "archived" {
		return k.command([]string{"reopen", task.ID}, tr(txt.HubsKanbanReopened, task.Title))
	}
	return k.command([]string{"complete", task.ID}, tr(txt.HubsKanbanCompleted, task.Title))
}

// Asks before archiving a task, or before deleting one already archived for good
func (k *kanbanHub) remove(l *cards.List, task tuigateway.KanbanTaskRow) tea.Cmd {
	if task.Status == "archived" {
		l.Confirm(tr(txt.HubsKanbanDeleteConfirm, task.Title), func() tea.Cmd {
			return k.command([]string{"archive", "--rm", task.ID}, tr(txt.HubsKanbanDeleted, task.Title))
		})
		return nil
	}
	l.Confirm(tr(txt.HubsKanbanArchiveConfirm, task.Title), func() tea.Cmd {
		return k.command([]string{"archive", task.ID}, tr(txt.HubsKanbanArchivedOne, task.Title))
	})
	return nil
}

// Asks for a comment and adds it to the task
func (k *kanbanHub) comment(task tuigateway.KanbanTaskRow) tea.Cmd {
	return k.a.hubAsk(k.h, task.Title, tr(txt.HubsKanbanActComment), "", false, func(text string) tea.Cmd {
		if text == "" {
			return nil
		}
		return k.command([]string{"comment", task.ID, text}, tr(txt.HubsKanbanCommented, task.Title))
	})
}

// Asks for the task's new title
func (k *kanbanHub) rename(task tuigateway.KanbanTaskRow) tea.Cmd {
	return k.a.hubAsk(k.h, task.Title, tr(txt.HubsKanbanFormTitle), task.Title, false, func(title string) tea.Cmd {
		if title == "" || title == task.Title {
			return nil
		}
		return k.command([]string{"edit", task.ID, "--title", title}, tr(txt.HubsKanbanSaved, title))
	})
}

// Profiles a task can go to, nobody first
func (k *kanbanHub) assigneeChoices(current string) []cards.Choice {
	out := []cards.Choice{{Value: "", Label: tr(txt.HubsKanbanFormNobody)}}
	for _, name := range k.view.Assignees {
		out = append(out, cards.Choice{Value: name, Label: name})
	}
	if current != "" && !slices.Contains(k.view.Assignees, current) {
		out = append(out, cards.Choice{Value: current, Label: current})
	}
	return out
}

// Picks the profile the task goes to, nobody unassigning it
func (k *kanbanHub) assign(task tuigateway.KanbanTaskRow) tea.Cmd {
	var choices []hubChoice
	for _, c := range k.assigneeChoices(task.Assignee) {
		choices = append(choices, hubChoice{id: c.Value, title: c.Label})
	}
	return k.a.hubPick(k.h, tr(txt.HubsKanbanAssignTitle, task.Title), choices, task.Assignee, func(c hubChoice) tea.Cmd {
		profile := c.id
		if profile == "" {
			profile = "none"
		}
		return k.command([]string{"assign", task.ID, profile}, tr(txt.HubsKanbanAssigned, task.Title, c.title))
	})
}

// Form of a new task, or of the task to change: title, details and priority, with assignee, skills and triage for a new one
func (k *kanbanHub) form(task *tuigateway.KanbanTaskRow) tea.Cmd {
	var was tuigateway.KanbanTaskRow
	if task != nil {
		was = *task
	}
	priority := ""
	if was.Priority != 0 {
		priority = strconv.FormatInt(was.Priority, 10)
	}
	fields := []cards.Field{
		{ID: "title", Label: tr(txt.HubsKanbanFormTitle), Help: tr(txt.HubsKanbanFormTitleHelp), Value: was.Title, Required: true},
		{ID: "body", Label: tr(txt.HubsKanbanFormBody), Help: tr(txt.HubsKanbanFormBodyHelp), Value: was.Body},
		{ID: "priority", Label: tr(txt.HubsKanbanFormPriority), Help: tr(txt.HubsKanbanFormPriorityHelp), Kind: cards.FieldNumber, Value: priority},
	}
	title, submit := tr(txt.HubsKanbanFormTitleNew), tr(txt.HubsKanbanFormCreate)
	if task == nil {
		fields = append(fields,
			cards.Field{ID: "assignee", Label: tr(txt.HubsKanbanColWho), Help: tr(txt.HubsKanbanFormAssigneeHelp), Kind: cards.FieldChoice, Choices: k.assigneeChoices("")},
			cards.Field{ID: "skills", Label: tr(txt.HubsKanbanFormSkills), Help: tr(txt.HubsKanbanFormSkillsHelp), More: true},
			cards.Field{ID: "triage", Label: tr(txt.HubsKanbanFormTriage), Help: tr(txt.HubsKanbanFormTriageHelp), Kind: cards.FieldBool, Value: "false", More: true})
	} else {
		title, submit = was.Title, tr(txt.HubsKanbanFormSave)
	}
	return k.a.hubForm(k.h, cards.FormSpec{Title: title, Submit: submit, Fields: fields}, func(_ *cards.Form, v map[string]string) tea.Cmd {
		name := strings.TrimSpace(v["title"])
		if task == nil {
			args := []string{"create", name}
			if body := strings.TrimSpace(v["body"]); body != "" {
				args = append(args, "--body", body)
			}
			if p := strings.TrimSpace(v["priority"]); p != "" {
				args = append(args, "--priority", p)
			}
			if who := v["assignee"]; who != "" {
				args = append(args, "--assignee", who)
			}
			for _, s := range strings.Split(v["skills"], ",") {
				if s = strings.TrimSpace(s); s != "" {
					args = append(args, "--skill", s)
				}
			}
			if v["triage"] == "true" {
				args = append(args, "--triage")
			}
			return k.command(args, tr(txt.HubsKanbanAdded, name))
		}
		args := []string{"edit", was.ID}
		if name != was.Title {
			args = append(args, "--title", name)
		}
		if body := strings.TrimSpace(v["body"]); body != was.Body {
			args = append(args, "--body", body)
		}
		if p := strings.TrimSpace(v["priority"]); p != priority && p != "" {
			args = append(args, "--priority", p)
		}
		if len(args) == 2 {
			return nil
		}
		return k.command(args, tr(txt.HubsKanbanSaved, name))
	})
}

// Task screen: everything about the task, its comments and attempts, and what can be done to it
func (k *kanbanHub) open(task tuigateway.KanbanTaskRow) tea.Cmd {
	var full *tuigateway.KanbanTaskResult
	current := func() tuigateway.KanbanTaskRow {
		for _, t := range k.view.Tasks {
			if t.ID == task.ID {
				return t
			}
		}
		return task
	}
	back := func(next func(tuigateway.KanbanTaskRow) tea.Cmd) func() tea.Cmd {
		return func() tea.Cmd { return tea.Batch(k.h.Pop(), next(current())) }
	}
	screen := k.a.hubActions(k.h, task.Title, func() []string { return k.taskLines(current(), full) }, func() []hubAction {
		now := current()
		acts := []hubAction{
			{key: "tui.kanban.move", label: tr(txt.HubsKanbanActMove), does: tr(txt.HubsKanbanActMoveDoes), run: back(k.move)},
			{key: "hub.edit", label: tr(txt.HubsKanbanActEdit), does: tr(txt.HubsKanbanActEditDoes), run: back(func(t tuigateway.KanbanTaskRow) tea.Cmd { return k.form(&t) })},
			{key: "tui.kanban.comment", label: tr(txt.HubsKanbanActComment), does: tr(txt.HubsKanbanActCommentDoes), run: back(k.comment)},
			{key: "tui.kanban.assign", label: tr(txt.HubsKanbanActAssign), does: tr(txt.HubsKanbanActAssignDoes), run: back(k.assign)},
		}
		if now.Status == "done" || now.Status == "archived" {
			acts = append(acts, hubAction{key: "hub.toggle", label: tr(txt.HubsKanbanActReopen), does: tr(txt.HubsKanbanActReopenDoes), run: back(k.toggle)})
		} else {
			acts = append(acts, hubAction{key: "hub.toggle", label: tr(txt.HubsKanbanActComplete), does: tr(txt.HubsKanbanActCompleteDoes), run: back(k.toggle)})
		}
		if now.Status == "archived" {
			return append(acts, hubAction{key: "hub.remove", label: tr(txt.HubsKanbanActDelete), does: tr(txt.HubsKanbanActDeleteDoes), confirm: tr(txt.HubsKanbanDeleteConfirm, now.Title),
				run: back(func(t tuigateway.KanbanTaskRow) tea.Cmd {
					return k.command([]string{"archive", "--rm", t.ID}, tr(txt.HubsKanbanDeleted, t.Title))
				})})
		}
		return append(acts, hubAction{key: "hub.remove", label: tr(txt.HubsKanbanActArchive), does: tr(txt.HubsKanbanActArchiveDoes), confirm: tr(txt.HubsKanbanArchiveConfirm, now.Title),
			run: back(func(t tuigateway.KanbanTaskRow) tea.Cmd {
				return k.command([]string{"archive", t.ID}, tr(txt.HubsKanbanArchivedOne, t.Title))
			})})
	})
	read := hubCall(k.a, k.h, "", tuigateway.KanbanTaskShow, tuigateway.KanbanTaskParams{Board: k.view.Board, TaskID: task.ID}, func(r *tuigateway.KanbanTaskResult) tea.Cmd {
		full = r
		return nil
	})
	return tea.Batch(screen, read)
}

// Picks the board to show among every board, or a new one to make
func (k *kanbanHub) boards() tea.Cmd {
	const fresh = "\x00new"
	choices := []hubChoice{{id: fresh, title: tr(txt.HubsKanbanNewBoard)}}
	for _, b := range k.view.Boards {
		name := b.Name
		if name == "" {
			name = b.Slug
		}
		total := int64(0)
		for _, n := range b.Counts {
			total += n
		}
		choices = append(choices, hubChoice{id: b.Slug, title: strings.TrimSpace(b.Icon + " " + name), detail: b.Slug + " · " + render.Count(int(total), "task")})
	}
	board := func(args []string, done string, slug string) tea.Cmd {
		params := tuigateway.KanbanCommandParams{Args: args}
		return hubCall(k.a, k.h, tr(txt.HubsKanbanSaving), tuigateway.KanbanBoardCommand, params, func(r *tuigateway.KanbanCommandResult) tea.Cmd {
			if !r.OK {
				return k.h.Fail(firstText(kanbanLastLine(r.Output), tr(txt.HubsKanbanFailed)))
			}
			k.view.Board = slug
			return tea.Batch(k.h.Done(done), k.load(false))
		})
	}
	return k.a.hubPick(k.h, tr(txt.HubsKanbanBoardTitle), choices, k.view.Board, func(c hubChoice) tea.Cmd {
		if c.id != fresh {
			return board([]string{"switch", c.id}, tr(txt.HubsKanbanSwitched, c.title), c.id)
		}
		spec := cards.FormSpec{Title: tr(txt.HubsKanbanFormBoardNew), Submit: tr(txt.HubsKanbanFormBoardCreate), Fields: []cards.Field{
			{ID: "slug", Label: tr(txt.HubsKanbanFormSlug), Help: tr(txt.HubsKanbanFormSlugHelp), Required: true},
			{ID: "name", Label: tr(txt.HubsKanbanFormBoardName), Help: tr(txt.HubsKanbanFormBoardNameHelp)}}}
		return k.a.hubForm(k.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
			slug := strings.TrimSpace(v["slug"])
			args := []string{"create", slug, "--switch"}
			if name := strings.TrimSpace(v["name"]); name != "" {
				args = append(args, "--name", name)
			}
			return board(args, tr(txt.HubsKanbanBoardCreated, firstText(strings.TrimSpace(v["name"]), slug)), slug)
		})
	})
}
