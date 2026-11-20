package app

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Short fixed width tag of a session lifecycle status, a dash for none
func sessionStatusTag(status string) string {
	switch status {
	case "complete":
		return "done"
	case "interrupted":
		return "intr"
	case "error":
		return "err"
	case "empty":
		return "empty"
	}
	return "-"
}

// Attaches the lifecycle status of each row through one indexed lookup, rows left bare when it fails
func annotateSessionStatuses(r *sessionsRun, sessions []*pyjson.Object, db *state.SessionDB) {
	if db == nil || len(sessions) == 0 {
		return
	}
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, rowText(s, "id"))
	}
	statuses, err := db.SessionLifecycleStatuses(r.ctx, ids)
	if err != nil {
		return
	}
	for _, s := range sessions {
		s.Set("_status", statuses[rowText(s, "id")])
	}
}

// Title of a row, else its preview, else its id
func sessionLabel(s *pyjson.Object) string {
	if t := pystr.Strip(rowText(s, "title")); t != "" {
		return t
	}
	if p := pystr.Strip(rowText(s, "preview")); p != "" {
		return p
	}
	return rowText(s, "id")
}

// Text cut to n code points with a trailing ellipsis when it was longer
func clipText(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return pystr.Slice(text, 0, n-3) + "..."
}

// Message count of a row as text, a dash when it is not an integer
func msgsStr(s *pyjson.Object) string {
	switch n := rowGet(s, "message_count").(type) {
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	}
	return "-"
}

// Case insensitive substring match of a query over title, preview, id and source
func sessionMatches(s *pyjson.Object, query string) bool {
	q := pystr.Lower(query)
	for _, key := range []string{"title", "preview", "id", "source"} {
		if strings.Contains(pystr.Lower(rowText(s, key)), q) {
			return true
		}
	}
	return false
}

const browseFixedCols = 3 + 5 + 2 + 5 + 2 + 12 + 6 + 18 + 6

// One picker row with the name column sized to the screen width
func formatRow(s *pyjson.Object, maxX int, now time.Time) string {
	nameWidth := max(20, maxX-browseFixedCols)
	sid := runePrefix(rowText(s, "id"), 18)
	name := pystr.Strip(rowText(s, "title"))
	if name == "" {
		name = pystr.Strip(rowText(s, "preview"))
	}
	if name = runePrefix(name, nameWidth); name == "" {
		name = sid
	}
	return padRight(name, nameWidth) + "  " + padRight(sessionStatusTag(rowText(s, "_status")), 5) + "  " +
		padLeft(msgsStr(s), 5) + "  " + padRight(pydatetime.RelativeTime(rowGet(s, "last_active"), rowText(s, "id"), now), 10) + "  " +
		padRight(runePrefix(rowText(s, "source"), 6), 5) + " " + sid
}

// State and render loop of the full screen picker
type cursesBrowser struct {
	scr           cliui.Screen
	sessions      []*pyjson.Object
	deleteFn      func(sessionID string) (bool, error)
	result        string
	cursor        int
	scroll        int
	search        string
	confirmDelete *pyjson.Object
	flash         string
	filtered      []*pyjson.Object
}

// Browser over the sessions, a nil delete function turning the delete key off
func newCursesBrowser(scr cliui.Screen, sessions []*pyjson.Object, deleteFn func(string) (bool, error)) *cursesBrowser {
	return &cursesBrowser{scr: scr, sessions: sessions, deleteFn: deleteFn, filtered: append([]*pyjson.Object(nil), sessions...)}
}

// Color pair attribute on a color terminal, the fallback attribute otherwise
func (b *cursesBrowser) pair(n int, fallback cliui.Attr) cliui.Attr {
	if b.scr.HasColors() {
		return cliui.Attr{Pair: n}
	}
	return fallback
}

// Bold attribute carrying a color pair on a color terminal
func (b *cursesBrowser) boldPair(n int) cliui.Attr {
	a := b.pair(n, cliui.Attr{})
	a.Bold = true
	return a
}

// Attribute of a status tag, plain for a status with no color
func (b *cursesBrowser) statusAttr(status string) cliui.Attr {
	pair, ok := map[string]int{"complete": 1, "interrupted": 2, "error": 5, "empty": 4}[status]
	if !ok {
		return cliui.Attr{}
	}
	return b.pair(pair, cliui.Attr{})
}

// Writes text at a cell, a write that falls off the screen dropped
func (b *cursesBrowser) put(y, x int, text string, n int, attr cliui.Attr) {
	_ = b.scr.AddNStr(y, x, text, n, attr)
}

// Rebuilds the filtered list from the search text, optionally returning the cursor to the top
func (b *cursesBrowser) refilter(resetCursor bool) {
	b.filtered = b.filtered[:0:0]
	for _, s := range b.sessions {
		if b.search == "" || sessionMatches(s, b.search) {
			b.filtered = append(b.filtered, s)
		}
	}
	if resetCursor {
		b.cursor, b.scroll = 0, 0
	}
}

// Draws the header, the column header, the visible rows and the footer
func (b *cursesBrowser) draw(maxY, maxX int) {
	dim := cliui.Attr{Dim: true}
	header, headerAttr := "  Browse sessions — ↑↓ navigate  Enter select  Type to filter  Esc quit", b.boldPair(2)
	if b.search != "" {
		header, headerAttr = "  Browse sessions — filter: "+b.search+"█", b.boldPair(3)
	}
	b.put(0, 0, header, maxX-1, headerAttr)
	nameWidth := max(20, maxX-browseFixedCols)
	colHeader := "   " + padRight("Title / Preview", nameWidth) + "  " + padRight("Stat", 5) + "  " + padLeft("Msgs", 5) + "  " +
		padRight("Active", 10) + "  " + padRight("Src", 5) + " ID"
	b.put(1, 0, colHeader, maxX-1, b.pair(4, dim))
	visibleRows := max(maxY-4, 1)
	filtered := b.filtered
	if len(filtered) == 0 {
		b.put(3, 0, "  No sessions match the filter.", maxX-1, dim)
	} else {
		b.cursor = max(min(b.cursor, len(filtered)-1), 0)
		if b.cursor < b.scroll {
			b.scroll = b.cursor
		} else if b.cursor >= b.scroll+visibleRows {
			b.scroll = b.cursor - visibleRows + 1
		}
		tagX := 3 + max(20, (maxX-3)-browseFixedCols) + 2
		now := time.Now()
		for i := b.scroll; i < min(len(filtered), b.scroll+visibleRows); i++ {
			y := i - b.scroll + 3
			if y >= maxY-1 {
				break
			}
			s, selected := filtered[i], i == b.cursor
			prefix, attr := "   ", cliui.Attr{}
			if selected {
				prefix, attr = " → ", b.boldPair(1)
			}
			if err := b.scr.AddNStr(y, 0, prefix+formatRow(s, maxX-3, now), maxX-1, attr); err != nil {
				continue
			}
			if !selected && tagX+5 < maxX-1 {
				status := rowText(s, "_status")
				_ = b.scr.AddNStr(y, tagX, padRight(sessionStatusTag(status), 5), 5, b.statusAttr(status))
			}
		}
	}
	footerAttr := b.pair(4, dim)
	var footer string
	switch {
	case b.confirmDelete != nil:
		footer = "  Delete session '" + clipText(sessionLabel(b.confirmDelete), 40) + "'? [y/N]"
		footerAttr = b.boldPair(5)
	case b.flash != "":
		footer = "  " + b.flash
		b.flash = ""
	default:
		position, total := 0, len(filtered)
		if len(filtered) > 0 {
			position = b.cursor + 1
		} else {
			total = len(b.sessions)
		}
		footer = fmt.Sprintf("  %d/%d sessions", position, total)
		if len(filtered) > 0 && len(filtered) < len(b.sessions) {
			footer += fmt.Sprintf(" (filtered from %d)", len(b.sessions))
		}
		if b.deleteFn != nil && b.search == "" {
			footer += "   d delete"
		}
	}
	b.put(maxY-1, 0, footer, maxX-1, footerAttr)
}

// Applies one keypress and reports whether the picker should exit
func (b *cursesBrowser) handleKey(key int) bool {
	if b.confirmDelete != nil {
		target := b.confirmDelete
		b.confirmDelete = nil
		if key != 'y' && key != 'Y' {
			return false
		}
		targetID := rowText(target, "id")
		ok, err := b.deleteFn(targetID)
		var guard *state.SessionActiveWriteGuardError
		if errors.As(err, &guard) {
			b.flash = "Session is active (a live turn owns it); try again when it finishes."
			return false
		}
		if !ok {
			b.flash = "Delete failed."
			return false
		}
		kept := b.sessions[:0]
		for _, s := range b.sessions {
			if rowText(s, "id") != targetID {
				kept = append(kept, s)
			}
		}
		b.sessions = kept
		b.refilter(false)
		b.flash = "Deleted."
		return len(b.sessions) == 0
	}
	switch {
	case key == cliui.KeyUp || key == cliui.KeyDown:
		if n := len(b.filtered); n > 0 {
			step := -1
			if key == cliui.KeyDown {
				step = 1
			}
			b.cursor = ((b.cursor+step)%n + n) % n
		}
	case key == cliui.KeyEnter || key == 10 || key == 13:
		if len(b.filtered) > 0 {
			b.result = rowText(b.filtered[b.cursor], "id")
		}
		return true
	case key == 27 && b.search == "":
		return true
	case key == 27:
		b.search = ""
		b.refilter(true)
	case key == cliui.KeyBackspace || key == 127 || key == 8:
		if b.search != "" {
			b.search = pystr.Slice(b.search, 0, utf8.RuneCountInString(b.search)-1)
			b.refilter(true)
		}
	case key == 'q' && b.search == "":
		return true
	case key == 'd' && b.search == "" && b.deleteFn != nil && len(b.filtered) > 0:
		b.confirmDelete = b.filtered[b.cursor]
	case 32 <= key && key <= 126:
		b.search += string(rune(key))
		b.refilter(true)
	}
	return false
}

// Render loop of the picker, returning once a key exits it or the terminal is too small
func (b *cursesBrowser) run() error {
	gray := cliui.ColorWhite
	if b.scr.Colors() > 8 {
		gray = 8
	}
	if err := b.scr.InitPairs(map[int]int{1: cliui.ColorGreen, 2: cliui.ColorYellow, 3: cliui.ColorCyan, 4: gray, 5: 1}); err != nil {
		return err
	}
	for {
		b.scr.Clear()
		maxY, maxX := b.scr.GetMaxYX()
		if maxY < 5 || maxX < 40 {
			_ = b.scr.AddNStr(0, 0, "Terminal too small", len("Terminal too small"), cliui.Attr{})
			if err := b.scr.Refresh(); err != nil {
				return err
			}
			_, err := b.scr.Getch()
			return err
		}
		b.draw(maxY, maxX)
		if err := b.scr.Refresh(); err != nil {
			return err
		}
		key, err := b.scr.Getch()
		if err != nil {
			return err
		}
		if b.handleKey(key) {
			return nil
		}
	}
}

// Numbered list picker for a terminal with no full screen support, same columns and no delete
func fallbackPicker(r *sessionsRun, sessions []*pyjson.Object) (string, error) {
	r.print("\n  Browse sessions  (enter number to resume, q to cancel)\n")
	now := time.Now()
	for i, s := range sessions {
		r.print("  " + padLeft(strconv.Itoa(i+1), 3) + ". " + padRight(clipText(sessionLabel(s), 50), 50) + "  " +
			padRight(sessionStatusTag(rowText(s, "_status")), 5) + "  " + padLeft(msgsStr(s), 5) + "  " +
			padRight(pydatetime.RelativeTime(rowGet(s, "last_active"), rowText(s, "id"), now), 10) + "  " + runePrefix(rowText(s, "source"), 6))
	}
	for {
		line, err := console().Input(fmt.Sprintf("\n  Select [1-%d]: ", len(sessions)))
		if errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF) {
			r.print("")
			return "", nil
		}
		if err != nil {
			return "", err
		}
		val := pystr.Strip(line)
		if lowered := pystr.Lower(val); val == "" || lowered == "q" || lowered == "quit" || lowered == "exit" {
			return "", nil
		}
		n, ok := pyint.Parse(val)
		if !ok {
			r.print("  Invalid input. Enter a number or q to cancel.")
			continue
		}
		if n.IsInt64() && 1 <= n.Int64() && n.Int64() <= int64(len(sessions)) {
			return rowText(sessions[n.Int64()-1], "id"), nil
		}
		r.printf("  Invalid selection. Enter 1-%d or q to cancel.", len(sessions))
	}
}

// Session browser with live search returning the selected session id, empty when cancelled
func sessionBrowsePicker(r *sessionsRun, sessions []*pyjson.Object, db *state.SessionDB) (string, error) {
	if len(sessions) == 0 {
		r.print("No sessions found.")
		return "", nil
	}
	annotateSessionStatuses(r, sessions, db)
	var deleteFn func(string) (bool, error)
	if db != nil {
		deleteFn = func(sessionID string) (bool, error) {
			deleted, err := db.DeleteSession(r.ctx, sessionID, state.DeleteSessionOptions{SessionsDir: r.sessionsDir(), ExcludeActiveWriteGuards: true})
			var guard *state.SessionActiveWriteGuardError
			if errors.As(err, &guard) {
				return false, err
			}
			return deleted && err == nil, nil
		}
	}
	scr, closeScreen, err := console().OpenFullScreen()
	if err != nil {
		return fallbackPicker(r, sessions)
	}
	browser := newCursesBrowser(scr, sessions, deleteFn)
	err = browser.run()
	closeScreen()
	if errors.Is(err, cliui.ErrInterrupt) {
		return "", err
	}
	if err != nil {
		return fallbackPicker(r, sessions)
	}
	return browser.result, nil
}
