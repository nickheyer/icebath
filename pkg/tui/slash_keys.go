package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keybind file of the profile, read over the defaults at launch and written by /keys edit
func keybindsPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), keys.FileName)
}

// Applies the keybind file of the profile at launch, a notice naming any entry it could not take
func (a *app) loadKeybinds() tea.Cmd {
	if err := a.keys.Load(keybindsPath(a.ctx)); err != nil {
		return wm.Notify("keybinds: "+err.Error(), wm.Warn)
	}
	return nil
}

// Commands that show the key bindings and change them
func (a *app) keyCommands() []slashCommand {
	return []slashCommand{{name: "keys", aliases: []string{"keybinds", "hotkeys"}, help: "show key bindings; /keys edit changes them",
		usage: "[filter|edit|reload|reset]", suggest: func(string) []string { return []string{"edit", "reload", "reset"} },
		run: func(r *slashRun) tea.Cmd {
			switch arg := trimJS(r.arg); strings.ToLower(arg) {
			case "edit":
				return a.editKeybinds()
			case "reload":
				return a.reloadKeybinds()
			case "reset":
				return a.resetKeybinds()
			default:
				a.showKeybinds(arg)
				return nil
			}
		}}}
}

// Panel of every action with the keys it is on, grouped by where it applies, narrowed to the actions whose name, help or keys hold the filter
func (a *app) showKeybinds(filter string) {
	needle := strings.ToLower(filter)
	var sections []panelSection
	for _, ctx := range a.keys.Contexts() {
		var rows [][2]string
		for _, act := range a.keys.Actions() {
			if act.Context != ctx || !keyLinked(act.ID) {
				continue
			}
			chords := a.keys.DisplayAll(act.ID)
			if chords == "" {
				chords = "unbound"
			}
			label := act.LabelText()
			if doc := act.DocText(); doc != "" && doc != label {
				label += " — " + doc
			}
			if needle != "" && !strings.Contains(strings.ToLower(label+" "+chords+" "+act.ID), needle) {
				continue
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
	note := "Change them with /keys edit (" + home.Display(a.keys.Path()) + "). They apply as soon as the editor closes."
	if len(sections) == 0 {
		note = "No action matches \"" + filter + "\". " + note
	}
	sections = append(sections, panelSection{Text: &note})
	a.panel("Key bindings", sections)
}

// Opens the keybind file in the user's editor in a terminal window beside the session, writing the bindings in force first when there is no
// file yet, and applies it once the editor closes
func (a *app) editKeybinds() tea.Cmd {
	path := a.keys.Path()
	if path == "" {
		a.sys(errorLine("no keybind file for this profile"))
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
		return tea.Batch(changed, wm.Notify("no keybind file yet, /keys edit makes one", wm.Info))
	case err != nil:
		a.sys(warningLine(err.Error()))
		return tea.Batch(changed, wm.Notify("key bindings applied, with errors", wm.Warn))
	}
	return tea.Batch(changed, wm.Notify("key bindings applied", wm.Success))
}

// Puts every action back on its default keys and removes the keybind file
func (a *app) resetKeybinds() tea.Cmd {
	a.keys.ResetAll()
	if path := a.keys.Path(); path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.sys(errorLine(err.Error()))
		}
	}
	return tea.Batch(func() tea.Msg { return keys.ChangedMsg{} }, wm.Notify("key bindings reset to the defaults", wm.Success))
}
