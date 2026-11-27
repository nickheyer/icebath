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
	{ID: "agent.rewind.fork", Context: rewindContext, Label: "fork", Doc: "start a new session from before this prompt, the prompt ready to edit", Default: []string{"f"}},
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
		c.log.note("↺ Rewind cancelled; nothing was removed.")
		return nil
	}
	if time.Since(c.primed) > escAgain {
		c.primed = time.Now()
		if !c.log.conversed() {
			return nil
		}
		key := c.kit.WM.Keys().Display("session.stop")
		return c.kit.WM.Toast(strings.TrimSpace(key+" again to go back to an earlier prompt"), wm.Info, escAgain)
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
				c.say("/rewind", tui.Reason(err))
				return nil
			}
			if len(response.GetPrompts()) == 0 {
				c.say("/rewind", "Nothing to rewind to.")
				return nil
			}
			return then(response.GetPrompts())
		}
	})
}

// /rewind [N | fork N]
func (c *chat) rewind(argument string) (tea.Cmd, bool) {
	echo := strings.TrimSpace("/rewind " + argument)
	words := strings.Fields(argument)
	fork := len(words) > 0 && words[0] == "fork"
	if fork {
		words = words[1:]
	}
	number := 0
	if len(words) > 1 {
		c.say(echo, "Usage: /rewind [N | fork N]")
		return nil, true
	}
	if len(words) == 1 {
		n, err := strconv.Atoi(words[0])
		if err != nil || n < 1 {
			c.say(echo, "Usage: /rewind [N | fork N]")
			return nil, true
		}
		number = n
	}
	switch {
	case c.id == "":
		c.say(echo, "Nothing to rewind to.")
		return nil, true
	case c.log.busy:
		c.say(echo, "Still replying. Stop the reply first ("+c.kit.WM.Keys().Display("session.stop")+"), then run /rewind.")
		return nil, true
	}
	return c.prompts(func(prompts []*sessionsv1.Prompt) tea.Cmd {
		if number == 0 {
			return c.rewindPicker(prompts)
		}
		if number > len(prompts) {
			c.say(echo, fmt.Sprintf("No prompt %d — pick 1 to %d.", number, len(prompts)))
			return nil
		}
		prompt := prompts[len(prompts)-number]
		if fork {
			return c.forkAt(prompt, number)
		}
		return c.arm(prompt, number)
	}), true
}

// Picker of the session's prompts, newest first: enter edits the one picked, the fork key starts a new session from before it, and typing
// goes to the prompt
func (c *chat) rewindPicker(prompts []*sessionsv1.Prompt) tea.Cmd {
	kit := c.kit
	var hub *cards.Hub
	total := len(prompts)
	leave := func(cmd tea.Cmd) tea.Cmd { return tea.Batch(kit.WM.DismissCard(hub), cmd) }
	list := cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Rewind",
		Glyph:   kit.Env.Theme.Glyphs.Fork,
		Key:     c.key + ":rewind",
		Context: cards.HubContext,
		Shared:  []string{rewindContext},
		Primary: "hub.use",
		Columns: []render.Column{{Title: "#", Right: true}, {Title: "Prompt", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(prompts))
			for i, prompt := range prompts {
				rows[i] = []string{strconv.Itoa(total - i), prompt.GetPreview()}
			}
			return rows
		},
		Detail: func(i int) []string { return strings.Split(prompts[i].GetText(), "\n") },
		Empty:  func() string { return "No previous message to edit." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "edit"}, {Action: "agent.rewind.fork"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":           func(_ *cards.List, i int) tea.Cmd { return leave(c.arm(prompts[i], total-i)) },
			"agent.rewind.fork": func(_ *cards.List, i int) tea.Cmd { return leave(c.forkAt(prompts[i], total-i)) },
		},
		Typed: func(_ *cards.List, text string, _ bool) tea.Cmd {
			return leave(tea.Batch(kit.WM.Focus(c), c.composer.Insert(text)))
		},
	})
	hub = kit.Hub(c.key+":rewind", list)
	return kit.WM.ShowOverlay(hub)
}

// Picks the prompt to edit: it fills the prompt, and the next line sent takes it and everything after it back and goes in its place
func (c *chat) arm(prompt *sessionsv1.Prompt, number int) tea.Cmd {
	if prompt.GetMedia() {
		c.say("/rewind", fmt.Sprintf("Prompt %d holds more than text and cannot be edited — pick another prompt.", number))
		return nil
	}
	c.editing = &rewinding{id: prompt.GetId(), number: number}
	keymap := c.kit.WM.Keys()
	c.composer.Hint = fmt.Sprintf("↺ Editing prompt %d — %s rewinds here and sends · %s cancels", number, keymap.Display("composer.send"),
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
				c.log.note("↺ Edit not sent: " + tui.Reason(err) + " The text is back in the prompt.")
				if c.composer.Empty() {
					return c.composer.SetValue(text)
				}
				return nil
			}
			c.log.note(fmt.Sprintf("↺ Rewound to prompt %d (%d messages removed).", picked.number, response.GetRemoved()))
			return nil
		}
	})
}

// Starts a new session holding what came before the prompt, the prompt ready to edit and send in it
func (c *chat) forkAt(prompt *sessionsv1.Prompt, number int) tea.Cmd {
	if prompt.GetMedia() {
		c.say("/rewind", fmt.Sprintf("Prompt %d holds more than text and cannot be edited — pick another prompt.", number))
		return nil
	}
	text := prompt.GetText()
	if prompt.GetThrough() == 0 {
		c.reset()
		c.log.note("Started a new session — edit the prompt and press " + c.kit.WM.Keys().Display("composer.send") + " to send.")
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
				c.say("/rewind", "Failed to fork: "+tui.Reason(err))
				return nil
			}
			cmd := c.switchTo(response.GetSession())
			c.log.note(fmt.Sprintf("Forked from before prompt %d — edit the prompt and press %s to send.", number,
				c.kit.WM.Keys().Display("composer.send")))
			return tea.Batch(cmd, c.kit.WM.Focus(c), c.composer.SetValue(text))
		}
	})
}
