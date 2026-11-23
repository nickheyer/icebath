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

// Keymap contexts of the scheduled jobs screen and of a job's notepad
const (
	cronContext        = "tui.cron"
	cronNotepadContext = "tui.cron.notepad"
)

// How often an open scheduled jobs screen reads the jobs again, so a run in progress and its end show
const cronRefreshEvery = 5 * time.Second

// Keys of the scheduled jobs screen beyond the shared hub keys
func registerCronKeys(km *keys.Map) {
	for name, doc := range map[string]string{cronContext: "scheduled jobs screen", cronNotepadContext: "notepad of a scheduled job"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	for _, act := range []keys.Action{
		{ID: "tui.cron.run", Context: cronContext, Label: "run now", Doc: "start a run of the highlighted job now", Default: []string{"n"}},
		{ID: "tui.cron.history", Context: cronContext, Label: "runs", Doc: "show every run of the highlighted job", Default: []string{"h"}},
		{ID: "tui.cron.health", Context: cronContext, Label: "health", Doc: "check the scheduler and show failed runs", Default: []string{"c"}},
		{ID: "tui.cron.service", Context: cronContext, Label: "install service", Doc: "install the background service so jobs run while icebath is closed", Default: []string{"s"}},
		{ID: "tui.cron.notepad.set", Context: cronNotepadContext, Label: "set", Doc: "add a note or change one", Default: []string{"a"}},
		{ID: "tui.cron.notepad.delete", Context: cronNotepadContext, Label: "delete", Doc: "delete a note", Default: []string{"x"}},
	} {
		km.Register(act)
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "cron", order: 70, title: txt.HubsTitleCron, short: txt.HubsShortCron, slash: "cron", usage: "[list|create|edit|pause|resume|run|remove|runs …]", open: func(a *app, _ string) tea.Cmd { return a.openCron() }, run: (*app).cronSlash,
		suggest: func(string) []string {
			return []string{"list", "create", "edit", "pause", "resume", "run", "cancel", "remove", "runs", "status", "doctor", "incidents"}
		}, keys: registerCronKeys})
}

// /cron: the scheduled jobs screen, or the words after it run by the background service as icebath cron runs them
func (a *app) cronSlash(r *slashRun) tea.Cmd {
	if strings.TrimSpace(r.arg) == "" {
		return tea.Batch(r.ran(), a.openCron())
	}
	return r.viaGateway(tr(txt.HubsTitleCron), tr(txt.SlashCmdCoreNoOutputFor, "cron"))
}

// Scheduled jobs screen: every job with its schedule and last outcome, kept current while it is open
type cronHub struct {
	a    *app
	h    *cards.Hub
	list *cards.List
	jobs []tuigateway.CronJobRow
	// Whether the background service starts with the computer, and whether the jobs were read once
	installed, loaded bool
}

// Opens the scheduled jobs screen and reads the jobs into it
func (a *app) openCron() tea.Cmd {
	if line, absent := linktier.Refusal("/cron", linktier.Extras); absent {
		return wm.Notify(line, wm.Warn)
	}
	c := &cronHub{a: a}
	c.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleCron), Glyph: a.toolkit.Glyphs.Jobs, Key: "tui:cron", Context: cronContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsCronColJob), Flex: 2, Max: 36}, {Title: tr(txt.HubsCronColSchedule), Max: 22},
			{Title: tr(txt.HubsCronColNext), Right: true}, {Title: tr(txt.HubsCronColLast), Flex: 1}},
		Yield:  []int{4, 3},
		Rows:   c.rows,
		Detail: func(i int) []string { return c.detail(i) },
		Banner: c.banner,
		Status: func() string {
			if !c.loaded {
				return ""
			}
			return render.Count(len(c.jobs), "job")
		},
		Empty: func() string {
			if !c.loaded {
				return tr(txt.HubsCronLoading)
			}
			return tr(txt.HubsCronEmpty, a.keys.Display("hub.add"))
		},
		Hints: c.hints,
		Free:  map[string]bool{"hub.add": true, "tui.cron.health": true, "tui.cron.service": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return c.actions(i) },
			"hub.add": func(*cards.List, int) tea.Cmd { return c.form(nil) },
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				return c.withJob(i, func(j tuigateway.CronJobRow) tea.Cmd { return c.form(&j) })
			},
			"hub.rename": func(_ *cards.List, i int) tea.Cmd { return c.withJob(i, c.rename) },
			"hub.toggle": func(_ *cards.List, i int) tea.Cmd { return c.withJob(i, c.toggle) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				return c.withJob(i, func(j tuigateway.CronJobRow) tea.Cmd { return c.remove(l, j) })
			},
			"tui.cron.run":     func(_ *cards.List, i int) tea.Cmd { return c.withJob(i, c.run) },
			"tui.cron.history": func(_ *cards.List, i int) tea.Cmd { return c.withJob(i, c.history) },
			"tui.cron.health":  func(*cards.List, int) tea.Cmd { return c.health() },
			"tui.cron.service": func(*cards.List, int) tea.Cmd { return c.installService() },
		},
	})
	c.h = a.newHub("tui:cron", c.list)
	return tea.Batch(a.wm.ShowOverlay(c.h), c.load(true), c.schedule())
}

// Order of a job's state on the screen: running and scheduled first, then failing, paused and finished
func cronStateRank(j tuigateway.CronJobRow) int {
	switch {
	case j.Running:
		return 0
	case j.State == "scheduled":
		return 1
	case j.State == "error":
		return 2
	case j.State == "paused":
		return 3
	}
	return 4
}

// Reads the jobs again, saying so in the hub on the first read and leaving the rows as they were when the read fails
func (c *cronHub) load(first bool) tea.Cmd {
	busy := ""
	if first {
		busy = tr(txt.HubsCronLoading)
	}
	return hubCall(c.a, c.h, busy, tuigateway.CronJobsList, tuigateway.CronJobsListParams{All: true}, func(r *tuigateway.CronJobsListResult) tea.Cmd {
		anchor := ""
		if i := c.list.Index(); i >= 0 && i < len(c.jobs) {
			anchor = c.jobs[i].ID
		}
		c.jobs, c.installed, c.loaded = r.Jobs, r.ServiceInstalled, true
		slices.SortStableFunc(c.jobs, func(x, y tuigateway.CronJobRow) int {
			if d := cronStateRank(x) - cronStateRank(y); d != 0 {
				return d
			}
			return strings.Compare(x.NextRunAt, y.NextRunAt)
		})
		c.list.Refresh()
		for i, j := range c.jobs {
			if j.ID == anchor {
				c.list.Select(i)
			}
		}
		return nil
	})
}

// Next quiet read of the jobs while the screen is open
func (c *cronHub) schedule() tea.Cmd {
	return tea.Tick(cronRefreshEvery, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			if !c.a.wm.ShowsOverlay(c.h) {
				return nil
			}
			return tea.Batch(c.load(false), c.schedule())
		}}
	})
}

// Runs fn on the job of row i, nothing for no row
func (c *cronHub) withJob(i int, fn func(j tuigateway.CronJobRow) tea.Cmd) tea.Cmd {
	if i < 0 || i >= len(c.jobs) {
		return nil
	}
	return fn(c.jobs[i])
}

// Name a job shows under, its id when it has none
func cronJobName(j tuigateway.CronJobRow) string {
	if j.Name != "" {
		return j.Name
	}
	return j.ID
}

// Mark of a job's state
func cronMark(j tuigateway.CronJobRow) string {
	switch {
	case j.Running:
		return "▶"
	case j.State == "paused":
		return "⏸"
	case j.State == "completed":
		return "✓"
	case j.State == "error":
		return "✗"
	}
	return "●"
}

// Word for a job's state in the language of the screen
func cronStateWord(j tuigateway.CronJobRow) string {
	if j.Running {
		return tr(txt.HubsCronStateRunning)
	}
	return trOr("hubs.cron.state."+j.State, j.State)
}

// Parsed stamp of the job store, false for an empty or unreadable one
func cronStamp(iso string) (time.Time, bool) {
	if iso == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, iso, clockLocation()); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Short local time of a store stamp: the clock for today, the date and clock otherwise
func cronWhen(iso string) string {
	t, ok := cronStamp(iso)
	if !ok {
		return iso
	}
	t = t.In(clockLocation())
	if now := time.Now().In(clockLocation()); now.YearDay() == t.YearDay() && now.Year() == t.Year() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// Word for a run's outcome
func cronResultWord(status string) string { return trOr("hubs.cron.result."+status, status) }

// Cells of every job
func (c *cronHub) rows() [][]string {
	out := make([][]string, len(c.jobs))
	for i, j := range c.jobs {
		next := "—"
		if j.NextRunAt != "" && j.State != "paused" && j.State != "completed" {
			next = cronWhen(j.NextRunAt)
		}
		last := ""
		if j.LastRunAt != "" {
			last = cronWhen(j.LastRunAt)
			if j.LastStatus != "" {
				last += " " + cronResultWord(j.LastStatus)
			}
		}
		if j.Running {
			last = tr(txt.HubsCronStateRunning)
		}
		out[i] = []string{cronMark(j), cronJobName(j), j.Schedule, next, last}
	}
	return out
}

// Lines of a job's detail pane: its state, task, schedule and options
func (c *cronHub) detail(i int) []string {
	if i < 0 || i >= len(c.jobs) {
		return nil
	}
	j := c.jobs[i]
	t := c.a.toolkit
	field := func(label txt.Key, value string) string { return render.Dim(tr(label)+": ", t) + value }
	out := []string{render.Heading(cronJobName(j), t) + "  " + render.Dim(cronStateWord(j)+" · "+j.ID, t)}
	out = append(out, field(txt.HubsCronDetailTask, j.Prompt), field(txt.HubsCronFormSchedule, j.Schedule))
	if j.NextRunAt != "" && j.State != "paused" && j.State != "completed" {
		out = append(out, field(txt.HubsCronDetailNext, cronWhen(j.NextRunAt)))
	}
	if j.LastRunAt != "" {
		out = append(out, field(txt.HubsCronDetailLast, strings.TrimSpace(cronWhen(j.LastRunAt)+" "+cronResultWord(j.LastStatus))))
	}
	if j.LastError != "" {
		out = append(out, render.Bad(tr(txt.HubsCronDetailError)+": "+j.LastError, t))
	}
	if j.PausedReason != "" {
		out = append(out, field(txt.HubsCronDetailPausedWhy, j.PausedReason))
	}
	if j.RepeatTimes != nil {
		out = append(out, field(txt.HubsCronDetailRepeat, tr(txt.HubsCronDetailRepeatOf, j.RepeatDone, *j.RepeatTimes)))
	}
	for _, f := range []struct {
		label txt.Key
		value string
	}{{txt.HubsCronDetailSkills, strings.Join(j.Skills, ", ")}, {txt.HubsCronDetailDeliver, j.Deliver}, {txt.HubsCronDetailModel, strings.Trim(j.Provider+" "+j.Model, " ")},
		{txt.HubsCronFormEffort, j.ReasoningEffort}, {txt.HubsCronDetailWorkdir, j.Workdir}, {txt.HubsCronDetailScript, j.Script}, {txt.HubsCronDetailTimezone, j.Timezone}} {
		if f.value != "" {
			out = append(out, field(f.label, f.value))
		}
	}
	return out
}

// Line above the jobs while they only run with icebath open, naming the key that installs the background service
func (c *cronHub) banner() string {
	if c.installed || !slices.ContainsFunc(c.jobs, func(j tuigateway.CronJobRow) bool { return j.Enabled }) {
		return ""
	}
	return tr(txt.HubsCronServiceNeeded, c.a.keys.Display("tui.cron.service"))
}

// Legend of the highlighted job, pause or resume as its state needs
func (c *cronHub) hints(_ *cards.List, i int) []wm.Hint {
	out := []wm.Hint{}
	if i >= 0 && i < len(c.jobs) {
		toggle := tr(txt.HubsCronActPause)
		if c.jobs[i].State == "paused" {
			toggle = tr(txt.HubsCronActResume)
		}
		out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "tui.cron.run"}, wm.Hint{Action: "hub.toggle", Label: strings.ToLower(toggle)},
			wm.Hint{Action: "hub.edit"}, wm.Hint{Action: "tui.cron.history"}, wm.Hint{Action: "hub.remove"})
	}
	return append(out, wm.Hint{Action: "hub.add"}, wm.Hint{Action: "tui.cron.health"})
}

// Shows what a change of a job said, its failure staying in the hub
func (c *cronHub) settled(r *tuigateway.CronJobResult, done string) tea.Cmd {
	if !r.OK {
		return c.h.Fail(firstText(r.Output, tr(txt.HubsCronFailed)))
	}
	return tea.Batch(c.h.Done(done), c.load(false))
}

// Actions screen of a job: run, pause or resume, edit, runs, notepad, stop a live run, remove
func (c *cronHub) actions(i int) tea.Cmd {
	return c.withJob(i, func(j tuigateway.CronJobRow) tea.Cmd {
		current := func() tuigateway.CronJobRow {
			for _, now := range c.jobs {
				if now.ID == j.ID {
					return now
				}
			}
			return j
		}
		detail := func() []string {
			for k, now := range c.jobs {
				if now.ID == j.ID {
					return c.detail(k)
				}
			}
			return nil
		}
		back := func(next func(tuigateway.CronJobRow) tea.Cmd) func() tea.Cmd {
			return func() tea.Cmd { return tea.Batch(c.h.Pop(), next(current())) }
		}
		return c.a.hubActions(c.h, cronJobName(j), detail, func() []hubAction {
			now := current()
			acts := []hubAction{{key: "tui.cron.run", label: tr(txt.HubsCronActRun), does: tr(txt.HubsCronActRunDoes), run: back(c.run)}}
			if now.State == "paused" {
				acts = append(acts, hubAction{key: "hub.toggle", label: tr(txt.HubsCronActResume), does: tr(txt.HubsCronActResumeDoes), run: back(c.toggle)})
			} else if now.State != "completed" {
				acts = append(acts, hubAction{key: "hub.toggle", label: tr(txt.HubsCronActPause), does: tr(txt.HubsCronActPauseDoes), run: back(c.toggle)})
			}
			acts = append(acts,
				hubAction{key: "hub.edit", label: tr(txt.HubsCronActEdit), does: tr(txt.HubsCronActEditDoes), run: back(func(j tuigateway.CronJobRow) tea.Cmd { return c.form(&j) })},
				hubAction{key: "tui.cron.history", label: tr(txt.HubsCronActHistory), does: tr(txt.HubsCronActHistoryDoes), run: back(c.history)},
				hubAction{label: tr(txt.HubsCronActNotepad), does: tr(txt.HubsCronActNotepadDoes), run: back(c.notepad)})
			if now.Running {
				acts = append(acts, hubAction{label: tr(txt.HubsCronActCancel), does: tr(txt.HubsCronActCancelDoes), confirm: tr(txt.HubsCronCancelConfirm, cronJobName(now)),
					run: back(c.cancel)})
			}
			return append(acts, hubAction{key: "hub.remove", label: tr(txt.HubsCronActRemove), does: tr(txt.HubsCronActRemoveDoes),
				confirm: tr(txt.HubsCronRemoveConfirm, cronJobName(now)), run: back(c.removeNow)})
		})
	})
}

// Starts a run of the job now, its result landing in its runs
func (c *cronHub) run(j tuigateway.CronJobRow) tea.Cmd {
	return hubCall(c.a, c.h, tr(txt.HubsCronStarting, cronJobName(j)), tuigateway.CronJobsRun, tuigateway.CronJobActionParams{JobID: j.ID}, func(r *tuigateway.CronJobResult) tea.Cmd {
		return c.settled(r, tr(txt.HubsCronStarted, cronJobName(j)))
	})
}

// Pauses a scheduled job or resumes a paused one
func (c *cronHub) toggle(j tuigateway.CronJobRow) tea.Cmd {
	if j.State == "completed" {
		return nil
	}
	if j.State == "paused" {
		return hubCall(c.a, c.h, tr(txt.HubsCronSaving), tuigateway.CronJobsResume, tuigateway.CronJobActionParams{JobID: j.ID}, func(r *tuigateway.CronJobResult) tea.Cmd {
			return c.settled(r, tr(txt.HubsCronResumed, cronJobName(j)))
		})
	}
	return hubCall(c.a, c.h, tr(txt.HubsCronSaving), tuigateway.CronJobsPause, tuigateway.CronJobActionParams{JobID: j.ID}, func(r *tuigateway.CronJobResult) tea.Cmd {
		return c.settled(r, tr(txt.HubsCronPaused, cronJobName(j)))
	})
}

// Stops the run of the job going on now, its schedule left as it is
func (c *cronHub) cancel(j tuigateway.CronJobRow) tea.Cmd {
	return hubCall(c.a, c.h, tr(txt.HubsCronSaving), tuigateway.CronJobsCancel, tuigateway.CronJobActionParams{JobID: j.ID}, func(r *tuigateway.CronJobResult) tea.Cmd {
		return c.settled(r, tr(txt.HubsCronCancelled, cronJobName(j)))
	})
}

// Asks before removing the job
func (c *cronHub) remove(l *cards.List, j tuigateway.CronJobRow) tea.Cmd {
	l.Confirm(tr(txt.HubsCronRemoveConfirm, cronJobName(j)), func() tea.Cmd { return c.removeNow(j) })
	return nil
}

// Removes the job
func (c *cronHub) removeNow(j tuigateway.CronJobRow) tea.Cmd {
	return hubCall(c.a, c.h, tr(txt.HubsCronSaving), tuigateway.CronJobsRemove, tuigateway.CronJobActionParams{JobID: j.ID}, func(r *tuigateway.CronJobResult) tea.Cmd {
		return c.settled(r, tr(txt.HubsCronRemoved, cronJobName(j)))
	})
}

// Asks for the job's new name
func (c *cronHub) rename(j tuigateway.CronJobRow) tea.Cmd {
	return c.a.hubAsk(c.h, tr(txt.HubsCronFormTitleEdit, cronJobName(j)), tr(txt.HubsCronFormName), j.Name, false, func(name string) tea.Cmd {
		if name == "" || name == j.Name {
			return nil
		}
		params := tuigateway.CronJobWriteParams{JobID: j.ID, Fields: tuigateway.CronJobFields{Name: &name}}
		return hubCall(c.a, c.h, tr(txt.HubsCronSaving), tuigateway.CronJobsUpdate, params, func(r *tuigateway.CronJobResult) tea.Cmd {
			return c.settled(r, tr(txt.HubsCronSaved, name))
		})
	})
}

// Queues the install of the background service for after exit, saying in one sentence what it does
func (c *cronHub) installService() tea.Cmd {
	if c.installed {
		return nil
	}
	return c.a.queueAfterExitCmd([]string{"gateway", "install"}, tr(txt.HubsCronServiceWhy))
}

// Reasoning levels a job may pin, after the choice of following the settings
var cronEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// Form of a new job, or of the job to change, sending only what changed on an edit
func (c *cronHub) form(j *tuigateway.CronJobRow) tea.Cmd {
	var was tuigateway.CronJobRow
	if j != nil {
		was = *j
	}
	repeat := ""
	if was.RepeatTimes != nil {
		repeat = strconv.FormatInt(*was.RepeatTimes, 10)
	}
	efforts := []cards.Choice{{Value: "", Label: tr(txt.HubsCronFormEffortDefault)}}
	for _, e := range cronEfforts {
		efforts = append(efforts, cards.Choice{Value: e, Label: e})
	}
	fields := []cards.Field{
		{ID: "schedule", Label: tr(txt.HubsCronFormSchedule), Help: tr(txt.HubsCronFormScheduleHelp), Value: was.ScheduleText, Required: true, Placeholder: "every 1h"},
		{ID: "prompt", Label: tr(txt.HubsCronDetailTask), Help: tr(txt.HubsCronFormPromptHelp), Value: was.Prompt, Required: j == nil || !was.NoAgent},
		{ID: "name", Label: tr(txt.HubsCronFormName), Help: tr(txt.HubsCronFormNameHelp), Value: was.Name},
		{ID: "deliver", Label: tr(txt.HubsCronDetailDeliver), Help: tr(txt.HubsCronFormDeliverHelp), Value: was.Deliver, Placeholder: "local"},
		{ID: "skills", Label: tr(txt.HubsCronDetailSkills), Help: tr(txt.HubsCronFormSkillsHelp), Value: strings.Join(was.Skills, ", "), More: true},
		{ID: "repeat", Label: tr(txt.HubsCronDetailRepeat), Help: tr(txt.HubsCronFormRepeatHelp), Kind: cards.FieldNumber, Value: repeat, More: true},
		{ID: "model", Label: tr(txt.HubsCronDetailModel), Help: tr(txt.HubsCronFormModelHelp), Value: was.Model, More: true},
		{ID: "provider", Label: tr(txt.HubsCronFormProvider), Help: tr(txt.HubsCronFormProviderHelp), Value: was.Provider, More: true},
		{ID: "effort", Label: tr(txt.HubsCronFormEffort), Kind: cards.FieldChoice, Value: was.ReasoningEffort, Choices: efforts, More: true},
		{ID: "workdir", Label: tr(txt.HubsCronDetailWorkdir), Help: tr(txt.HubsCronFormWorkdirHelp), Value: was.Workdir, More: true},
	}
	title, submit, busy := tr(txt.HubsCronFormTitleNew), tr(txt.HubsCronFormCreate), tr(txt.HubsCronCreating)
	if j == nil {
		fields = append(fields, cards.Field{ID: "paused", Label: tr(txt.HubsCronFormPaused), Help: tr(txt.HubsCronFormPausedHelp), Kind: cards.FieldBool, Value: "false", More: true})
	} else {
		title, submit, busy = tr(txt.HubsCronFormTitleEdit, cronJobName(was)), tr(txt.HubsCronFormSave), tr(txt.HubsCronSaving)
	}
	spec := cards.FormSpec{Title: title, Fields: fields, Submit: submit}
	return c.a.hubForm(c.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		fields := tuigateway.CronJobFields{}
		// A field is sent when it is new or changed, so an edit never rewrites what it did not touch
		text := func(id, before string) *string {
			value := strings.TrimSpace(v[id])
			if j != nil && value == before {
				return nil
			}
			if j == nil && value == "" {
				return nil
			}
			return &value
		}
		fields.Schedule, fields.Prompt, fields.Name = text("schedule", was.ScheduleText), text("prompt", was.Prompt), text("name", was.Name)
		fields.Deliver, fields.Model, fields.Provider = text("deliver", was.Deliver), text("model", was.Model), text("provider", was.Provider)
		fields.ReasoningEffort, fields.Workdir = text("effort", was.ReasoningEffort), text("workdir", was.Workdir)
		if n := strings.TrimSpace(v["repeat"]); n != repeat && n != "" {
			count, err := strconv.ParseInt(n, 10, 64)
			if err != nil {
				return c.h.Fail(err.Error())
			}
			fields.Repeat = &count
		}
		var skills []string
		for _, s := range strings.Split(v["skills"], ",") {
			if s = strings.TrimSpace(s); s != "" {
				skills = append(skills, s)
			}
		}
		if !slices.Equal(skills, was.Skills) && (j != nil || len(skills) > 0) {
			fields.SetSkills, fields.Skills = true, skills
		}
		fields.Paused = v["paused"] == "true"
		if j == nil {
			return hubCall(c.a, c.h, busy, tuigateway.CronJobsCreate, tuigateway.CronJobWriteParams{Fields: fields}, func(r *tuigateway.CronJobResult) tea.Cmd {
				name := firstText(v["name"], v["prompt"])
				if r.Job != nil {
					name = cronJobName(*r.Job)
				}
				return c.settled(r, tr(txt.HubsCronCreated, name))
			})
		}
		return hubCall(c.a, c.h, busy, tuigateway.CronJobsUpdate, tuigateway.CronJobWriteParams{JobID: was.ID, Fields: fields}, func(r *tuigateway.CronJobResult) tea.Cmd {
			return c.settled(r, tr(txt.HubsCronSaved, cronJobName(was)))
		})
	})
}

// Runs screen of a job: every attempt with its outcome, enter showing the output it saved
func (c *cronHub) history(j tuigateway.CronJobRow) tea.Cmd {
	var runs []tuigateway.CronJobRun
	var outputs []tuigateway.CronJobOutput
	loaded := false
	var list *cards.List
	read := func() tea.Cmd {
		return hubCall(c.a, c.h, "", tuigateway.CronJobsRuns, tuigateway.CronJobRunsParams{JobID: j.ID, Limit: 100}, func(r *tuigateway.CronJobRunsResult) tea.Cmd {
			runs, outputs, loaded = r.Runs, r.Outputs, true
			list.Refresh()
			return nil
		})
	}
	list = cards.NewList(c.a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsCronHistoryTitle), Key: "tui:cron:runs", Context: hubActionsContext, Primary: "hub.run",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsColWhen)}, {Title: tr(txt.HubsColStatus)}, {Title: tr(txt.HubsCronHistoryColSource)}, {Title: tr(txt.HubsCronDetailError), Flex: 1}},
		Yield:   []int{3, 4},
		Rows: func() [][]string {
			out := make([][]string, len(runs))
			for i, r := range runs {
				mark := "·"
				switch r.Status {
				case "completed":
					mark = "✓"
				case "failed", "error", "crashed", "timed_out":
					mark = "✗"
				case "running", "claimed":
					mark = "▶"
				}
				out[i] = []string{mark, cronWhen(r.ClaimedAt), cronResultWord(r.Status), r.Source, strings.ReplaceAll(r.Error, "\n", " ")}
			}
			return out
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(runs) {
				return nil
			}
			r := runs[i]
			out := []string{render.Heading(cronWhen(r.ClaimedAt)+" · "+cronResultWord(r.Status), c.a.toolkit), r.ID}
			if r.FinishedAt != "" {
				out = append(out, tr(txt.HubsCronHistoryFinished, cronWhen(r.FinishedAt)))
			}
			if r.Delivery != "" {
				out = append(out, tr(txt.HubsCronHistoryDelivery, r.Delivery))
			}
			if r.Error != "" {
				out = append(out, render.Bad(r.Error, c.a.toolkit))
			}
			return out
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsCronLoading)
			}
			return tr(txt.HubsCronHistoryEmpty)
		},
		Hints: func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.run", Label: tr(txt.HintsLabelOpen)}} },
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.run": func(_ *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(runs) {
					return nil
				}
				return c.output(j, runs, i, outputs)
			},
		},
	})
	return tea.Batch(c.h.Push(list), read())
}

// Output a run saved: the saved output written while the run went on, or right after it ended
func cronRunOutput(runs []tuigateway.CronJobRun, i int, outputs []tuigateway.CronJobOutput) (tuigateway.CronJobOutput, bool) {
	r := runs[i]
	from, ok := cronStamp(firstText(r.StartedAt, r.ClaimedAt))
	if !ok {
		return tuigateway.CronJobOutput{}, false
	}
	until := from.Add(24 * time.Hour)
	if i > 0 {
		if next, ok := cronStamp(runs[i-1].ClaimedAt); ok {
			until = next
		}
	}
	for _, o := range outputs {
		at, err := time.ParseInLocation("2006-01-02 15:04:05", o.At, clockLocation())
		if err == nil && !at.Before(from.Add(-time.Second)) && at.Before(until) {
			return o, true
		}
	}
	return tuigateway.CronJobOutput{}, false
}

// Page of what one run saved, or of how it ended when it saved nothing
func (c *cronHub) output(j tuigateway.CronJobRow, runs []tuigateway.CronJobRun, i int, outputs []tuigateway.CronJobOutput) tea.Cmd {
	r := runs[i]
	title := cronWhen(r.ClaimedAt) + " · " + cronResultWord(r.Status)
	saved, found := cronRunOutput(runs, i, outputs)
	if !found {
		lines := []string{tr(txt.HubsCronHistoryNoOutput)}
		if r.Error != "" {
			lines = append(lines, "", render.Bad(r.Error, c.a.toolkit))
		}
		return c.h.Push(cards.NewPage(c.a.hubEnv(), cards.PageSpec{Title: title, Key: "tui:cron:output", Lines: func(int) []string { return lines }}))
	}
	params := tuigateway.CronJobOutputParams{JobID: j.ID, Name: saved.Name}
	return hubCall(c.a, c.h, tr(txt.HubsCronReading), tuigateway.CronJobsOutput, params, func(res *tuigateway.CronJobOutputResult) tea.Cmd {
		lines := strings.Split(strings.TrimRight(res.Content, "\n"), "\n")
		return c.h.Push(cards.NewPage(c.a.hubEnv(), cards.PageSpec{Title: title, Key: "tui:cron:output", Lines: func(int) []string { return lines }}))
	})
}

// Notepad of a job: the notes it keeps between runs, with set and delete
func (c *cronHub) notepad(j tuigateway.CronJobRow) tea.Cmd {
	text := ""
	var page *cards.Page
	read := func() tea.Cmd {
		params := tuigateway.CronReportParams{Command: "notepad", JobID: j.ID, Action: "list"}
		return hubCall(c.a, c.h, "", tuigateway.CronReport, params, func(r *tuigateway.CronReportResult) tea.Cmd {
			text = r.Output
			return nil
		})
	}
	change := func(params tuigateway.CronReportParams, done string) tea.Cmd {
		return hubCall(c.a, c.h, tr(txt.HubsCronSaving), tuigateway.CronReport, params, func(r *tuigateway.CronReportResult) tea.Cmd {
			if !r.OK {
				return c.h.Fail(firstText(r.Output, tr(txt.HubsCronFailed)))
			}
			return tea.Batch(c.h.Done(done), read())
		})
	}
	page = cards.NewPage(c.a.hubEnv(), cards.PageSpec{Title: tr(txt.HubsCronNotepadTitle), Key: "tui:cron:notepad", Context: cronNotepadContext,
		Lines: func(int) []string { return strings.Split(text, "\n") },
		Hints: func(*cards.Page) []wm.Hint {
			return []wm.Hint{{Action: "tui.cron.notepad.set"}, {Action: "tui.cron.notepad.delete"}}
		},
		Actions: map[string]func(p *cards.Page) tea.Cmd{
			"tui.cron.notepad.set": func(*cards.Page) tea.Cmd {
				spec := cards.FormSpec{Title: tr(txt.HubsCronNotepadTitle), Submit: tr(txt.HubsCronNotepadSet), Fields: []cards.Field{
					{ID: "key", Label: tr(txt.HubsCronNotepadKey), Required: true}, {ID: "value", Label: tr(txt.HubsCronNotepadValue), Required: true}}}
				return c.a.hubForm(c.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
					key, value := strings.TrimSpace(v["key"]), v["value"]
					return change(tuigateway.CronReportParams{Command: "notepad", JobID: j.ID, Action: "set", Key: &key, Value: &value}, tr(txt.HubsCronNotepadSaved, key))
				})
			},
			"tui.cron.notepad.delete": func(*cards.Page) tea.Cmd {
				return c.a.hubAsk(c.h, tr(txt.HubsCronNotepadTitle), tr(txt.HubsCronNotepadKey), "", false, func(key string) tea.Cmd {
					return change(tuigateway.CronReportParams{Command: "notepad", JobID: j.ID, Action: "delete", Key: &key}, tr(txt.HubsCronNotepadDeleted, key))
				})
			},
		},
	})
	return tea.Batch(c.h.Push(page), read())
}

// Health screen: the scheduler's status, a check of every job, then the failures still open, each acknowledged from its own actions
func (c *cronHub) health() tea.Cmd {
	var incidents []tuigateway.CronIncident
	var list *cards.List
	read := func() tea.Cmd {
		return hubCall(c.a, c.h, "", tuigateway.CronIncidentsList, tuigateway.CronIncidentsParams{}, func(r *tuigateway.CronIncidentsResult) tea.Cmd {
			incidents = slices.DeleteFunc(r.Incidents, func(inc tuigateway.CronIncident) bool { return inc.State == "closed" })
			list.Refresh()
			return nil
		})
	}
	report := func(command, title string) tea.Cmd {
		return hubCall(c.a, c.h, tr(txt.HubsCronReading), tuigateway.CronReport, tuigateway.CronReportParams{Command: command}, func(r *tuigateway.CronReportResult) tea.Cmd {
			lines := strings.Split(r.Output, "\n")
			return c.h.Push(cards.NewPage(c.a.hubEnv(), cards.PageSpec{Title: title, Key: "tui:cron:" + command, Lines: func(int) []string { return lines }}))
		})
	}
	fixed := 2
	list = cards.NewList(c.a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsCronHealthTitle), Key: "tui:cron:health", Context: hubActionsContext, Primary: "hub.run",
		Columns: []render.Column{{Title: tr(txt.HubsColAction), Max: 36}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Rows: func() [][]string {
			out := [][]string{{tr(txt.HubsCronHealthStatus), tr(txt.HubsCronHealthStatusDoes)}, {tr(txt.HubsCronHealthDoctor), tr(txt.HubsCronHealthDoctorDoes)}}
			for _, inc := range incidents {
				out = append(out, []string{render.Bad("✗ "+inc.JobID, c.a.toolkit), cronWhen(inc.LastSeenAt) + " · " + strings.ReplaceAll(inc.Error, "\n", " ")})
			}
			if len(incidents) == 0 {
				out = append(out, []string{render.Dim(tr(txt.HubsCronHealthNoFailures), c.a.toolkit), ""})
			}
			return out
		},
		Detail: func(i int) []string {
			if i < fixed || i-fixed >= len(incidents) {
				return nil
			}
			inc := incidents[i-fixed]
			return []string{render.Heading(inc.JobID+" · "+inc.State, c.a.toolkit), inc.FailureType, tr(txt.HubsCronHealthSeen, cronWhen(inc.FirstSeenAt), cronWhen(inc.LastSeenAt)), "", inc.Error}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i >= fixed && i-fixed < len(incidents) {
				return []wm.Hint{{Action: "hub.run", Label: strings.ToLower(tr(txt.HubsCronIncidentAck))}}
			}
			return []wm.Hint{{Action: "hub.run"}}
		},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.run": func(l *cards.List, i int) tea.Cmd {
				switch {
				case i == 0:
					return report("status", tr(txt.HubsCronHealthStatus))
				case i == 1:
					return report("doctor", tr(txt.HubsCronHealthDoctor))
				case i-fixed >= len(incidents):
					return nil
				}
				inc := incidents[i-fixed]
				l.Confirm(tr(txt.HubsCronAckConfirm, inc.JobID), func() tea.Cmd {
					return hubCall(c.a, c.h, tr(txt.HubsCronSaving), tuigateway.CronIncidentsAck, tuigateway.CronIncidentAckParams{ID: inc.ID}, func(r *tuigateway.CronReportResult) tea.Cmd {
						if !r.OK {
							return c.h.Fail(firstText(r.Output, tr(txt.HubsCronFailed)))
						}
						return tea.Batch(c.h.Done(tr(txt.HubsCronAcked, inc.JobID)), read())
					})
				})
				return nil
			},
		},
	})
	return tea.Batch(c.h.Push(list), read())
}
