package tui

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the row keys every screen of things to manage answers the same way
const hubContext = cards.HubContext

// Keymap context of a list a hub picks one choice from
const hubPickContext = "hub.pick"

// Keymap context of the actions screen of one row of a hub
const hubActionsContext = "hub.actions"

// Keys shared by every screen of things to manage and those of each screen
func registerHubKeys(km *keys.Map) {
	cards.RegisterHubKeys(km)
	for name, doc := range map[string]string{hubPickContext: "screens of things to manage, a list to pick one choice from",
		hubActionsContext: "screens of things to manage, the actions of one row", helpContext: "help screen"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	km.Register(keys.Action{ID: "hub.pick", Context: hubPickContext, Label: "pick", Doc: "pick the highlighted choice", Default: []string{"enter"}})
	km.Register(keys.Action{ID: "hub.run", Context: hubActionsContext, Label: "run", Doc: "run the highlighted action", Default: []string{"enter"}})
	for _, d := range hubDefs() {
		if d.keys != nil {
			d.keys(km)
		}
	}
}

// One screen of things to manage, the names it answers to, how it opens and its own keys
type hubDef struct {
	id    string
	order int
	title txt.Key
	short txt.Key
	slash string
	usage string
	open  func(a *app, arg string) tea.Cmd
	// Slash command of a hub whose arguments do more than open it, nil opening it with the argument
	run func(a *app, r *slashRun) tea.Cmd
	// Words the first argument completes to
	suggest func(prefix string) []string
	keys    func(km *keys.Map)
}

// Screens of things to manage, each hub file adding its own when the package loads
var hubRegistry []hubDef

// Every screen of things to manage in the order the hubs screen lists them
func hubDefs() []hubDef {
	return slices.SortedStableFunc(slices.Values(hubRegistry), func(x, y hubDef) int { return x.order - y.order })
}

// Palette commands of every hub and of the screen listing them, the one place hubs join the palette
func (a *app) hubPaletteCommands() []wm.Command {
	idle := func() bool { return !a.dialogs.waiting() }
	var out []wm.Command
	for _, d := range hubDefs() {
		out = append(out, wm.Command{ID: "open." + d.id, Title: tr(d.title), Text: func() string { return tr(d.title) }, When: idle,
			Run: func() tea.Cmd { return d.open(a, "") }})
	}
	return append(out, wm.Command{ID: "open.hubs", Title: tr(txt.HubsTitleHubs), Text: func() string { return tr(txt.HubsTitleHubs) }, When: idle, Run: a.openHubs})
}

// Slash commands of every hub and of the screen listing them
func (a *app) hubCommands() []slashCommand {
	var out []slashCommand
	for _, d := range hubDefs() {
		run := func(r *slashRun) tea.Cmd { return tea.Batch(r.ran(), d.open(a, strings.TrimSpace(r.arg))) }
		if d.run != nil {
			run = func(r *slashRun) tea.Cmd { return d.run(a, r) }
		}
		out = append(out, slashCommand{name: d.slash, help: tr(d.short), helpFn: helpOf(d.short), usage: d.usage, suggest: d.suggest, run: run})
	}
	return append(out, slashCommand{name: "hubs", help: tr(txt.HubsShortHubs), helpFn: helpOf(txt.HubsShortHubs), run: func(r *slashRun) tea.Cmd { return tea.Batch(r.ran(), a.openHubs()) }})
}

// Screen listing every screen of things to manage, enter opening one
func (a *app) openHubs() tea.Cmd {
	defs := hubDefs()
	list := cards.NewList(a.cardEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleHubs), Glyph: a.toolkit.Glyphs.Ext, Key: "tui:hubs", Context: hubPickContext, Primary: "hub.pick",
		Columns: []render.Column{{Title: tr(txt.HubsColScreen)}, {Title: tr(txt.HubsColCommand)}, {Title: tr(txt.HubsColWhatItCovers), Flex: 1}},
		Yield:   []int{1},
		Rows: func() [][]string {
			out := make([][]string, len(defs))
			for i, d := range defs {
				out[i] = []string{tr(d.title), "/" + d.slash, tr(d.short)}
			}
			return out
		},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.pick", Label: tr(txt.HintsLabelOpen)}}
		},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.pick": func(_ *cards.List, i int) tea.Cmd { return defs[i].open(a, "") },
		},
	})
	return a.wm.ShowOverlay(a.newHub("tui:hubs", list))
}

// Environment of the views of a hub, able to hand a message to the program from outside its update loop
func (a *app) hubEnv() *cards.Env {
	env := a.cardEnv()
	env.Send = a.post
	return env
}

// Hub over its root view, sized from the screen
func (a *app) newHub(key string, root wm.Card) *cards.Hub {
	return cards.NewHub(a.hubEnv(), key, root, a.wm.Size)
}

// Request of a hub: its note says the work is in flight, a failure stays in the hub, and the answer goes on
func hubCall[P, R any](a *app, h *cards.Hub, busy string, m tuigateway.Method[P, R], params P, then func(*R) tea.Cmd) tea.Cmd {
	if busy != "" {
		h.Busy(busy)
	}
	return call(a.gw, m, params, func(r *R, err error) tea.Cmd {
		if err != nil {
			return h.Fail(rpcErrorMessage(err))
		}
		if h.Waiting() {
			h.Quiet()
		}
		return then(r)
	})
}

// One choice of a picker inside a hub
type hubChoice struct {
	id, title, detail string
}

// Puts a picker on the hub, typing narrowing it from the first key, enter picking the highlighted choice and closing the picker
func (a *app) hubPick(h *cards.Hub, title string, choices []hubChoice, current string, pick func(c hubChoice) tea.Cmd) tea.Cmd {
	var list *cards.List
	list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: title, Key: "hub:pick", Context: hubPickContext, Primary: "hub.pick",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsColChoice), Max: 56}, {Title: "", Flex: 1}},
		Yield:   []int{2},
		Rows: func() [][]string {
			out := make([][]string, len(choices))
			for i, c := range choices {
				mark := ""
				if c.id == current && current != "" {
					mark = "●"
				}
				out[i] = []string{mark, c.title, c.detail}
			}
			return out
		},
		Empty: func() string { return tr(txt.HubsPickEmpty) },
		Pick:  true,
		Hints: func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.pick"}} },
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.pick": func(_ *cards.List, i int) tea.Cmd {
				c := choices[i]
				return tea.Batch(h.Close(list), pick(c))
			},
		},
	})
	cmd := h.Push(list)
	for i, c := range choices {
		if c.id == current {
			list.Select(i)
		}
	}
	return cmd
}

// Puts a form on the hub that closes once sent, its values handed on
func (a *app) hubForm(h *cards.Hub, spec cards.FormSpec, done func(f *cards.Form, values map[string]string) tea.Cmd) tea.Cmd {
	var form *cards.Form
	spec.Done = func(f *cards.Form, values map[string]string) tea.Cmd {
		return tea.Batch(h.Close(form), done(f, values))
	}
	if spec.Pick == nil {
		spec.Pick = func(f *cards.Form, field cards.Field) tea.Cmd {
			choices := make([]hubChoice, len(field.Choices))
			for i, c := range field.Choices {
				choices[i] = hubChoice{id: c.Value, title: c.Label, detail: c.Meaning}
			}
			return a.hubPick(h, field.Label, choices, field.Value, func(c hubChoice) tea.Cmd { return f.Commit(field.ID, c.id) })
		}
	}
	form = cards.NewForm(a.hubEnv(), spec)
	return h.Push(form)
}

// Asks for one line of text on the hub, masked for a secret, an empty answer closing the question without an answer
func (a *app) hubAsk(h *cards.Hub, title, label, initial string, secret bool, done func(text string) tea.Cmd) tea.Cmd {
	kind := cards.FieldText
	if secret {
		kind = cards.FieldSecret
	}
	spec := cards.FormSpec{Title: title, Fields: []cards.Field{{ID: "value", Label: label, Kind: kind, Value: initial, Required: true}}}
	return a.hubForm(h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		return done(strings.TrimSpace(values["value"]))
	})
}

// One action of a row of a hub, listed on its actions screen
type hubAction struct {
	// Keymap action that runs it straight from the hub's list, shown beside it, empty for none
	key   string
	label string
	does  string
	// Question asked before it runs, empty to run at once
	confirm string
	run     func() tea.Cmd
}

// Actions screen of one row: what the row is, then its actions, enter running the highlighted one
func (a *app) hubActions(h *cards.Hub, title string, detail func() []string, actions func() []hubAction) tea.Cmd {
	var acts []hubAction
	list := cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: title, Key: "hub:actions", Context: hubActionsContext, Primary: "hub.run",
		Columns: []render.Column{{Title: tr(txt.HubsColAction)}, {Title: tr(txt.HubsColKey)}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Yield:   []int{1},
		Rows: func() [][]string {
			acts = actions()
			out := make([][]string, len(acts))
			for i, act := range acts {
				out[i] = []string{act.label, a.keys.Display(act.key), act.does}
			}
			return out
		},
		Detail: func(int) []string { return detail() },
		Help:   func(int) []string { return detail() },
		OnFocus: func(l *cards.List) tea.Cmd {
			if !l.DetailShown() {
				return l.ToggleDetail()
			}
			return nil
		},
		Hints: func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.run"}} },
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.run": func(l *cards.List, i int) tea.Cmd {
				act := acts[i]
				if act.confirm != "" {
					l.Confirm(act.confirm, act.run)
					return nil
				}
				return act.run()
			},
		},
	})
	return h.Push(list)
}

// Copies text to the clipboard and says so in the hub's note, through the terminal when no clipboard tool answers
func (a *app) hubCopy(h *cards.Hub, text, what string) tea.Cmd {
	return func() tea.Msg {
		ok := clip.WriteText(a.ctx, text)
		return asyncDoneMsg{run: func() tea.Cmd {
			if ok {
				return h.Done(tr(txt.HubsCopied, what))
			}
			return tea.Batch(h.Done(tr(txt.HubsCopiedTerminal, what)), clip.Send(text))
		}}
	}
}

// One run of an icebath command inside a hub, its output as it arrives and how it ended
type cliRun struct {
	argv  []string
	lines []string
	ended bool
	code  int
	err   error
	stop  func()
}

// Runs icebath with the words, its output filling a page on the hub as it arrives and then saying how it ended, then handed on
func (a *app) hubRunCLI(h *cards.Hub, title string, argv []string, then func(run *cliRun) tea.Cmd) tea.Cmd {
	run := &cliRun{argv: argv}
	t := a.toolkit
	line := "icebath " + joinCommandLine(argv)
	page := cards.NewPage(a.hubEnv(), cards.PageSpec{Title: title, Key: "hub:run", Follow: true,
		Lines: func(int) []string {
			out := []string{render.Dim("$ "+line, t), ""}
			out = append(out, run.lines...)
			switch {
			case run.err != nil:
				out = append(out, "", render.Bad(tr(txt.HubsRunFailed, run.err.Error()), t))
			case run.ended && run.code == 0:
				out = append(out, "", render.Good(tr(txt.HubsRunDone), t))
			case run.ended:
				out = append(out, "", render.Bad(tr(txt.HubsRunExit, run.code), t))
			default:
				out = append(out, "", render.Warm(tr(txt.HubsRunRunning), t))
			}
			return out
		},
	})
	h.Busy(tr(txt.HubsRunBusy, line))
	finish := func() tea.Cmd {
		h.Quiet()
		if run.err != nil {
			return tea.Batch(h.Fail(tr(txt.HubsRunFailed, run.err.Error())), then(run))
		}
		if run.code != 0 {
			return tea.Batch(h.Fail(tr(txt.HubsRunExit, run.code)), then(run))
		}
		return then(run)
	}
	start := func() tea.Msg {
		exe, err := os.Executable()
		if err != nil {
			run.err, run.ended = err, true
			return asyncDoneMsg{run: finish}
		}
		cmd := exec.CommandContext(a.ctx, exe, argv...)
		cmd.Env = append(os.Environ(), "NO_COLOR=1")
		cmd.Dir = a.store.sessionCwd()
		r, w := io.Pipe()
		cmd.Stdout, cmd.Stderr = w, w
		if err := cmd.Start(); err != nil {
			run.err, run.ended = err, true
			return asyncDoneMsg{run: finish}
		}
		run.stop = func() { _ = cmd.Process.Kill() }
		go func() {
			err := cmd.Wait()
			_ = w.Close()
			var exit *exec.ExitError
			code := 0
			switch {
			case errors.As(err, &exit):
				code, err = exit.ExitCode(), nil
			}
			a.post(asyncDoneMsg{run: func() tea.Cmd {
				run.ended, run.code, run.err = true, code, err
				return finish()
			}})
		}()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			text := strings.TrimRight(scanner.Text(), "\r")
			a.post(asyncDoneMsg{run: func() tea.Cmd {
				run.lines = append(run.lines, text)
				return nil
			}})
		}
		return nil
	}
	h.OnClose(func() tea.Cmd {
		if run.stop != nil && !run.ended {
			run.stop()
		}
		return nil
	})
	return tea.Batch(h.Push(page), start)
}
