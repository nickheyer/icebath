package cards

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

// Rows the input grows to before it scrolls
const maxInputRows = 6

// Rows the input keeps while it holds less
const minInputRows = 2

// Rows the completion popup shows at most
const popupRows = 8

// One file attached to the next message
type attachment struct {
	Path  string
	Media string
	Data  []byte
}

// Candidates the app offers for one prompt text, the highlighted one and the offset they replace from
type completion struct {
	value   string
	items   []string
	descs   []string
	texts   []string
	idx     int
	replace int
	// Whether the highlight was moved, the only case besides a command name in which enter takes the suggestion instead of sending
	moved bool
	// Whether every row only informs, such as a row saying completion is unavailable, the keys passing through to the prompt
	info bool
}

// Candidate an app completion source offers, its text put in from the replace offset; one without text only informs
type Offer struct {
	Text    string
	Display string
	Meta    string
}

// Pasted text or an attached file a label in the prompt stands for, kept with the prompt in the history and the drafts
type Chip struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Text  string `json:"text,omitempty"`
	Path  string `json:"path,omitempty"`
	Mime  string `json:"mime,omitempty"`
}

// Prompt as it was sent or left as a draft, with the chips of its labels, the session it was typed in, the folder and when
type Entry struct {
	Text    string `json:"text"`
	Chips   []Chip `json:"chips,omitempty"`
	Session string `json:"sid,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	At      int64  `json:"ts,omitempty"`
}

// Prompts every composer recalls, one store shared by all of them and read at each recall
type History interface {
	// Entries oldest first
	Entries() []Entry
	// Records a sent prompt, an older entry with the same text giving way to it
	Add(Entry)
}

// Clickable send button on the first input row, in composer coordinates
type sendButton struct {
	x, y, w int
}

// Most edits the undo trail keeps
const undoLimit = 200

// Prompt an undo or redo returns to, its text and the cursor as a rune offset
type composerSnap struct {
	value  string
	cursor int
}

// Multi line prompt with a history ring over the shared store, the app's completion candidates, attachments and a draft kept per session
type Composer struct {
	ta      textarea.Model
	focused bool
	// Entries of the store as they read when recall left the draft, the ring the cursor walks until the next send or the return to the draft
	ring   []Entry
	cursor int
	draft  string
	// What was typed into recalled entries, kept while moving through the ring and dropped at the next send
	edits map[int]string
	// Reverse search through the history while it runs
	search      *historySearch
	comp        *completion
	quiet       string
	attachments []attachment
	dir         string
	draftPath   string
	last        string
	baseDir     func() string
	theme       theme.Theme
	keys        *keys.Map
	busy        bool
	button      sendButton
	cut         int
	// Rows the input took in the last view
	shown      int
	width      int
	undo, redo []composerSnap
	// Number that changes whenever the draft is replaced as a whole rather than edited in place
	revision int
	// Rows the prompt keeps while it holds less
	minRows   int
	SessionID string
	// Store of the prompts the history ring recalls, nil keeping nothing
	History History
	// Chips the labels of a prompt stand for, kept with it in the history and its draft
	Chips func(text string) []Chip
	// Called with an entry put into the prompt from the history or a kept draft, so the app takes its chips back
	OnRecall func(Entry)
	// Reports whether nothing typed may reach disk, the draft and history kept in memory alone
	Volatile func() bool
	// Hands every sent line to the app as typed, commands and shell lines included
	Literal bool
	// Text an empty prompt shows in place of the command and palette legend
	Hint string
	// Puts the highlighted offer in from its replace offset when the popup is accepted
	Apply func(text string, replace int) tea.Cmd
	// Called before the prompt is read for sending, for an app that rewrites its draft first
	Sending func()
	// Stops the running reply, what the send button does while a reply runs and nothing is typed
	OnStop func() tea.Cmd
	// Whether the session is out of reach, the send button showing it cannot send
	Offline bool
	// Frame count of the owner's animation clock, the send button pulsing with it while a reply runs
	Phase int
}

// File the draft of a session is kept in under dir
func DraftPath(dir, sessionID string) string {
	return filepath.Join(dir, "drafts", sessionID+".json")
}

// Builds a composer on the keymap keeping the drafts of the sessions it binds under dir, none when empty, completing paths against base
func NewComposer(t theme.Theme, km *keys.Map, dir string, base func() string) *Composer {
	ta := textarea.New()
	ta.Prompt = t.Glyphs.Prompt + " "
	ta.Placeholder = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.DynamicHeight = true
	ta.MinHeight = minInputRows
	ta.MaxHeight = maxInputRows
	ta.SetHeight(minInputRows)
	ta.SetVirtualCursor(false)
	c := &Composer{ta: ta, baseDir: base, keys: km, dir: dir, minRows: minInputRows}
	c.Rebind()
	c.SetTheme(t)
	c.last = c.ta.Value()
	return c
}

// Directory the drafts of the bound sessions are kept under, empty keeping them in memory
func (c *Composer) SetDraftDir(dir string) { c.dir = dir }

// Binds the prompt to a session: text typed before any session was bound moves to it, a prompt bound to another session keeps its draft there and
// takes the draft of the new one
func (c *Composer) Bind(sessionID string) tea.Cmd {
	prev := c.SessionID
	c.SessionID = sessionID
	if c.dir == "" || sessionID == prev {
		return nil
	}
	old := c.draftPath
	c.draftPath = ""
	if sessionID != "" {
		c.draftPath = DraftPath(c.dir, sessionID)
	}
	if (prev == "" || old == "") && strings.TrimSpace(c.ta.Value()) != "" {
		c.last = ""
		return c.persist()
	}
	c.leaveRing()
	c.forget()
	c.revision++
	c.comp, c.search = nil, nil
	entry, ok := c.readDraft()
	c.ta.SetValue(entry.Text)
	c.ta.MoveToEnd()
	c.last = c.ta.Value()
	if ok && c.OnRecall != nil {
		c.OnRecall(entry)
	}
	return nil
}

// Draft of the bound session as kept on disk, false when there is none
func (c *Composer) readDraft() (Entry, bool) {
	if c.draftPath == "" {
		return Entry{}, false
	}
	data, err := os.ReadFile(c.draftPath)
	if err != nil {
		return Entry{}, false
	}
	var e Entry
	if json.Unmarshal(data, &e) != nil {
		return Entry{}, false
	}
	return e, true
}

// Rebuilds the editing bindings of the input from the keymap, every other key only typing text
func (c *Composer) Rebind() { c.ta.KeyMap = editingKeys(c.keys) }

// Text editing bindings of the multi line input, taken from the text context of the keymap every field shares, and the row moves of the prompt
func editingKeys(km *keys.Map) textarea.KeyMap {
	bind := func(id string) key.Binding {
		var spelled []string
		for _, chord := range km.Keys(id) {
			spelled = append(spelled, keys.Press(chord).String())
		}
		if len(spelled) == 0 {
			return key.Binding{}
		}
		return key.NewBinding(key.WithKeys(spelled...))
	}
	return textarea.KeyMap{
		CharacterBackward:       bind("text.left"),
		CharacterForward:        bind("text.right"),
		WordBackward:            bind("text.word.left"),
		WordForward:             bind("text.word.right"),
		LinePrevious:            bind("composer.up"),
		LineNext:                bind("composer.down"),
		LineStart:               bind("text.line.start"),
		LineEnd:                 bind("text.line.end"),
		DeleteCharacterBackward: bind("text.delete.back"),
		DeleteCharacterForward:  bind("text.delete.forward"),
		DeleteWordBackward:      bind("text.delete.word.back"),
		DeleteWordForward:       bind("text.delete.word.forward"),
		DeleteBeforeCursor:      bind("text.delete.line.before"),
		DeleteAfterCursor:       bind("text.delete.line.after"),
		SelectAll:               bind("text.select.all"),
		SelectCharacterBackward: bind("text.select.left"),
		SelectCharacterForward:  bind("text.select.right"),
		SelectWordBackward:      bind("text.select.word.left"),
		SelectWordForward:       bind("text.select.word.right"),
		SelectLineUp:            bind("text.select.up"),
		SelectLineDown:          bind("text.select.down"),
		CopySelection:           bind("text.copy"),
		Paste:                   bind("text.paste"),
	}
}

// Paints the input with the theme, the prompt mark on the first row only so a long prompt never reads as several
func (c *Composer) SetTheme(t theme.Theme) {
	c.ta.SetStyles(editorStyles(t))
	c.theme = t
	mark := t.Glyphs.Prompt + " "
	c.ta.Prompt = mark
	width := ansi.StringWidth(mark)
	c.ta.SetPromptFunc(width, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return mark
		}
		return strings.Repeat(" ", width)
	})
}

// Styles of a multi line input in the theme, the cursor line left plain
func editorStyles(t theme.Theme) textarea.Styles {
	st := textarea.DefaultDarkStyles()
	if t.Name == "light" {
		st = textarea.DefaultLightStyles()
	}
	st.Focused.Prompt = st.Focused.Prompt.Foreground(t.Accent)
	st.Focused.Text = st.Focused.Text.Foreground(t.Text)
	st.Focused.CursorLine = lipgloss.NewStyle()
	st.Focused.Base = lipgloss.NewStyle()
	st.Focused.EndOfBuffer = lipgloss.NewStyle().Foreground(t.Surface)
	st.Blurred.Prompt = st.Blurred.Prompt.Foreground(t.TextDim)
	st.Blurred.Text = st.Blurred.Text.Foreground(t.Text)
	st.Blurred.Base = lipgloss.NewStyle()
	st.Blurred.EndOfBuffer = lipgloss.NewStyle().Foreground(t.Surface)
	return st
}

// Tells the send button whether a run is going, so it offers to steer it
func (c *Composer) SetBusy(on bool) { c.busy = on }

// Text typed so far
func (c *Composer) Value() string { return c.ta.Value() }

// Number that changes whenever the draft is replaced as a whole, by a send, a recall, a search, a session switch or the app setting it, so work
// started for an older draft can tell it is stale
func (c *Composer) Revision() int { return c.revision }

// Replaces the text with the cursor at its end, the completion popup closed until the app offers again and a recalled entry left for the draft slot
func (c *Composer) SetValue(text string) tea.Cmd {
	c.revision++
	c.forget()
	c.ta.SetValue(text)
	c.ta.MoveToEnd()
	c.comp = nil
	c.leaveRing()
	return c.persist()
}

// Replaces the text with the cursor at a rune offset of it, otherwise as SetValue does
func (c *Composer) SetValueAt(text string, offset int) tea.Cmd {
	c.revision++
	c.forget()
	c.restore(composerSnap{value: text, cursor: offset})
	c.comp = nil
	c.leaveRing()
	return c.persist()
}

// Inserts text at the cursor as if typed
func (c *Composer) Insert(text string) tea.Cmd {
	before := c.snap()
	c.ta.InsertString(text)
	c.tracked(before)
	c.follow()
	return c.persist()
}

// Inserts text at the end of the prompt, the cursor following it
func (c *Composer) Append(text string) tea.Cmd {
	c.ta.MoveToEnd()
	return c.Insert(text)
}

// Span of prompt text a person selected
type Selection struct {
	Text       string
	start, end textarea.Position
}

// Selected span of the prompt, false when nothing is selected
func (c *Composer) Selection() (Selection, bool) {
	start, end, ok := c.ta.Selection()
	if !ok {
		return Selection{}, false
	}
	return Selection{Text: c.ta.SelectedText(), start: start, end: end}, true
}

// Removes the span when it is still the one selected, as a cut does once the clipboard holds its text
func (c *Composer) Cut(span Selection) tea.Cmd {
	now, ok := c.Selection()
	if !ok || now != span {
		return nil
	}
	before := c.snap()
	c.ta.DeleteSelection()
	c.tracked(before)
	return c.persist()
}

// Reports whether a cell of the composer shows the text of the input, where a press places the cursor and a drag selects
func (c *Composer) Holds(x, y int) bool {
	right := c.width
	if c.button.w > 0 {
		right = c.button.x - 1
	}
	return x >= 0 && x < right && y >= c.button.y && y < c.button.y+c.shown
}

// Cell of the text area at which the grapheme drawn on a cell of the composer starts. The text area places a cell rune by rune, which lands
// inside an emoji joined from several or between a letter and its marks, so the row is walked grapheme by grapheme here
func (c *Composer) cell(x, y int) (int, int) {
	y += c.cut - c.button.y
	gutter := ansi.StringWidth(c.ta.Prompt)
	first, last := c.ta.PositionAt(gutter, y), c.ta.PositionAt(gutter+c.width, y)
	lines := strings.Split(c.ta.Value(), "\n")
	if first.Row != last.Row || first.Row >= len(lines) {
		return x, y
	}
	line := []rune(lines[first.Row])
	if first.Col > last.Col || last.Col > len(line) {
		return x, y
	}
	row, col, cells := line[first.Col:last.Col], 0, 0
	for _, stop := range graphemeStops(row)[1:] {
		wide := ansi.StringWidth(string(row[col:stop]))
		if cells+wide > x-gutter {
			break
		}
		col, cells = stop, cells+wide
	}
	at := gutter
	for _, r := range row[:col] {
		at += ansi.StringWidth(string(r))
	}
	return at, y
}

// Starts a selection at a cell of the composer, the cursor going to the grapheme drawn there
func (c *Composer) Press(x, y int) {
	x, y = c.cell(x, y)
	c.ta.BeginSelection(x, y)
}

// Extends the selection a press started to a cell of the composer
func (c *Composer) Drag(x, y int) {
	x, y = c.cell(x, y)
	c.ta.ExtendSelection(x, y)
}

// Ends the selection a press started, a press and release on one place leaving the cursor there with nothing selected
func (c *Composer) Release() { c.ta.EndSelection() }

// Selects the whole prompt
func (c *Composer) SelectAll() { c.ta.SelectAll() }

// Place of the cursor in the prompt, counted in runes from its start
func (c *Composer) Offset() int { return c.snap().cursor }

// Reports whether an up key leaves the prompt, it being empty or the cursor on its first screen row with nothing selected
func (c *Composer) AtTop() bool {
	return c.ta.Value() == "" || (!c.ta.HasSelection() && c.ta.Line() == 0 && c.ta.LineInfo().RowOffset == 0)
}

// Reports whether a down key leaves the prompt, it being empty or the cursor on its last screen row with nothing selected
func (c *Composer) AtBottom() bool {
	info := c.ta.LineInfo()
	return c.ta.Value() == "" || (!c.ta.HasSelection() && c.ta.Line() == c.ta.LineCount()-1 && info.RowOffset >= info.Height-1)
}

// Drops the selection, the text left as it is
func (c *Composer) ClearSelection() { c.ta.ClearSelection() }

// Leaves a recalled history entry for the unsent draft, what was typed into the entry kept for when it comes back
func (c *Composer) BackToDraft() {
	if c.recalling() {
		c.recall(len(c.ring) - c.cursor)
	}
}

// Whether the prompt shows a history entry rather than the draft
func (c *Composer) recalling() bool { return c.ring != nil && c.cursor < len(c.ring) }

// Position in the history while recalling, false at the unsent draft
func (c *Composer) Recalling() (int, bool) { return c.cursor, c.recalling() }

// Texts the prompt keeps out of view while it shows another slot, the draft and the edited history entries, whose chips must stay
func (c *Composer) Kept() []string {
	if c.ring == nil {
		return nil
	}
	out := []string{c.draft}
	for _, e := range c.edits {
		out = append(out, e)
	}
	return out
}

// Forgets the ring and its edits, the prompt in view becoming the draft
func (c *Composer) leaveRing() {
	c.ring, c.edits, c.draft, c.cursor = nil, nil, "", 0
}

// Files attached to the next message
func (c *Composer) Attachments() []string {
	out := make([]string, 0, len(c.attachments))
	for _, a := range c.attachments {
		out = append(out, a.Path)
	}
	return out
}

// Popup of the candidates offered for exactly the text typed now, nil while the text moved on and the next answer is awaited
func (c *Composer) live() *completion {
	if c.comp != nil && c.comp.value != c.ta.Value() {
		return nil
	}
	return c.comp
}

// Whether the prompt is a slash command name still being typed, where enter takes the highlighted command
func (c *Composer) namingCommand() bool {
	v := c.ta.Value()
	return strings.HasPrefix(v, "/") && !strings.ContainsAny(v, " \n")
}

// Candidates shown in the popup, nil when it is closed
func (c *Composer) Completions() []string {
	if c.live() == nil {
		return nil
	}
	return append([]string(nil), c.comp.items...)
}

// Reports whether the completion popup takes keys for the text typed now, a popup that only informs taking none
func (c *Composer) Completing() bool {
	comp := c.live()
	return comp != nil && !comp.info
}

// Shows the candidates the app found for a prompt text, the first one highlighted, none closing the popup. An answer for a text no longer
// typed is dropped; the list on screen follows the typing on its own until the answer for the current text replaces it
func (c *Composer) Offer(value string, offers []Offer, replace int) {
	if value != c.ta.Value() {
		return
	}
	if len(offers) == 0 || value == c.quiet {
		c.comp = nil
		return
	}
	c.quiet = ""
	comp := &completion{value: value, replace: replace, info: true}
	for _, o := range offers {
		label := o.Display
		if label == "" {
			label = o.Text
		}
		comp.items = append(comp.items, label)
		comp.descs = append(comp.descs, o.Meta)
		comp.texts = append(comp.texts, o.Text)
		comp.info = comp.info && o.Text == ""
	}
	c.comp = comp
}

// Keeps the popup true to the text typed now: typing on past the text it answered narrows its rows to those the new word still matches, any
// other change closes it until the app offers again, so a row for older text is never drawn
func (c *Composer) follow() {
	comp := c.comp
	value := c.ta.Value()
	if comp == nil || comp.value == value {
		return
	}
	if comp.info || !strings.HasPrefix(value, comp.value) || comp.replace > utf8.RuneCountInString(value) {
		c.comp = nil
		return
	}
	word := strings.ToLower(strings.TrimPrefix(string([]rune(value)[comp.replace:]), "/"))
	kept := &completion{value: value, replace: comp.replace}
	for i, text := range comp.texts {
		if !subsequence(word, strings.ToLower(strings.TrimPrefix(text, "/"))) && !subsequence(word, strings.ToLower(ansi.Strip(comp.items[i]))) {
			continue
		}
		kept.items = append(kept.items, comp.items[i])
		kept.descs = append(kept.descs, comp.descs[i])
		kept.texts = append(kept.texts, text)
	}
	if len(kept.items) == 0 {
		c.comp = nil
		return
	}
	c.comp = kept
}

// Whether every rune of the word appears in the text in order
func subsequence(word, text string) bool {
	runes := []rune(text)
	i := 0
	for _, r := range word {
		for i < len(runes) && runes[i] != r {
			i++
		}
		if i == len(runes) {
			return false
		}
		i++
	}
	return true
}

// Sets the rows the prompt keeps while it holds less, one on a small terminal where every row counts
func (c *Composer) SetMinRows(n int) {
	n = min(max(n, 1), maxInputRows)
	if c.minRows == n {
		return
	}
	c.minRows = n
	c.ta.MinHeight = n
}

// Rows the composer wants, two at least unless set lower, growing with the wrapped text only. The completion popup floats over the rows above the composer
// (see Popup) so opening, narrowing or closing it never moves the layout
func (c *Composer) Height() int {
	return max(c.ta.Height(), c.minRows)
}

// Reports whether the input is empty with nothing attached
func (c *Composer) Empty() bool {
	return strings.TrimSpace(c.ta.Value()) == "" && len(c.attachments) == 0
}

// Moves the input focus with the card focus
func (c *Composer) SetFocus(on bool) tea.Cmd {
	c.focused = on
	if on {
		return c.ta.Focus()
	}
	c.ta.Blur()
	c.comp = nil
	return nil
}

// Takes pasted text and typed keys the keymap left unbound, which only ever insert their text
func (c *Composer) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		if c.search != nil {
			c.searchTyped(msg)
			return nil
		}
		before := c.snap()
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(msg)
		c.tracked(before)
		c.follow()
		return tea.Batch(cmd, c.persist())
	case tea.KeyPressMsg:
		if c.search != nil {
			c.searchTyped(msg)
			return nil
		}
		if msg.Text == "" {
			return nil
		}
		before := c.snap()
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(msg)
		c.tracked(before)
		c.follow()
		return tea.Batch(cmd, c.persist())
	}
	return nil
}

// Carries out a keymap action of the prompt or its completion popup
func (c *Composer) Action(id string) tea.Cmd {
	if c.search != nil {
		if cmd, used := c.SearchKey(id); used {
			return cmd
		}
	}
	if c.live() != nil && !c.comp.info {
		switch id {
		case "complete.next":
			c.comp.idx = (c.comp.idx + 1) % len(c.comp.items)
			c.comp.moved = true
			return nil
		case "complete.prev":
			c.comp.idx = (c.comp.idx - 1 + len(c.comp.items)) % len(c.comp.items)
			c.comp.moved = true
			return nil
		case "composer.send":
			if c.Apply != nil && (c.comp.moved || c.namingCommand()) {
				return c.Apply(c.comp.texts[c.comp.idx], c.comp.replace)
			}
			c.comp = nil
		case "complete.accept":
			if c.Apply == nil {
				return nil
			}
			return c.Apply(c.comp.texts[c.comp.idx], c.comp.replace)
		case "complete.close":
			c.quiet = c.ta.Value()
			c.comp = nil
			return nil
		}
	}
	switch id {
	case "composer.history.prev":
		c.recall(-1)
		return nil
	case "composer.history.next":
		c.recall(1)
		return nil
	case "composer.history.search":
		c.startSearch()
		return nil
	case "composer.send":
		if c.escaped() {
			before := c.snap()
			c.breakLine()
			c.tracked(before)
			return c.persist()
		}
		return c.submit()
	case "text.undo", "text.redo":
		if id == "text.undo" {
			c.swap(&c.undo, &c.redo)
		} else {
			c.swap(&c.redo, &c.undo)
		}
		c.follow()
		return c.persist()
	case "text.newline":
		return c.Insert("\n")
	case "composer.up":
		if c.AtTop() {
			c.recall(-1)
			return nil
		}
	case "composer.down":
		if c.AtBottom() {
			c.recall(1)
			return nil
		}
	case "composer.attachments.clear":
		c.attachments = nil
		return wm.Notify(lang.T(txt.ComposerAttachmentsCleared), wm.Info)
	case "text.copy":
		if c.ta.HasSelection() {
			return clip.Copy(c.ta.SelectedText())
		}
		return nil
	}
	if !strings.HasPrefix(id, "text.") && id != "composer.up" && id != "composer.down" {
		return nil
	}
	press := keys.Press(c.keys.First(id))
	if press.Code == 0 {
		return nil
	}
	before := c.snap()
	var cmds []tea.Cmd
	for range c.graphemeSteps(id) {
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(press)
		cmds = append(cmds, cmd)
	}
	c.settle(id)
	c.tracked(before)
	c.follow()
	return tea.Batch(append(cmds, c.persist())...)
}

// Rune offsets at which the grapheme clusters of a line start, the length of the line last
func graphemeStops(line []rune) []int {
	stops := []int{0}
	for rest := string(line); rest != ""; {
		cluster, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if cluster == "" {
			break
		}
		stops = append(stops, stops[len(stops)-1]+utf8.RuneCountInString(cluster))
		rest = rest[len(cluster):]
	}
	return stops
}

// Line the cursor is on as runes and the cursor column in it
func (c *Composer) cursorLine() ([]rune, int) {
	lines := strings.Split(c.ta.Value(), "\n")
	line := []rune(lines[min(c.ta.Line(), len(lines)-1)])
	return line, min(c.ta.Column(), len(line))
}

// Rune steps of the input a key takes so that it crosses the whole grapheme beside the cursor, a letter with its combining marks or an emoji joined
// from several, one step for every key that moves or deletes by something else
func (c *Composer) graphemeSteps(id string) int {
	line, col := c.cursorLine()
	stops := graphemeStops(line)
	switch id {
	case "text.delete.back", "text.delete.forward":
		if c.ta.HasSelection() {
			return 1
		}
	}
	switch id {
	case "text.left", "text.select.left", "text.delete.back":
		for i := len(stops) - 1; i >= 0 && col > 0; i-- {
			if stops[i] < col {
				return col - stops[i]
			}
		}
	case "text.right", "text.select.right", "text.delete.forward":
		for _, stop := range stops {
			if stop > col {
				return stop - col
			}
		}
	}
	return 1
}

// Brings a cursor a line move left inside a grapheme back to the start of that grapheme, a selection being extended following it
func (c *Composer) settle(id string) {
	switch id {
	case "composer.up", "composer.down", "text.select.up", "text.select.down":
	default:
		return
	}
	line, col := c.cursorLine()
	start := 0
	for _, stop := range graphemeStops(line) {
		if stop <= col {
			start = stop
		}
	}
	switch {
	case start == col:
	case strings.HasPrefix(id, "text.select."):
		back := keys.Press(c.keys.First("text.select.left"))
		for ; back.Code != 0 && col > start; col-- {
			c.ta, _ = c.ta.Update(back)
		}
	default:
		c.ta.SetCursorColumn(start)
	}
}

// Prompt text and cursor offset now
func (c *Composer) snap() composerSnap {
	lines := strings.Split(c.ta.Value(), "\n")
	row := min(c.ta.Line(), len(lines)-1)
	off := 0
	for _, line := range lines[:row] {
		off += utf8.RuneCountInString(line) + 1
	}
	return composerSnap{value: c.ta.Value(), cursor: off + min(c.ta.Column(), utf8.RuneCountInString(lines[row]))}
}

// Puts the prompt back to a snapshot with the cursor at its offset
func (c *Composer) restore(s composerSnap) {
	c.ta.SetValue(s.value)
	lines := strings.Split(s.value, "\n")
	row, col := 0, s.cursor
	for row < len(lines)-1 && col > utf8.RuneCountInString(lines[row]) {
		col -= utf8.RuneCountInString(lines[row]) + 1
		row++
	}
	c.ta.MoveToBegin()
	for c.ta.Line() < row {
		line, at := c.ta.Line(), c.ta.Column()
		c.ta.CursorDown()
		if c.ta.Line() == line && c.ta.Column() == at {
			break
		}
	}
	c.ta.SetCursorColumn(col)
}

// Records the prompt before a changing edit on the undo trail and drops the redo trail; an edited history entry stays the one in view
func (c *Composer) tracked(before composerSnap) {
	if c.ta.Value() == before.value {
		return
	}
	c.undo = append(c.undo, before)
	if len(c.undo) > undoLimit {
		c.undo = c.undo[len(c.undo)-undoLimit:]
	}
	c.redo = nil
}

// Drops both trails when the prompt is replaced from outside the typing
func (c *Composer) forget() { c.undo, c.redo = nil, nil }

// Steps back or forward one edit, the prompt left behind recorded on the other trail
func (c *Composer) swap(from, to *[]composerSnap) {
	n := len(*from)
	if n == 0 {
		return
	}
	entry := (*from)[n-1]
	*from = (*from)[:n-1]
	*to = append(*to, c.snap())
	c.restore(entry)
}

// Turns the backslash before the cursor into a line break, leaving the cursor at the start of the new line
func (c *Composer) breakLine() {
	lines := strings.Split(c.ta.Value(), "\n")
	row := min(c.ta.Line(), len(lines)-1)
	runes := []rune(lines[row])
	col := min(c.ta.Column(), len(runes))
	lines[row] = string(runes[:col-1])
	lines = append(lines[:row+1], append([]string{string(runes[col:])}, lines[row+1:]...)...)
	c.ta.SetValue(strings.Join(lines, "\n"))
	c.ta.MoveToBegin()
	for c.ta.Line() < row+1 {
		line, col := c.ta.Line(), c.ta.Column()
		c.ta.CursorDown()
		if c.ta.Line() == line && c.ta.Column() == col {
			break
		}
	}
	c.ta.CursorStart()
}

// Reports whether the bound session keeps nothing typed on disk
func (c *Composer) ghost() bool { return c.Volatile != nil && c.Volatile() }

// Entry of a prompt text with the chips its labels stand for, the session and the folder it is typed in
func (c *Composer) entry(text string) Entry {
	e := Entry{Text: text, Session: c.SessionID, Cwd: c.base(), At: time.Now().UnixMilli()}
	if c.Chips != nil {
		e.Chips = c.Chips(text)
	}
	return e
}

// Writes the draft of the bound session to disk when the text changed, the draft slot only, a recalled entry in view leaving it as it was
func (c *Composer) persist() tea.Cmd {
	v := c.ta.Value()
	if c.recalling() {
		v = c.draft
	}
	if v == c.last {
		return nil
	}
	c.last = v
	if c.draftPath == "" || c.ghost() {
		return nil
	}
	if strings.TrimSpace(v) == "" {
		os.Remove(c.draftPath)
		return nil
	}
	data, err := json.Marshal(c.entry(v))
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.draftPath), 0o755); err != nil {
		return wm.Notify(err.Error(), wm.Warn)
	}
	if err := os.WriteFile(c.draftPath, data, 0o600); err != nil {
		return wm.Notify(err.Error(), wm.Warn)
	}
	return nil
}

// Adds a sent prompt to the shared history, keeping nothing while the session is ghost
func (c *Composer) remember(text string) {
	if c.ghost() || c.History == nil {
		return
	}
	c.History.Add(c.entry(text))
}

// Moves through the history ring by d, clamped at both ends with the unsent draft past the newest, an older entry opening at its end and a newer at
// its start; the ring is read from the store when recall leaves the draft
func (c *Composer) recall(d int) {
	if c.ring != nil && !c.recalling() && len(c.edits) == 0 {
		c.ring = nil
	}
	if c.ring == nil {
		if d > 0 || c.History == nil {
			return
		}
		c.draft = c.ta.Value()
		c.ring = c.History.Entries()
		c.cursor = len(c.ring)
		if len(c.ring) == 0 {
			c.ring = nil
			return
		}
	}
	c.keepSlot()
	next := min(max(c.cursor+d, 0), len(c.ring))
	if next == c.cursor {
		return
	}
	c.cursor = next
	c.forget()
	c.revision++
	c.ta.SetValue(c.slotText(next))
	c.comp, c.quiet = nil, c.ta.Value()
	if d > 0 && next < len(c.ring) {
		c.ta.MoveToBegin()
	} else {
		c.ta.MoveToEnd()
	}
	if next < len(c.ring) && c.OnRecall != nil {
		c.OnRecall(c.ring[next])
	}
}

// Keeps the text in view in the slot it came from, the draft or an edited history entry
func (c *Composer) keepSlot() {
	v := c.ta.Value()
	if c.cursor >= len(c.ring) {
		c.draft = v
		return
	}
	if v == c.ring[c.cursor].Text {
		delete(c.edits, c.cursor)
		return
	}
	if c.edits == nil {
		c.edits = map[int]string{}
	}
	c.edits[c.cursor] = v
}

// Text of a ring slot as last left, the draft past the newest
func (c *Composer) slotText(i int) string {
	if i >= len(c.ring) {
		return c.draft
	}
	if e, ok := c.edits[i]; ok {
		return e
	}
	return c.ring[i].Text
}

// Parses the text into a message or a command and resets the input
func (c *Composer) submit() tea.Cmd {
	if c.Sending != nil {
		c.Sending()
	}
	text := strings.TrimSpace(c.ta.Value())
	if text == "" && len(c.attachments) == 0 {
		return nil
	}
	c.remember(text)
	c.leaveRing()
	c.forget()
	c.revision++
	c.ta.Reset()
	c.comp = nil
	c.quiet = ""
	cmd := c.persist()
	if c.Literal {
		files := make([]Attachment, 0, len(c.attachments))
		for _, a := range c.attachments {
			files = append(files, Attachment{Path: a.Path, MediaType: a.Media, Data: a.Data})
		}
		c.attachments = nil
		id := c.SessionID
		return tea.Batch(cmd, func() tea.Msg { return SubmitMsg{Session: id, Text: text, Attachments: files} })
	}
	if line, ok := strings.CutPrefix(text, "!"); ok && strings.TrimSpace(line) != "" {
		id := c.SessionID
		return tea.Batch(cmd, func() tea.Msg { return RunMsg{Session: id, Line: strings.TrimSpace(line)} })
	}
	if strings.HasPrefix(text, "/") {
		name, args, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
		name = strings.ToLower(strings.TrimSpace(name))
		args = strings.TrimSpace(args)
		if name == "attach" {
			return tea.Batch(cmd, c.attach(args))
		}
		id := c.SessionID
		return tea.Batch(cmd, func() tea.Msg { return CommandMsg{Session: id, Name: name, Args: args} })
	}
	files := make([]Attachment, 0, len(c.attachments))
	for _, a := range c.attachments {
		files = append(files, Attachment{Path: a.Path, MediaType: a.Media, Data: a.Data})
	}
	c.attachments = nil
	id := c.SessionID
	return tea.Batch(cmd, func() tea.Msg { return SubmitMsg{Session: id, Text: text, Attachments: files} })
}

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// Reads an image file into an attachment for the next message
func (c *Composer) attach(path string) tea.Cmd {
	path = strings.TrimSpace(path)
	if path == "" {
		return wm.Notify("no path", wm.Warn)
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.base(), path)
	}
	media, ok := imageTypes[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return wm.Notify("not a png, jpeg, gif or webp", wm.Warn)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	c.attachments = append(c.attachments, attachment{Path: path, Media: media, Data: data})
	return wm.Notify("attached "+filepath.Base(path)+" ("+render.Bytes(int64(len(data)))+")", wm.Success)
}

// Attaches the image at the path, as the attach command does
func (c *Composer) Attach(path string) tea.Cmd { return c.attach(path) }

// Directory relative attachment paths resolve against
func (c *Composer) base() string {
	if c.baseDir != nil {
		if dir := c.baseDir(); dir != "" {
			return dir
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// Send button, one arrow whose look follows the session: dim on an empty prompt, a lit pill once there is something to send, a pulsing steer
// arrow while a reply runs and something is typed, a pulsing stop mark while a reply runs on an empty prompt and a cross while the session is
// out of reach. It always takes three cells so the input never re-wraps as the state changes
func (c *Composer) buttonLabel() string {
	t := c.theme
	g := t.Glyphs
	glyph := func(mark, fallback string) string {
		if mark == "" {
			return fallback
		}
		return mark
	}
	send, steer, stop, off := glyph(g.Send, ">"), glyph(g.Steer, "~"), glyph(g.Done, "#"), glyph(g.Fail, "x")
	empty := c.Empty()
	if t.Plain {
		mark := send
		switch {
		case c.Offline:
			mark = off
		case c.busy && empty:
			mark = stop
		case c.busy:
			mark = steer
		}
		if empty && !c.busy && !c.Offline {
			return lipgloss.NewStyle().Foreground(t.TextDim).Render(" " + mark + " ")
		}
		return lipgloss.NewStyle().Reverse(true).Render(" " + mark + " ")
	}
	pulse := (1 + math.Sin(float64(c.Phase)*0.35)) / 2
	cell := lipgloss.NewStyle().Bold(true)
	switch {
	case c.Offline:
		return cell.Foreground(t.Alarm).Render(" " + off + " ")
	case c.busy && empty:
		return cell.Foreground(theme.Mix(t.TextDim, t.Accent, 0.3+0.7*pulse)).Render(" " + stop + " ")
	case c.busy:
		return cell.Foreground(t.Bg).Background(theme.Mix(t.Steel, t.Accent, 0.45+0.55*pulse)).Render(" " + steer + " ")
	case empty:
		return cell.Foreground(t.Steel).Render(" " + send + " ")
	}
	return cell.Foreground(t.Bg).Background(t.Ice).Render(" " + send + " ")
}

// Empty prompt line, the mark then how to reach commands and the palette, all muted
func (c *Composer) placeholder(w int) string {
	t := c.theme
	prompt := t.TextDim
	if c.focused {
		prompt = t.Accent
	}
	hint := c.Hint
	if hint == "" {
		hint = lang.T(txt.KitComposerPlaceholder, c.keys.Chord("palette"))
	}
	line := lipgloss.NewStyle().Foreground(prompt).Render(c.ta.Prompt) + lipgloss.NewStyle().Foreground(t.TextDim).Render(hint)
	return render.Fit(line, w, t)
}

// Fits the input beside the send button at width w, wrapping the text there so the height reads true
func (c *Composer) Fit(w int) int {
	c.width = w
	bw := ansi.StringWidth(c.buttonLabel())
	iw := w
	c.button = sendButton{}
	if w-bw-1 >= 12 {
		iw = w - bw - 1
		c.button = sendButton{x: iw + 1, w: bw}
	}
	c.ta.SetWidth(iw)
	return iw
}

func (c *Composer) View(w, h int) string {
	rows := max(h, 1)
	button := c.buttonLabel()
	iw := c.Fit(w)
	c.button.y, c.shown = 0, rows
	input := strings.Split(c.ta.View(), "\n")
	c.cut = 0
	if len(input) > rows {
		cy := 0
		if cur := c.ta.Cursor(); cur != nil {
			cy = cur.Y
		}
		c.cut = min(max(cy-rows+1, 0), len(input)-rows)
		input = input[c.cut : c.cut+rows]
	}
	if c.ta.LineCount() == 1 && c.ta.LineInfo().Height <= 1 {
		for i := 1; i < len(input); i++ {
			input[i] = ""
		}
	}
	if c.ta.Value() == "" && len(input) > 0 {
		input[0] = c.placeholder(iw)
	}
	if c.search != nil && len(input) > 0 {
		input = append([]string{c.searchLine(iw)}, input...)[:len(input)]
	}
	for i := range input {
		input[i] = render.Fit(input[i], iw, c.theme)
	}
	if c.button.w > 0 && len(input) > 0 {
		input[0] += " " + button
	}
	parts := make([]string, 0, h)
	parts = append(parts, input...)
	for len(parts) < h {
		parts = append(parts, "")
	}
	return strings.Join(parts[:h], "\n")
}

// Sends when the click lands on the send button, or stops the running reply when nothing is typed, reporting whether it took the click
func (c *Composer) Click(x, y int) (tea.Cmd, bool) {
	b := c.button
	if b.w == 0 || y != b.y || x < b.x || x >= b.x+b.w {
		return nil, false
	}
	if c.busy && c.Empty() && c.OnStop != nil {
		return c.OnStop(), true
	}
	return c.submit(), true
}

// Completion rows the owner paints over the rows right above the composer, only those for the text typed now, nil when closed or unfocused
func (c *Composer) Popup(w int) []string {
	comp := c.live()
	if comp == nil || !c.focused || len(comp.items) == 0 {
		return nil
	}
	t := c.theme
	limit := min(len(comp.items), popupRows)
	from := 0
	if comp.idx >= limit {
		from = comp.idx - limit + 1
	}
	base := lipgloss.NewStyle().Foreground(t.Text).Background(t.Surface)
	dim := lipgloss.NewStyle().Foreground(t.TextDim).Background(t.Surface)
	lit := lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
	var rows []string
	for i := from; i < from+limit && i < len(comp.items); i++ {
		name, desc := " "+ansi.Strip(comp.items[i]), ""
		if i < len(comp.descs) && comp.descs[i] != "" {
			name, desc = " "+render.Fit(ansi.Strip(comp.items[i]), 14, t)+" ", ansi.Strip(comp.descs[i])
		}
		line := render.Fit(name+desc, w, t)
		if i == comp.idx && !comp.info {
			rows = append(rows, lit.Render(line))
			continue
		}
		cut := min(ansi.StringWidth(name), ansi.StringWidth(line))
		rows = append(rows, base.Render(ansi.Truncate(line, cut, ""))+dim.Render(ansi.TruncateLeft(line, cut, "")))
	}
	return rows
}

// Paints the completion popup over the last rows of the lines above the composer, leaving the layout where it was
func OverlayPopup(above []string, popup []string) []string {
	if len(popup) == 0 || len(above) == 0 {
		return above
	}
	popup = popup[max(len(popup)-len(above), 0):]
	copy(above[len(above)-len(popup):], popup)
	return above
}

// Cursor inside the input area, on the search line while a search runs
func (c *Composer) Cursor() *tea.Cursor {
	if !c.focused {
		return nil
	}
	if c.search != nil {
		_, caret := c.search.query.Line(max(c.width-c.searchLabelWidth(), 1))
		return tea.NewCursor(c.searchLabelWidth()+caret, 0)
	}
	cur := c.ta.Cursor()
	if cur == nil {
		return nil
	}
	cur.Y -= c.cut
	return cur
}

// Short trailer naming the attachments with their size and the history position
func (c *Composer) Status() string {
	var parts []string
	if n := len(c.attachments); n > 0 {
		size := 0
		for _, a := range c.attachments {
			size += len(a.Data)
		}
		parts = append(parts, itoa(n)+" attached "+render.Bytes(int64(size)))
	}
	if c.recalling() {
		parts = append(parts, lang.T(txt.KitComposerHistoryAt, c.cursor+1, len(c.ring)))
	}
	return strings.Join(parts, " "+c.theme.Glyphs.Sep+" ")
}

func itoa(n int) string { return strconv.Itoa(n) }

// Reports whether a backslash sits right before the cursor
func (c *Composer) escaped() bool {
	lines := strings.Split(c.ta.Value(), "\n")
	row := min(c.ta.Line(), len(lines)-1)
	runes := []rune(lines[row])
	col := min(c.ta.Column(), len(runes))
	return col > 0 && runes[col-1] == '\\'
}

// Reverse search through the history, readline style, with the prompt it started from kept to put back on cancel
type historySearch struct {
	query *field.Field
	// Ring index of the match in view, len(ring) while nothing matches
	at          int
	saved       string
	savedCursor int
	// Whether only the prompts of the bound session are searched
	mine bool
}

// Starts a reverse search from the prompt as it stands, over every session's prompts
func (c *Composer) startSearch() {
	if c.ring == nil {
		if c.History == nil {
			return
		}
		c.draft = c.ta.Value()
		c.ring = c.History.Entries()
		c.cursor = len(c.ring)
	}
	c.keepSlot()
	snap := c.snap()
	c.search = &historySearch{query: field.New(""), at: len(c.ring), saved: snap.value, savedCursor: snap.cursor}
	c.comp = nil
}

// Whether a reverse search runs
func (c *Composer) Searching() bool { return c.search != nil }

// Whether ring entry i matches the search, its text holding the query and its session the bound one while the search keeps to it
func (c *Composer) matches(i int) bool {
	s := c.search
	if i < 0 || i >= len(c.ring) {
		return false
	}
	e := c.ring[i]
	if s.mine && e.Session != c.SessionID {
		return false
	}
	return strings.Contains(strings.ToLower(e.Text), strings.ToLower(s.query.Value()))
}

// Puts the newest match at or before index i in view, keeping the one in view when nothing older matches
func (c *Composer) searchFrom(i int) {
	for ; i >= 0; i-- {
		if c.matches(i) {
			c.search.at = i
			c.revision++
			c.ta.SetValue(c.slotText(i))
			c.ta.MoveToEnd()
			return
		}
	}
}

// Takes typed or pasted text into the query, the search narrowing from the match in view and else from the newest entry
func (c *Composer) searchTyped(msg tea.Msg) {
	if !c.search.query.Take(msg) {
		return
	}
	c.searchFrom(min(c.search.at, len(c.ring)-1))
	if !c.matches(c.search.at) {
		c.searchFrom(len(c.ring) - 1)
	}
}

// Takes an action while a search runs: the search key steps older, the completion key narrows it to the bound session or widens it again, editing
// keys change the query, escape puts the prompt back, enter and any other key keep the match for editing; reports whether the action was used up and
// the command it needs
func (c *Composer) SearchKey(id string) (tea.Cmd, bool) {
	s := c.search
	if s == nil {
		return nil, false
	}
	switch {
	case id == "composer.history.search":
		c.searchFrom(s.at - 1)
	case id == "complete.start":
		s.mine = !s.mine
		c.searchFrom(len(c.ring) - 1)
	case strings.HasPrefix(id, "text."):
		before := s.query.Value()
		cmd, used := s.query.Action(id)
		if s.query.Value() != before {
			c.searchFrom(len(c.ring) - 1)
		}
		if used {
			return cmd, true
		}
		c.keepMatch()
		return nil, false
	case id == "session.stop":
		c.search = nil
		c.forget()
		c.revision++
		c.restore(composerSnap{value: s.saved, cursor: s.savedCursor})
		c.comp, c.quiet = nil, s.saved
	default:
		c.keepMatch()
		return nil, id == "composer.send"
	}
	return nil, true
}

// Ends the search with the match in view as the slot being edited, the draft past the newest entry when nothing matched
func (c *Composer) keepMatch() {
	s := c.search
	c.search = nil
	if s.at < len(c.ring) {
		c.cursor = s.at
		if c.OnRecall != nil {
			c.OnRecall(c.ring[s.at])
		}
	}
	c.comp, c.quiet = nil, c.ta.Value()
}

// Columns the label of the search line takes
func (c *Composer) searchLabelWidth() int {
	return ansi.StringWidth(c.searchLabel()) + 1
}

// Label of the search line, its scope and whether the entry in view matches the query
func (c *Composer) searchLabel() string {
	s := c.search
	key := txt.ComposerHistorySearch
	if s.mine {
		key = txt.KitComposerHistorySearchMine
	}
	if s.query.Value() != "" && !c.matches(s.at) {
		key = txt.ComposerHistorySearchNone
	}
	return lang.T(key)
}

// Line the composer shows while a search runs, the label and the query
func (c *Composer) searchLine(w int) string {
	label := c.searchLabel()
	query, _ := c.search.query.Line(max(w-c.searchLabelWidth(), 1))
	return render.Fit(lipgloss.NewStyle().Foreground(c.theme.Accent).Render(label+" ")+query, w, c.theme)
}
