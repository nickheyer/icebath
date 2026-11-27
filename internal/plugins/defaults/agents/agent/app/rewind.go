package app

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// How long the first esc on the empty idle prompt waits for the second that opens the rewind picker
const escAgain = 2 * time.Second

// Prompts a rewind picks among, at most
const rewindPrompts = 500

const rewindContext = "agent.rewind"

var rewindKeys = []keys.Action{
	{ID: "agent.rewind.fork", Context: rewindContext, Label: "branch", Doc: "Start a new session from before this message, ready to edit it", Default: []string{"f"}},
}

// Prompt picked to rewind to: the message and its number among the session's prompts
type rewinding struct {
	id     int64
	number int
}

// Esc on the empty prompt of an idle session: cancels an edit picked, else the first press primes and the second opens the rewind picker
func (c *chat) escape() tea.Cmd {
	if c.editing != nil {
		c.unedit()
		c.log.note("Stopped changing the message. Nothing was taken back.")
		return nil
	}
	if time.Since(c.primed) > escAgain {
		c.primed = time.Now()
		if !c.log.conversed() {
			return nil
		}
		key := c.kit.WM.Keys().Display("session.stop")
		return c.kit.WM.Toast("Press "+key+" again to go back to an earlier message.", wm.Info, escAgain)
	}
	c.primed = time.Time{}
	return c.prompts(func(prompts []*sessionsv1.Prompt) tea.Cmd { return c.rewindPicker(prompts) })
}

// Forgets the edit picked
func (c *chat) unedit() {
	c.editing = nil
	c.composer.Hint = c.hint()
}

// Reads the session's prompts, newest first, and hands them on; says so when there is none
func (c *chat) prompts(then func(prompts []*sessionsv1.Prompt) tea.Cmd) tea.Cmd {
	id, epoch := c.id, c.epoch
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.sessions.Prompts(c.kit.Env.Ctx, &sessionsv1.PromptsRequest{Id: id, Limit: rewindPrompts})
		return func() tea.Cmd {
			if epoch != c.epoch {
				return nil
			}
			if err != nil {
				c.say("/rewind", "Your earlier messages could not be read. "+tui.Reason(err))
				return nil
			}
			if len(response.GetPrompts()) == 0 {
				c.say("/rewind", noRewind)
				return nil
			}
			return then(response.GetPrompts())
		}
	})
}

// What /rewind and the rewind list say when the session holds no message of yours
const noRewind = "This session has no message of yours to go back to."

// What /rewind says when it is typed wrong: how it is typed and what each form does
const rewindHelp = "Usage: /rewind " + rewindUsage + ". A number goes back to that message, counting your first as 1. fork starts a new session from before it."

// /rewind [n | fork n]
func (c *chat) rewind(argument string) (tea.Cmd, bool) {
	echo := strings.TrimSpace("/rewind " + argument)
	words := strings.Fields(argument)
	fork := len(words) > 0 && words[0] == "fork"
	if fork {
		words = words[1:]
	}
	number := 0
	if len(words) > 1 {
		c.say(echo, rewindHelp)
		return nil, true
	}
	if len(words) == 1 {
		n, err := strconv.Atoi(words[0])
		if err != nil || n < 1 {
			c.say(echo, rewindHelp)
			return nil, true
		}
		number = n
	}
	switch {
	case c.id == "":
		c.say(echo, noRewind)
		return nil, true
	case c.log.busy:
		c.say(echo, "The agent is still replying. Press "+c.kit.WM.Keys().Display("session.stop")+" to stop the reply, then run /rewind again.")
		return nil, true
	}
	return c.prompts(func(prompts []*sessionsv1.Prompt) tea.Cmd {
		if number == 0 {
			return c.rewindPicker(prompts)
		}
		if number > len(prompts) {
			c.say(echo, fmt.Sprintf("There is no message %d. Pick a number from 1 to %d.", number, len(prompts)))
			return nil
		}
		prompt := prompts[len(prompts)-number]
		if fork {
			return c.forkAt(prompt, number)
		}
		return c.arm(prompt, number)
	}), true
}

// What the chat says of a message that holds more than text, which can't be sent again
func unsendable(number int) string {
	return fmt.Sprintf("Message %d holds more than text, so it can't be sent again. Pick another message.", number)
}

// List of the session's messages, newest first: enter puts the one picked back in the box to change, the branch key starts a new session from
// before it, and typing goes on in the box, since the list opens on a second esc that may have been pressed by mistake
func (c *chat) rewindPicker(prompts []*sessionsv1.Prompt) tea.Cmd {
	kit := c.kit
	t := kit.Env.Theme
	var hub *cards.Hub
	var list *cards.List
	total := len(prompts)
	leave := func(cmd tea.Cmd) tea.Cmd { return tea.Batch(kit.WM.DismissCard(hub), cmd) }
	list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Rewind",
		Glyph:   t.Glyphs.Fork,
		Key:     c.key + ":rewind",
		Context: cards.HubContext,
		Shared:  []string{rewindContext},
		Primary: "hub.use",
		Intro: func() []string {
			return []string{"Go back to one of your messages. Change it and send it, and everything after it is taken back. Typing goes on in the message box."}
		},
		Columns: []render.Column{{Right: true}, {Flex: 1}, {}, {Right: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(prompts))
			for i, prompt := range prompts {
				state := ""
				if prompt.GetMedia() {
					state = "has attachments"
				}
				rows[i] = []string{render.Dim(strconv.Itoa(total-i), t), strings.Join(strings.Fields(prompt.GetPreview()), " "), render.Dim(state, t),
					render.Dim(sent(prompt.GetTime().AsTime()), t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			lines := strings.Split(strings.TrimRight(prompts[i].GetText(), "\n"), "\n")
			facts := "Message " + strconv.Itoa(total-i) + " of " + strconv.Itoa(total)
			if at := prompts[i].GetTime(); at != nil {
				facts += " · Sent " + at.AsTime().Local().Format("Jan 2 15:04")
			}
			lines = append(lines, "", render.Dim(facts, t))
			if prompts[i].GetMedia() {
				lines = append(lines, render.Warm("This message holds more than text, so it can't be changed or sent again from here.", t))
			}
			return lines
		},
		Empty: func() string { return noRewind },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || prompts[i].GetMedia() {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "agent.rewind.fork"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":           func(_ *cards.List, i int) tea.Cmd { return leave(c.arm(prompts[i], total-i)) },
			"agent.rewind.fork": func(_ *cards.List, i int) tea.Cmd { return leave(c.forkAt(prompts[i], total-i)) },
		},
		Typed: func(_ *cards.List, text string, _ bool) tea.Cmd {
			return leave(tea.Batch(kit.WM.Focus(c), c.composer.Insert(text)))
		},
	})
	list.ToggleDetail()
	hub = kit.Hub(c.key+":rewind", list)
	return kit.WM.ShowOverlay(hub)
}

// When a message was sent: the time today, else the day
func sent(at time.Time) string {
	if at.IsZero() || at.Unix() == 0 {
		return ""
	}
	at = at.Local()
	if now := time.Now(); at.YearDay() == now.YearDay() && at.Year() == now.Year() {
		return at.Format("15:04")
	}
	return at.Format("Jan 2")
}

// Picks the message to change: it fills the box, and the next line sent takes it and everything after it back and goes in its place
func (c *chat) arm(prompt *sessionsv1.Prompt, number int) tea.Cmd {
	if prompt.GetMedia() {
		c.say("/rewind", unsendable(number))
		return nil
	}
	c.editing = &rewinding{id: prompt.GetId(), number: number}
	keymap := c.kit.WM.Keys()
	c.composer.Hint = fmt.Sprintf("Changing message %d · %s takes back everything after it and sends · %s stops", number, keymap.Display("composer.send"),
		keymap.Display("session.stop"))
	text := prompt.GetText()
	if draft := strings.TrimSpace(c.composer.Value()); draft != "" {
		text += " " + draft
	}
	return tea.Batch(c.kit.WM.Focus(c), c.composer.SetValue(text))
}

// Sends the line in place of the prompt picked, which goes back with all after it; the line comes back to the prompt when that fails
func (c *chat) resend(text string) tea.Cmd {
	picked, id, epoch := c.editing, c.id, c.epoch
	c.unedit()
	c.quiet = true
	c.follow = true
	// The text sent in the prompt's place tells the model when the person cut a spoken reply off before it, as every message does
	interrupted := c.desk.voice.cut()
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.agents.Edit(c.kit.Env.Ctx, &agentv1.EditRequest{SessionId: id, MessageId: picked.id, Text: text, Interrupted: interrupted})
		return func() tea.Cmd {
			if epoch != c.epoch {
				return nil
			}
			if err != nil {
				c.quiet = false
				c.editing = picked
				c.log.note("**The changed message was not sent.** " + tui.Reason(err) + " Your text is back in the message box.")
				if c.composer.Empty() {
					return c.composer.SetValue(text)
				}
				return nil
			}
			c.log.note(fmt.Sprintf("Went back to message %d and took back %s.", picked.number, render.Count(int(response.GetRemoved()), "message")))
			return nil
		}
	})
}

// Starts a new session holding what came before the prompt, the prompt ready to edit and send in it
func (c *chat) forkAt(prompt *sessionsv1.Prompt, number int) tea.Cmd {
	if prompt.GetMedia() {
		c.say("/rewind", unsendable(number))
		return nil
	}
	text := prompt.GetText()
	send := c.kit.WM.Keys().Display("composer.send")
	if prompt.GetThrough() == 0 {
		c.reset()
		c.log.note("Started a new session. Change the message and press " + send + " to send it.")
		return tea.Batch(c.kit.WM.Focus(c), c.composer.SetValue(text))
	}
	id, epoch := c.id, c.epoch
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.sessions.Branch(c.kit.Env.Ctx, &sessionsv1.BranchRequest{Id: id, Before: prompt.GetId()})
		return func() tea.Cmd {
			if epoch != c.epoch {
				return nil
			}
			if err != nil {
				c.say("/rewind", "The new session was not made. "+tui.Reason(err))
				return nil
			}
			cmd := c.switchTo(response.GetSession())
			c.log.note(fmt.Sprintf("Started a new session from before message %d. Change the message and press %s to send it.", number, send))
			return tea.Batch(cmd, c.kit.WM.Focus(c), c.composer.SetValue(text))
		}
	})
}
