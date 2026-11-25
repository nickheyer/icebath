package app

import (
	"cmp"
	"context"
	"errors"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"

	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type chat struct {
	kit      *tui.Kit
	sessions sessionsv1connect.SessionServiceClient
	agents   agentv1connect.AgentServiceClient
	composer *cards.Composer
	markdown *render.Markdown
	key      string
	id       string
	title    string
	starting bool
	note     string
	stop     context.CancelFunc

	log      transcript
	blocks   []string
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

func newChat(kit *tui.Kit, sessions sessionsv1connect.SessionServiceClient, agents agentv1connect.AgentServiceClient, key string, session *sessionsv1.Session) *chat {
	c := &chat{
		kit:      kit,
		sessions: sessions,
		agents:   agents,
		composer: cards.NewComposer(kit.Env.Theme, kit.Env.Keys, "", kit.Env.WorkDir),
		markdown: render.NewMarkdown(),
		key:      key,
		id:       session.GetId(),
		title:    session.GetTitle(),
		follow:   true,
	}
	c.composer.SessionID = key
	c.composer.Literal = true
	c.composer.Hint = "Message the agent, / for commands"
	c.composer.OnStop = c.interrupt
	return c
}

func (c *chat) Title() string       { return cmp.Or(c.title, "New chat") }
func (c *chat) Key() string         { return c.key }
func (c *chat) Glyph() string       { return c.kit.Env.Theme.Glyphs.Session }
func (c *chat) Selectable() bool    { return true }
func (c *chat) MinSize() (int, int) { return 40, 10 }

func (c *chat) Init() tea.Cmd {
	if c.id == "" {
		return nil
	}
	return c.watch()
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
	return c.composer.Status()
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
		if c.stop != nil {
			c.stop()
		}
	case wm.FocusMsg:
		return c, c.composer.SetFocus(msg.Focused)
	case wm.ActionMsg:
		return c, c.action(msg.ID)
	case tea.KeyPressMsg, tea.PasteMsg:
		return c, c.composer.Update(msg)
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			c.scroll(-3)
		case tea.MouseWheelDown:
			c.scroll(3)
		}
	case cards.SubmitMsg:
		if msg.Session == c.key {
			return c, c.send(msg.Text)
		}
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
	case "session.stop":
		if c.composer.Searching() || c.composer.Completing() || !c.log.busy {
			return c.composer.Action(id)
		}
		return c.interrupt()
	default:
		return c.composer.Action(id)
	}
	return nil
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
	rendered := make(map[string][]string, len(c.blocks))
	c.lines = c.lines[:0]
	for i, block := range c.blocks {
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

func (c *chat) send(text string) tea.Cmd {
	if c.starting {
		c.note = "starting the session"
		return c.composer.SetValue(text)
	}
	id := c.id
	c.starting = id == ""
	if c.title == "" {
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
	ctx, stop := context.WithCancel(c.kit.Env.Ctx)
	c.stop = stop
	id := c.id
	return func() tea.Msg {
		stream, err := c.agents.Watch(ctx, &agentv1.WatchRequest{SessionId: id})
		if err == nil {
			defer stream.Close()
		}
		for err == nil {
			var response *agentv1.WatchResponse
			if response, err = stream.Receive(); err == nil {
				c.kit.Apply(func() tea.Cmd {
					c.blocks = c.log.apply(response)
					return nil
				})
			}
		}
		if ctx.Err() == nil && !errors.Is(err, io.EOF) {
			c.kit.Apply(func() tea.Cmd {
				c.note = "error: " + strings.ReplaceAll(err.Error(), "\n", " ")
				return nil
			})
		}
		return nil
	}
}
