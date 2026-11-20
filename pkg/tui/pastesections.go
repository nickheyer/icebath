package tui

import (
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Submissions whose paste bodies wait for the row that shows them
const maxStagedPastes = 16

// UTF-16 units of prompt text and paste bodies kept across the waiting submissions
const maxStagedPasteUnits = 8 * 1024 * 1024

// Chip display and paste bodies of one submission, under the text the model receives
type stagedPastes struct {
	text    string
	display string
	pastes  []pasteSection
}

// UTF-16 units the waiting submissions hold, their prompt texts and paste bodies
func stagedPasteUnits(entries []stagedPastes) int {
	total := 0
	for _, entry := range entries {
		total += unitLen(entry.text)
		for _, paste := range entry.pastes {
			total += unitLen(paste.Text)
		}
	}
	return total
}

// Remembers the body behind each chip of a submission's display, paired left to right the way the chips expand, under the text the model receives, the oldest submissions dropped past the count and size kept
func (s *tuiStore) stagePastes(display, text string, tokens []composerToken) {
	bodies := map[string][]string{}
	for _, token := range tokens {
		if token.Kind == "paste" {
			bodies[token.Label] = append(bodies[token.Label], token.Text)
		}
	}
	var pastes []pasteSection
	for _, label := range pasteSnippetRe.FindAllString(display, -1) {
		if waiting := bodies[label]; len(waiting) > 0 {
			pastes = append(pastes, pasteSection{Label: label, Text: waiting[0], Digest: hashText(waiting[0]) + ":" + strconv.Itoa(len(waiting[0]))})
			bodies[label] = waiting[1:]
		}
	}
	if len(pastes) == 0 {
		return
	}
	kept := slices.DeleteFunc(slices.Clone(s.sentPastes), func(entry stagedPastes) bool { return entry.text == text })
	kept = append(kept, stagedPastes{text: text, display: display, pastes: pastes})
	for len(kept) > 1 && (len(kept) > maxStagedPastes || stagedPasteUnits(kept) > maxStagedPasteUnits) {
		kept = kept[1:]
	}
	s.sentPastes = kept
}

// Takes the chip display and paste bodies waiting under the text the model receives, false when none do
func (s *tuiStore) takePastes(text string) (stagedPastes, bool) {
	at := slices.IndexFunc(s.sentPastes, func(entry stagedPastes) bool { return entry.text == text })
	if at < 0 {
		return stagedPastes{}, false
	}
	entry := s.sentPastes[at]
	s.sentPastes = slices.Delete(slices.Clone(s.sentPastes), at, at+1)
	return entry, true
}

// Sections under a sent prompt, one per collapsed paste, each header taking a click and an open one followed by the text the model received
func pasteRows(m msg, c renderCtx, st blockState) blockRows {
	pad := strings.Repeat(" ", transcriptIndent)
	room := max(c.width-transcriptIndent, 8)
	var out blockRows
	for i, paste := range m.Pastes {
		part := partPaste + strconv.Itoa(i)
		open := st.open(part, st.pastes)
		lines := strings.Count(paste.Text, "\n") + 1
		hint := tr("chatBits.messageLine.pasteSectionShowHint", lines)
		if open {
			hint = tr("chatBits.messageLine.pasteSectionHideHint", lines)
		}
		head := chevron(open, c) + toneStyle(c.t.Color.Accent).Bold(true).Render(tr("chatBits.messageLine.pasteSection", i+1)) + toneStyle(c.t.Color.Muted).Render(" "+hint)
		out.head(part, open, pad+ansi.Truncate(head, room, "…"))
		if !open {
			continue
		}
		rail := toneStyle(c.t.Color.Border).Render("│") + " "
		for _, row := range strings.Split(strings.ReplaceAll(stripAnsi(paste.Text), "\t", "    "), "\n") {
			for _, piece := range strings.Split(ansi.Wrap(row, max(room-2, 8), ""), "\n") {
				out.lines = append(out.lines, pad+rail+toneStyle(c.t.Color.Text).Render(piece))
			}
		}
	}
	return out
}
