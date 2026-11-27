package app

import (
	"cmp"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/sessions/export"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// How often the app looks at what the server's open sessions do: while one works, and while none does
const (
	pollBusy = 1500 * time.Millisecond
	pollIdle = 10 * time.Second
)

// What an open session is doing, as the background notices tell it apart
const (
	phaseIdle    = "idle"
	phaseBusy    = "busy"
	phaseWaiting = "waiting"
)

// Id of this window, which the server counts the sessions it holds open by
func windowID() string {
	return "tui-" + strs.Random(8)
}

// Starts the desk's look at what the server's open sessions do, the first time a screen of the desk opens
func (d *desk) begin() tea.Cmd {
	if d.polling {
		return nil
	}
	d.polling = true
	return d.poll()
}

// Follows whether this terminal keeps a breadcrumb of the session it shows
func (d *desk) follow(_ *tui.Kit, value string) tea.Cmd {
	on := true
	yaml.Unmarshal([]byte(value), &on)
	d.crumbs = on
	return nil
}

// Label a session is named by in notices: its title, else the start of its id
func label(id, title string) string {
	if title != "" {
		return title
	}
	return id[:min(12, len(id))]
}

// Notes the session a chat shows: the run names it on the way out and this terminal's breadcrumb points at it
func (d *desk) showing(c *chat) tea.Cmd {
	sessions.Show(d.kit.Env.Ctx, c.id)
	delete(d.left, c.id)
	return d.remember(c)
}

// Writes this terminal's breadcrumb to the chat's session, when breadcrumbs are kept
func (d *desk) remember(c *chat) tea.Cmd {
	id, directory := c.id, d.kit.Env.WorkDir()
	if !d.crumbs || id == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		dir, err := export.Folder("")
		if err == nil {
			err = sessions.Remember(dir, id, directory)
		}
		return func() tea.Cmd { return nil }
	})
}

// Notes the session a chat leaves, which keeps running on the server, so the window hears when it finishes or waits on the user
func (d *desk) leave(c *chat) {
	if c.id == "" {
		return
	}
	d.left[c.id] = label(c.id, c.title)
	if c.log.busy {
		d.phases[c.id] = phaseBusy
	}
}

// Whether a chat of this window shows the session
func (d *desk) shows(id string) *chat {
	for _, c := range d.chats {
		if c.id == id {
			return c
		}
	}
	return nil
}

// Shows the session in this chat in place of its own, which keeps running on the server when it was working
func (c *chat) switchTo(session *sessionsv1.Session) tea.Cmd {
	if session.GetId() == c.id {
		return nil
	}
	if other := c.desk.shows(session.GetId()); other != nil {
		return c.kit.WM.Focus(other)
	}
	running := c.log.busy
	c.reset()
	c.id, c.title, c.profile = session.GetId(), session.GetTitle(), cmp.Or(session.GetProfile(), c.profile)
	if running {
		c.log.note("Switched sessions. The previous one keeps running; /sessions lists it.")
	}
	cmds := []tea.Cmd{c.watch()}
	if c.title == "" {
		cmds = append(cmds, c.retitle())
	}
	return tea.Batch(cmds...)
}

// Looks at what the server's open sessions do, telling the window when one it left finishes its turn or waits on the user, and looks again
func (d *desk) poll() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := d.agents.Live(d.kit.Env.Ctx, &agentv1.LiveRequest{})
		return func() tea.Cmd {
			wait := pollIdle
			if err == nil {
				if d.heard(response.GetSessions()) {
					wait = pollBusy
				}
			}
			if d.kit.Env.Ctx.Err() != nil {
				return nil
			}
			return tui.Async(func() func() tea.Cmd {
				select {
				case <-time.After(wait):
				case <-d.kit.Env.Ctx.Done():
				}
				return func() tea.Cmd {
					if d.kit.Env.Ctx.Err() != nil {
						return nil
					}
					return d.poll()
				}
			})
		}
	})
}

// Takes in what the open sessions do, announcing the changes of those left running; whether any works
func (d *desk) heard(live []*agentv1.LiveSession) bool {
	working, unseen := false, 0
	open := map[string]bool{}
	for _, session := range live {
		id := session.GetSessionId()
		open[id] = true
		phase := phaseIdle
		switch {
		case session.GetWaiting():
			phase = phaseWaiting
		case session.GetBusy():
			phase, working = phaseBusy, true
		}
		before := d.phases[id]
		d.phases[id] = phase
		if d.shows(id) != nil {
			continue
		}
		unseen++
		name, left := d.left[id]
		if !left || before == phase {
			continue
		}
		switch {
		case phase == phaseWaiting:
			d.announce(fmt.Sprintf("Background session %q waits for your answer · %s switches to it", name, d.sessionsKey()))
		case before == phaseBusy && phase == phaseIdle:
			d.announce(fmt.Sprintf("Background session %q finished its turn · %s switches to it", name, d.sessionsKey()))
		}
	}
	for id := range d.phases {
		if !open[id] {
			delete(d.phases, id)
			delete(d.left, id)
		}
	}
	d.unseen = unseen
	return working
}

// Key that opens the sessions screen, as notices name it
func (d *desk) sessionsKey() string {
	if bound := d.kit.WM.Keys().Display("open." + sessionsScreen); bound != "" {
		return bound
	}
	return "/sessions"
}

// Puts a line in the transcript the person looks at
func (d *desk) announce(text string) {
	target, _ := d.kit.Focused().(*chat)
	if target == nil && len(d.chats) > 0 {
		target = d.chats[len(d.chats)-1]
	}
	if target != nil {
		target.log.note(text)
	}
}

// Dial on the bar counting the sessions the server holds open that no window here shows
func (d *desk) dial(status *wm.Status) {
	if d.unseen == 0 {
		return
	}
	text := fmt.Sprintf("%d more sessions", d.unseen)
	if d.unseen == 1 {
		text = "1 more session"
	}
	status.Dials = append(status.Dials, wm.Segment{Text: text, Action: "open." + sessionsScreen})
}
