package tui

import (
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Kind of answer one setup field collects
type fieldKind int

const (
	fieldChoice fieldKind = iota
	fieldText
	fieldSecret
	fieldToggle
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
	action   string
	argv     []string
	// Hooks over the loaded state, the options, wording, starting answer and action parameters of a field
	choicesFrom func(run *sectionRun) []sectionChoice
	labelFrom   func(run *sectionRun) string
	seed        func(run *sectionRun) any
	params      func(run *sectionRun) map[string]any
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
	env     map[string]any
	secrets map[string]bool
	answers map[string]any
	facts   map[string]bool
	values  map[string]any
}

// Wire name of the file a target lives in
func (t sectionTarget) file() string {
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
func sectionWrite(t sectionTarget, value any) map[string]any {
	if _, held := value.(sectionHeld); held && t.env {
		return map[string]any{"target": "env", "key": t.key, "kind": "held", "value": ""}
	}
	if t.env {
		return map[string]any{"target": "env", "key": t.key, "kind": "text", "value": sectionText(value)}
	}
	kind := "text"
	switch value.(type) {
	case bool:
		kind = "bool"
	case int:
		kind = "whole"
	case float64:
		kind = "decimal"
	}
	return map[string]any{"target": "config", "key": t.key, "kind": kind, "value": value}
}

// Value the first target holds in the type of the field, a toggle on only when every target is on
func (run *sectionRun) current(f sectionField) any {
	if f.seed != nil {
		return f.seed(run)
	}
	if f.kind == fieldSecret || len(f.targets) == 0 {
		return nil
	}
	if f.kind == fieldToggle {
		for _, t := range f.targets {
			if t.env && !strings.EqualFold(sectionText(run.env[t.key]), "true") || !t.env && run.held[t.key] != true {
				return false
			}
		}
		return true
	}
	value, found := run.held[f.targets[0].key]
	if f.targets[0].env {
		value, found = run.env[f.targets[0].key]
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
	params := map[string]any{"config_keys": []any{}, "env_keys": []any{}, "secret_config_keys": []any{}, "secret_env_keys": []any{}, "facts": []any{}, "values": []any{}}
	for _, name := range spec.facts {
		params["facts"] = append(params["facts"].([]any), name)
	}
	for _, name := range spec.values {
		params["values"] = append(params["values"].([]any), name)
	}
	for _, t := range spec.reads {
		params[t.file()+"_keys"] = append(params[t.file()+"_keys"].([]any), t.key)
	}
	for _, f := range spec.fields {
		for _, t := range f.targets {
			name := t.file() + "_keys"
			if f.kind == fieldSecret {
				name = "secret_" + name
			}
			params[name] = append(params[name].([]any), t.key)
		}
	}
	a.store.ui.Status = tr("setupFlow.section.loading")
	return a.events.rpc("setup.section.load", params, func(res map[string]any) tea.Cmd {
		a.store.ui.Status = a.store.statusFromBusy()
		if res == nil {
			return nil
		}
		run := &sectionRun{spec: spec, secrets: map[string]bool{}, answers: map[string]any{}, facts: map[string]bool{}}
		facts, _ := res["facts"].(map[string]any)
		for name, holds := range facts {
			run.facts[name] = holds == true
		}
		run.held, _ = res["config"].(map[string]any)
		run.env, _ = res["env"].(map[string]any)
		run.values, _ = res["values"].(map[string]any)
		for _, key := range stringList(res["secrets"]) {
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
	if f.kind == fieldChoice || f.kind == fieldToggle {
		choices := f.choices
		if f.kind == fieldToggle {
			choices = []sectionChoice{{true, tr("setupFlow.section.on"), ""}, {false, tr("setupFlow.section.off"), ""}}
		}
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
	writes, removals := []any{}, []any{}
	secret, sent := map[string]bool{}, map[string]string{}
	split := 0
	for _, c := range run.spec.constants {
		switch {
		case c.when != nil && !c.when(run):
		case c.value == nil:
			removals = append(removals, map[string]any{"target": c.target.file(), "key": c.target.key})
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
			removals = append(removals, map[string]any{"target": t.file(), "key": t.key})
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
	first := map[string]any{"writes": writes[:split], "removals": removals, "full_save": run.spec.fullSave}
	return a.events.rpc("setup.section.save", first, func(res map[string]any) tea.Cmd {
		if res == nil || split == len(writes) {
			return a.reportSectionSave(run, writes, secret, sent, res)
		}
		return a.events.rpc("setup.section.save", map[string]any{"writes": writes[split:], "removals": []any{}, "full_save": run.spec.fullSave},
			func(rest map[string]any) tea.Cmd {
				if rest == nil {
					return a.reportSectionSave(run, writes[:split], secret, sent, res)
				}
				held, _ := res["config"].(map[string]any)
				if held == nil {
					held = map[string]any{}
					res["config"] = held
				}
				later, _ := rest["config"].(map[string]any)
				for key, value := range later {
					held[key] = value
				}
				res["env"] = append(append([]any{}, wireAny(res["env"])...), wireAny(rest["env"])...)
				return a.reportSectionSave(run, writes, secret, sent, res)
			})
	})
}

// Elements of a wire list, none for anything else
func wireAny(v any) []any {
	list, _ := v.([]any)
	return list
}

// Prints what the files hold for every write after a section save
func (a *app) reportSectionSave(run *sectionRun, writes []any, secret map[string]bool, sent map[string]string, res map[string]any) tea.Cmd {
	a.store.ui.Status = a.store.statusFromBusy()
	if res == nil {
		return nil
	}
	lines := []string{tr("setupFlow.section.saved", run.spec.title)}
	held, _ := res["config"].(map[string]any)
	inEnv := map[string]bool{}
	for _, key := range stringList(res["env"]) {
		inEnv[key] = true
	}
	for _, raw := range writes {
		row := raw.(map[string]any)
		key := row["key"].(string)
		stored, inConfig := held[key]
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
	for _, key := range stringList(res["removed"]) {
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

// Shared metrics consent, one answer written to the collection key and the sending key
func telemetrySection() sectionSpec {
	return sectionSpec{key: "telemetry", title: tr("setupFlow.telemetry.title"), detail: tr("setupFlow.telemetry.detail"), intro: tr("setupFlow.telemetry.intro"), fullSave: true,
		fields: []sectionField{{id: "share", kind: fieldToggle, label: tr("setupFlow.telemetry.share"),
			targets: []sectionTarget{{key: "telemetry.shared_metrics.enabled"}, {key: "telemetry.shared_metrics.send"}}}}}
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
	var follow func(job string, after int) tea.Cmd
	follow = func(job string, after int) tea.Cmd {
		return a.events.rpc("setup.action.poll", map[string]any{"job": job, "after": after}, func(res map[string]any) tea.Cmd {
			if res == nil {
				a.store.ui.Status = a.store.statusFromBusy()
				return nil
			}
			lines := stringList(res["lines"])
			if len(lines) > 0 {
				a.sys(strings.Join(lines, "\n"))
			}
			if res["done"] != true {
				return follow(job, after+len(lines))
			}
			a.store.ui.Status = a.store.statusFromBusy()
			run.answers[f.id] = res["ok"] == true
			return a.askSectionField(run, index+1)
		})
	}
	start := map[string]any{"action": f.action}
	if f.params != nil {
		start["params"] = f.params(run)
	}
	return a.events.rpc("setup.action.start", start, func(res map[string]any) tea.Cmd {
		if res == nil {
			a.store.ui.Status = a.store.statusFromBusy()
			return nil
		}
		job, _ := res["job"].(string)
		return follow(job, 0)
	})
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
