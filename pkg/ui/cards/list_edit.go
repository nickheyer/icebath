package cards

import tea "charm.land/bubbletea/v2"

// Opens a cell of the selected row for typing from initial, enter handing the trimmed text to done
func (l *List) EditCell(col int, initial string, done func(l *List, text string) tea.Cmd) {
	l.editCell(col, initial, func(_ *lister, text string) tea.Cmd { return done(l, text) })
}
