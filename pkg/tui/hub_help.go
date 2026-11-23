package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the help screen
const helpContext = "tui.help"

// One row of the help screen, a key or a command with what it does
type helpRow struct {
	name, does string
	// Command enter puts in the prompt, empty for a key
	run string
}

// Help screen: the keys of the session, the keys after the leader, then every command, one line each
func (a *app) openHelp() tea.Cmd {
	var rows []helpRow
	for _, r := range a.hotkeyRows() {
		rows = append(rows, helpRow{name: r[0], does: r[1]})
	}
	leader := a.keys.Display("leader")
	for _, act := range a.keys.InContexts([]string{keys.Leader}) {
		if act.Context != keys.Leader || act.Quiet {
			continue
		}
		chord := a.keys.Display(act.ID)
		if act.ID == "leader.literal" {
			chord = leader
		}
		if chord != "" {
			rows = append(rows, helpRow{name: leader + " " + chord, does: act.DocText()})
		}
	}
	for _, act := range a.keys.InContexts([]string{keys.View}) {
		if act.Context != keys.View {
			continue
		}
		if chord := strings.ReplaceAll(a.keys.DisplayAll(act.ID), " ", ", "); chord != "" {
			rows = append(rows, helpRow{name: chord, does: act.DocText()})
		}
	}
	keyRows := len(rows)
	seen := map[string]bool{}
	addCommand := func(name, usage, does string) {
		name = "/" + strings.TrimPrefix(name, "/")
		if seen[name] || name == "/" {
			return
		}
		seen[name] = true
		shown := name
		if usage != "" {
			shown += " " + usage
		}
		rows = append(rows, helpRow{name: shown, does: does, run: name})
	}
	for _, c := range a.localCommands {
		addCommand(c.name, c.usage, c.helpText())
	}
	if a.catalog != nil {
		for _, pair := range a.catalog.Pairs {
			if f := strings.Fields(pair[0]); len(f) > 0 {
				addCommand(f[0], strings.Join(f[1:], " "), pair[1])
			}
		}
	}
	slices.SortStableFunc(rows[keyRows:], func(x, y helpRow) int { return strings.Compare(x.name, y.name) })
	isCommand := func(i int) bool { return i >= 0 && i < len(rows) && rows[i].run != "" }
	run := func(l *cards.List, i int) tea.Cmd {
		if !isCommand(i) {
			return nil
		}
		a.card.composer.SetValue(rows[i].run + " ")
		return wm.RequestDismissOf(l)
	}
	list := cards.NewList(a.cardEnv(), cards.ListSpec{
		Title: tr(txt.HelpTitle), Glyph: a.toolkit.Glyphs.Info, Key: "tui:help", Context: helpContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HelpColKeyOrCommand), Max: 28}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(rows))
			for i, r := range rows {
				out[i] = []string{r.name, r.does}
			}
			return out
		},
		Hints: func(l *cards.List, i int) []wm.Hint {
			if !isCommand(i) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelInsert)}}
		},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{"hub.use": run},
	})
	return a.wm.ShowOverlay(list)
}
