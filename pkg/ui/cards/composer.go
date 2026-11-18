package cards

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Lines of history the composer remembers
const historyLimit = 200

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

// Candidates the app offers for the prompt, the highlighted one and the offset they replace from
type completion struct {
	items   []string
	descs   []string
	texts   []string
	idx     int
	replace int
}

// Candidate an app completion source offers, its text put in from the replace offset
type Offer struct {
	Text    string
	Display string
	Meta    string
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

// Multi line prompt with history, the app's completion candidates, attachments and a draft kept per session
type Composer struct {
	ta          textarea.Model
	focused     bool
	history     []string
	cursor      int
	draft       string
	comp        *completion
	quiet       string
	attachments []attachment
	dir         string
	draftPath   string
	historyPath string
	last        string
	baseDir     func() string
	theme       theme.Theme
	keys        *keys.Map
	busy        bool
	button      sendButton
	cut         int
	ask         *asking
	width       int
	undo, redo  []composerSnap
	SessionID   string
	// Reports whether nothing typed may reach disk, the draft and history kept in memory alone
	Volatile func() bool
	// Hands every sent line to the app as typed, commands and shell lines included
	Literal bool
	// Text an empty prompt shows in place of the command and palette legend
	Hint string
	// Called with each line the history takes, for an app keeping its own history file
	OnRemember func(text string)
	// Puts the highlighted offer in from its replace offset when the popup is accepted
	Apply func(text string, replace int) tea.Cmd
}

// File the draft of a session is kept in under dir
func DraftPath(dir, sessionID string) string {
	return filepath.Join(dir, "drafts", sessionID+".txt")
}

// Builds a composer on the keymap keeping history and bound drafts under dir, completing paths against base
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
	c := &Composer{ta: ta, baseDir: base, keys: km, dir: dir}
	c.Rebind()
	c.SetTheme(t)
	if dir != "" {
		c.historyPath = filepath.Join(dir, "history.txt")
		if data, err := os.ReadFile(c.historyPath); err == nil {
			for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n\x1e\n") {
				if line != "" {
					c.history = append(c.history, line)
				}
			}
		}
	}
	c.cursor = len(c.history)
	c.last = c.ta.Value()
	return c
}

// Binds the prompt to a session, text already typed moving to it and otherwise its kept draft coming back
func (c *Composer) Bind(sessionID string) {
	c.SessionID = sessionID
	if c.dir == "" {
		return
	}
	old := c.draftPath
	c.draftPath = DraftPath(c.dir, sessionID)
	if old == c.draftPath {
		return
	}
	if c.ta.Value() != "" {
		if old != "" {
			os.Remove(old)
		}
		c.last = ""
		c.persist()
		return
	}
	data, err := os.ReadFile(c.draftPath)
	if err != nil {
		return
	}
	c.ta.SetValue(string(data))
	c.last = c.ta.Value()
}

// Rebuilds the editing bindings of the input from the keymap, every other key only typing text
func (c *Composer) Rebind() { c.ta.KeyMap = editingKeys(c.keys) }

// Text editing bindings every multi line input takes from the composer actions of the keymap
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
		CharacterBackward:       bind("composer.left"),
		CharacterForward:        bind("composer.right"),
		WordBackward:            bind("composer.word.left"),
		WordForward:             bind("composer.word.right"),
		LinePrevious:            bind("composer.up"),
		LineNext:                bind("composer.down"),
		LineStart:               bind("composer.line.start"),
		LineEnd:                 bind("composer.line.end"),
		DeleteCharacterBackward: bind("composer.delete.back"),
		DeleteCharacterForward:  bind("composer.delete.forward"),
		DeleteWordBackward:      bind("composer.delete.word.back"),
		DeleteWordForward:       bind("composer.delete.word.forward"),
		DeleteBeforeCursor:      bind("composer.delete.line.before"),
		DeleteAfterCursor:       bind("composer.delete.line.after"),
		SelectAll:               bind("composer.select.all"),
		SelectCharacterBackward: bind("composer.select.left"),
		SelectCharacterForward:  bind("composer.select.right"),
		SelectWordBackward:      bind("composer.select.word.left"),
		SelectWordForward:       bind("composer.select.word.right"),
		SelectLineUp:            bind("composer.select.up"),
		SelectLineDown:          bind("composer.select.down"),
		CopySelection:           bind("composer.copy"),
		Paste:                   bind("composer.paste"),
	}
}

// Paints the input with the theme
func (c *Composer) SetTheme(t theme.Theme) {
	c.ta.SetStyles(editorStyles(t))
	c.theme = t
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

// Replaces the text with the cursor at its end, the completion popup closed until the app offers again
func (c *Composer) SetValue(text string) tea.Cmd {
	c.forget()
	c.ta.SetValue(text)
	c.ta.MoveToEnd()
	c.comp = nil
	return c.persist()
}

// Inserts text at the cursor as if typed
func (c *Composer) Insert(text string) tea.Cmd {
	before := c.snap()
	c.ta.InsertString(text)
	c.tracked(before)
	return c.persist()
}

// Replaces the remembered lines, oldest first, for an app that keeps the history file itself
func (c *Composer) Seed(lines []string) {
	if len(lines) > historyLimit {
		lines = lines[len(lines)-historyLimit:]
	}
	c.history = append([]string(nil), lines...)
	c.cursor = len(c.history)
	c.historyPath = ""
}

// Position in the remembered lines while recalling, false at the unsent draft
func (c *Composer) Recalling() (int, bool) { return c.cursor, c.cursor < len(c.history) }

// Lines the composer remembers, oldest first
func (c *Composer) History() []string { return append([]string(nil), c.history...) }

// Files attached to the next message
func (c *Composer) Attachments() []string {
	out := make([]string, 0, len(c.attachments))
	for _, a := range c.attachments {
		out = append(out, a.Path)
	}
	return out
}

// Candidates shown in the popup, nil when it is closed
func (c *Composer) Completions() []string {
	if c.comp == nil {
		return nil
	}
	return append([]string(nil), c.comp.items...)
}

// Reports whether the completion popup is open
func (c *Composer) Completing() bool { return c.comp != nil }

// Shows the app's candidates for the prompt as it stands, the first one highlighted, none closing the popup
func (c *Composer) Offer(offers []Offer, replace int) {
	if len(offers) == 0 || c.ta.Value() == c.quiet {
		c.comp = nil
		return
	}
	c.quiet = ""
	comp := &completion{replace: replace}
	for _, o := range offers {
		label := o.Display
		if label == "" {
			label = o.Text
		}
		comp.items = append(comp.items, label)
		comp.descs = append(comp.descs, o.Meta)
		comp.texts = append(comp.texts, o.Text)
	}
	c.comp = comp
}

// Rows the composer wants, two at least, growing with the wrapped text, the question waiting and the completion popup
func (c *Composer) Height() int {
	rows := max(c.ta.Height(), minInputRows)
	if c.comp != nil {
		rows += min(len(c.comp.items), popupRows)
	}
	if c.ask != nil {
		rows += len(c.ask.layout(c.width, c.theme))
	}
	return rows
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
		before := c.snap()
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(msg)
		c.tracked(before)
		return tea.Batch(cmd, c.persist())
	case tea.KeyPressMsg:
		if msg.Text == "" {
			return nil
		}
		if cmd, ok := c.questionDigit(msg.Text); ok {
			return cmd
		}
		before := c.snap()
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(msg)
		c.tracked(before)
		if c.ask != nil && strings.TrimSpace(c.ta.Value()) != "" {
			c.ask.idx = c.ask.other()
		}
		return tea.Batch(cmd, c.persist())
	}
	return nil
}

// Carries out a keymap action of the prompt, the question waiting in it or its completion popup
func (c *Composer) Action(id string) tea.Cmd {
	if cmd, ok := c.questionAction(id); ok {
		return cmd
	}
	if c.comp != nil {
		switch id {
		case "complete.next":
			c.comp.idx = (c.comp.idx + 1) % len(c.comp.items)
			return nil
		case "complete.prev":
			c.comp.idx = (c.comp.idx - 1 + len(c.comp.items)) % len(c.comp.items)
			return nil
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
	case "composer.send":
		if c.escaped() {
			before := c.snap()
			c.breakLine()
			c.tracked(before)
			return c.persist()
		}
		return c.submit()
	case "composer.undo", "composer.redo":
		if id == "composer.undo" {
			c.swap(&c.undo, &c.redo)
		} else {
			c.swap(&c.redo, &c.undo)
		}
		return c.persist()
	case "composer.up":
		if c.ta.Line() == 0 {
			c.recall(-1)
			return nil
		}
	case "composer.down":
		if c.ta.Line() == c.ta.LineCount()-1 {
			c.recall(1)
			return nil
		}
	case "composer.attachments.clear":
		c.attachments = nil
		return wm.Notify("attachments cleared", wm.Info)
	}
	if !strings.HasPrefix(id, "composer.") {
		return nil
	}
	press := keys.Press(c.keys.First(id))
	if press.Code == 0 {
		return nil
	}
	before := c.snap()
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(press)
	c.tracked(before)
	return tea.Batch(cmd, c.persist())
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

// Records the prompt before a changing edit on the undo trail and drops the redo trail
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

// Writes the draft of the bound session to disk when the text changed
func (c *Composer) persist() tea.Cmd {
	v := c.ta.Value()
	if v == c.last {
		return nil
	}
	c.last = v
	if c.draftPath == "" || c.ghost() {
		return nil
	}
	if v == "" {
		os.Remove(c.draftPath)
		return nil
	}
	os.MkdirAll(filepath.Dir(c.draftPath), 0o755)
	os.WriteFile(c.draftPath, []byte(v), 0o600)
	return nil
}

// Adds a sent prompt to the history and writes it out, keeping nothing while the session is ghost
func (c *Composer) remember(text string) {
	c.cursor = len(c.history)
	if c.ghost() {
		return
	}
	if n := len(c.history); n > 0 && c.history[n-1] == text {
		return
	}
	c.history = append(c.history, text)
	if c.OnRemember != nil {
		c.OnRemember(text)
	}
	if len(c.history) > historyLimit {
		c.history = c.history[len(c.history)-historyLimit:]
	}
	c.cursor = len(c.history)
	if c.historyPath != "" {
		os.MkdirAll(filepath.Dir(c.historyPath), 0o755)
		os.WriteFile(c.historyPath, []byte(strings.Join(c.history, "\n\x1e\n")+"\n"), 0o600)
	}
}

// Moves through history by d, keeping the unsent draft at the newest end
func (c *Composer) recall(d int) {
	if len(c.history) == 0 {
		return
	}
	if c.cursor == len(c.history) {
		c.draft = c.ta.Value()
	}
	next := min(max(c.cursor+d, 0), len(c.history))
	if next == c.cursor {
		return
	}
	c.cursor = next
	c.forget()
	if next == len(c.history) {
		c.ta.SetValue(c.draft)
	} else {
		c.ta.SetValue(c.history[next])
	}
	c.ta.MoveToEnd()
}

// Parses the text into a message or a command and resets the input
func (c *Composer) submit() tea.Cmd {
	text := strings.TrimSpace(c.ta.Value())
	if text == "" && len(c.attachments) == 0 {
		return nil
	}
	c.remember(text)
	c.forget()
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

// Send or steer button in capitals, dim while the input is empty and lit once there is something to send
func (c *Composer) buttonLabel() string {
	t := c.theme
	word := "send"
	if c.busy {
		word = "steer"
	}
	if t.Plain {
		return "[ " + word + " ]"
	}
	text := " " + strings.ToUpper(word) + " "
	lit := t.Ice
	if c.busy {
		lit = t.Accent
	}
	if c.Empty() {
		return lipgloss.NewStyle().Foreground(lit).Background(t.Ghost).Render(text)
	}
	return lipgloss.NewStyle().Foreground(t.Bg).Background(lit).Bold(true).Render(text)
}

// Empty prompt line, the mark then how to reach commands and the palette, all muted
func (c *Composer) placeholder(w int) string {
	t := c.theme
	prompt := t.TextDim
	if c.focused {
		prompt = t.Accent
	}
	if c.Hint != "" {
		line := lipgloss.NewStyle().Foreground(prompt).Render(c.ta.Prompt) + lipgloss.NewStyle().Foreground(t.TextDim).Render(c.Hint)
		return render.Fit(line, w, t)
	}
	hint := lipgloss.NewStyle().Foreground(t.TextDim).Render(`type `) + "/" + lipgloss.NewStyle().Foreground(t.TextDim).Render(` for commands`)
	if chord := c.keys.Display("palette"); chord != "" {
		hint += lipgloss.NewStyle().Foreground(t.TextDim).Render(` or `) + chord + lipgloss.NewStyle().Foreground(t.TextDim).Render(` for palette`)
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
	pop := c.popup(w)
	popup := append(c.askRows(w, max(h-minInputRows-len(pop), 1)), pop...)
	rows := max(h-len(popup), 1)
	button := c.buttonLabel()
	iw := c.Fit(w)
	c.button.y = len(popup)
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
		if c.ask != nil {
			input[0] = c.questionPlaceholder(iw)
		}
	}
	for i := range input {
		input[i] = render.Fit(input[i], iw, c.theme)
	}
	if c.button.w > 0 && len(input) > 0 {
		input[0] += " " + button
	}
	parts := make([]string, 0, h)
	parts = append(parts, popup...)
	parts = append(parts, input...)
	for len(parts) < h {
		parts = append(parts, "")
	}
	return strings.Join(parts[:h], "\n")
}

// Sends when the click lands on the send button, reporting whether it did
func (c *Composer) Click(x, y int) (tea.Cmd, bool) {
	if cmd, ok := c.questionClick(y); ok {
		return cmd, true
	}
	b := c.button
	if b.w == 0 || y != b.y || x < b.x || x >= b.x+b.w {
		return nil, false
	}
	return c.submit(), true
}

// Completion rows shown above the input
func (c *Composer) popup(w int) []string {
	if c.comp == nil {
		return nil
	}
	t := c.theme
	limit := min(len(c.comp.items), popupRows)
	from := 0
	if c.comp.idx >= limit {
		from = c.comp.idx - limit + 1
	}
	var rows []string
	for i := from; i < from+limit && i < len(c.comp.items); i++ {
		item := c.comp.items[i]
		label := item
		if i < len(c.comp.descs) && c.comp.descs[i] != "" {
			label = render.Fit(item, 14, t) + " " + render.Dim(c.comp.descs[i], t)
		}
		st := lipgloss.NewStyle().Foreground(t.Text)
		if i == c.comp.idx {
			st = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
			label = ansi.Strip(label)
		}
		rows = append(rows, st.Render(render.Fit(" "+label, w, t)))
	}
	return rows
}

// Cursor inside the input area, below any completion popup
func (c *Composer) Cursor() *tea.Cursor {
	if !c.focused {
		return nil
	}
	cur := c.ta.Cursor()
	if cur == nil {
		return nil
	}
	cur.Y -= c.cut
	if c.comp != nil {
		cur.Y += min(len(c.comp.items), popupRows)
	}
	if c.ask != nil {
		cur.Y += len(c.ask.shown)
	}
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
	if c.cursor < len(c.history) {
		parts = append(parts, "history "+itoa(c.cursor+1)+"/"+itoa(len(c.history)))
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
