package tui

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Parts of a block that take a click, the three trail sections named as the details sections are, and a paste section and a tool row by their
// place after the prefix
const (
	partThinking = "thinking"
	partTools    = "tools"
	partActivity = "activity"
	partTodos    = "todos"
	partSystem   = "system"
	partPrompt   = "prompt"
	partPaste    = "paste:"
	partTool     = "tool:"
)

// Identity of the rows of the turn in flight that belong to no transcript row, the running tools, the activity and the task list
const liveBlock = ^uint64(0)

// Part of a rendered block that takes a click, the rows it covers counted from the first row of the block and whether it is drawn open
type blockPart struct {
	name     string
	from, to int
	open     bool
}

// Rows of a rendered block, the parts of it that take a click and the pictures the terminal draws over blank rows of it
type blockRows struct {
	lines  []string
	parts  []blockPart
	images []imageSpot
}

// Appends the rows of another block, its parts and pictures moved down by the rows already held
func (b *blockRows) add(more blockRows) {
	for _, part := range more.parts {
		part.from, part.to = part.from+len(b.lines), part.to+len(b.lines)
		b.parts = append(b.parts, part)
	}
	for _, spot := range more.images {
		spot.row += len(b.lines)
		b.images = append(b.images, spot)
	}
	b.lines = append(b.lines, more.lines...)
}

// Appends a header row that takes a click as the named part
func (b *blockRows) head(name string, open bool, row string) {
	b.parts = append(b.parts, blockPart{name: name, from: len(b.lines), to: len(b.lines) + 1, open: open})
	b.lines = append(b.lines, row)
}

// Choices of the user one block is painted with, each part opened or closed over its default, and whether its pasted text shows unless a section says otherwise
type blockState struct {
	chosen map[string]bool
	pastes bool
}

// Whether a part is drawn open, the choice of the user over the default
func (s blockState) open(part string, fallback bool) bool {
	if open, chosen := s.chosen[part]; chosen {
		return open
	}
	return fallback
}

// Text telling two paintings of one block under different choices apart, empty when the user made none
func (s blockState) key() string {
	if len(s.chosen) == 0 && !s.pastes {
		return ""
	}
	parts := make([]string, 0, len(s.chosen)+1)
	for part, open := range s.chosen {
		parts = append(parts, part+"="+strconv.FormatBool(open))
	}
	slices.Sort(parts)
	if s.pastes {
		parts = append(parts, "pastes")
	}
	return strings.Join(parts, ",")
}

// Block laid out in the transcript rows, its identity, the rows it takes from its first and the parts of it that take a click
type placedBlock struct {
	id     uint64
	row    int
	rows   int
	parts  []blockPart
	images []imageSpot
}

// Row of a block the window is held at while the rows above it change
type rowAnchor struct {
	id  uint64
	rel int
}

// Mark before a header that opens and closes, pointing down while open
func chevron(open bool, c renderCtx) string {
	mark := "▸ "
	if open {
		mark = "▾ "
	}
	return toneStyle(c.t.Color.Accent).Render(mark)
}

// Rows moved two columns in, under the header that opens them
func nested(rows []string) []string {
	for i := range rows {
		rows[i] = "  " + rows[i]
	}
	return rows
}

// Gives a row its identity the first time it is laid out
func (v *transcriptView) identify(m *msg) {
	if m.ID == 0 {
		v.lastID++
		m.ID = v.lastID
	}
}

// Choices of the user a row is painted with
func (v *transcriptView) state(m msg) blockState {
	return blockState{chosen: v.open[m.ID], pastes: v.pastesOpen && len(m.Pastes) > 0}
}

// Transcript row shown on a row of the window, false past the last row and on the row the kept prompt covers
func (v *transcriptView) rowAt(y int) (int, bool) {
	row := v.top() + y
	if y < 0 || y >= v.height || row >= v.total || (y == 0 && v.sticky) {
		return 0, false
	}
	return row, true
}

// Part of a block a transcript row falls on, with the identity of the block
func (v *transcriptView) partAt(row int) (uint64, blockPart, bool) {
	for _, block := range v.placed {
		for _, part := range block.parts {
			if rel := row - block.row; rel >= part.from && rel < part.to {
				return block.id, part, true
			}
		}
	}
	return 0, blockPart{}, false
}

// Parts the last layout drew for a block
func (v *transcriptView) partsOf(id uint64) []blockPart {
	for _, block := range v.placed {
		if block.id == id {
			return block.parts
		}
	}
	return nil
}

// Records a choice of the user for a part of a block, the choices of blocks no longer laid out dropped
func (v *transcriptView) choose(id uint64, part string, open bool) {
	laid := map[uint64]bool{}
	for _, block := range v.placed {
		laid[block.id] = true
	}
	for known := range v.open {
		if !laid[known] {
			delete(v.open, known)
		}
	}
	if v.open[id] == nil {
		v.open[id] = map[string]bool{}
	}
	v.open[id][part] = open
}

// Opens or closes the part of a block clicked on a transcript row, the row kept in the window
func (v *transcriptView) setOpen(id uint64, part string, open bool, row int) {
	v.choose(id, part, open)
	v.keep = row
}

// Opens every trail section a block shows, the row clicked kept in the window
func (v *transcriptView) openSections(id uint64, row int) {
	for _, part := range v.partsOf(id) {
		if isSectionName(part.name) {
			v.choose(id, part.name, true)
		}
	}
	v.keep = row
}

// Forgets what the user opened or closed among the trail sections, the details in view deciding again
func (v *transcriptView) dropSections() {
	for id, chosen := range v.open {
		for _, section := range sectionNames {
			delete(chosen, section)
		}
		if len(chosen) == 0 {
			delete(v.open, id)
		}
	}
}

// Shows or hides the pasted text of every sent prompt, each section following whatever was clicked before, the block at the top of the window held there
func (v *transcriptView) togglePastes() {
	v.pastesOpen = !v.pastesOpen
	for id, chosen := range v.open {
		for part := range chosen {
			if strings.HasPrefix(part, partPaste) {
				delete(chosen, part)
			}
		}
		if len(chosen) == 0 {
			delete(v.open, id)
		}
	}
	if v.follow {
		return
	}
	top := v.top()
	for _, block := range v.placed {
		if block.row+block.rows > top {
			v.anchor = &rowAnchor{id: block.id, rel: top - block.row}
			return
		}
	}
}

// Holds the window where a click or key changed what blocks show, a clicked header kept on screen where following would lift it out of the window or under the prompt kept in view, and the block at the top kept there
func (v *transcriptView) settle() {
	last := max(v.total-v.height, 0)
	if row := v.keep; row >= 0 {
		v.keep = -1
		if v.follow && (row < last || (row == last && v.height > 2 && v.stickyPrompt(last) != "")) {
			v.offset, v.follow = max(row-1, 0), false
		}
	}
	if anchor := v.anchor; anchor != nil {
		v.anchor = nil
		for _, block := range v.placed {
			if block.id == anchor.id && !v.follow {
				v.offset = min(max(block.row+anchor.rel, 0), last)
				break
			}
		}
	}
}

// Whether the window shows a scrollbar, in its last column
func (v *transcriptView) barShown() bool { return v.total > v.height && v.width > 4 }

// Rows of the scrollbar thumb, the row of the bar it starts at and the rows it can travel
func (v *transcriptView) thumb() (size, start, travel int) {
	if !v.barShown() {
		return v.height, 0, 0
	}
	size = max(v.height*v.height/v.total, 1)
	travel = v.height - size
	return size, v.top() * travel / (v.total - v.height), travel
}

// Scrolls so the thumb of the scrollbar starts at a row of the bar, following again at its last row
func (v *transcriptView) scrollThumb(start int) {
	_, _, travel := v.thumb()
	if travel <= 0 {
		return
	}
	last := v.total - v.height
	top := (min(max(start, 0), travel)*last + travel - 1) / travel
	v.offset, v.follow, v.manualAt = top, top >= last, v.now()
}

// Row of the transcript with an identity, false once it left the history
func (a *app) blockByID(id uint64) (msg, bool) {
	for _, m := range a.history {
		if m.ID == id {
			return m, true
		}
	}
	return msg{}, false
}

// Carries out a click on a part of a transcript block, a trail section, a task list, a long system message or a paste section opened or closed, every section of the block opened with Shift or Ctrl held, and the system prompt paged
func (a *app) clickPart(id uint64, part blockPart, row int, mod tea.KeyMod) tea.Cmd {
	view := a.card.view
	switch {
	case part.name == partPrompt:
		if m, found := a.blockByID(id); found {
			if prompt := val(infoOf(m.Info).SystemPrompt); prompt != "" {
				return a.page(prompt, tr(txt.ChatBitsBrandingSystemPrompt))
			}
		}
	case isSectionName(part.name) && (mod.Contains(tea.ModShift) || mod.Contains(tea.ModCtrl)):
		view.openSections(id, row)
	case id == liveBlock && part.name == partTodos:
		a.store.turn.TodoCollapsed = !a.store.turn.TodoCollapsed
		view.keep = row
	default:
		view.setOpen(id, part.name, !part.open, row)
	}
	return nil
}

// Shows or hides the pasted text under every sent prompt, what the paste key does when the prompt holds no chip to expand
func (a *app) toggleSentPastes() { a.card.view.togglePastes() }
