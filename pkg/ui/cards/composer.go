package cards

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/protocol"
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

// Slash command the composer completes and hands to the app
type Command struct {
	Name string
	Desc string
	// Takes an argument, shown after the name
	Arg string
	// Values the argument may take, offered as the argument is typed
	Suggest func(prefix string) []string
	// Keymap action that does the same, its key shown beside the command
	Action string
}

// One file attached to the next message
type attachment struct {
	Path string
	Part protocol.Part
	Size int
}

// Candidates offered for the token under the cursor
type completion struct {
	items []string
	descs []string
	acts  []string
	idx   int
	start int
	end   int
	slash bool
	arg   bool
}

// Words the empty prompt shows
const placeholderText = "Type / for available commands"

// Clickable send button on the first input row, in composer coordinates
type sendButton struct {
	x, y, w int
}

// Multi line prompt with history, slash commands listed as typed, path completion, attachments and a saved draft
type Composer struct {
	ta          textarea.Model
	focused     bool
	history     []string
	cursor      int
	draft       string
	comp        *completion
	quiet       string
	attachments []attachment
	draftPath   string
	historyPath string
	commands    []Command
	last        string
	baseDir     func() string
	theme       theme.Theme
	keys        *keys.Map
	busy        bool
	button      sendButton
	cut         int
	w, h        int
	// Session the input belongs to, stamped on every message sent
	SessionID string
}

// Builds a composer bound to the keymap that keeps its draft and history under dir and completes paths against base
func NewComposer(t theme.Theme, km *keys.Map, dir string, base func() string) *Composer {
	ta := textarea.New()
	ta.Prompt = "› "
	ta.Placeholder = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.DynamicHeight = true
	ta.MinHeight = minInputRows
	ta.MaxHeight = maxInputRows
	ta.SetHeight(minInputRows)
	ta.SetVirtualCursor(false)
	c := &Composer{ta: ta, baseDir: base, keys: km}
	c.Rebind()
	c.SetTheme(t)
	if dir != "" {
		c.draftPath = filepath.Join(dir, "draft.txt")
		c.historyPath = filepath.Join(dir, "history.txt")
		if data, err := os.ReadFile(c.draftPath); err == nil {
			c.ta.SetValue(string(data))
		}
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

// Rebuilds the editing bindings of the input from the keymap, every other key only typing text
func (c *Composer) Rebind() {
	bind := func(id string) key.Binding {
		var spelled []string
		for _, chord := range c.keys.Keys(id) {
			spelled = append(spelled, keys.Press(chord).String())
		}
		if len(spelled) == 0 {
			return key.Binding{}
		}
		return key.NewBinding(key.WithKeys(spelled...))
	}
	c.ta.KeyMap = textarea.KeyMap{
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
	c.ta.SetStyles(st)
	c.theme = t
}

// Replaces the slash commands offered as they are typed
func (c *Composer) SetCommands(cmds []Command) { c.commands = cmds }

// Tells the send button whether a run is going, so it offers to steer it
func (c *Composer) SetBusy(on bool) { c.busy = on }

// Text typed so far
func (c *Composer) Value() string { return c.ta.Value() }

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

// Rows the composer wants, two at least, growing with the wrapped text and the completion popup
func (c *Composer) Height() int {
	rows := max(c.ta.Height(), minInputRows)
	if c.comp != nil {
		rows += min(len(c.comp.items), popupRows)
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
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(msg)
		c.autoComplete()
		return tea.Batch(cmd, c.persist())
	case tea.KeyPressMsg:
		if msg.Text == "" {
			return nil
		}
		c.comp = nil
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(msg)
		c.autoComplete()
		return tea.Batch(cmd, c.persist())
	}
	return nil
}

// Carries out a keymap action of the prompt or its completion popup
func (c *Composer) Action(id string) tea.Cmd {
	if c.comp != nil {
		switch id {
		case "complete.next":
			c.comp.idx = (c.comp.idx + 1) % len(c.comp.items)
			return nil
		case "complete.prev":
			c.comp.idx = (c.comp.idx - 1 + len(c.comp.items)) % len(c.comp.items)
			return nil
		case "complete.accept":
			return c.pick()
		case "complete.close":
			c.quiet = c.ta.Value()
			c.comp = nil
			return nil
		}
	}
	switch id {
	case "composer.send":
		if c.escaped() {
			c.breakLine()
			return c.persist()
		}
		return c.submit()
	case "composer.complete":
		c.complete()
		return nil
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
	c.comp = nil
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(press)
	c.autoComplete()
	return tea.Batch(cmd, c.persist())
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

// Writes the draft to disk when the text changed
func (c *Composer) persist() tea.Cmd {
	v := c.ta.Value()
	if v == c.last {
		return nil
	}
	c.last = v
	if c.draftPath == "" {
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

func (c *Composer) remember(text string) {
	if n := len(c.history); n > 0 && c.history[n-1] == text {
		c.cursor = len(c.history)
		return
	}
	c.history = append(c.history, text)
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
	c.ta.Reset()
	c.comp = nil
	c.quiet = ""
	cmd := c.persist()
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
	parts := make([]protocol.Part, 0, len(c.attachments)+1)
	for _, a := range c.attachments {
		parts = append(parts, a.Part)
	}
	if text != "" {
		parts = append(parts, protocol.Text{Text: text})
	}
	c.attachments = nil
	id := c.SessionID
	return tea.Batch(cmd, func() tea.Msg { return SubmitMsg{Session: id, Parts: parts, Text: text} })
}

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// Reads an image file into an attachment for the next message
func (c *Composer) attach(path string) tea.Cmd {
	path = strings.TrimSpace(path)
	if path == "" {
		return wm.Notify("attach needs a path", wm.Warn)
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
		return wm.Notify("only png, jpeg, gif and webp images attach", wm.Warn)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	c.attachments = append(c.attachments, attachment{Path: path, Part: protocol.Image{MediaType: media, Data: data}, Size: len(data)})
	return wm.Notify("attached "+filepath.Base(path)+" ("+render.Bytes(int64(len(data)))+")", wm.Success)
}

// Attaches the image at the path, as the attach command does
func (c *Composer) Attach(path string) tea.Cmd { return c.attach(path) }

// Directory relative paths complete against
func (c *Composer) base() string {
	if c.baseDir != nil {
		if dir := c.baseDir(); dir != "" {
			return dir
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// First line of the text and the cursor column inside it, -1 when the cursor is on a later line
func (c *Composer) firstLine() (string, int) {
	line, _, _ := strings.Cut(c.ta.Value(), "\n")
	if c.ta.Line() != 0 {
		return line, -1
	}
	return line, min(c.ta.Column(), len([]rune(line)))
}

// Command named by the first word of a slash line
func (c *Composer) command(name string) *Command {
	for i := range c.commands {
		if c.commands[i].Name == strings.ToLower(name) {
			return &c.commands[i]
		}
	}
	return nil
}

// Opens the popup over the commands or argument values a slash line matches, closing it anywhere else
func (c *Composer) autoComplete() {
	line, col := c.firstLine()
	text := c.ta.Value()
	if col < 0 || !strings.HasPrefix(line, "/") || text == c.quiet {
		if c.comp != nil && (c.comp.slash || c.comp.arg) {
			c.comp = nil
		}
		return
	}
	c.quiet = ""
	runes := []rune(line)
	typed := string(runes[1:col])
	name, rest, hasSpace := strings.Cut(typed, " ")
	if !hasSpace {
		var items, descs, acts []string
		for _, cmd := range c.commands {
			if strings.HasPrefix(cmd.Name, strings.ToLower(name)) {
				items = append(items, "/"+cmd.Name)
				descs = append(descs, cmd.Desc)
				acts = append(acts, cmd.Action)
			}
		}
		c.setPopup(items, descs, 0, col, true, false)
		if c.comp != nil {
			c.comp.acts = acts
		}
		return
	}
	cmd := c.command(name)
	if cmd == nil || cmd.Suggest == nil {
		c.comp = nil
		return
	}
	start := 1 + len([]rune(name)) + 1
	var items []string
	for _, v := range cmd.Suggest(rest) {
		if strings.HasPrefix(strings.ToLower(v), strings.ToLower(rest)) {
			items = append(items, v)
		}
	}
	c.setPopup(items, nil, start, col, false, true)
}

func (c *Composer) setPopup(items, descs []string, start, end int, slash, arg bool) {
	if len(items) == 0 {
		c.comp = nil
		return
	}
	idx := 0
	if c.comp != nil && c.comp.idx < len(items) && (c.comp.slash == slash && c.comp.arg == arg) {
		idx = c.comp.idx
	}
	c.comp = &completion{items: items, descs: descs, idx: idx, start: start, end: end, slash: slash, arg: arg}
}

// Takes the popup choice, sending a finished command at once and otherwise leaving the cursor after it
func (c *Composer) pick() tea.Cmd {
	comp := c.comp
	c.accept()
	cmd := c.persist()
	if comp == nil {
		return cmd
	}
	if comp.arg {
		return tea.Batch(cmd, c.submit())
	}
	if comp.slash {
		line, _ := c.firstLine()
		name := strings.TrimPrefix(strings.TrimSpace(line), "/")
		if known := c.command(name); known != nil && known.Arg == "" {
			return tea.Batch(cmd, c.submit())
		}
		c.autoComplete()
	}
	return cmd
}

// Token the cursor sits in or just after, on the current line, with its offsets inside the line
func (c *Composer) token() (line string, start, end int) {
	lines := strings.Split(c.ta.Value(), "\n")
	row := min(c.ta.Line(), len(lines)-1)
	line = lines[row]
	col := min(c.ta.Column(), len([]rune(line)))
	runes := []rune(line)
	start = col
	for start > 0 && runes[start-1] != ' ' {
		start--
	}
	end = col
	return line, start, end
}

// Offers paths for the token under the cursor, taking the only candidate at once
func (c *Composer) complete() {
	line, start, end := c.token()
	runes := []rune(line)
	prefix := string(runes[start:end])
	if c.ta.Line() == 0 && strings.HasPrefix(line, "/") {
		c.autoComplete()
		if c.comp != nil && len(c.comp.items) == 1 {
			c.accept()
			c.autoComplete()
		}
		return
	}
	items := c.paths(prefix)
	if len(items) == 0 {
		c.comp = nil
		return
	}
	c.comp = &completion{items: items, start: start, end: end}
	if len(items) == 1 {
		c.accept()
	}
}

// Paths starting with the prefix, directories trailing a slash, sorted
func (c *Composer) paths(prefix string) []string {
	pattern := prefix
	if strings.HasPrefix(pattern, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			pattern = filepath.Join(home, pattern[2:])
		}
	}
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(c.base(), pattern)
		if strings.HasSuffix(prefix, "/") || prefix == "" {
			pattern += string(filepath.Separator)
		}
	}
	matches, err := filepath.Glob(pattern + "*")
	if err != nil {
		return nil
	}
	base := c.base()
	var items []string
	for _, m := range matches {
		item := m
		if !filepath.IsAbs(prefix) && !strings.HasPrefix(prefix, "~") {
			if rel, err := filepath.Rel(base, m); err == nil {
				item = rel
			}
		}
		if st, err := os.Stat(m); err == nil && st.IsDir() {
			item += "/"
		}
		items = append(items, item)
	}
	sort.Strings(items)
	if len(items) > 50 {
		items = items[:50]
	}
	return items
}

// Replaces the token with the chosen candidate
func (c *Composer) accept() {
	if c.comp == nil {
		return
	}
	item := c.comp.items[c.comp.idx]
	lines := strings.Split(c.ta.Value(), "\n")
	row := 0
	if !c.comp.slash && !c.comp.arg {
		row = min(c.ta.Line(), len(lines)-1)
	}
	runes := []rune(lines[row])
	end := min(c.comp.end, len(runes))
	tail := string(runes[end:])
	replaced := string(runes[:min(c.comp.start, len(runes))]) + item
	if c.comp.slash || (!strings.HasSuffix(item, "/") && tail == "") {
		replaced += " "
	}
	lines[row] = replaced + tail
	c.ta.SetValue(strings.Join(lines, "\n"))
	c.ta.MoveToEnd()
	more := !c.comp.slash && !c.comp.arg && strings.HasSuffix(item, "/")
	c.comp = nil
	if more {
		c.complete()
	}
}

// Send button as a symbol alone, an arrow that sends or a hook that steers a run, dim while empty
func (c *Composer) buttonLabel() string {
	t := c.theme
	glyph := "➤"
	if c.busy {
		glyph = "↪"
	}
	if t.Plain {
		glyph = ">"
		if c.busy {
			glyph = "~"
		}
		return "[ " + glyph + " ]"
	}
	text := "  " + glyph + "  "
	lit := t.Ice
	if c.busy {
		lit = t.Accent
	}
	if c.Empty() {
		return lipgloss.NewStyle().Foreground(lit).Background(t.Ghost).Render(text)
	}
	return lipgloss.NewStyle().Foreground(t.Bg).Background(lit).Bold(true).Render(text)
}

// Prompt line shown while the input is empty, the prompt mark before the muted placeholder
func (c *Composer) placeholder(w int) string {
	t := c.theme
	prompt := t.TextDim
	if c.focused {
		prompt = t.Accent
	}
	return render.Fit(lipgloss.NewStyle().Foreground(prompt).Render(c.ta.Prompt)+lipgloss.NewStyle().Foreground(t.TextDim).Render(placeholderText), w)
}

// Fits the input beside the send button at width w, wrapping the text there so the height reads true
func (c *Composer) Fit(w int) int {
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
	c.w, c.h = w, h
	popup := c.popup(w)
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
	}
	for i := range input {
		input[i] = render.Fit(input[i], iw)
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
			label = render.Fit(item, 14) + " " + render.Dim(c.comp.descs[i], t)
		}
		bound := ""
		if i < len(c.comp.acts) && c.comp.acts[i] != "" {
			bound = keys.Format(c.keys.First(c.comp.acts[i]))
		}
		st := lipgloss.NewStyle().Foreground(t.Text)
		key := lipgloss.NewStyle().Foreground(t.Accent)
		if i == c.comp.idx {
			st = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
			key = st
			label = ansi.Strip(label)
		}
		kw := ansi.StringWidth(bound)
		if bound == "" || kw+16 > w {
			rows = append(rows, st.Render(render.Fit(" "+label, w)))
			continue
		}
		rows = append(rows, st.Render(render.Fit(" "+label, w-kw-1))+key.Render(bound)+st.Render(" "))
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
	return cur
}

// Short trailer naming the history position and attachments
func (c *Composer) Status() string {
	var parts []string
	if n := len(c.attachments); n > 0 {
		parts = append(parts, itoa(n)+" attached")
	}
	if c.cursor < len(c.history) {
		parts = append(parts, "history "+itoa(c.cursor+1)+"/"+itoa(len(c.history)))
	}
	return strings.Join(parts, " · ")
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
