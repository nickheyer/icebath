package app

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	slashv1 "github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	reconnect   = 3 * time.Second
	completions = 50
	titled      = 60
)

type chat struct {
	desk     *desk
	kit      *tui.Kit
	composer *cards.Composer
	markdown *render.Markdown
	shell    *cards.Terminal
	sheet    *approval
	commands []*slashv1.Command
	// Tools of the server by name, for the title, glyph and subject of each call
	tools map[string]*toolsv1.Tool
	key   string
	id    string
	// Profile the session runs on, the one in use when the chat was opened until the session is made
	profile  string
	title    string
	named    string
	starting bool
	entering bool
	replying bool
	unseen   bool
	stranded bool
	waiting  []string
	epoch    int
	stop     context.CancelFunc

	log      transcript
	folds    map[string]bool
	expanded bool
	thinking string
	rendered map[string][]string
	kept     map[string][]string
	width    int
	lines    []row
	rows     int
	shown    int
	top      int
	follow   bool
	prompt   int
}

var _ interface {
	wm.Card
	wm.Keyed
	wm.Glypher
	wm.Statuser
	wm.Cursorer
	wm.Contexter
	wm.Selectable
	wm.Sized
	wm.Kinded
	wm.Answerer
	wm.Grabber
	wm.SheetHolder
} = (*chat)(nil)

func newChat(desk *desk, key string, session *sessionsv1.Session, profile string) *chat {
	kit := desk.kit
	c := &chat{
		desk:     desk,
		kit:      kit,
		composer: cards.NewComposer(kit.Env.Theme, kit.Env.Keys, "", kit.Env.WorkDir),
		markdown: render.NewMarkdown(),
		key:      key,
		id:       session.GetId(),
		profile:  cmp.Or(session.GetProfile(), profile),
		title:    session.GetTitle(),
		folds:    map[string]bool{},
		follow:   true,
	}
	c.composer.SessionID = key
	c.composer.Literal = true
	c.composer.History = kit.History
	c.composer.Hint = c.hint()
	c.composer.OnStop = c.interrupt
	c.composer.Apply = c.take
	return c
}

// Text the empty prompt shows: what typing does, and the shell a leading ! runs a line in
func (c *chat) hint() string {
	return "Message the agent · / for commands · ! to run in " + filepath.Base(c.kit.Env.Shell().Path)
}

func (c *chat) Title() string { return cmp.Or(c.title, "New chat") }

// Profile the chat acts on: the one its session runs on now, empty for the one in use before it has a session
func (c *chat) on() string          { return cmp.Or(c.log.state.GetProfile(), c.profile) }
func (c *chat) Key() string         { return c.key }
func (c *chat) Kind() string        { return "Chat" }
func (c *chat) Glyph() string       { return c.kit.Env.Theme.Glyphs.Session }
func (c *chat) Selectable() bool    { return true }
func (c *chat) MinSize() (int, int) { return 40, 10 }

func (c *chat) Answers(action string) bool {
	return action != "composer.attachments.clear"
}

func (c *chat) Grabs(_, y int) bool {
	return y >= 0 && y < c.shown && c.top+y < len(c.lines) && c.lines[c.top+y].fold != ""
}

func (c *chat) Init() tea.Cmd {
	load := tui.Async(func() func() tea.Cmd {
		ctx := c.kit.Env.Ctx
		response, err := c.desk.slash.List(ctx, &slashv1.ListRequest{})
		listed, listErr := c.desk.tools.List(ctx, &toolsv1.ListRequest{})
		var thinking string
		if setting, err := c.desk.settings.Get(ctx, &configv1.GetRequest{Key: agentconfig.Thinking, Profile: c.on()}); err == nil {
			yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), &thinking)
		}
		return func() tea.Cmd {
			if err == nil {
				c.commands = response.GetCommands()
			}
			if listErr == nil {
				c.tools = map[string]*toolsv1.Tool{}
				for _, tool := range listed.GetTools() {
					c.tools[tool.GetName()] = tool
				}
			}
			if c.thinking == "" {
				c.thinking = thinking
			}
			return nil
		}
	})
	if c.id == "" {
		return tea.Batch(load, c.check())
	}
	return tea.Batch(load, c.watch())
}

func (c *chat) check() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := c.kit.Env.Ctx
		response, err := c.desk.providers.List(ctx, &providerv1.ListRequest{Profile: c.on()})
		stranded := err == nil && !slices.ContainsFunc(response.GetProviders(), func(provider *providerv1.Provider) bool {
			return provider.GetUsable() && provider.GetCredentialOrigin() != providerv1.Origin_ORIGIN_UNSPECIFIED
		})
		if stranded {
			var chosen string
			if setting, err := c.desk.settings.Get(ctx, &configv1.GetRequest{Key: agentconfig.Provider, Profile: c.on()}); err == nil {
				yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), &chosen)
			}
			stranded = chosen == ""
		}
		return func() tea.Cmd {
			c.stranded = stranded
			return nil
		}
	})
}

func (c *chat) Status() string {
	switch {
	case c.log.approval != nil:
		return "waiting for approval"
	case c.log.busy:
		return "working"
	}
	if status := c.composer.Status(); status != "" {
		return status
	}
	state := c.log.state
	status := state.GetProvider()
	if state.GetModel() != "" {
		status += "/" + state.GetModel()
	}
	if profile := c.on(); profile != "" {
		status = strings.TrimSpace("@" + profile + " " + status)
	}
	if state.GetReasoningEffort() != "" {
		status += " " + state.GetReasoningEffort()
	}
	if state.GetYolo() {
		status += " yolo"
	}
	return status
}

func (c *chat) Contexts() []string {
	if c.composer.Completing() {
		return []string{keys.Complete, keys.Session, keys.Text}
	}
	return []string{keys.Session, keys.Text}
}

func (c *chat) Cursor() *tea.Cursor {
	cursor := c.composer.Cursor()
	if cursor != nil {
		cursor.Y += c.prompt
	}
	return cursor
}

func (c *chat) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.ClosedMsg:
		c.epoch++
		if c.stop != nil {
			c.stop()
		}
		c.desk.closed(c)
	case wm.FocusMsg:
		if msg.Focused {
			c.unseen = false
		}
		if msg.Focused && c.stranded {
			return c, tea.Batch(c.composer.SetFocus(true), c.check())
		}
		return c, c.composer.SetFocus(msg.Focused)
	case wm.ActionMsg:
		return c, c.action(msg.ID)
	case tea.KeyPressMsg, tea.PasteMsg:
		cmd := c.composer.Update(msg)
		c.complete()
		return c, cmd
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			c.scroll(-3)
		case tea.MouseWheelDown:
			c.scroll(3)
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && c.Grabs(msg.X, msg.Y) {
			id := c.lines[c.top+msg.Y].fold
			c.folds[id] = !c.open(id, c.unfolded(id))
			c.follow = false
		}
	case tui.ThemeMsg:
		c.composer.SetTheme(c.kit.Env.Theme)
		c.rendered = nil
	case cards.SettingChangedMsg:
		switch msg.Key {
		case cards.TerminalShell:
			if !c.replying {
				c.composer.Hint = c.hint()
			}
		case agentconfig.Thinking:
			var thinking string
			if yaml.Unmarshal([]byte(msg.Value), &thinking) == nil && thinking != "" {
				c.thinking = thinking
				c.refold(":thinking")
			}
		}
	case cards.SubmitMsg:
		if msg.Session == c.key {
			return c, c.submit(msg.Text)
		}
	}
	return c, nil
}

func (c *chat) unfolded(id string) bool {
	if strings.HasSuffix(id, ":thinking") {
		return c.thinking == agentconfig.ThinkingOpen
	}
	return c.expanded
}

func (c *chat) refold(suffixes ...string) {
	for id := range c.folds {
		for _, suffix := range suffixes {
			if strings.HasSuffix(id, suffix) {
				delete(c.folds, id)
			}
		}
	}
}

func (c *chat) action(id string) tea.Cmd {
	page := max(c.shown-2, 1)
	switch id {
	case "session.scroll.up":
		c.scroll(-page)
	case "session.scroll.down":
		c.scroll(page)
	case "session.scroll.top":
		c.top, c.follow = 0, false
	case "session.scroll.end":
		c.follow = true
	case "session.expand", "session.collapse":
		c.expanded = id == "session.expand"
		c.refold(":call", ":result")
	case "session.thinking":
		if c.thinking == agentconfig.ThinkingOpen {
			c.thinking = agentconfig.ThinkingFolded
		} else {
			c.thinking = agentconfig.ThinkingOpen
		}
		c.refold(":thinking")
	case "session.stop":
		switch {
		case c.composer.Searching() || c.composer.Completing():
			return c.composer.Action(id)
		case c.replying && c.composer.Empty():
			c.replying, c.composer.Hint = false, c.hint()
			return c.ask()
		case c.log.busy && !c.replying:
			return c.interrupt()
		}
		c.follow = true
	case "complete.start":
		c.paths()
	case "composer.send":
		c.entering = true
		before := c.composer.Value()
		cmd := c.composer.Action(id)
		c.entering = false
		if now := c.composer.Value(); now != before && now != "" {
			c.complete()
		}
		return cmd
	default:
		before := c.composer.Value()
		cmd := c.composer.Action(id)
		if c.composer.Value() != before {
			c.complete()
		}
		return cmd
	}
	return nil
}

func (c *chat) take(text string, replace int) tea.Cmd {
	typed := []rune(c.composer.Value())
	set := c.composer.SetValue(string(typed[:min(replace, len(typed))]) + text)
	if command, ok := c.command(text); c.entering && replace == 0 && ok && !strings.Contains(command.GetUsage(), "<") {
		return tea.Batch(set, c.composer.Send())
	}
	return set
}

func (c *chat) complete() {
	value := c.composer.Value()
	typed, typing := strings.CutPrefix(value, "/")
	if !typing || strings.Contains(typed, "\n") {
		return
	}
	if strings.Contains(typed, " ") {
		if command, ok := c.command(value); ok && command.GetUsage() != "" {
			c.composer.Offer(value, []cards.Offer{{Display: "/" + command.GetName() + " " + command.GetUsage(), Meta: command.GetHelp()}}, 0)
		}
		return
	}
	var offers []cards.Offer
	for _, command := range c.catalog() {
		name := command.GetName()
		if !strings.HasPrefix(name, typed) {
			for _, alias := range command.GetAliases() {
				if strings.HasPrefix(alias, typed) {
					name = alias
					break
				}
			}
		}
		if strings.HasPrefix(name, typed) {
			offers = append(offers, cards.Offer{Text: "/" + name + " ", Display: "/" + name, Meta: command.GetHelp()})
		}
	}
	c.composer.Offer(value, offers, 0)
}

func (c *chat) paths() {
	value := c.composer.Value()
	start := strings.LastIndexAny(value, " \t\n") + 1
	word := value[start:]
	target := word
	if rest, home := strings.CutPrefix(word, "~/"); home {
		if directory, err := os.UserHomeDir(); err == nil {
			target = directory + string(filepath.Separator) + rest
		}
	} else if !filepath.IsAbs(word) {
		target = c.kit.Env.WorkDir() + string(filepath.Separator) + word
	}
	directory, prefix := filepath.Split(target)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	var offers []cards.Offer
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if entry.IsDir() {
			name += "/"
		}
		if offers = append(offers, cards.Offer{Text: word[:len(word)-len(prefix)] + name, Display: name}); len(offers) == completions {
			break
		}
	}
	c.composer.Offer(value, offers, utf8.RuneCountInString(value[:start]))
}

func (c *chat) scroll(rows int) {
	last := max(len(c.lines)-c.shown, 0)
	c.top = min(max(c.top+rows, 0), last)
	c.follow = c.top == last
}

func (c *chat) View(w, h int) string {
	c.composer.Fit(w)
	c.composer.SetBusy(c.log.busy)
	prompt := min(c.composer.Height(), max(h-3, 1))
	c.rows = max(h-prompt-1, 1)
	c.prompt = c.rows + 1
	c.shown = c.rows
	if c.sheet != nil {
		c.shown = max(c.rows-len(c.sheet.lines(max(w-4, 1)))-2, 1)
	}
	if w != c.width {
		c.width, c.rendered = w, nil
	}
	c.kept = map[string][]string{}
	if c.lines = c.paint(w); len(c.lines) == 0 && c.stranded {
		c.lines = []row{{}, {text: render.Dim("No provider connected. Type /providers to connect one.", c.kit.Env.Theme)}}
	}
	c.rendered = c.kept
	last := max(len(c.lines)-c.shown, 0)
	if c.follow {
		c.top = last
	}
	c.top = min(c.top, last)
	body := make([]string, c.rows)
	for i := range c.shown {
		if c.top+i < len(c.lines) {
			body[i] = c.lines[c.top+i].text
		}
	}
	above := cards.OverlayPopup(append(body, ""), c.composer.Popup(w))
	return strings.Join(append(above, c.composer.View(w, prompt)), "\n")
}

func (c *chat) submit(text string) tea.Cmd {
	c.follow = true
	if c.replying {
		c.replying, c.composer.Hint = false, c.hint()
		return c.decide(agentv1.ApprovalScope_APPROVAL_SCOPE_UNSPECIFIED, strings.TrimSpace(text))
	}
	if line, shell := strings.CutPrefix(text, "!"); shell && strings.TrimSpace(line) != "" {
		return c.run(strings.TrimSpace(line))
	}
	if cmd, ok := c.local(text); ok {
		return cmd
	}
	if cmd, ok := c.kit.Slash(text); ok {
		return cmd
	}
	return c.send(text)
}

func (c *chat) run(line string) tea.Cmd {
	if c.shell != nil && c.kit.WM.Has(c.shell.Key()) {
		return c.shell.Run(line)
	}
	c.shell = c.kit.Terminal()
	return tea.Batch(c.kit.WM.OpenHere(c.shell), c.shell.Run(line), c.kit.WM.Focus(c))
}

func (c *chat) send(text string) tea.Cmd {
	if c.starting {
		c.waiting = append(c.waiting, text)
		return nil
	}
	id, epoch, named, profile := c.id, c.epoch, c.named, c.profile
	c.starting = id == ""
	if c.title == "" && !strings.HasPrefix(text, "/") {
		line, _, _ := strings.Cut(text, "\n")
		c.title = string([]rune(line)[:min(utf8.RuneCountInString(line), titled)])
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := c.kit.Env.Ctx
		var err error
		if id == "" {
			var response *sessionsv1.CreateResponse
			if response, err = c.desk.sessions.Create(ctx, &sessionsv1.CreateRequest{Directory: c.kit.Env.WorkDir(), Profile: profile}); err == nil {
				id, profile = response.GetSession().GetId(), response.GetSession().GetProfile()
			}
			if err == nil && named != "" {
				_, err = c.desk.sessions.Rename(ctx, &sessionsv1.RenameRequest{Id: id, Title: named})
			}
		}
		if err == nil {
			_, err = c.desk.agents.Send(ctx, &agentv1.SendRequest{SessionId: id, Text: text})
		}
		return func() tea.Cmd {
			if epoch != c.epoch {
				return nil
			}
			var cmds []tea.Cmd
			if c.starting && id != "" {
				c.id, c.named, c.profile = id, "", profile
				cmds = append(cmds, c.watch())
			}
			c.starting = false
			if err != nil {
				c.log.note("**Not sent:** " + tui.Reason(err))
				if c.composer.Empty() {
					cmds = append(cmds, c.composer.SetValue(text))
				}
			}
			waiting := c.waiting
			c.waiting = nil
			for _, next := range waiting {
				cmds = append(cmds, c.send(next))
			}
			if strings.HasPrefix(text, "/") && err == nil {
				cmds = append(cmds, c.retitle())
			}
			return tea.Batch(cmds...)
		}
	})
}

func (c *chat) retitle() tea.Cmd {
	id, epoch := c.id, c.epoch
	if id == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.sessions.Get(c.kit.Env.Ctx, &sessionsv1.GetRequest{Id: id})
		return func() tea.Cmd {
			if err == nil && epoch == c.epoch && response.GetSession().GetTitle() != "" {
				c.title = response.GetSession().GetTitle()
			}
			return nil
		}
	})
}

func (c *chat) interrupt() tea.Cmd {
	id := c.id
	if id == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		_, err := c.desk.agents.Stop(c.kit.Env.Ctx, &agentv1.StopRequest{SessionId: id})
		return func() tea.Cmd {
			if err != nil {
				c.log.note("**Couldn't stop:** " + tui.Reason(err))
			}
			return nil
		}
	})
}

func (c *chat) reset() {
	if c.stop != nil {
		c.stop()
	}
	c.epoch++
	c.id, c.title, c.named, c.stop, c.starting, c.waiting = "", "", "", nil, false, nil
	c.log, c.folds, c.rendered = transcript{}, map[string]bool{}, nil
	c.top, c.follow = 0, true
}

func (c *chat) watch() tea.Cmd {
	if c.stop != nil {
		c.stop()
	}
	ctx, stop := context.WithCancel(c.kit.Env.Ctx)
	c.stop = stop
	id, epoch := c.id, c.epoch
	return func() tea.Msg {
		stream, err := c.desk.agents.Watch(ctx, &agentv1.WatchRequest{SessionId: id})
		if err == nil {
			defer stream.Close()
		}
		first := true
		for err == nil {
			var response *agentv1.WatchResponse
			if response, err = stream.Receive(); err == nil {
				replay := first
				first = false
				c.kit.Apply(func() tea.Cmd { return c.received(epoch, response, replay) })
			}
		}
		if ctx.Err() == nil {
			c.kit.Apply(func() tea.Cmd { return c.dropped(epoch, err) })
		}
		return nil
	}
}

func (c *chat) received(epoch int, response *agentv1.WatchResponse, replay bool) tea.Cmd {
	if epoch != c.epoch {
		return nil
	}
	if replay {
		c.log.replay()
	}
	busy := c.log.busy
	taken := c.log.apply(response)
	var cmds []tea.Cmd
	if response.GetRewind() != nil {
		c.folds = map[string]bool{}
	}
	if taken != "" && c.composer.Empty() {
		cmds = append(cmds, c.composer.SetValue(taken))
	}
	if busy && !c.log.busy {
		c.unseen = c.kit.Focused() != wm.Card(c)
		cmds = append(cmds, c.retitle())
	}
	return tea.Batch(append(cmds, c.ask())...)
}

func (c *chat) dropped(epoch int, err error) tea.Cmd {
	if epoch != c.epoch {
		return nil
	}
	c.log.busy = false
	if connect.CodeOf(err) == connect.CodeNotFound {
		c.log.note("**This session was deleted.** Send a message to start a new one.")
		c.id, c.title, c.stop = "", "", nil
		return c.ask()
	}
	return tui.Async(func() func() tea.Cmd {
		select {
		case <-time.After(reconnect):
		case <-c.kit.Env.Ctx.Done():
		}
		return func() tea.Cmd {
			if epoch != c.epoch || c.kit.Env.Ctx.Err() != nil {
				return nil
			}
			return c.watch()
		}
	})
}
