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
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Lines of history the composer remembers
const historyLimit = 200

// Slash command the composer completes and hands to the app
type Command struct {
	Name string
	Desc string
	// Takes an argument, shown after the name
	Arg string
}

// Built in commands every composer offers
var DefaultCommands = []Command{
	{Name: "help", Desc: "show the keymap"},
	{Name: "model", Desc: "switch the model", Arg: "id"},
	{Name: "mode", Desc: "cycle or set the permission mode", Arg: "name"},
	{Name: "profile", Desc: "switch the endpoint profile", Arg: "name"},
	{Name: "attach", Desc: "attach an image to the next message", Arg: "path"},
	{Name: "mission", Desc: "start a mission with an objective", Arg: "objective"},
	{Name: "compact", Desc: "summarize the transcript now"},
	{Name: "clear", Desc: "start a fresh session"},
	{Name: "fork", Desc: "fork the session here"},
	{Name: "sessions", Desc: "open the session list"},
	{Name: "history", Desc: "fly through the session as moments"},
	{Name: "findings", Desc: "open the findings"},
	{Name: "config", Desc: "open the configuration"},
	{Name: "theme", Desc: "switch the palette", Arg: "name"},
	{Name: "cancel", Desc: "stop the run"},
	{Name: "interrupt", Desc: "stop after the current tool"},
	{Name: "export", Desc: "write the transcript as markdown", Arg: "path"},
	{Name: "quit", Desc: "leave icebath"},
}

// One file attached to the next message
type attachment struct {
	Path string
	Part protocol.Part
	Size int
}

// Candidates offered for the token under the cursor
type completion struct {
	items  []string
	idx    int
	start  int
	end    int
	prefix string
	slash  bool
}

// Multi line prompt with history, slash commands, path completion, attachments, a steer queue and a persisted draft
type Composer struct {
	env         *Env
	ta          textarea.Model
	focused     bool
	armed       bool
	history     []string
	cursor      int
	draft       string
	comp        *completion
	attachments []attachment
	draftPath   string
	historyPath string
	commands    []Command
	last        string
	w, h        int
	// Reports whether a run is active so the composer labels input as steering
	Busy func() bool
	// Names the model and permission mode for the indicator line
	Model    func() string
	ModeName func() string
}

var (
	sendHint    = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send"))
	newlineHint = key.NewBinding(key.WithKeys("alt+enter"), key.WithHelp("alt+enter", "newline"))
	releaseHint = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "release"))
	tabHint     = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "complete"))
	jackInHint  = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "jack in"))
)

// Builds a composer that keeps its draft and history under dir
func NewComposer(env *Env, dir string) *Composer {
	t := env.Theme
	ta := textarea.New()
	ta.Prompt = "› "
	ta.Placeholder = "jack in"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetVirtualCursor(false)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j", "shift+enter"))
	st := textarea.DefaultDarkStyles()
	if t.Name == "light" {
		st = textarea.DefaultLightStyles()
	}
	st.Focused.Prompt = st.Focused.Prompt.Foreground(t.Accent)
	st.Focused.Text = st.Focused.Text.Foreground(t.Text)
	st.Focused.Placeholder = st.Focused.Placeholder.Foreground(t.TextDim)
	st.Focused.CursorLine = lipgloss.NewStyle()
	st.Focused.Base = lipgloss.NewStyle()
	st.Focused.EndOfBuffer = lipgloss.NewStyle().Foreground(t.Surface)
	st.Blurred.Prompt = st.Blurred.Prompt.Foreground(t.TextDim)
	st.Blurred.Text = st.Blurred.Text.Foreground(t.TextDim)
	st.Blurred.Placeholder = st.Blurred.Placeholder.Foreground(t.TextDim)
	st.Blurred.Base = lipgloss.NewStyle()
	st.Blurred.EndOfBuffer = lipgloss.NewStyle().Foreground(t.Surface)
	ta.SetStyles(st)
	c := &Composer{env: env, ta: ta, commands: DefaultCommands, Busy: func() bool { return false }, Model: func() string { return "" }, ModeName: func() string { return "" }}
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

// Replaces the slash commands offered for completion
func (c *Composer) SetCommands(cmds []Command) { c.commands = cmds }

func (c *Composer) Title() string       { return "UPLINK" }
func (c *Composer) Glyph() string       { return "⌁" }
func (c *Composer) Key() string         { return "uplink" }
func (c *Composer) Init() tea.Cmd       { return nil }
func (c *Composer) Pinned() bool        { return true }
func (c *Composer) MinSize() (int, int) { return 20, 3 }
func (c *Composer) Capturing() bool     { return c.focused && c.armed }

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

// Keys for sending and releasing while armed, or the key that arms typing again
func (c *Composer) Hints() []key.Binding {
	if c.Capturing() {
		return []key.Binding{sendHint, newlineHint, tabHint, releaseHint}
	}
	return []key.Binding{jackInHint}
}

// Keys taken while focused but not yet armed
func (c *Composer) Claims() []string { return []string{"i"} }

func (c *Composer) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case deck.FocusMsg:
		c.focused = msg.Focused
		if !msg.Focused {
			c.comp = nil
		}
		return c, c.arm(msg.Focused)
	case tea.MouseClickMsg:
		if c.focused && !c.armed && msg.Button == tea.MouseLeft {
			return c, c.arm(true)
		}
		return c, nil
	case tea.PasteMsg:
		if !c.armed {
			return c, nil
		}
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(msg)
		return c, tea.Batch(cmd, c.persist())
	case tea.KeyPressMsg:
		return c.key(msg)
	}
	return c, nil
}

func (c *Composer) key(msg tea.KeyPressMsg) (deck.Card, tea.Cmd) {
	if !c.armed {
		switch msg.String() {
		case "i", "enter":
			return c, c.arm(true)
		}
		return c, nil
	}
	if c.comp != nil {
		switch msg.String() {
		case "tab", "down":
			c.comp.idx = (c.comp.idx + 1) % len(c.comp.items)
			return c, nil
		case "shift+tab", "up":
			c.comp.idx = (c.comp.idx - 1 + len(c.comp.items)) % len(c.comp.items)
			return c, nil
		case "enter":
			c.accept()
			return c, c.persist()
		case "esc":
			c.comp = nil
			return c, nil
		}
	}
	switch msg.String() {
	case "esc":
		return c, c.arm(false)
	case "enter":
		return c, c.submit()
	case "tab":
		c.complete()
		return c, nil
	case "up":
		if c.ta.Line() == 0 {
			c.recall(-1)
			return c, nil
		}
	case "down":
		if c.ta.Line() == c.ta.LineCount()-1 {
			c.recall(1)
			return c, nil
		}
	case "ctrl+u":
		c.ta.Reset()
		return c, c.persist()
	case "ctrl+x":
		c.attachments = nil
		return c, deck.Notify("attachments cleared", deck.Info)
	}
	c.comp = nil
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	return c, tea.Batch(cmd, c.persist())
}

// Switches typing on or off and moves the input focus with it
func (c *Composer) arm(on bool) tea.Cmd {
	c.armed = on
	if on {
		return c.ta.Focus()
	}
	c.ta.Blur()
	c.comp = nil
	return nil
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
	cmd := c.persist()
	if strings.HasPrefix(text, "/") {
		name, args, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
		name = strings.ToLower(strings.TrimSpace(name))
		args = strings.TrimSpace(args)
		if name == "attach" {
			return tea.Batch(cmd, c.attach(args))
		}
		return tea.Batch(cmd, func() tea.Msg { return CommandMsg{Name: name, Args: args} })
	}
	parts := make([]protocol.Part, 0, len(c.attachments)+1)
	for _, a := range c.attachments {
		parts = append(parts, a.Part)
	}
	if text != "" {
		parts = append(parts, protocol.Text{Text: text})
	}
	c.attachments = nil
	return tea.Batch(cmd, func() tea.Msg { return SubmitMsg{Parts: parts, Text: text} })
}

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// Reads an image file into an attachment for the next message
func (c *Composer) attach(path string) tea.Cmd {
	path = strings.TrimSpace(path)
	if path == "" {
		return deck.Notify("attach needs a path", deck.Warn)
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.baseDir(), path)
	}
	media, ok := imageTypes[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return deck.Notify("only png, jpeg, gif and webp images attach", deck.Warn)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return deck.Notify(err.Error(), deck.Error)
	}
	c.attachments = append(c.attachments, attachment{Path: path, Part: protocol.Image{MediaType: media, Data: data}, Size: len(data)})
	return deck.Notify("attached "+filepath.Base(path)+" ("+render.Bytes(int64(len(data)))+")", deck.Success)
}

// Directory relative paths complete against
func (c *Composer) baseDir() string {
	if c.env != nil && c.env.Shell != nil {
		return c.env.Shell.Dir()
	}
	if c.env != nil && c.env.ProjectDir != "" {
		return c.env.ProjectDir
	}
	wd, _ := os.Getwd()
	return wd
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

// Offers slash commands at the start of the text or paths for the token under the cursor
func (c *Composer) complete() {
	line, start, end := c.token()
	runes := []rune(line)
	prefix := string(runes[start:end])
	var items []string
	slash := false
	if c.ta.Line() == 0 && start == 0 && strings.HasPrefix(prefix, "/") {
		slash = true
		want := strings.ToLower(strings.TrimPrefix(prefix, "/"))
		for _, cmd := range c.commands {
			if strings.HasPrefix(cmd.Name, want) {
				items = append(items, "/"+cmd.Name)
			}
		}
	} else {
		items = c.paths(prefix)
	}
	if len(items) == 0 {
		c.comp = nil
		return
	}
	c.comp = &completion{items: items, start: start, end: end, prefix: prefix, slash: slash}
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
		pattern = filepath.Join(c.baseDir(), pattern)
		if strings.HasSuffix(prefix, "/") || prefix == "" {
			pattern += string(filepath.Separator)
		}
	}
	matches, err := filepath.Glob(pattern + "*")
	if err != nil {
		return nil
	}
	base := c.baseDir()
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
	row := min(c.ta.Line(), len(lines)-1)
	runes := []rune(lines[row])
	tail := string(runes[c.comp.end:])
	replaced := string(runes[:c.comp.start]) + item
	if c.comp.slash || (!strings.HasSuffix(item, "/") && tail == "") {
		replaced += " "
	}
	lines[row] = replaced + tail
	c.ta.SetValue(strings.Join(lines, "\n"))
	c.ta.MoveToEnd()
	more := strings.HasSuffix(item, "/")
	c.comp = nil
	if more {
		c.complete()
	}
}

func (c *Composer) View(w, h int) string {
	c.w, c.h = w, h
	t := c.env.Theme
	indicator := c.indicator(w)
	popup := c.popup(w)
	rows := h - 1 - len(popup)
	if rows < 1 {
		rows = 1
	}
	c.ta.SetWidth(w)
	c.ta.SetHeight(rows)
	parts := make([]string, 0, h)
	parts = append(parts, popup...)
	parts = append(parts, strings.Split(c.ta.View(), "\n")...)
	for len(parts) < h-1 {
		parts = append(parts, "")
	}
	parts = parts[:h-1]
	parts = append(parts, lipgloss.NewStyle().Foreground(t.TextDim).Render(indicator))
	return strings.Join(parts, "\n")
}

// Line naming the mode, model, steer state and attachments
func (c *Composer) indicator(w int) string {
	t := c.env.Theme
	var segs []string
	if c.Busy != nil && c.Busy() {
		segs = append(segs, render.Hot("steer", t))
	}
	if m := c.ModeName(); m != "" {
		segs = append(segs, render.Cool(m, t))
	}
	if m := c.Model(); m != "" {
		segs = append(segs, m)
	}
	if n := len(c.attachments); n > 0 {
		segs = append(segs, render.Warm("⌦ "+itoa(n), t))
	}
	if c.cursor < len(c.history) {
		segs = append(segs, render.Dim("history "+itoa(c.cursor+1)+"/"+itoa(len(c.history)), t))
	}
	if len(segs) == 0 {
		return ""
	}
	return render.Truncate(strings.Join(segs, lipgloss.NewStyle().Foreground(t.Steel).Render(" · ")), w)
}

// Completion rows shown above the input
func (c *Composer) popup(w int) []string {
	if c.comp == nil {
		return nil
	}
	t := c.env.Theme
	limit := min(len(c.comp.items), 6)
	from := 0
	if c.comp.idx >= limit {
		from = c.comp.idx - limit + 1
	}
	var rows []string
	for i := from; i < from+limit && i < len(c.comp.items); i++ {
		item := c.comp.items[i]
		label := item
		if c.comp.slash {
			for _, cmd := range c.commands {
				if "/"+cmd.Name == item {
					label = render.Fit(item, 12) + " " + render.Dim(cmd.Desc, t)
				}
			}
		}
		st := lipgloss.NewStyle().Foreground(t.Text)
		if i == c.comp.idx {
			st = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
			label = ansi.Strip(label)
		}
		rows = append(rows, st.Render(render.Fit(" "+label, w)))
	}
	return rows
}

func (c *Composer) Cursor() *tea.Cursor {
	if !c.Capturing() {
		return nil
	}
	cur := c.ta.Cursor()
	if cur == nil {
		return nil
	}
	if c.comp != nil {
		cur.Y += min(len(c.comp.items), 6)
	}
	return cur
}

func (c *Composer) Status() string {
	switch {
	case c.Capturing() && c.Busy != nil && c.Busy():
		return "steering"
	case c.Capturing():
		return "live"
	case len(c.attachments) > 0:
		return itoa(len(c.attachments)) + " attached"
	}
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }
