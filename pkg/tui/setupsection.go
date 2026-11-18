package tui

import (
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Kind of answer one setup field collects
type fieldKind int

const (
	fieldChoice fieldKind = iota
	fieldText
	fieldSecret
	fieldNumber
	fieldNotice
	fieldAction
	fieldCommand
	fieldDerived
)

// Dotted config key or env key an answer is written to
type sectionTarget struct {
	env bool
	key string
}

// One option of a choice field, a nil value leaving the target as it is
type sectionChoice struct {
	value  any
	title  string
	detail string
}

// Test over a walk that decides whether a field is asked
type sectionCond = func(run *sectionRun) bool

// One question of a setup section, where its answer goes and which keys the answer retires
type sectionField struct {
	id       string
	kind     fieldKind
	label    string
	targets  []sectionTarget
	clears   []sectionTarget
	choices  []sectionChoice
	whole    bool
	fallback any
	when     sectionCond
	validate func(value any) string
	settle   func(run *sectionRun, value any) any
	action   tuigateway.SetupAction
	argv     []string
	// Hooks over the loaded state, the options, wording, starting answer and action parameters of a field
	choicesFrom func(run *sectionRun) []sectionChoice
	labelFrom   func(run *sectionRun) string
	seed        func(run *sectionRun) any
	params      func(run *sectionRun) map[string]string
	// The wizard saves the config once this field is answered, so later new sections land after it
	savePoint bool
}

// Answer that saves the value a secret key already resolves to, from the environment or .env, into .env
type sectionHeld struct{}

// Value a section writes on every save without asking, only when its condition holds, a nil value removing the target
type sectionConstant struct {
	target sectionTarget
	value  any
	when   sectionCond
}

// Declarative setup section, its questions in order and its unconditional writes
type sectionSpec struct {
	key       string
	title     string
	detail    string
	intro     string
	fields    []sectionField
	constants []sectionConstant
	facts     []string
	// Named gateway values, keys read without being asked, and the whole config save the wizard ends with
	values   []string
	reads    []sectionTarget
	fullSave bool
}

// One walk through a section, what its targets held when it opened and the answers so far
type sectionRun struct {
	spec    sectionSpec
	held    map[string]any
	env     map[string]string
	secrets map[string]bool
	answers map[string]any
	facts   map[string]bool
	values  map[string]json.RawMessage
}

// Wire name of the file a target lives in
func (t sectionTarget) file() tuigateway.SetupTarget {
	if t.env {
		return "env"
	}
	return "config"
}

// Number as a float, false for anything that is not a number
func sectionFloat(value any) (float64, bool) {
	switch n := value.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// Number in the Go type a number field stores, false when it is not a number or not whole where required
func sectionNumber(value any, whole bool) (any, bool) {
	n, isNumber := sectionFloat(value)
	if !isNumber || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil, false
	}
	if whole {
		return int(n), n == math.Trunc(n)
	}
	return n, true
}

// Text a value is shown and written to .env as
func sectionText(value any) string {
	switch v := value.(type) {
	case bool:
		return strconv.FormatBool(v)
	case string:
		return v
	}
	if n, isNumber := sectionFloat(value); isNumber {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return fmt.Sprint(value)
}

// Refuses numbers outside the closed range with the given reason
func numberBetween(low, high float64, reason string) func(any) string {
	return func(value any) string {
		if n, _ := sectionFloat(value); n < low || n > high {
			return reason
		}
		return ""
	}
}

// One write of the save verb, the stored type following the Go type of the value
func sectionWrite(t sectionTarget, value any) tuigateway.SetupSectionWrite {
	if _, held := value.(sectionHeld); held && t.env {
		return tuigateway.SetupSectionWrite{Target: "env", Key: t.key, Kind: "held", Value: ""}
	}
	if t.env {
		return tuigateway.SetupSectionWrite{Target: "env", Key: t.key, Kind: "text", Value: sectionText(value)}
	}
	var kind tuigateway.SetupWriteKind = "text"
	switch value.(type) {
	case bool:
		kind = "bool"
	case int:
		kind = "whole"
	case float64:
		kind = "decimal"
	}
	return tuigateway.SetupSectionWrite{Target: "config", Key: t.key, Kind: kind, Value: value}
}

// Value the first target holds in the type of the field
func (run *sectionRun) current(f sectionField) any {
	if f.seed != nil {
		return f.seed(run)
	}
	if f.kind == fieldSecret || len(f.targets) == 0 {
		return nil
	}
	value, found := run.held[f.targets[0].key]
	if text, held := run.env[f.targets[0].key]; f.targets[0].env {
		value, found = text, held
	}
	if !found {
		value = f.fallback
	}
	if f.kind != fieldNumber {
		return value
	}
	if text, isText := value.(string); isText {
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return f.fallback
		}
		value = parsed
	}
	number, _ := sectionNumber(value, f.whole)
	return number
}

// Loads what the targets of a section hold, prints its introduction and asks the first field
func (a *app) runSection(spec sectionSpec) tea.Cmd {
	params := tuigateway.SetupSectionLoadParams{Facts: spec.facts, Values: spec.values}
	keys := func(t sectionTarget, secret bool) *[]string {
		switch {
		case t.env && secret:
			return &params.SecretEnvKeys
		case t.env:
			return &params.EnvKeys
		case secret:
			return &params.SecretConfigKeys
		}
		return &params.ConfigKeys
	}
	for _, t := range spec.reads {
		*keys(t, false) = append(*keys(t, false), t.key)
	}
	for _, f := range spec.fields {
		for _, t := range f.targets {
			*keys(t, f.kind == fieldSecret) = append(*keys(t, f.kind == fieldSecret), t.key)
		}
	}
	a.store.ui.Status = tr("setupFlow.section.loading")
	return ask(a.events.rpc, a.sys, tuigateway.SetupSectionLoad, params, func(res *tuigateway.SetupSectionLoadResult) tea.Cmd {
		a.store.ui.Status = a.store.statusFromBusy()
		if res == nil {
			return nil
		}
		run := &sectionRun{spec: spec, held: res.Config, env: res.Env, values: res.Values, secrets: map[string]bool{}, answers: map[string]any{}, facts: res.Facts}
		for _, key := range res.Secrets {
			run.secrets[key] = true
		}
		if spec.intro != "" {
			a.sys(spec.intro)
		}
		return a.askSectionField(run, 0)
	})
}

// Asks the field at the index, passing over fields whose condition fails, and saves after the last one
func (a *app) askSectionField(run *sectionRun, index int) tea.Cmd {
	for index < len(run.spec.fields) && run.spec.fields[index].when != nil && !run.spec.fields[index].when(run) {
		index++
	}
	if index == len(run.spec.fields) {
		return a.saveSection(run)
	}
	f := run.spec.fields[index]
	if f.labelFrom != nil {
		f.label = f.labelFrom(run)
	}
	if f.choicesFrom != nil {
		f.choices = f.choicesFrom(run)
	}
	current := run.current(f)
	answer := func(value any) tea.Cmd {
		if value != nil {
			run.answers[f.id] = value
		}
		return a.askSectionField(run, index+1)
	}
	switch f.kind {
	case fieldNotice:
		a.sys(f.label)
		return a.askSectionField(run, index+1)
	case fieldDerived:
		return answer(current)
	case fieldAction:
		return a.runSectionAction(run, index)
	case fieldCommand:
		return tty.Exec(exec.Command(f.argv[0], f.argv[1:]...), func(err error) tea.Msg {
			return sectionCommandMsg{run: run, index: index, err: err}
		})
	}
	if f.kind == fieldChoice {
		choices := f.choices
		listed := false
		for _, c := range choices {
			listed = listed || c.value != nil && c.value == current
		}
		items := make([]wm.Item, len(choices))
		for i, c := range choices {
			mark := "○ "
			if c.value == current {
				mark = "● "
			}
			detail := c.detail
			if c.value == nil && !listed && current != nil && sectionText(current) != "" {
				detail = tr("setupFlow.section.keeps", sectionText(current))
			}
			items[i] = wm.Item{ID: "setup:" + run.spec.key + ":" + f.id + ":" + strconv.Itoa(i), Title: mark + c.title, Detail: detail, Run: func() tea.Cmd { return answer(c.value) }}
		}
		sort.SliceStable(items, func(i, j int) bool {
			return strings.HasPrefix(items[i].Title, "● ") && !strings.HasPrefix(items[j].Title, "● ")
		})
		p := wm.NewPalette(a.toolkit, items)
		p.Placeholder = f.label
		return a.wm.ShowOverlay(p)
	}
	placeholder := f.label
	switch {
	case f.kind == fieldSecret && run.secrets[f.targets[0].key]:
		placeholder = tr("setupFlow.section.secretHeld", f.label)
	case current != nil && sectionText(current) != "":
		placeholder = tr("setupFlow.section.keep", f.label, sectionText(current))
	}
	submit := func(text string) tea.Cmd {
		entered := trimJS(text)
		if entered == "" {
			return answer(current)
		}
		var value any = entered
		if f.kind == fieldNumber {
			parsed, err := strconv.ParseFloat(entered, 64)
			number, fits := sectionNumber(parsed, f.whole)
			if err != nil || !fits {
				if f.whole {
					a.sys(tr("setupFlow.section.notWhole", entered, f.label))
				} else {
					a.sys(tr("setupFlow.section.notNumber", entered, f.label))
				}
				return a.askSectionField(run, index)
			}
			value = number
		}
		if f.validate != nil {
			if reason := f.validate(value); reason != "" {
				a.sys(reason)
				return a.askSectionField(run, index)
			}
		}
		return answer(value)
	}
	if f.kind == fieldSecret {
		d := &dialogCard{app: a, kind: "setupSecret", requestID: run.spec.key + ":" + f.id, marks: map[int]bool{}, title: placeholder,
			fields: []dialogField{{label: placeholder, masked: true}}}
		d.onAnswer = func(_ *dialogChoice, _ []string, fields []string) tea.Cmd { return submit(fields[0]) }
		return a.wm.ShowOverlay(d)
	}
	return a.wm.ShowOverlay(wm.NewPrompt(a.toolkit, placeholder, "", submit))
}

// Sends every constant and answer of the section in one save and prints what the files hold after it
func (a *app) saveSection(run *sectionRun) tea.Cmd {
	writes, removals := []tuigateway.SetupSectionWrite{}, []tuigateway.SetupSectionRemoval{}
	secret, sent := map[string]bool{}, map[string]string{}
	split := 0
	for _, c := range run.spec.constants {
		switch {
		case c.when != nil && !c.when(run):
		case c.value == nil:
			removals = append(removals, tuigateway.SetupSectionRemoval{Target: c.target.file(), Key: c.target.key})
		default:
			writes = append(writes, sectionWrite(c.target, c.value))
		}
	}
	for _, f := range run.spec.fields {
		value, answered := run.answers[f.id]
		if !answered {
			continue
		}
		if f.settle != nil {
			if value = f.settle(run, value); value == nil {
				continue
			}
		}
		for _, t := range f.targets {
			writes = append(writes, sectionWrite(t, value))
			secret[t.key] = f.kind == fieldSecret
			sent[t.key] = sectionText(value)
		}
		for _, t := range f.clears {
			removals = append(removals, tuigateway.SetupSectionRemoval{Target: t.file(), Key: t.key})
		}
		if f.savePoint && split == 0 {
			split = len(writes)
		}
	}
	if len(writes)+len(removals) == 0 && !run.spec.fullSave {
		a.sys(tr("setupFlow.section.unchanged", run.spec.title))
		return nil
	}
	a.store.ui.Status = tr("setupFlow.section.saving")
	if split == 0 || split == len(writes) {
		split = len(writes)
	}
	first := tuigateway.SetupSectionSaveParams{Writes: writes[:split], Removals: removals, FullSave: run.spec.fullSave}
	return ask(a.events.rpc, a.sys, tuigateway.SetupSectionSave, first, func(res *tuigateway.SetupSectionSaveResult) tea.Cmd {
		if res == nil || split == len(writes) {
			return a.reportSectionSave(run, writes, secret, sent, res)
		}
		later := tuigateway.SetupSectionSaveParams{Writes: writes[split:], Removals: []tuigateway.SetupSectionRemoval{}, FullSave: run.spec.fullSave}
		return ask(a.events.rpc, a.sys, tuigateway.SetupSectionSave, later, func(rest *tuigateway.SetupSectionSaveResult) tea.Cmd {
			if rest == nil {
				return a.reportSectionSave(run, writes[:split], secret, sent, res)
			}
			if res.Config == nil {
				res.Config = map[string]any{}
			}
			for key, value := range rest.Config {
				res.Config[key] = value
			}
			res.Env = append(res.Env, rest.Env...)
			return a.reportSectionSave(run, writes, secret, sent, res)
		})
	})
}

// Prints what the files hold for every write after a section save
func (a *app) reportSectionSave(run *sectionRun, writes []tuigateway.SetupSectionWrite, secret map[string]bool, sent map[string]string, res *tuigateway.SetupSectionSaveResult) tea.Cmd {
	a.store.ui.Status = a.store.statusFromBusy()
	if res == nil {
		return nil
	}
	lines := []string{tr("setupFlow.section.saved", run.spec.title)}
	inEnv := map[string]bool{}
	for _, key := range res.Env {
		inEnv[key] = true
	}
	for _, write := range writes {
		key := write.Key
		stored, inConfig := res.Config[key]
		switch {
		case secret[key] && (inEnv[key] || inConfig):
			lines = append(lines, tr("setupFlow.section.secretSaved", key))
		case inEnv[key]:
			lines = append(lines, tr("setupFlow.section.envValue", key, sent[key]))
		case inConfig:
			lines = append(lines, tr("setupFlow.section.value", key, sectionText(stored)))
		default:
			lines = append(lines, tr("setupFlow.section.missing", key))
		}
	}
	for _, key := range res.Removed {
		lines = append(lines, tr("setupFlow.section.removed", key))
	}
	a.sys(strings.Join(lines, "\n"))
	return nil
}

// Iteration budget, tool progress mode and context compression of the agent
func agentSection() sectionSpec {
	progress := []sectionChoice{
		{nil, tr("setupFlow.agent.progress.keep"), tr("setupFlow.agent.progress.keepDetail")},
		{"off", "off", tr("setupFlow.agent.progress.off")},
		{"new", "new", tr("setupFlow.agent.progress.new")},
		{"all", "all", tr("setupFlow.agent.progress.all")},
		{"verbose", "verbose", tr("setupFlow.agent.progress.verbose")},
		{"log", "log", tr("setupFlow.agent.progress.log")},
	}
	return sectionSpec{key: "agent", title: tr("setupFlow.agent.title"), detail: tr("setupFlow.agent.detail"), intro: tr("setupFlow.agent.intro"), fullSave: true,
		constants: []sectionConstant{{target: sectionTarget{key: "compression.enabled"}, value: true}},
		fields: []sectionField{
			{id: "max_turns", kind: fieldNumber, whole: true, label: tr("setupFlow.agent.maxTurns"),
				seed:     func(run *sectionRun) any { return run.held["agent.max_turns"] },
				targets:  []sectionTarget{{key: "agent.max_turns"}},
				clears:   []sectionTarget{{key: "max_turns"}, {env: true, key: "ICEBATH_MAX_ITERATIONS"}},
				validate: numberBetween(1, math.Inf(1), tr("setupFlow.agent.maxTurnsRange"))},
			{id: "tool_progress", kind: fieldChoice, label: tr("setupFlow.agent.progressLabel"),
				targets: []sectionTarget{{key: "display.tool_progress"}}, choices: progress, savePoint: true},
			{id: "threshold", kind: fieldNumber, fallback: 0.5, label: tr("setupFlow.agent.threshold"),
				targets:  []sectionTarget{{key: "compression.threshold"}},
				validate: numberBetween(0.5, 0.95, tr("setupFlow.agent.thresholdRange"))},
		}}
}

// Condition true when the gateway reported the named fact as holding
func factHolds(name string) sectionCond {
	return func(run *sectionRun) bool { return run.facts[name] }
}

// Condition true when the field holds the answer, nil matching a field without one
func answerIs(id string, value any) sectionCond {
	return func(run *sectionRun) bool { return run.answers[id] == value }
}

// Condition true when every listed condition holds
func allOf(conds ...sectionCond) sectionCond {
	return func(run *sectionRun) bool {
		for _, cond := range conds {
			if !cond(run) {
				return false
			}
		}
		return true
	}
}

// Condition true when the wrapped condition fails
func notCond(cond sectionCond) sectionCond {
	return func(run *sectionRun) bool { return !cond(run) }
}

// Starts the gateway action of a field and prints its output as it arrives, the outcome becoming the answer
func (a *app) runSectionAction(run *sectionRun, index int) tea.Cmd {
	f := run.spec.fields[index]
	a.store.ui.Status = f.label
	shown := ""
	var follow func(job string, after int) tea.Cmd
	follow = func(job string, after int) tea.Cmd {
		return ask(a.events.rpc, a.sys, tuigateway.SetupActionPoll, tuigateway.SetupActionPollParams{Job: job, After: int64(after), Live: ptr(shown)}, func(res *tuigateway.SetupActionPollResult) tea.Cmd {
			if res == nil {
				a.dropLiveRow(shown)
				a.store.ui.Status = a.store.statusFromBusy()
				return nil
			}
			lines, live := res.Lines, res.Live
			if len(lines) > 0 || live != shown {
				a.dropLiveRow(shown)
				if len(lines) > 0 {
					a.sys(strings.Join(lines, "\n"))
				}
				if shown = live; live != "" {
					a.sys(live)
				}
			}
			if !res.Done {
				return follow(job, after+len(lines))
			}
			a.store.ui.Status = a.store.statusFromBusy()
			run.answers[f.id] = res.Ok
			return a.askSectionField(run, index+1)
		})
	}
	start := tuigateway.SetupActionStartParams{Action: f.action}
	if f.params != nil {
		start.Params = f.params(run)
	}
	return ask(a.events.rpc, a.sys, tuigateway.SetupActionStart, start, func(res *tuigateway.SetupActionStartResult) tea.Cmd {
		if res == nil {
			a.store.ui.Status = a.store.statusFromBusy()
			return nil
		}
		return follow(res.Job, 0)
	})
}

// Removes the redrawn progress row when it is still the last transcript row
func (a *app) dropLiveRow(text string) {
	if n := len(a.history); text != "" && n > 0 && a.history[n-1].Role == "system" && a.history[n-1].Kind == "" && a.history[n-1].Text == text {
		a.history = a.history[:n-1]
		a.store.transcriptGen++
	}
}

// Exit of a command a setup field ran in the terminal
type sectionCommandMsg struct {
	run   *sectionRun
	index int
	err   error
}

// Records whether the command of a field succeeded, prints a failure and asks the next field
func (a *app) onSectionCommand(m sectionCommandMsg) tea.Cmd {
	f := m.run.spec.fields[m.index]
	if m.err != nil {
		a.sys(tr("setupFlow.section.commandFailed", strings.Join(f.argv, " "), m.err.Error()))
	}
	m.run.answers[f.id] = m.err == nil
	return a.askSectionField(m.run, m.index+1)
}
