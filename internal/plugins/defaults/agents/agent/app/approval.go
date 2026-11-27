package app

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const approvalContext = "agent.approval"

var approvalKeys = []keys.Action{
	{ID: "agent.approve", Context: approvalContext, Label: "allow", Doc: "allow this command once", Default: []string{"y"}},
	{ID: "agent.deny", Context: approvalContext, Label: "deny", Doc: "deny this command", Default: []string{"n"}},
	{ID: "agent.approve.session", Context: approvalContext, Label: "allow for session", Doc: "allow commands like this for the rest of the session", Default: []string{"s"}},
	{ID: "agent.approve.always", Context: approvalContext, Label: "always allow", Doc: "always allow commands like this, saving an allow rule", Default: []string{"a"}},
	{ID: "agent.approval.page", Context: approvalContext, Label: "more", Doc: "show the next page of a long command, the first one after the last", Default: []string{"v", "V"}},
	{ID: "agent.approval.top", Context: approvalContext, Label: "top", Doc: "show the first page of a long command", Default: []string{"g", "home"}, Quiet: true},
	{ID: "agent.approval.down", Context: approvalContext, Label: "down", Doc: "move to the next answer", Default: []string{"j"}, Quiet: true},
	{ID: "agent.approval.up", Context: approvalContext, Label: "up", Doc: "move to the answer before", Default: []string{"k"}, Quiet: true},
	{ID: "agent.approval.scroll.down", Context: approvalContext, Label: "page down", Doc: "scroll the chat behind the prompt down a page", Default: []string{"pgdown"}, Quiet: true},
	{ID: "agent.approval.scroll.up", Context: approvalContext, Label: "page up", Doc: "scroll the chat behind the prompt up a page", Default: []string{"pgup"}, Quiet: true},
	{ID: "agent.yolo", Context: keys.Session, Label: "yolo", Doc: "run commands without asking, or ask again (yolo)", Default: []string{"alt+y"}},
}

// Rows of a long command shown at once, the page key bringing the rest
const commandPage = 10

// Approval prompt asked in the chat in place of the prompt: what it was flagged as, the command a page at a time, the answers it takes
// picked with the arrows and enter or their number, and the row that goes to the prompt to deny and tell the agent what to do instead.
// Enter on an untouched prompt denies
type approval struct {
	chat    *chat
	request *agentv1.Approval
	// Prompts waiting, this one the oldest
	waiting int
	// Row under the cursor: a choice, or the last row to tell the agent what to do
	row  int
	page int
	// Line of the row under the cursor, kept in view
	cursor int
}

func newApproval(c *chat, request *agentv1.Approval, waiting int) *approval {
	a := &approval{chat: c, request: request, waiting: waiting}
	a.row = max(slices.IndexFunc(request.GetChoices(), func(choice *agentv1.Choice) bool { return choice.GetValue() == "deny" }), 0)
	return a
}

func (a *approval) Title() string {
	title := "⚠ approval required"
	if a.request.GetTitle() != "" {
		title = a.request.GetTitle()
	}
	if a.waiting > 1 {
		title += fmt.Sprintf(" (1 of %d)", a.waiting)
	}
	return title
}

func (a *approval) Key() string               { return a.chat.key + ":approval" }
func (a *approval) Init() tea.Cmd             { return nil }
func (a *approval) Contexts() []string        { return []string{approvalContext} }
func (a *approval) ClosesItself() bool        { return true }
func (a *approval) FitIn(w, h int) (int, int) { return w, min(len(a.lines(w)), h) }

// Rows the cursor moves over: the choices, then telling the agent what to do
func (a *approval) rows() int {
	return len(a.request.GetChoices()) + 1
}

func (a *approval) offers(value string) bool {
	return slices.ContainsFunc(a.request.GetChoices(), func(choice *agentv1.Choice) bool { return choice.GetValue() == value })
}

func (a *approval) Hints() []wm.Hint {
	hints := []wm.Hint{{Action: "palette.pick", Label: "choose"}, {Action: "agent.approve"}, {Action: "agent.deny"}}
	if a.offers("session") {
		hints = append(hints, wm.Hint{Action: "agent.approve.session"})
	}
	if a.offers("always") {
		hints = append(hints, wm.Hint{Action: "agent.approve.always"})
	}
	return append(hints, wm.Hint{Action: "palette.close", Label: "deny with reason"})
}

func (a *approval) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.ActionMsg:
		switch msg.ID {
		case "palette.up", "agent.approval.up":
			a.row = (a.row - 1 + a.rows()) % a.rows()
		case "palette.down", "agent.approval.down":
			a.row = (a.row + 1) % a.rows()
		case "agent.approval.top":
			a.page = 0
		case "agent.approval.scroll.up":
			return a, a.chat.action("session.scroll.up")
		case "agent.approval.scroll.down":
			return a, a.chat.action("session.scroll.down")
		case "palette.pick":
			return a, a.pick()
		case "agent.approve":
			return a, a.choose("once")
		case "agent.approve.session":
			return a, a.choose("session")
		case "agent.approve.always":
			return a, a.choose("always")
		case "agent.deny":
			return a, a.choose("deny")
		case "agent.approval.page":
			a.page++
		case "palette.close":
			return a, a.chat.reply()
		}
	case tea.KeyPressMsg:
		if n, err := strconv.Atoi(msg.String()); err == nil && n >= 1 && n <= a.rows() {
			a.row = n - 1
			return a, a.pick()
		}
	}
	return a, nil
}

// Answers with the row under the cursor, or goes to the prompt to tell the agent what to do instead
func (a *approval) pick() tea.Cmd {
	choices := a.request.GetChoices()
	if a.row >= len(choices) {
		return a.chat.reply()
	}
	return a.choose(choices[a.row].GetValue())
}

// Answers with the choice when the prompt takes it
func (a *approval) choose(value string) tea.Cmd {
	if !a.offers(value) {
		return nil
	}
	scope := map[string]agentv1.ApprovalScope{"once": agentv1.ApprovalScope_APPROVAL_SCOPE_ONCE, "session": agentv1.ApprovalScope_APPROVAL_SCOPE_SESSION,
		"always": agentv1.ApprovalScope_APPROVAL_SCOPE_ALWAYS}[value]
	return a.chat.decide(a.request.GetId(), scope, "")
}

// Page of the command in view, with the line counting its rows when it runs past one page
func (a *approval) command(w int) []string {
	look := a.chat.kit.Env.Theme
	var lines []string
	for _, line := range strings.Split(a.request.GetCommand(), "\n") {
		lines = append(lines, strings.Split(ansi.Hardwrap(line, max(w-2, 8), true), "\n")...)
	}
	size := commandPage
	if len(lines) <= size+1 {
		size = len(lines)
	}
	pages := (len(lines) + size - 1) / size
	start := a.page % pages * size
	end := min(start+size, len(lines))
	var out []string
	for _, line := range lines[start:end] {
		out = append(out, " "+line)
	}
	if pages > 1 {
		key := a.chat.kit.Env.Keys.Display("agent.approval.page")
		text := fmt.Sprintf("lines %d–%d of %d · %s shows the rest before you choose", start+1, end, len(lines), key)
		if end == len(lines) {
			text = fmt.Sprintf("lines %d–%d of %d (end) · %s goes back to the top", start+1, end, len(lines), key)
		}
		out = append(out, " "+render.Warm(text, look))
	}
	return out
}

func (a *approval) lines(w int) []string {
	look := a.chat.kit.Env.Theme
	g := look.Glyphs
	wrap := func(text string, indent int) []string {
		return strings.Split(render.Wrap(text, max(w-indent, 8)), "\n")
	}
	var lines []string
	for _, line := range wrap(a.request.GetReason(), 1) {
		lines = append(lines, " "+render.Warm(line, look))
	}
	if a.request.GetSmartDenied() {
		for _, line := range wrap("The safety check denied this command, allowing it overrides that verdict", 1) {
			lines = append(lines, " "+render.Bad(line, look))
		}
	}
	lines = append(append(lines, ""), a.command(w)...)
	lines = append(lines, "")
	labels := make([]string, 0, a.rows())
	for _, choice := range a.request.GetChoices() {
		labels = append(labels, choice.GetLabel())
	}
	labels = append(labels, "No, and tell the agent what to do instead ›")
	for r, label := range labels {
		cursor := "  "
		if r == a.row {
			cursor = g.Forward + " "
		}
		for n, line := range wrap(fmt.Sprintf("%d  %s", r+1, label), 7) {
			if n > 0 {
				line = "   " + line
			}
			if r == a.row {
				a.cursor = len(lines)
				lines = append(lines, " "+cursor+render.Warm(line, look))
			} else {
				lines = append(lines, " "+cursor+line)
			}
		}
	}
	return lines
}

func (a *approval) View(w, h int) string {
	lines := a.lines(w)
	start := max(a.cursor-h+1, 0)
	lines = lines[start:min(start+h, len(lines))]
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (c *chat) SheetArea(w, _ int) wm.Box {
	return wm.Box{W: w, H: c.rows}
}

// Shows the oldest approval prompt waiting in place of the prompt, keeping its place while the same one waits, and takes it away once none does
func (c *chat) ask() tea.Cmd {
	if !c.log.busy {
		c.outcome = ""
	}
	pending := c.log.approvals
	if c.replying && !slices.ContainsFunc(pending, func(p *agentv1.Approval) bool { return p.GetId() == c.replyTo }) {
		c.replying, c.replyTo, c.composer.Hint = false, "", c.hint()
	}
	if c.replying {
		return nil
	}
	var request *agentv1.Approval
	if len(pending) > 0 {
		request = pending[0]
	}
	if c.sheet != nil && request != nil && c.sheet.request.GetId() == request.GetId() {
		c.sheet.request, c.sheet.waiting = request, len(pending)
		return nil
	}
	var cmds []tea.Cmd
	if c.sheet != nil {
		cmds = append(cmds, c.kit.WM.DismissCard(c.sheet))
		c.sheet = nil
	}
	if request == nil {
		return tea.Batch(cmds...)
	}
	c.sheet = newApproval(c, request, len(pending))
	return tea.Batch(append(cmds, c.kit.WM.Attach(c, c.sheet))...)
}

// Moves to the prompt to deny the command shown and tell the agent what to do instead; sending the words denies, an empty prompt comes back
func (c *chat) reply() tea.Cmd {
	sheet := c.sheet
	if sheet == nil {
		return nil
	}
	c.sheet, c.replying, c.replyTo, c.composer.Hint = nil, true, sheet.request.GetId(), "Tell the agent what to do instead"
	return c.kit.WM.DismissCard(sheet)
}

// Answers the prompt: yes once, for the session or always, or no with what the user said
func (c *chat) decide(request string, scope agentv1.ApprovalScope, reason string) tea.Cmd {
	id := c.id
	return tui.Async(func() func() tea.Cmd {
		var err error
		outcome := "denied"
		if scope == agentv1.ApprovalScope_APPROVAL_SCOPE_UNSPECIFIED {
			_, err = c.desk.agents.Deny(c.kit.Env.Ctx, &agentv1.DenyRequest{SessionId: id, Id: request, Reason: reason})
		} else {
			_, err = c.desk.agents.Approve(c.kit.Env.Ctx, &agentv1.ApproveRequest{SessionId: id, Id: request, Scope: scope})
			outcome = "approved (" + map[agentv1.ApprovalScope]string{agentv1.ApprovalScope_APPROVAL_SCOPE_ONCE: "once",
				agentv1.ApprovalScope_APPROVAL_SCOPE_SESSION: "session", agentv1.ApprovalScope_APPROVAL_SCOPE_ALWAYS: "always"}[scope] + ")"
		}
		return func() tea.Cmd {
			if err != nil {
				c.log.note("**Couldn't answer:** " + tui.Reason(err))
				return nil
			}
			c.outcome = outcome
			return nil
		}
	})
}

// Asks in the chat before a command that deletes or resets something, unless confirmations are off, then runs it; a cancel says so
func (c *chat) confirm(command, detail string, then func() tea.Cmd) tea.Cmd {
	id := c.id
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.agents.Confirm(c.kit.Env.Ctx, &agentv1.ConfirmRequest{SessionId: id, Command: command, Detail: detail})
		return func() tea.Cmd {
			switch {
			case err != nil:
				c.say("/"+command, tui.Reason(err))
				return nil
			case !response.GetOk():
				c.say("/"+command, response.GetNote())
				return nil
			}
			if note := strings.TrimSpace(response.GetNote()); note != "" {
				c.say("/"+command, note)
			}
			return then()
		}
	})
}

// Turns asking before risky commands off for the session, or on again, as /yolo does
func (c *chat) yolo() tea.Cmd {
	if c.id == "" {
		c.log.note("yolo needs an active session")
		return nil
	}
	return c.send("/yolo")
}
