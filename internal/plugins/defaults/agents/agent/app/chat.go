package app

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	slashv1 "github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1/slashv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	approvalContext = "approval"
	reconnect       = 3 * time.Second
	completions     = 50
)

var shells = 1000

type chat struct {
	kit      *tui.Kit
	sessions sessionsv1connect.SessionServiceClient
	agents   agentv1connect.AgentServiceClient
	slash    slashv1connect.SlashServiceClient
	commands []*slashv1.Command
	pick     func() tea.Cmd
	open     func(session *sessionsv1.Session) tea.Cmd
	composer *cards.Composer
	markdown *render.Markdown
	shell    *cards.Terminal
	key      string
	id       string
	title    string
	starting bool
	note     string
	epoch    int
	stop     context.CancelFunc
	asked    *agentv1.Approval
	warned   *agentv1.Approval

	log      transcript
	rendered map[string][]string
	width    int
	lines    []string
	rows     int
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
} = (*chat)(nil)

func newChat(kit *tui.Kit, sessions sessionsv1connect.SessionServiceClient, agents agentv1connect.AgentServiceClient, slash slashv1connect.SlashServiceClient, key string, session *sessionsv1.Session) *chat {
	if _, registered := kit.Env.Keys.Action("approval.once"); !registered {
		if keys.ContextDocs[approvalContext] == "" {
			if err := keys.AddContext(approvalContext, "answering a command that waits for approval"); err != nil {
				panic(err)
			}
		}
		for _, action := range []keys.Action{
			{ID: "approval.once", Context: approvalContext, Label: "approve", Doc: "run the command this once", Default: []string{"y", "enter"}},
			{ID: "approval.session", Context: approvalContext, Label: "for this session", Doc: "run the command, and others like it until the session ends", Default: []string{"s"}},
			{ID: "approval.always", Context: approvalContext, Label: "always", Doc: "run the command, and others like it from now on", Default: []string{"a"}},
			{ID: "approval.deny", Context: approvalContext, Label: "deny", Doc: "do not run the command", Default: []string{"n"}},
		} {
			kit.Env.Keys.Register(action)
		}
	}
	c := &chat{
		kit:      kit,
		sessions: sessions,
		agents:   agents,
		slash:    slash,
		composer: cards.NewComposer(kit.Env.Theme, kit.Env.Keys, "", kit.Env.WorkDir),
		markdown: render.NewMarkdown(),
		key:      key,
		id:       session.GetId(),
		title:    session.GetTitle(),
		follow:   true,
	}
	c.composer.SessionID = key
	c.composer.Literal = true
	c.composer.History = kit.History
	c.composer.Hint = "Message the agent, / for commands, ! for a shell line"
	c.composer.OnStop = c.interrupt
	c.composer.Apply = func(text string, replace int) tea.Cmd {
		typed := []rune(c.composer.Value())
		return c.composer.SetValue(string(typed[:min(replace, len(typed))]) + text)
	}
	return c
}

func (c *chat) Title() string       { return cmp.Or(c.title, "New chat") }
func (c *chat) Key() string         { return c.key }
func (c *chat) Glyph() string       { return c.kit.Env.Theme.Glyphs.Session }
func (c *chat) Selectable() bool    { return true }
func (c *chat) MinSize() (int, int) { return 40, 10 }

func (c *chat) Init() tea.Cmd {
	load := tui.Async(func() func() tea.Cmd {
		response, err := c.slash.List(c.kit.Env.Ctx, &slashv1.ListRequest{})
		return func() tea.Cmd {
			if err == nil {
				c.commands = response.GetCommands()
			}
			return nil
		}
	})
	if c.id == "" {
		return load
	}
	return tea.Batch(load, c.watch())
}

func (c *chat) Status() string {
	switch {
	case c.note != "":
		return c.note
	case c.log.approval != nil:
		return "waiting for approval"
	case c.log.busy:
		return "working"
	}
	if status := c.composer.Status(); status != "" {
		return status
	}
	state := c.log.state
	if state.GetModel() == "" {
		return state.GetProvider()
	}
	status := state.GetProvider() + "/" + state.GetModel()
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
	case wm.FocusMsg:
		focus := c.composer.SetFocus(msg.Focused)
		if msg.Focused {
			return c, tea.Batch(focus, c.raise())
		}
		return c, focus
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
	case tui.ThemeMsg:
		c.composer.SetTheme(c.kit.Env.Theme)
		c.rendered = nil
	case cards.SubmitMsg:
		if msg.Session != c.key {
			break
		}
		if line, shell := strings.CutPrefix(msg.Text, "!"); shell && strings.TrimSpace(line) != "" {
			return c, c.run(strings.TrimSpace(line))
		}
		if cmd, ok := c.local(msg.Text); ok {
			return c, cmd
		}
		return c, c.send(msg.Text)
	}
	return c, nil
}

func (c *chat) action(id string) tea.Cmd {
	page := max(c.rows-2, 1)
	switch id {
	case "session.scroll.up":
		c.scroll(-page)
	case "session.scroll.down":
		c.scroll(page)
	case "session.scroll.top":
		c.top, c.follow = 0, false
	case "session.scroll.end":
		c.follow = true
	case "session.expand":
		c.log.expanded = true
	case "session.collapse":
		c.log.expanded = false
	case "session.thinking":
		c.log.quiet = !c.log.quiet
	case "session.stop":
		if c.composer.Searching() || c.composer.Completing() || !c.log.busy {
			return c.composer.Action(id)
		}
		return c.interrupt()
	case "complete.start":
		c.paths()
	case "composer.send":
		if c.log.approval != nil && c.composer.Empty() {
			return c.ask(c.log.approval)
		}
		return c.composer.Action(id)
	default:
		cmd := c.composer.Action(id)
		c.complete()
		return cmd
	}
	return nil
}

func (c *chat) complete() {
	value := c.composer.Value()
	name, typing := strings.CutPrefix(value, "/")
	if !typing || strings.ContainsAny(name, " \n") {
		return
	}
	var offers []cards.Offer
	for _, command := range c.catalog() {
		if strings.HasPrefix(command.GetName(), name) {
			offers = append(offers, cards.Offer{
				Text:    "/" + command.GetName() + " ",
				Display: strings.TrimSpace("/" + command.GetName() + " " + command.GetUsage()),
				Meta:    command.GetHelp(),
			})
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
	last := max(len(c.lines)-c.rows, 0)
	c.top = min(max(c.top+rows, 0), last)
	c.follow = c.top == last
}

func (c *chat) View(w, h int) string {
	c.composer.Fit(w)
	c.composer.SetBusy(c.log.busy)
	prompt := min(c.composer.Height(), max(h-3, 1))
	c.rows = max(h-prompt-1, 1)
	c.prompt = c.rows + 1
	c.layout(w)
	last := max(len(c.lines)-c.rows, 0)
	if c.follow {
		c.top = last
	}
	c.top = min(c.top, last)
	body := make([]string, c.rows)
	copy(body, c.lines[c.top:min(c.top+c.rows, len(c.lines))])
	above := cards.OverlayPopup(append(body, ""), c.composer.Popup(w))
	return strings.Join(append(above, c.composer.View(w, prompt)), "\n")
}

func (c *chat) layout(w int) {
	if w != c.width {
		c.width, c.rendered = w, nil
	}
	blocks := c.log.blocks()
	rendered := make(map[string][]string, len(blocks))
	c.lines = c.lines[:0]
	for i, block := range blocks {
		lines, ok := c.rendered[block]
		if !ok {
			lines = strings.Split(strings.Trim(c.markdown.Render(block, w, c.kit.Env.Theme), "\n"), "\n")
		}
		rendered[block] = lines
		if i > 0 {
			c.lines = append(c.lines, "")
		}
		c.lines = append(c.lines, lines...)
	}
	c.rendered = rendered
}

func (c *chat) run(line string) tea.Cmd {
	if c.shell != nil && c.kit.WM.Has(c.shell.Key()) {
		return c.shell.Run(line)
	}
	shells++
	c.shell = cards.NewTerminal(c.kit.Env, shells, c.kit.Env.WorkDir())
	return tea.Batch(c.kit.WM.OpenHere(c.shell), c.shell.Run(line), c.kit.WM.Focus(c))
}

func (c *chat) send(text string) tea.Cmd {
	if c.starting {
		c.note = "starting the session"
		return c.composer.SetValue(text)
	}
	id, epoch := c.id, c.epoch
	c.starting = id == ""
	if c.title == "" && !strings.HasPrefix(text, "/") {
		c.title, _, _ = strings.Cut(text, "\n")
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := c.kit.Env.Ctx
		var err error
		if id == "" {
			var response *sessionsv1.CreateResponse
			if response, err = c.sessions.Create(ctx, &sessionsv1.CreateRequest{Directory: c.kit.Env.WorkDir()}); err == nil {
				id = response.GetSession().GetId()
			}
		}
		if err == nil {
			_, err = c.agents.Send(ctx, &agentv1.SendRequest{SessionId: id, Text: text})
		}
		return func() tea.Cmd {
			if epoch != c.epoch {
				return nil
			}
			var watch tea.Cmd
			if c.starting && id != "" {
				c.id = id
				watch = c.watch()
			}
			c.starting, c.note = false, ""
			if err != nil {
				c.note = "error: " + strings.ReplaceAll(err.Error(), "\n", " ")
				return tea.Batch(watch, c.composer.SetValue(text))
			}
			return watch
		}
	})
}

func (c *chat) interrupt() tea.Cmd {
	id := c.id
	if id == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		_, err := c.agents.Stop(c.kit.Env.Ctx, &agentv1.StopRequest{SessionId: id})
		return func() tea.Cmd {
			if err != nil {
				c.note = "error: " + err.Error()
			}
			return nil
		}
	})
}

func (c *chat) watch() tea.Cmd {
	if c.stop != nil {
		c.stop()
	}
	ctx, stop := context.WithCancel(c.kit.Env.Ctx)
	c.stop = stop
	id, epoch := c.id, c.epoch
	return func() tea.Msg {
		stream, err := c.agents.Watch(ctx, &agentv1.WatchRequest{SessionId: id})
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
		c.log = transcript{expanded: c.log.expanded, quiet: c.log.quiet}
		c.note = ""
	}
	c.log.apply(response)
	return c.raise()
}

func (c *chat) dropped(epoch int, err error) tea.Cmd {
	if epoch != c.epoch {
		return nil
	}
	c.log.busy = false
	if connect.CodeOf(err) == connect.CodeNotFound {
		c.note = "error: this session no longer exists"
		return nil
	}
	c.note = "connection lost, reconnecting"
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

func (c *chat) raise() tea.Cmd {
	approval := c.log.approval
	if approval == nil || approval == c.asked {
		return nil
	}
	if c.kit.WM.Overlay() == nil && c.kit.Focused() == wm.Card(c) {
		return c.ask(approval)
	}
	if approval == c.warned {
		return nil
	}
	c.warned = approval
	return tea.Batch(c.kit.WM.Attention(c, wm.Pulse), c.kit.WM.Toast(c.Title()+" waits for your approval", wm.Warn, wm.DefaultToastTTL))
}

func (c *chat) ask(approval *agentv1.Approval) tea.Cmd {
	c.asked = approval
	var hub *cards.Hub
	answer := func(scope agentv1.ApprovalScope) func(*cards.Page) tea.Cmd {
		return func(*cards.Page) tea.Cmd {
			return tea.Batch(c.kit.WM.DismissCard(hub), c.decide(scope))
		}
	}
	look := c.kit.Env.Theme
	hub = c.kit.Hub(c.key+":approval", cards.NewPage(c.kit.Env, cards.PageSpec{
		Title:   "Approve " + approval.GetReason() + "?",
		Key:     c.key + ":approval",
		Context: approvalContext,
		Lines: func(int) []string {
			lines := []string{render.Heading(c.Title(), look), render.Warm(approval.GetReason(), look), ""}
			return append(lines, strings.Split(approval.GetCommand(), "\n")...)
		},
		Hints: func(*cards.Page) []wm.Hint {
			return []wm.Hint{{Action: "approval.once"}, {Action: "approval.session"}, {Action: "approval.always"}, {Action: "approval.deny"}}
		},
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"approval.once":    answer(agentv1.ApprovalScope_APPROVAL_SCOPE_ONCE),
			"approval.session": answer(agentv1.ApprovalScope_APPROVAL_SCOPE_SESSION),
			"approval.always":  answer(agentv1.ApprovalScope_APPROVAL_SCOPE_ALWAYS),
			"approval.deny":    answer(agentv1.ApprovalScope_APPROVAL_SCOPE_UNSPECIFIED),
		},
	}))
	return c.kit.WM.ShowOverlay(hub)
}

func (c *chat) decide(scope agentv1.ApprovalScope) tea.Cmd {
	id := c.id
	return tui.Async(func() func() tea.Cmd {
		var err error
		if scope == agentv1.ApprovalScope_APPROVAL_SCOPE_UNSPECIFIED {
			_, err = c.agents.Deny(c.kit.Env.Ctx, &agentv1.DenyRequest{SessionId: id})
		} else {
			_, err = c.agents.Approve(c.kit.Env.Ctx, &agentv1.ApproveRequest{SessionId: id, Scope: scope})
		}
		return func() tea.Cmd {
			if err != nil {
				c.note = "error: " + strings.ReplaceAll(err.Error(), "\n", " ")
			}
			return nil
		}
	})
}
