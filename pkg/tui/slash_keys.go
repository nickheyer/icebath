package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keybind file of the profile, read over the defaults at launch and written by /keys edit
func keybindsPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), keys.FileName)
}

// Applies the keybind file of the profile at launch, a notice naming any entry it could not take, and lets catalog texts name the keys as bound
func (a *app) loadKeybinds() tea.Cmd {
	km := a.keys
	lang.InstallKeys(func(id string) string {
		if chord := km.Chord(id); chord != "" {
			return chord
		}
		if act, ok := km.Action(id); ok {
			return act.LabelText()
		}
		return id
	})
	if err := km.Load(keybindsPath(a.ctx)); err != nil {
		return wm.Notify(tr(txt.KeysPanelFileError, err.Error()), wm.Warn)
	}
	return nil
}

// Commands that show the key bindings and change them
func (a *app) keyCommands() []slashCommand {
	return []slashCommand{{name: "keys", help: tr(txt.SlashHelpKeys), helpFn: helpOf(txt.SlashHelpKeys),
		usage: "[filter|edit|reload|reset|option on|off]", suggest: func(string) []string { return []string{"edit", "reload", "reset", "option"} },
		suggestAfter: func(done []string) []string {
			if len(done) == 1 && done[0] == "option" {
				return []string{"on", "off"}
			}
			return nil
		},
		run: func(r *slashRun) tea.Cmd {
			arg := trimJS(r.arg)
			word, rest, _ := strings.Cut(strings.ToLower(arg), " ")
			switch word {
			case "edit":
				return a.editKeybinds()
			case "reload":
				return a.reloadKeybinds()
			case "reset":
				return a.resetKeybinds()
			case "option":
				return a.optionKey(trimJS(rest))
			default:
				a.showKeybinds(arg)
				return nil
			}
		}}}
}

// Why this terminal may not send a chord, empty when it can: the reasons the keymap knows, and on a Mac an alt chord while Option types symbols
func (a *app) chordProblem(chord string) string {
	switch a.keys.Unsendable(chord) {
	case "kitty":
		return tr(txt.KeysPanelNeedsKitty)
	case "cmd":
		return tr(txt.KeysPanelNeedsCmd)
	}
	if runtime.GOOS == "darwin" && strings.Contains(chord, "alt+") && !keys.OptionOn() {
		return tr(txt.KeysPanelNeedsOption)
	}
	return ""
}

// Chords of an action as the panel shows them, each one this terminal cannot send marked
func (a *app) panelChords(id string) string {
	var out []string
	for _, chord := range a.keys.Keys(id) {
		shown := keys.Format(chord)
		if problem := a.chordProblem(chord); problem != "" {
			shown += " (" + problem + ")"
		}
		out = append(out, shown)
	}
	if len(out) == 0 {
		return tr(txt.KeysPanelUnbound)
	}
	return strings.Join(out, ", ")
}

// Window keys a terminal window passes on rather than sending them to its program: the leader and the global alt keys
func (a *app) terminalPasses() []string {
	out := []string{keys.Format(a.keys.First("leader"))}
	for _, act := range a.keys.Actions() {
		if act.Context != keys.Global {
			continue
		}
		for _, chord := range a.keys.Keys(act.ID) {
			if strings.Contains(chord, "alt+") || strings.Contains(chord, "super+") {
				out = append(out, keys.Format(chord))
			}
		}
	}
	return slices.Compact(out)
}

// Panel of every action with the keys it is on, grouped by where it applies, narrowed to the actions whose name, help or keys hold the filter; keys
// this terminal cannot send are marked, and the keys a terminal window passes on are named
func (a *app) showKeybinds(filter string) {
	needle := strings.ToLower(filter)
	var sections []panelSection
	for _, ctx := range a.keys.Contexts() {
		var rows [][2]string
		for _, act := range a.keys.Actions() {
			if act.Context != ctx || !keyLinked(act.ID) {
				continue
			}
			chords := a.panelChords(act.ID)
			label := act.LabelText()
			if doc := act.DocText(); doc != "" && doc != label {
				label += " — " + doc
			}
			if needle != "" && !strings.Contains(strings.ToLower(label+" "+chords+" "+act.ID), needle) {
				continue
			}
			if ctx == keys.Leader {
				chords = keys.Format(a.keys.First("leader")) + " " + chords
			}
			rows = append(rows, [2]string{chords, label})
		}
		if len(rows) == 0 {
			continue
		}
		title := ctx
		if doc := keys.ContextDocs[ctx]; doc != "" {
			title += " · " + doc
		}
		sections = append(sections, panelSection{Rows: rows, Title: &title})
	}
	passes := tr(txt.KeysPanelTerminalPasses, strings.Join(a.terminalPasses(), ", "))
	option := tr(txt.KeysPanelOptionOff)
	if keys.OptionOn() {
		option = tr(txt.KeysPanelOptionOn)
	}
	note := tr(txt.KeysPanelNote, home.Display(a.keys.Path()))
	if len(sections) == 0 {
		note = tr(txt.KeysPanelNoMatch, filter) + " " + note
	}
	sections = append(sections, panelSection{Text: &passes}, panelSection{Text: &option}, panelSection{Text: &note})
	a.panel(tr(txt.KeysPanelTitle), sections)
}

// Turns reading Option typed characters as alt chords on or off and keeps the choice in the keybind file, or says how it stands
func (a *app) optionKey(word string) tea.Cmd {
	switch word {
	case "on", "off":
		keys.SetOption(word == "on")
		if err := a.keys.Save(); err != nil {
			a.sys(errorLine(err.Error()))
			return nil
		}
	case "":
	default:
		a.sys(errorLine(tr(txt.KeysPanelOptionUsage)))
		return nil
	}
	if keys.OptionOn() {
		return wm.Notify(tr(txt.KeysPanelOptionOn), wm.Info)
	}
	return wm.Notify(tr(txt.KeysPanelOptionOff), wm.Info)
}

// Opens the keybind file in the user's editor in a terminal window beside the session, writing the bindings in force first when there is no
// file yet, and applies it once the editor closes
func (a *app) editKeybinds() tea.Cmd {
	path := a.keys.Path()
	if path == "" {
		a.sys(errorLine(tr(txt.KeysPanelNoFile)))
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := a.keys.Save(); err != nil {
			a.sys(errorLine(err.Error()))
			return nil
		}
	}
	editor := resolveEditor(os.LookupEnv, runtime.GOOS)
	return a.runInTerminal(append(editor, path), func(cards.TerminalEnd) tea.Cmd { return a.reloadKeybinds() })
}

// Reads the keybind file again, every card rebinding, the entries it could not take named
func (a *app) reloadKeybinds() tea.Cmd {
	changed := func() tea.Msg { return keys.ChangedMsg{} }
	err := a.keys.Reload()
	switch {
	case errors.Is(err, os.ErrNotExist):
		return tea.Batch(changed, wm.Notify(tr(txt.KeysPanelNoFileYet), wm.Info))
	case err != nil:
		a.sys(warningLine(err.Error()))
		return tea.Batch(changed, wm.Notify(tr(txt.KeysPanelAppliedErrors), wm.Warn))
	}
	return tea.Batch(changed, wm.Notify(tr(txt.KeysPanelApplied), wm.Success))
}

// Puts every action back on its default keys and removes the keybind file
func (a *app) resetKeybinds() tea.Cmd {
	a.keys.ResetAll()
	keys.SetOption(false)
	if path := a.keys.Path(); path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.sys(errorLine(err.Error()))
		}
	}
	return tea.Batch(func() tea.Msg { return keys.ChangedMsg{} }, wm.Notify(tr(txt.KeysPanelReset), wm.Success))
}
