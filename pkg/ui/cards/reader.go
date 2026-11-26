package cards

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/field"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Text a reader moves through: its rows as plain text, the window of rows on screen and where its prompts and blocks lie
type Readable interface {
	// Rows of the text
	ReadRows() int
	// Plain text of a row
	ReadRow(i int) string
	// First row in view and how many rows show
	ReadWindow() (top, height int)
	// Scrolls so that the row is the first in view
	ReadScroll(top int)
	// Row of the prompt nearest before the row for a negative step or after it for a positive one, false when there is none that way
	ReadPrompt(from, d int) (int, bool)
	// Rows from and up to of the message or command output that holds the row
	ReadBlock(row int) (int, int)
}

// Match of a search in one row, its cell columns from and up to
type readHit struct {
	row, from, to int
}

// Reading mode over a text: a cursor row moved with the reading keys, prompts to jump between, a search with its matches, a selection of rows and
// copying; the same for a transcript, a terminal's scrollback and a long text
type Reader struct {
	src  Readable
	keys *keys.Map
	on   bool
	// Row the cursor is on
	cursor int
	// Row a selection started at, minus one while nothing is selected
	anchor int
	// Query being typed, nil while no search is typed
	typing *field.Field
	// Whether the search typed or last run goes upward
	back  bool
	query string
	hits  []readHit
	// Match the cursor last moved to
	hit int
}

// Reader over a text, not reading yet
func NewReader(src Readable, km *keys.Map) *Reader {
	return &Reader{src: src, keys: km, anchor: -1}
}

// Whether the reader holds the keys
func (r *Reader) Active() bool { return r.on }

// Whether a search query is being typed
func (r *Reader) Typing() bool { return r.typing != nil }

// Starts reading at the last row in view, with a search being typed when asked
func (r *Reader) Start(find bool) {
	top, height := r.src.ReadWindow()
	r.on, r.anchor, r.hit = true, -1, -1
	r.cursor = min(max(top+height-1, 0), max(r.src.ReadRows()-1, 0))
	r.typing = nil
	if find {
		r.typing, r.back = field.New(""), true
	}
}

// Stops reading, the search and selection dropped
func (r *Reader) Stop() {
	r.on, r.typing, r.anchor, r.query, r.hits = false, nil, -1, "", nil
}

// Keymap contexts while reading: the search bar and the field keys while a query is typed, else the reading keys
func (r *Reader) Contexts() []string {
	if r.typing != nil {
		return []string{keys.Search, keys.Text}
	}
	return []string{keys.View}
}

// Keeps the cursor row in view, scrolling the least
func (r *Reader) reveal() {
	rows := r.src.ReadRows()
	r.cursor = min(max(r.cursor, 0), max(rows-1, 0))
	top, height := r.src.ReadWindow()
	switch {
	case r.cursor < top:
		r.src.ReadScroll(r.cursor)
	case r.cursor >= top+height:
		r.src.ReadScroll(r.cursor - height + 1)
	}
}

// Moves the cursor by n rows
func (r *Reader) move(n int) {
	r.cursor += n
	r.reveal()
}

// Takes typed or pasted text into the query being typed, reporting whether the reader used it
func (r *Reader) Take(msg tea.Msg) bool {
	if r.typing == nil {
		return r.on
	}
	r.typing.Take(msg)
	return true
}

// Carries out a reading key, reporting whether the reader took it; the open key is left to the text's owner
func (r *Reader) Action(id string) (tea.Cmd, bool) {
	if !r.on {
		return nil, false
	}
	if r.typing != nil {
		switch id {
		case "search.accept":
			r.query = strings.TrimSpace(r.typing.Value())
			r.typing = nil
			r.find()
			r.step(1)
		case "search.close":
			r.typing = nil
		default:
			if strings.HasPrefix(id, "text.") {
				cmd, _ := r.typing.Action(id)
				return cmd, true
			}
		}
		return nil, true
	}
	_, height := r.src.ReadWindow()
	switch id {
	case "view.down":
		r.move(1)
	case "view.up":
		r.move(-1)
	case "view.page.down":
		r.move(max(height-1, 1))
	case "view.page.up":
		r.move(-max(height-1, 1))
	case "view.half.down":
		r.move(max(height/2, 1))
	case "view.half.up":
		r.move(-max(height/2, 1))
	case "view.top":
		r.cursor = 0
		r.reveal()
	case "view.bottom":
		r.cursor = r.src.ReadRows() - 1
		r.reveal()
	case "view.prompt.prev", "view.prompt.next":
		d := 1
		if id == "view.prompt.prev" {
			d = -1
		}
		row, ok := r.src.ReadPrompt(r.cursor, d)
		if !ok {
			return wm.Notify(lang.T(txt.KitReaderNoPrompt), wm.Info), true
		}
		r.cursor = row
		r.src.ReadScroll(row)
		r.reveal()
	case "view.search", "view.search.back":
		r.typing, r.back = field.New(r.query), id == "view.search.back"
	case "view.match.next":
		r.step(1)
	case "view.match.prev":
		r.step(-1)
	case "view.select":
		if r.anchor >= 0 {
			r.anchor = -1
		} else {
			r.anchor = r.cursor
		}
	case "view.copy":
		return r.copy(), true
	case "view.leave":
		switch {
		case r.anchor >= 0:
			r.anchor = -1
		case r.query != "":
			r.query, r.hits = "", nil
		default:
			r.Stop()
		}
	default:
		return nil, false
	}
	return nil, true
}

// Finds every match of the query in the text, case folded
func (r *Reader) find() {
	r.hits, r.hit = nil, -1
	q := strings.ToLower(r.query)
	if q == "" {
		return
	}
	for row := range r.src.ReadRows() {
		text := strings.ToLower(r.src.ReadRow(row))
		for at := 0; ; {
			i := strings.Index(text[at:], q)
			if i < 0 {
				break
			}
			from := ansi.StringWidth(text[:at+i])
			r.hits = append(r.hits, readHit{row: row, from: from, to: from + ansi.StringWidth(q)})
			at += i + len(q)
		}
	}
}

// Moves to the next match in the direction of the search, or against it for a negative step, wrapping around
func (r *Reader) step(d int) {
	if len(r.hits) == 0 {
		return
	}
	if r.back {
		d = -d
	}
	next := -1
	if d > 0 {
		next = slices.IndexFunc(r.hits, func(h readHit) bool { return h.row > r.cursor })
		if next < 0 {
			next = 0
		}
	} else {
		for i := len(r.hits) - 1; i >= 0; i-- {
			if r.hits[i].row < r.cursor {
				next = i
				break
			}
		}
		if next < 0 {
			next = len(r.hits) - 1
		}
	}
	r.hit = next
	r.cursor = r.hits[next].row
	r.reveal()
}

// Copies the selected rows, else the message or command output under the cursor, with a note of how much
func (r *Reader) copy() tea.Cmd {
	from, to := r.src.ReadBlock(r.cursor)
	if r.anchor >= 0 {
		from, to = min(r.anchor, r.cursor), max(r.anchor, r.cursor)+1
		r.anchor = -1
	}
	var lines []string
	for row := from; row < to && row < r.src.ReadRows(); row++ {
		lines = append(lines, strings.TrimRight(r.src.ReadRow(row), " "))
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	text := strings.Join(lines, "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	label := lang.T(txt.KitWmCopiedLine)
	if n := len(lines); n > 1 {
		label = lang.T(txt.KitWmCopiedLines, n)
	}
	return tea.Batch(clip.Copy(text), wm.Notify(label, wm.Success))
}

// Whether the row lies in the selection
func (r *Reader) selected(row int) bool {
	return r.anchor >= 0 && row >= min(r.anchor, r.cursor) && row <= max(r.anchor, r.cursor)
}

// One rendered row of the window with the reading marks over it: the cursor and selection in its first cell and the matches lit, its colors kept
func (r *Reader) Mark(row int, line string, t theme.Theme) string {
	if !r.on {
		return line
	}
	for i := len(r.hits) - 1; i >= 0; i-- {
		h := r.hits[i]
		if h.row != row {
			continue
		}
		st := lipgloss.NewStyle().Background(t.SteelBright).Foreground(t.On(t.SteelBright))
		if i == r.hit {
			st = lipgloss.NewStyle().Background(t.Accent).Foreground(t.Bg)
		}
		line = ansi.Cut(line, 0, h.from) + st.Render(ansi.Strip(ansi.Cut(line, h.from, h.to))) + ansi.Cut(line, h.to, ansi.StringWidth(line))
	}
	mark := ""
	switch {
	case row == r.cursor:
		mark = lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Render("▌")
	case r.selected(row):
		mark = lipgloss.NewStyle().Foreground(t.Ice).Render("┃")
	default:
		return line
	}
	return mark + ansi.Cut(line, 1, max(ansi.StringWidth(line), 1))
}

// Puts the reading marks onto the cells of a painted window whose first row is the text row top: the matches lit and the cursor and selection rows
// tinted
func (r *Reader) MarkBuffer(buf *uv.Buffer, top int, t theme.Theme) {
	if !r.on {
		return
	}
	w, h := buf.Width(), buf.Height()
	for y := range h {
		row := top + y
		for i, hit := range r.hits {
			if hit.row != row {
				continue
			}
			bg, fg := t.SteelBright, t.On(t.SteelBright)
			if i == r.hit {
				bg, fg = t.Accent, t.Bg
			}
			for x := hit.from; x < hit.to && x < w; x++ {
				if c := buf.CellAt(x, y); c != nil {
					c.Style.Bg, c.Style.Fg = bg, fg
				}
			}
		}
		// A program's rows have no margin to mark, so the cursor and the selection tint the whole row instead
		if row == r.cursor || r.selected(row) {
			tint := theme.Mix(t.Surface, t.Ice, 0.12)
			if row == r.cursor {
				tint = theme.Mix(t.Surface, t.Accent, 0.3)
			}
			for x := range w {
				if c := buf.CellAt(x, y); c != nil {
					c.Style.Bg = tint
				}
			}
		}
	}
}

// Line naming the mode with the place of the cursor, the search and the keys that matter, the query with its caret while one is typed
func (r *Reader) Line(w int, t theme.Theme) (string, int) {
	base := lipgloss.NewStyle().Background(t.Steel)
	if r.typing != nil {
		lead := "/"
		if r.back {
			lead = "?"
		}
		text, caret := r.typing.Line(max(w-3, 1))
		line := base.Foreground(t.Over(t.Steel, t.Accent)).Bold(true).Render(" "+lead) + base.Foreground(t.Over(t.Steel, t.Text)).Render(text)
		return render.Fit(line+base.Render(strings.Repeat(" ", max(w-ansi.StringWidth(line), 0))), w, t), 2 + caret
	}
	parts := []string{lang.T(txt.KitReaderMode), lang.T(txt.KitReaderAt, r.cursor+1, r.src.ReadRows())}
	if r.query != "" {
		n := len(r.hits)
		at := 0
		if r.hit >= 0 {
			at = r.hit + 1
		}
		parts = append(parts, lang.T(txt.KitReaderMatches, r.query, at, n))
	}
	if r.anchor >= 0 {
		parts = append(parts, lang.T(txt.KitReaderSelecting, max(r.anchor, r.cursor)-min(r.anchor, r.cursor)+1))
	}
	var legend []string
	for _, id := range []string{"view.search", "view.prompt.prev", "view.select", "view.copy", "view.leave"} {
		if k := r.keys.Display(id); k != "" {
			a, _ := r.keys.Action(id)
			legend = append(legend, k+" "+a.LabelText())
		}
	}
	left := base.Foreground(t.Over(t.Steel, t.Accent)).Bold(true).Render(" "+parts[0]) + base.Foreground(t.Over(t.Steel, t.Text)).Render(" · "+strings.Join(parts[1:], " · "))
	right := base.Foreground(t.Over(t.Steel, t.TextDim)).Render(strings.Join(legend, " · ") + " ")
	if ansi.StringWidth(left)+ansi.StringWidth(right)+1 > w {
		right = ""
	}
	pad := max(w-ansi.StringWidth(left)-ansi.StringWidth(right), 0)
	return render.Fit(left+base.Render(strings.Repeat(" ", pad))+right, w, t), -1
}

// Buttons while reading, none while a search is typed
func (r *Reader) Hints() []wm.Hint {
	if r.typing != nil {
		return nil
	}
	return []wm.Hint{{Action: "view.copy"}, {Action: "view.select"}, {Action: "view.search"}, {Action: "view.leave"}}
}

// Row the cursor is on
func (r *Reader) Cursor() int { return r.cursor }
