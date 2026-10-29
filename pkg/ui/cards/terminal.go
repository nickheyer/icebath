package cards

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/shell"
	"github.com/nickheyer/icebath/pkg/term"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Pause after output arrives before the window draws it, so bursts coalesce into one frame
const termFrame = 8 * time.Millisecond

// Window for a second or third click to extend a selection to the word or the row
const clickSpan = 400 * time.Millisecond

// Columns and rows a terminal starts with before its window has a size
const (
	termCols = 80
	termRows = 24
)

// Window running a shell on a pseudo terminal, every key going to it while the window keys still reach the manager
type Terminal struct {
	env   *Env
	id    int
	sess  *term.Session
	err   error
	dir   string
	title string
	// Rows scrolled back into the scrollback, zero for the live screen
	scroll int
	// Rows ever scrolled out as of the last frame, so the view keeps its place while output flows
	scrolled  uint64
	sel       *term.Selection
	drag      bool
	clicks    int
	clickAt   time.Time
	clickPos  term.Pos
	focused   bool
	hostFocus bool
	locked    bool
	searching bool
	query     string
	matches   []term.Match
	current   int
	spans     map[uint64][]term.Span
	exit      *term.Exit
	starting  bool
	closed    bool
	// Line to type once the shell is up again
	pending string
	paste   bool
	w, h    int
	// Last frame painted, shown again while the program holds a synchronized update
	frame *uv.Buffer
	// Theme the emulator answers color queries with
	themed string
}

// Output arrived on the session of a terminal
type termWakeMsg struct {
	t *Terminal
	s *term.Session
}

func (m termWakeMsg) Addressee() wm.Card { return m.t }

// The program of a terminal ended
type termExitMsg struct {
	t    *Terminal
	s    *term.Session
	exit term.Exit
}

func (m termExitMsg) Addressee() wm.Card { return m.t }

// A terminal started its program, or failed to
type termStartMsg struct {
	t   *Terminal
	s   *term.Session
	err error
}

func (m termStartMsg) Addressee() wm.Card { return m.t }

// Builds terminal number id starting its shell in dir, the agent shell directory when empty
func NewTerminal(env *Env, id int, dir string) *Terminal {
	return &Terminal{env: env, id: id, dir: dir}
}

func (t *Terminal) Title() string {
	if t.title != "" {
		return render.Truncate(t.title, 40, t.env.Theme)
	}
	return "TERMINAL " + strconv.Itoa(t.id)
}

func (t *Terminal) Glyph() string       { return t.env.Theme.Glyphs.Terminal }
func (t *Terminal) Key() string         { return "terminal:" + strconv.Itoa(t.id) }
func (t *Terminal) ID() int             { return t.id }
func (t *Terminal) MinSize() (int, int) { return 20, 5 }
func (t *Terminal) Selectable() bool    { return false }
func (t *Terminal) Tracks() bool        { return true }

// Takes every key as typed while the shell runs and no search bar is open
func (t *Terminal) Raw() bool { return t.live() && !t.searching }

// Lets keys with the alt, super, hyper or meta modifier reach the window manager unless the keys are locked
func (t *Terminal) Passes(k tea.KeyPressMsg) bool {
	return !t.locked && k.Mod&(tea.ModAlt|tea.ModSuper|tea.ModHyper|tea.ModMeta) != 0
}

// Keymap contexts, the search bar ahead of the terminal keys while it is open
func (t *Terminal) Contexts() []string {
	if t.searching {
		return []string{keys.Search, keys.Terminal}
	}
	return []string{keys.Terminal}
}

// Reports whether the shell is running
func (t *Terminal) live() bool { return t.sess != nil && t.exit == nil && t.err == nil }

// Starts the shell
func (t *Terminal) Init() tea.Cmd { return t.start() }

// Directory the shell runs in, the saved one while it is not up
type terminalState struct {
	Dir string `json:"dir,omitempty"`
}

// Directory of the shell for the next run
func (t *Terminal) State() any { return terminalState{Dir: t.Cwd()} }

// Starts the next shell in the saved directory when it still exists
func (t *Terminal) SetState(raw json.RawMessage) error {
	var st terminalState
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	if info, err := os.Stat(st.Dir); err == nil && info.IsDir() {
		t.dir = st.Dir
	}
	return nil
}

// Working directory of the shell, what it announced, else what the kernel reports, else where it started
func (t *Terminal) Cwd() string {
	if t.sess != nil {
		if dir := t.sess.Cwd(); dir != "" {
			return dir
		}
	}
	return t.startDir()
}

// Directory a fresh shell starts in
func (t *Terminal) startDir() string {
	if t.dir != "" {
		return t.dir
	}
	if t.env.Shell != nil {
		return t.env.Shell.Dir()
	}
	return t.env.Root
}

// Command the configured shell runs, the login shell when none is configured
func (t *Terminal) command() term.Command {
	cmd := term.DefaultShell()
	if line := t.setting("terminal_shell"); line != "" {
		if words, err := shell.Words(line); err == nil && len(words) > 0 {
			cmd = term.Command{Path: words[0], Args: words[1:]}
		}
	}
	cmd.Dir = t.startDir()
	cmd.Env = append(term.Environ(os.Environ(), t.env.Version), "ICEBATH=1")
	return cmd
}

// Value of a setting as the app holds it
func (t *Terminal) setting(key string) string {
	if t.env.Settings == nil {
		return ""
	}
	return t.env.Settings()[key]
}

// Rows of scrollback the settings ask for
func (t *Terminal) scrollback() int {
	if n, err := strconv.Atoi(t.setting("terminal_scrollback")); err == nil && n >= 0 {
		return n
	}
	return harness.DefaultScrollback
}

// Starts the shell off the event loop at the size the window has
func (t *Terminal) start() tea.Cmd {
	if t.starting {
		return nil
	}
	t.starting = true
	cmd := t.command()
	th := t.env.Theme
	opts := term.Options{Scrollback: t.scrollback(), Fg: th.Text, Bg: th.Surface, Cursor: th.Ice, Version: "icebath(" + t.env.Version + ")"}
	cols, rows := t.w, t.h
	if cols <= 0 || rows <= 0 {
		cols, rows = termCols, termRows
	}
	return func() tea.Msg {
		s, err := term.Open(cmd, cols, rows, opts)
		return termStartMsg{t: t, s: s, err: err}
	}
}

// Ends the shell off the event loop and starts it again in its directory
func (t *Terminal) restart() tea.Cmd {
	if t.starting {
		return nil
	}
	old := t.sess
	if old != nil {
		t.dir = t.Cwd()
	}
	t.sess, t.exit, t.err = nil, nil, nil
	t.sel, t.scroll, t.scrolled = nil, 0, 0
	t.title = ""
	start := t.start()
	return func() tea.Msg {
		if old != nil {
			old.Close()
		}
		return start()
	}
}

// Waits for output or the end of the program off the event loop
func (t *Terminal) wait(s *term.Session) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-s.Wake():
			time.Sleep(termFrame)
			return termWakeMsg{t: t, s: s}
		case <-s.Done():
			return termExitMsg{t: t, s: s, exit: s.Exit()}
		}
	}
}

// Ends the shell and waits for it, for a program shutting down
func (t *Terminal) Close() {
	t.closed = true
	if t.sess != nil {
		t.sess.Close()
	}
}

// Types a line into the shell, starting it again first when it has ended
func (t *Terminal) Run(line string) tea.Cmd {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	if !t.live() {
		t.pending = line
		return t.restart()
	}
	t.sess.Input([]byte(line + "\r"))
	t.scroll = 0
	return nil
}

func (t *Terminal) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case termStartMsg:
		return t, t.started(msg)
	case termWakeMsg:
		if msg.s != t.sess {
			return t, nil
		}
		return t, tea.Batch(t.drain(), t.wait(msg.s))
	case termExitMsg:
		if msg.s != t.sess {
			return t, nil
		}
		exit := msg.exit
		t.exit = &exit
		t.sel, t.drag = nil, false
		return t, t.drain()
	case wm.FocusMsg:
		t.focused = msg.Focused
		if t.live() {
			t.sess.Input(t.sess.Emu.Focus(msg.Focused && t.hostFocus))
		}
		return t, nil
	case tea.FocusMsg, tea.BlurMsg:
		_, t.hostFocus = msg.(tea.FocusMsg)
		if t.live() && t.focused {
			t.sess.Input(t.sess.Emu.Focus(t.hostFocus))
		}
		return t, nil
	case wm.ClosedMsg:
		t.closed = true
		if s := t.sess; s != nil {
			go s.Close()
		}
		return t, nil
	case wm.ActionMsg:
		return t, t.act(msg.ID)
	case SettingChangedMsg:
		if msg.Key == "terminal_scrollback" && t.sess != nil {
			t.sess.Emu.SetScrollback(t.scrollback())
			t.scroll = min(t.scroll, t.sess.Emu.ScrollbackLen())
		}
		return t, nil
	case tea.KeyPressMsg:
		return t, t.key(msg)
	case tea.KeyReleaseMsg:
		if t.live() && !t.searching {
			t.sess.Input(t.sess.Emu.Key(keyOf(tea.Key(msg), true)))
		}
		return t, nil
	case tea.PasteMsg:
		return t, t.pasteText(msg.Content)
	case tea.ClipboardMsg:
		if !t.paste {
			return t, nil
		}
		t.paste = false
		return t, t.pasteText(msg.Content)
	case tea.MouseClickMsg:
		return t, t.click(msg)
	case tea.MouseMotionMsg:
		return t, t.motion(msg)
	case tea.MouseReleaseMsg:
		return t, t.release(msg)
	case tea.MouseWheelMsg:
		return t, t.wheel(msg)
	}
	return t, nil
}

// Adopts the started session, typing the line waiting for it, or keeps the error that stopped it
func (t *Terminal) started(msg termStartMsg) tea.Cmd {
	t.starting = false
	if msg.err != nil {
		t.err = msg.err
		return wm.Notify("terminal: "+msg.err.Error(), wm.Error)
	}
	if t.closed {
		go msg.s.Close()
		return nil
	}
	t.sess, t.exit, t.err = msg.s, nil, nil
	t.scrolled = msg.s.Emu.Scrolled()
	if t.w > 0 && t.h > 0 {
		msg.s.Resize(t.w, t.h)
	}
	if t.pending != "" {
		msg.s.Input([]byte(t.pending + "\r"))
		t.pending = ""
	}
	if t.focused {
		msg.s.Input(msg.s.Emu.Focus(t.hostFocus))
	}
	return t.wait(msg.s)
}

// Takes the events the program raised and keeps the view in place as rows scroll out
func (t *Terminal) drain() tea.Cmd {
	if t.sess == nil {
		return nil
	}
	e := t.sess.Emu
	scrolled := e.Scrolled()
	if t.scroll > 0 && scrolled > t.scrolled {
		t.scroll = min(t.scroll+int(scrolled-t.scrolled), e.ScrollbackLen())
	}
	t.scrolled = scrolled
	if e.AltScreen() {
		t.scroll = 0
	}
	if t.query != "" {
		t.find(false)
	}
	var cmds []tea.Cmd
	for _, ev := range e.Events() {
		switch ev.Kind {
		case term.EventBell:
			if !t.focused || !t.hostFocus {
				cmds = append(cmds, wm.RequestAttention(t, wm.Pulse))
			}
		case term.EventTitle:
			t.title = ev.Text
		case term.EventClipboard:
			if strings.ContainsAny(ev.Body, "cs") || ev.Body == "" {
				cmds = append(cmds, tea.SetClipboard(ev.Text))
			}
			if strings.Contains(ev.Body, "p") {
				cmds = append(cmds, tea.SetPrimaryClipboard(ev.Text))
			}
		case term.EventNotify:
			text := ev.Text
			if ev.Body != "" {
				text += ": " + ev.Body
			}
			cmds = append(cmds, wm.Notify(render.Truncate(text, 80, t.env.Theme), wm.Info))
			if !t.focused {
				cmds = append(cmds, wm.RequestAttention(t, wm.Pulse))
			}
		}
	}
	return tea.Batch(cmds...)
}

// Key event as the emulator encodes it
func keyOf(k tea.Key, release bool) term.Key {
	return term.Key{Code: k.Code, Shifted: k.ShiftedCode, Base: k.BaseCode, Text: k.Text, Mod: uv.KeyMod(k.Mod), Repeat: k.IsRepeat, Release: release}
}

// Sends a key to the shell, types into the search bar, or starts the shell again with enter once it ended
func (t *Terminal) key(msg tea.KeyPressMsg) tea.Cmd {
	if t.searching {
		if msg.Text != "" {
			t.query += msg.Text
			t.find(true)
		}
		return nil
	}
	if !t.live() {
		if msg.Code == tea.KeyEnter && !t.starting {
			return t.restart()
		}
		return nil
	}
	b := t.sess.Emu.Key(keyOf(tea.Key(msg), false))
	if len(b) == 0 {
		return nil
	}
	t.sess.Input(b)
	t.scroll = 0
	t.sel = nil
	return nil
}

// Hands pasted text to the shell, bracketed when it asked for that
func (t *Terminal) pasteText(text string) tea.Cmd {
	if !t.live() || text == "" {
		return nil
	}
	t.sess.Input(t.sess.Emu.Paste(text))
	t.scroll = 0
	return nil
}

// Carries out a keymap action
func (t *Terminal) act(id string) tea.Cmd {
	if strings.HasPrefix(id, "search.") {
		return t.searchAction(id)
	}
	switch id {
	case "terminal.copy":
		return t.copy()
	case "terminal.paste":
		t.paste = true
		return tea.ReadClipboard
	case "terminal.search":
		t.searching = !t.searching
		if t.searching {
			t.find(true)
		}
	case "terminal.scroll.up":
		t.scrollBy(max(t.h-1, 1))
	case "terminal.scroll.down":
		t.scrollBy(-max(t.h-1, 1))
	case "terminal.scroll.top":
		if t.sess != nil {
			t.scroll = t.sess.Emu.ScrollbackLen()
		}
	case "terminal.scroll.bottom":
		t.scroll = 0
	case "terminal.prompt.prev":
		return t.jumpPrompt(-1)
	case "terminal.prompt.next":
		return t.jumpPrompt(1)
	case "terminal.select.all":
		t.selectAll()
	case "terminal.lock":
		t.locked = !t.locked
		if t.locked {
			return wm.Notify("keys locked to the shell, "+t.env.keyed("terminal.lock", "unlocks"), wm.Info)
		}
		return wm.Notify("window keys unlocked", wm.Info)
	case "terminal.clear":
		if t.sess != nil {
			t.sess.Emu.ClearScrollback()
			t.scroll, t.sel = 0, nil
			t.matches, t.spans = nil, nil
		}
	case "terminal.restart":
		return t.restart()
	}
	return nil
}

// Moves the view by n rows into the scrollback, negative toward the live screen
func (t *Terminal) scrollBy(n int) {
	if t.sess == nil || t.sess.Emu.AltScreen() {
		t.scroll = 0
		return
	}
	t.scroll = min(max(t.scroll+n, 0), t.sess.Emu.ScrollbackLen())
}

// Absolute row of the top of the view
func (t *Terminal) top() uint64 {
	return t.sess.Emu.Base() - uint64(t.scroll)
}

// Scrolls so that the row sits at the top of the view
func (t *Terminal) show(row uint64) {
	e := t.sess.Emu
	base := e.Base()
	switch {
	case row >= base:
		t.scroll = 0
	default:
		t.scroll = min(int(base-row), e.ScrollbackLen())
	}
}

// Scrolls to the previous or next shell prompt
func (t *Terminal) jumpPrompt(d int) tea.Cmd {
	if t.sess == nil {
		return nil
	}
	row, ok := t.sess.Emu.Prompt(t.top(), d)
	if !ok {
		return wm.Notify("no prompt that way, shells mark prompts with OSC 133", wm.Info)
	}
	t.show(row)
	return nil
}

// Selects every row kept
func (t *Terminal) selectAll() {
	if t.sess == nil {
		return
	}
	e := t.sess.Emu
	first := e.Base() - uint64(e.ScrollbackLen())
	last := e.Base() + uint64(t.h-1)
	t.sel = &term.Selection{Anchor: term.Pos{Row: first, Col: 0}, Head: term.Pos{Row: last, Col: max(t.w-1, 0)}, Mode: term.SelLines}
}

// Copies the selection, or the screen when nothing is selected
func (t *Terminal) copy() tea.Cmd {
	if t.sess == nil {
		return nil
	}
	e := t.sess.Emu
	sel := t.sel
	if sel == nil {
		base := e.Base()
		sel = &term.Selection{Anchor: term.Pos{Row: base}, Head: term.Pos{Row: base + uint64(t.h-1), Col: max(t.w-1, 0)}, Mode: term.SelLines}
	}
	text := e.Text(*sel)
	if strings.TrimSpace(text) == "" {
		return wm.Notify("nothing to copy", wm.Info)
	}
	n := strings.Count(text, "\n") + 1
	return tea.Batch(tea.SetClipboard(text), wm.Notify("copied "+render.Count(n, "line"), wm.Success))
}

// Position of a content cell in absolute rows
func (t *Terminal) posAt(x, y int) term.Pos {
	return term.Pos{Row: t.top() + uint64(max(y, 0)), Col: max(x, 0)}
}

// Pointer event as the emulator encodes it
func mouseOf(mo tea.Mouse, kind term.MouseKind) term.Mouse {
	return term.Mouse{X: mo.X, Y: mo.Y, Button: mo.Button, Mod: uv.KeyMod(mo.Mod), Kind: kind}
}

// Hands a pointer event to the program when it tracks the mouse, reporting whether it took it
func (t *Terminal) track(mo tea.Mouse, kind term.MouseKind) bool {
	if !t.live() || t.scroll > 0 {
		return false
	}
	b, ok := t.sess.Emu.Mouse(mouseOf(mo, kind))
	if ok {
		t.sess.Input(b)
	}
	return ok
}

// Starts a selection, extends it to the word or row on repeated clicks, or pastes on the right button
func (t *Terminal) click(msg tea.MouseClickMsg) tea.Cmd {
	mo := tea.Mouse(msg)
	if t.sess == nil {
		return nil
	}
	if t.track(mo, term.MousePress) {
		return nil
	}
	switch msg.Button {
	case tea.MouseRight:
		t.paste = true
		return tea.ReadClipboard
	case tea.MouseLeft:
	default:
		return nil
	}
	pos := t.posAt(mo.X, mo.Y)
	now := time.Now()
	if now.Sub(t.clickAt) < clickSpan && pos == t.clickPos {
		t.clicks++
	} else {
		t.clicks = 1
	}
	t.clickAt, t.clickPos = now, pos
	mode := term.SelChars
	switch {
	case mo.Mod&tea.ModAlt != 0:
		mode = term.SelBlock
	case t.clicks == 2:
		mode = term.SelWords
	case t.clicks >= 3:
		mode = term.SelLines
	}
	t.sel = &term.Selection{Anchor: pos, Head: pos, Mode: mode}
	t.drag = true
	t.extend(pos)
	return nil
}

// Moves the head of the selection to the position, the ends snapping to words or rows in those modes
func (t *Terminal) extend(pos term.Pos) {
	if t.sel == nil {
		return
	}
	e := t.sess.Emu
	s := t.sel
	s.Head = pos
	switch s.Mode {
	case term.SelWords:
		af, at := e.WordAt(t.clickPos)
		hf, ht := e.WordAt(pos)
		if pos.Row < t.clickPos.Row || (pos.Row == t.clickPos.Row && pos.Col < t.clickPos.Col) {
			s.Anchor, s.Head = at, hf
		} else {
			s.Anchor, s.Head = af, ht
		}
	case term.SelLines:
		af, at := e.LineAt(t.clickPos)
		hf, ht := e.LineAt(pos)
		if pos.Row < t.clickPos.Row {
			s.Anchor, s.Head = at, hf
		} else {
			s.Anchor, s.Head = af, ht
		}
	}
}

// Extends the selection being dragged or reports motion to the program
func (t *Terminal) motion(msg tea.MouseMotionMsg) tea.Cmd {
	mo := tea.Mouse(msg)
	if t.drag && mo.Button == tea.MouseLeft {
		t.extend(t.posAt(mo.X, mo.Y))
		return nil
	}
	t.track(mo, term.MouseMotion)
	return nil
}

// Ends a drag, dropping an empty selection and copying a real one to the primary clipboard
func (t *Terminal) release(msg tea.MouseReleaseMsg) tea.Cmd {
	mo := tea.Mouse(msg)
	if !t.drag {
		t.track(mo, term.MouseRelease)
		return nil
	}
	t.drag = false
	if t.sel != nil && t.sel.Anchor == t.sel.Head && t.sel.Mode == term.SelChars {
		t.sel = nil
		return nil
	}
	if t.sel != nil {
		if text := t.sess.Emu.Text(*t.sel); strings.TrimSpace(text) != "" {
			return tea.SetPrimaryClipboard(text)
		}
	}
	return nil
}

// Scrolls the view, or the program when it tracks the mouse or scrolls the alternate screen with arrows
func (t *Terminal) wheel(msg tea.MouseWheelMsg) tea.Cmd {
	mo := tea.Mouse(msg)
	if t.sess == nil {
		return nil
	}
	if t.track(mo, term.MouseWheel) {
		return nil
	}
	up := msg.Button == tea.MouseWheelUp
	if !up && msg.Button != tea.MouseWheelDown {
		return nil
	}
	if t.live() && t.sess.Emu.AltScroll() {
		code := rune(tea.KeyDown)
		if up {
			code = tea.KeyUp
		}
		var b []byte
		for i := 0; i < wheelRows; i++ {
			b = append(b, t.sess.Emu.Key(term.Key{Code: code})...)
		}
		t.sess.Input(b)
		return nil
	}
	if up {
		t.scrollBy(wheelRows)
	} else {
		t.scrollBy(-wheelRows)
	}
	return nil
}

// Carries out a search bar action
func (t *Terminal) searchAction(id string) tea.Cmd {
	switch id {
	case "search.accept":
		t.searching = false
	case "search.close":
		t.searching = false
		t.query, t.matches, t.spans = "", nil, nil
	case "search.next":
		t.step(1)
	case "search.prev":
		t.step(-1)
	case "search.delete.back":
		if r := []rune(t.query); len(r) > 0 {
			t.query = string(r[:len(r)-1])
			t.find(true)
		}
	case "search.clear":
		t.query = ""
		t.find(true)
	case "search.delete.word":
		t.query = strings.TrimRight(t.query, " ")
		if i := strings.LastIndex(t.query, " "); i >= 0 {
			t.query = t.query[:i+1]
		} else {
			t.query = ""
		}
		t.find(true)
	}
	return nil
}

// Finds the query again, moving to the match nearest the view when asked
func (t *Terminal) find(move bool) {
	if t.sess == nil || t.query == "" {
		t.matches, t.spans = nil, nil
		return
	}
	t.matches = t.sess.Emu.Find(t.query)
	t.spans = make(map[uint64][]term.Span, len(t.matches))
	for _, m := range t.matches {
		t.spans[m.Row] = append(t.spans[m.Row], term.Span{From: m.From, To: m.To})
	}
	if len(t.matches) == 0 {
		t.current = 0
		return
	}
	if move {
		top := t.top()
		t.current = len(t.matches) - 1
		for i := len(t.matches) - 1; i >= 0; i-- {
			if t.matches[i].Row <= top+uint64(t.h-1) {
				t.current = i
				break
			}
		}
		t.reveal()
	}
	t.current = min(t.current, len(t.matches)-1)
}

// Moves to the next or previous match, wrapping around
func (t *Terminal) step(d int) {
	n := len(t.matches)
	if n == 0 {
		return
	}
	t.current = ((t.current+d)%n + n) % n
	t.reveal()
}

// Scrolls the least that brings the current match into view
func (t *Terminal) reveal() {
	if t.current >= len(t.matches) {
		return
	}
	row := t.matches[t.current].Row
	top := t.top()
	if row >= top && row < top+uint64(t.h) {
		return
	}
	t.show(row - uint64(min(int(row-(t.sess.Emu.Base()-uint64(t.sess.Emu.ScrollbackLen()))), t.h/2)))
}

// Resizes the program to the window and forgets what the old width placed
func (t *Terminal) fit(w, h int) {
	if w == t.w && h == t.h {
		return
	}
	t.w, t.h = w, h
	if t.sess == nil || w <= 0 || h <= 0 {
		return
	}
	t.sess.Resize(w, h)
	e := t.sess.Emu
	t.sel, t.drag = nil, false
	t.scroll = min(t.scroll, e.ScrollbackLen())
	t.scrolled = e.Scrolled()
	if t.query != "" {
		t.find(false)
	}
}

// Paints the screen with the search, selection, scrollbar and exit banner over it
func (t *Terminal) Paint(buf *uv.Buffer) {
	w, h := buf.Width(), buf.Height()
	t.fit(w, h)
	th := t.env.Theme
	if t.sess == nil {
		t.paintText(buf, h/2, t.startText())
		return
	}
	e := t.sess.Emu
	if th.Name != t.themed {
		e.SetDefaults(th.Text, th.Surface, th.Ice)
		t.themed = th.Name
	}
	if e.Synchronized() && t.frame != nil && t.frame.Width() == w && t.frame.Height() == h {
		copyBuffer(buf, t.frame)
		return
	}
	var current *term.Match
	if t.query != "" && t.current < len(t.matches) {
		m := t.matches[t.current]
		current = &m
	}
	e.Draw(buf, term.View{
		Scroll: t.scroll, Colors: th.Terminal, Fg: th.Text, Bg: th.Surface,
		SelFg: th.Bg, SelBg: th.Ice, Selection: t.sel,
		Matches: t.spans, MatchFg: th.Text, MatchBg: th.SteelBright, Current: current, CurFg: th.Bg, CurBg: th.Accent,
	})
	if t.scroll > 0 {
		t.paintScrollbar(buf)
	}
	if t.exit != nil || t.err != nil {
		t.paintText(buf, h-1, t.exitText())
	}
	if t.searching {
		t.paintSearch(buf)
	}
	if t.frame == nil || t.frame.Width() != w || t.frame.Height() != h {
		t.frame = uv.NewBuffer(w, h)
	}
	copyBuffer(t.frame, buf)
}

// Cell buffer measured by grapheme width, as the window manager draws it
type cells struct{ *uv.Buffer }

func (cells) WidthMethod() uv.WidthMethod { return uv.WidthMethod(ansi.GraphemeWidth) }

// Copies every cell of src into dst, both the same size
func copyBuffer(dst, src *uv.Buffer) {
	for y := range src.Lines {
		copy(dst.Lines[y], src.Lines[y])
	}
}

// Text shown before the shell is up
func (t *Terminal) startText() string {
	th := t.env.Theme
	if t.err != nil {
		return lipgloss.NewStyle().Foreground(th.Alarm).Render(th.Glyphs.Fail+" "+t.err.Error()) + lipgloss.NewStyle().Foreground(th.TextDim).Render(", enter starts it again")
	}
	return render.Dim("starting "+filepath.Base(t.command().Path), th)
}

// Banner naming how the shell ended and how to start it again
func (t *Terminal) exitText() string {
	th := t.env.Theme
	base := lipgloss.NewStyle().Background(th.Steel)
	var state string
	switch {
	case t.err != nil:
		state = base.Foreground(th.Alarm).Render(" " + th.Glyphs.Fail + " " + t.err.Error() + " ")
	case t.exit.Signal != "":
		state = base.Foreground(th.Alarm).Render(" " + th.Glyphs.Fail + " " + t.exit.Signal + " ")
	case t.exit.Code != 0:
		state = base.Foreground(th.Alarm).Render(fmt.Sprintf(" %s exit %d ", th.Glyphs.Fail, t.exit.Code))
	default:
		state = base.Foreground(th.Alive).Render(" " + th.Glyphs.Success + " exit 0 ")
	}
	hint := "enter"
	if k := t.env.key("terminal.restart"); k != "" {
		hint += " or " + k
	}
	return state + base.Foreground(th.TextDim).Render(" "+hint+" starts the shell again ")
}

// Writes styled text on one row of the buffer, from the left edge
func (t *Terminal) paintText(buf *uv.Buffer, y int, text string) {
	if y < 0 || y >= buf.Height() {
		return
	}
	uv.NewStyledString(ansi.Truncate(text, buf.Width(), t.env.Theme.Glyphs.Ellipsis)).Draw(cells{buf}, uv.Rect(0, y, buf.Width(), 1))
}

// Draws the search bar on the bottom row with the query and the match count
func (t *Terminal) paintSearch(buf *uv.Buffer) {
	th := t.env.Theme
	w := buf.Width()
	base := lipgloss.NewStyle().Background(th.Steel)
	count := "no match"
	if n := len(t.matches); n > 0 {
		count = fmt.Sprintf("%d/%d", t.current+1, n)
	}
	right := base.Foreground(th.TextDim).Render(" " + count + " ")
	room := max(w-ansi.StringWidth(right)-4, 1)
	query := t.query
	if ansi.StringWidth(query) > room {
		query = ansi.TruncateLeft(query, ansi.StringWidth(query)-room, "")
	}
	left := base.Foreground(th.Accent).Render(" "+th.Glyphs.Search+" ") + base.Foreground(th.Text).Render(query)
	pad := max(w-ansi.StringWidth(left)-ansi.StringWidth(right), 0)
	line := left + base.Render(strings.Repeat(" ", pad)) + right
	uv.NewStyledString(ansi.Truncate(line, w, "")).Draw(cells{buf}, uv.Rect(0, buf.Height()-1, w, 1))
}

// Draws a scrollbar in the last column showing where the view sits in the scrollback
func (t *Terminal) paintScrollbar(buf *uv.Buffer) {
	th := t.env.Theme
	w, h := buf.Width(), buf.Height()
	if w < 2 || h < 2 {
		return
	}
	e := t.sess.Emu
	total := e.ScrollbackLen() + h
	size := max(h*h/total, 1)
	start := (e.ScrollbackLen() - t.scroll) * (h - size) / max(e.ScrollbackLen(), 1)
	track := uv.Cell{Content: th.Glyphs.Track, Width: 1, Style: uv.Style{Fg: th.Steel}}
	thumb := uv.Cell{Content: th.Glyphs.Heavy, Width: 1, Style: uv.Style{Fg: th.Ice}}
	for y := 0; y < h; y++ {
		c := track
		if y >= start && y < start+size {
			c = thumb
		}
		if under := buf.Lines[y][w-1]; under.Style.Bg != nil {
			c.Style.Bg = under.Style.Bg
		}
		buf.Lines[y][w-1] = c
	}
}

// Screen as a string, for frames composed from text
func (t *Terminal) View(w, h int) string {
	buf := uv.NewBuffer(w, h)
	t.Paint(buf)
	return buf.Render()
}

// Cursor of the shell, or of the search bar while it is open, none while scrolled back or ended
func (t *Terminal) Cursor() *tea.Cursor {
	if t.searching {
		return tea.NewCursor(min(3+ansi.StringWidth(t.query), max(t.w-1, 0)), max(t.h-1, 0))
	}
	if !t.live() || t.scroll > 0 {
		return nil
	}
	c := t.sess.Emu.Cursor()
	if !c.Visible || c.Y >= t.h || c.X >= t.w {
		return nil
	}
	out := tea.NewCursor(c.X, c.Y)
	out.Blink = c.Blink
	out.Color = t.env.Theme.Ice
	if c.Color != nil {
		out.Color = c.Color
	}
	switch c.Shape {
	case term.CursorUnderline:
		out.Shape = tea.CursorUnderline
	case term.CursorBar:
		out.Shape = tea.CursorBar
	}
	return out
}

// Directory, scroll position, lock and how the shell ended
func (t *Terminal) Status() string {
	th := t.env.Theme
	var parts []string
	if dir := t.Cwd(); dir != "" {
		parts = append(parts, homed(dir))
	}
	if t.sess != nil {
		if state, value := t.sess.Emu.Progress(); state != 0 {
			parts = append(parts, strconv.Itoa(value)+"%")
		}
	}
	if t.scroll > 0 {
		parts = append(parts, th.Glyphs.Up+" "+strconv.Itoa(t.scroll))
	}
	if t.locked {
		parts = append(parts, "locked")
	}
	switch {
	case t.err != nil:
		parts = append(parts, "failed")
	case t.exit != nil:
		parts = append(parts, "exit "+strconv.Itoa(t.exit.Code))
	}
	return strings.Join(parts, " "+th.Glyphs.Sep+" ")
}

// Actions worth a reminder
func (t *Terminal) Hints() []wm.Hint {
	switch {
	case t.searching:
		return []wm.Hint{{Action: "search.accept"}, {Action: "search.next", Pair: "search.prev", Label: "matches"}, {Action: "search.close"}}
	case !t.live():
		return []wm.Hint{{Action: "terminal.restart"}, {Action: "terminal.copy"}}
	}
	return []wm.Hint{{Action: "terminal.copy"}, {Action: "terminal.paste"}, {Action: "terminal.search"}, {Action: "terminal.scroll.up", Pair: "terminal.scroll.down", Label: "scroll"}, {Action: "terminal.lock"}}
}
