package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const approvalContext = "agent.approval"

var approvalKeys = []keys.Action{
	{ID: "agent.approve", Context: approvalContext, Label: "allow", Doc: "allow this command once", Default: []string{"y"}},
	{ID: "agent.deny", Context: approvalContext, Label: "deny", Doc: "deny this command", Default: []string{"n"}},
	{ID: "agent.approve.session", Context: approvalContext, Label: "allow for session", Doc: "allow commands like this for the rest of the session", Default: []string{"s"}},
	{ID: "agent.approve.always", Context: approvalContext, Label: "always allow", Doc: "always allow commands like this", Default: []string{"a"}},
	{ID: "agent.deny.reply", Context: approvalContext, Label: "deny with reason", Doc: "deny and tell the agent what to do instead", Default: []string{"r"}},
}

type approval struct {
	*cards.Page
	request *agentv1.Approval
	deny    func() tea.Cmd
}

func (a *approval) ClosesItself() bool { return true }

func (a *approval) FitIn(w, h int) (int, int) {
	return w, min(len(a.lines(w)), h)
}

func (a *approval) lines(w int) []string {
	return append([]string{a.request.GetReason(), ""}, strings.Split(render.Wrap(a.request.GetCommand(), max(w, 1)), "\n")...)
}

func (a *approval) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	if action, ok := msg.(wm.ActionMsg); ok && action.ID == "palette.close" {
		return a, a.deny()
	}
	_, cmd := a.Page.Update(msg)
	return a, cmd
}

func (c *chat) SheetArea(w, _ int) wm.Box {
	return wm.Box{W: w, H: c.rows}
}

func (c *chat) ask() tea.Cmd {
	request := c.log.approval
	if request == nil && c.replying {
		c.replying, c.composer.Hint = false, c.hint()
	}
	if c.sheet != nil && c.sheet.request == request || request != nil && c.replying {
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
	look := c.kit.Env.Theme
	answer := func(scope agentv1.ApprovalScope) func(*cards.Page) tea.Cmd {
		return func(*cards.Page) tea.Cmd { return c.decide(scope, "") }
	}
	var hints []wm.Hint
	for _, action := range approvalKeys {
		hints = append(hints, wm.Hint{Action: action.ID})
	}
	sheet := &approval{request: request, deny: func() tea.Cmd { return c.decide(agentv1.ApprovalScope_APPROVAL_SCOPE_UNSPECIFIED, "") }}
	sheet.Page = cards.NewPage(c.kit.Env, cards.PageSpec{
		Title:   "Allow this command?",
		Key:     c.key + ":approval",
		Context: approvalContext,
		Lines: func(w int) []string {
			lines := sheet.lines(w)
			lines[0] = render.Warm(lines[0], look)
			return lines
		},
		Hints: func(*cards.Page) []wm.Hint { return hints },
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"agent.approve":         answer(agentv1.ApprovalScope_APPROVAL_SCOPE_ONCE),
			"agent.approve.session": answer(agentv1.ApprovalScope_APPROVAL_SCOPE_SESSION),
			"agent.approve.always":  answer(agentv1.ApprovalScope_APPROVAL_SCOPE_ALWAYS),
			"agent.deny":            answer(agentv1.ApprovalScope_APPROVAL_SCOPE_UNSPECIFIED),
			"agent.deny.reply":      func(*cards.Page) tea.Cmd { return c.reply() },
		},
	})
	c.sheet = sheet
	return tea.Batch(append(cmds, c.kit.WM.Attach(c, sheet))...)
}

func (c *chat) reply() tea.Cmd {
	sheet := c.sheet
	c.sheet, c.replying, c.composer.Hint = nil, true, "Tell the agent what to do instead"
	return c.kit.WM.DismissCard(sheet)
}

func (c *chat) decide(scope agentv1.ApprovalScope, reason string) tea.Cmd {
	id := c.id
	return tui.Async(func() func() tea.Cmd {
		var err error
		if scope == agentv1.ApprovalScope_APPROVAL_SCOPE_UNSPECIFIED {
			_, err = c.desk.agents.Deny(c.kit.Env.Ctx, &agentv1.DenyRequest{SessionId: id, Reason: reason})
		} else {
			_, err = c.desk.agents.Approve(c.kit.Env.Ctx, &agentv1.ApproveRequest{SessionId: id, Scope: scope})
		}
		return func() tea.Cmd {
			if err != nil {
				c.log.note("**Couldn't answer:** " + tui.Reason(err))
			}
			return nil
		}
	})
}
