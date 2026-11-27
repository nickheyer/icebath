package app

import (
	"cmp"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Wait before watching the connections again after the stream broke
const rewatch = 3 * time.Second

// States a connection target no longer waits in
var resolved = map[string]bool{"connected": true, "skipped": true, "not_connected": true}

var targetWords = map[string]string{"pending": "waiting for you", "initiated": "in progress", "connected": "connected", "skipped": "skipped",
	"failed": "failed", "not_connected": "not connected"}

var verbs = map[string]string{"install": "Install", "enable": "Enable", "authorize": "Sign in to"}

// Connection cards of the operations the agent's manage_connections calls run, one per operation, drawn while it waits on the user
type connections struct {
	kit     *tui.Kit
	service mcpv1connect.McpServiceClient
	cards   map[string]*connection
}

// Card of one operation
type connection struct {
	op  *mcpv1.Operation
	hub *cards.Hub
	// What the view in the hub was built for: the target, its state and link
	subject string
	// The user put the card away; the next change of the operation shows it again
	hidden bool
	ended  bool
}

// Watches the operations of every session for as long as the app runs, the stream opened again whenever it breaks
func (c *connections) watch() {
	go func() {
		ctx := c.kit.Env.Ctx
		for ctx.Err() == nil {
			stream, err := c.service.Watch(ctx, &mcpv1.WatchRequest{})
			if err == nil {
				for {
					op, err := stream.Receive()
					if err != nil {
						if !errors.Is(err, io.EOF) && ctx.Err() == nil {
							c.kit.Apply(c.clear)
						}
						break
					}
					c.kit.Apply(func() tea.Cmd { return c.update(op) })
				}
				stream.Close()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(rewatch):
			}
		}
	}()
}

// Drops every card, the server that ran their operations gone
func (c *connections) clear() tea.Cmd {
	var cmds []tea.Cmd
	for id, card := range c.cards {
		card.ended = true
		cmds = append(cmds, c.kit.WM.DismissCard(card.hub))
		delete(c.cards, id)
	}
	return tea.Batch(cmds...)
}

// Shows the operation as it stands: its card closed once it settled, else the view of the target that waits on the user. An operation of
// a session no window of this app shows is left to the app that shows it
func (c *connections) update(op *mcpv1.Operation) tea.Cmd {
	card := c.cards[op.GetId()]
	if card != nil && card.op.GetSeq() > op.GetSeq() {
		return nil
	}
	if card == nil && op.GetSettled() == "" && op.GetSession() != "" && !slices.Contains(c.kit.Sessions(), op.GetSession()) {
		return nil
	}
	if op.GetSettled() != "" {
		if card == nil {
			return nil
		}
		card.ended = true
		delete(c.cards, op.GetId())
		return c.kit.WM.DismissCard(card.hub)
	}
	if card == nil {
		card = &connection{}
		c.cards[op.GetId()] = card
	}
	changed := card.op == nil || card.op.GetSeq() != op.GetSeq()
	card.op = op
	view, subject := c.view(card)
	if card.hub == nil {
		card.hub, card.subject = c.kit.Hub(Screen+".connection:"+op.GetId(), view), subject
		card.hub.OnClose(func() tea.Cmd {
			if card.ended || card.hidden {
				return nil
			}
			card.hidden = true
			return c.kit.WM.Toast("The agent waits on the connection. /connections shows it again", wm.Info, wm.DefaultToastTTL)
		})
		return c.kit.WM.ShowOverlay(card.hub)
	}
	var cmds []tea.Cmd
	if subject != card.subject {
		card.subject = subject
		cmds = append(cmds, card.hub.Swap(view))
	}
	if card.hidden && changed {
		card.hidden = false
		cmds = append(cmds, c.kit.WM.ShowOverlay(card.hub))
	}
	return tea.Batch(cmds...)
}

// Shows every open card again
func (c *connections) reopen() tea.Cmd {
	var cmds []tea.Cmd
	for _, card := range c.cards {
		if card.hub != nil {
			card.hidden = false
			cmds = append(cmds, c.kit.WM.ShowOverlay(card.hub))
		}
	}
	if len(cmds) == 0 {
		return c.kit.WM.Toast("No connection waits on you", wm.Info, wm.DefaultToastTTL)
	}
	return tea.Batch(cmds...)
}

// First target of the operation that waits on the user: one not resolved, or one that connected while its tools could not be read
func active(op *mcpv1.Operation) *mcpv1.Target {
	for _, target := range op.GetTargets() {
		if !resolved[target.GetState()] || target.GetState() == "connected" && target.GetDiscoveryError() != "" {
			return target
		}
	}
	return nil
}

// Line of each target: its mark, name, state and detail
func (c *connections) lines(op *mcpv1.Operation) []string {
	t := c.kit.Env.Theme
	var out []string
	for _, target := range op.GetTargets() {
		mark := t.Glyphs.Hollow
		switch target.GetState() {
		case "connected":
			mark = t.Glyphs.Check
		case "skipped", "not_connected":
			mark = t.Glyphs.Skip
		case "initiated":
			mark = t.Glyphs.Active
		case "failed":
			mark = t.Glyphs.Fail
		}
		line := mark + " " + target.GetName() + " · " + cmp.Or(targetWords[target.GetState()], target.GetState())
		if detail := strings.TrimSpace(target.GetDetail()); detail != "" {
			line += " · " + detail
		}
		switch target.GetState() {
		case "connected":
			line = render.Good(line, t)
		case "failed":
			line = render.Warm(line, t)
		case "skipped", "not_connected":
			line = render.Dim(line, t)
		}
		out = append(out, line)
	}
	return out
}

// View of the card for the target that waits on the user, and what it was built for
func (c *connections) view(card *connection) (wm.Card, string) {
	op := card.op
	target := active(op)
	t := c.kit.Env.Theme
	answer := func(status string, env map[string]string, settle bool) tea.Cmd {
		var answers []*mcpv1.TargetAnswer
		if target != nil && !settle {
			answers = append(answers, &mcpv1.TargetAnswer{Name: target.GetName(), Status: status, Env: env})
		}
		id := op.GetId()
		return tui.Call(card.hub, func() (*mcpv1.RespondResponse, error) {
			return c.service.Respond(c.kit.Env.Ctx, &mcpv1.RespondRequest{Id: id, Answers: answers, Settle: settle})
		}, func(*mcpv1.RespondResponse) tea.Cmd { return nil })
	}
	page := func(subject string, lines func() []string, hints []wm.Hint, actions map[string]func(*cards.Page) tea.Cmd) (wm.Card, string) {
		return cards.NewPage(c.kit.Env, cards.PageSpec{
			Title:   "Connect",
			Key:     Screen + ".connection:" + op.GetId() + ":" + subject,
			Context: connectionContext,
			Lines: func(int) []string {
				return append(c.lines(card.op), append([]string{""}, lines()...)...)
			},
			Hints:   func(*cards.Page) []wm.Hint { return hints },
			Actions: actions,
		}), subject
	}
	if target == nil {
		return page("finishing", func() []string { return []string{render.Dim("Finishing…", t)} }, nil, nil)
	}
	name := target.GetName()
	subject := name + ":" + target.GetState() + ":" + target.GetConnectUrl()
	notes := func() []string {
		var out []string
		for _, line := range []string{target.GetInstructions(), target.GetDiscoveryError()} {
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
		return out
	}
	skip := func(*cards.Page) tea.Cmd { return answer("skipped", nil, false) }
	wake := func(*cards.Page) tea.Cmd {
		id := op.GetId()
		return tui.Call(card.hub, func() (*mcpv1.WakeResponse, error) {
			return c.service.Wake(c.kit.Env.Ctx, &mcpv1.WakeRequest{Id: id})
		}, func(*mcpv1.WakeResponse) tea.Cmd { return card.hub.Say("Checking "+name+"…", cards.NoteInfo) })
	}
	link := strings.TrimSpace(target.GetConnectUrl())
	switch {
	case target.GetState() == "connected":
		return page(subject, func() []string {
			return append([]string{render.Good("Signed in to "+name+", but its tools could not be listed yet.", t)}, notes()...)
		}, []wm.Hint{{Action: "mcp.connection.continue"}}, map[string]func(*cards.Page) tea.Cmd{
			"mcp.connection.continue": func(*cards.Page) tea.Cmd { return answer("", nil, true) },
		})
	case target.GetState() == "failed":
		return page(subject, func() []string {
			out := notes()
			if strings.TrimSpace(target.GetDetail()) == "" {
				out = append(out, "Connecting "+name+" failed.")
			}
			return out
		}, []wm.Hint{{Action: "mcp.connection.retry"}, {Action: "mcp.connection.skip"}}, map[string]func(*cards.Page) tea.Cmd{
			"mcp.connection.retry": func(*cards.Page) tea.Cmd {
				id := op.GetId()
				return tui.Call(card.hub, func() (*mcpv1.RetryResponse, error) {
					return c.service.Retry(c.kit.Env.Ctx, &mcpv1.RetryRequest{Id: id, Names: []string{name}})
				}, func(*mcpv1.RetryResponse) tea.Cmd { return nil })
			},
			"mcp.connection.skip": skip,
		})
	case link != "":
		hints := []wm.Hint{{Action: "mcp.connection.open"}, {Action: "mcp.connection.copy"}, {Action: "mcp.connection.wake"}, {Action: "mcp.connection.skip"}}
		if target.GetSignin() != "" {
			hints = append(hints, wm.Hint{Action: "mcp.connection.paste"})
		}
		view, subject := page(subject, func() []string {
			return append(notes(), "Finish connecting "+name+" in the browser:", render.Warm(link, t))
		}, hints, map[string]func(*cards.Page) tea.Cmd{
			"mcp.connection.open":  func(*cards.Page) tea.Cmd { return open(c.kit, card.hub, link) },
			"mcp.connection.copy":  func(*cards.Page) tea.Cmd { return copyLink(c.kit, card.hub, link) },
			"mcp.connection.paste": func(*cards.Page) tea.Cmd { return paste(c.kit, card.hub, c.service, target.GetSignin()) },
			"mcp.connection.wake":  wake,
			"mcp.connection.skip":  skip,
		})
		return view, subject
	case target.GetState() == "initiated":
		return page(subject, func() []string {
			return append(notes(), render.Dim("Working on "+name+"…", t))
		}, []wm.Hint{{Action: "mcp.connection.wake"}, {Action: "mcp.connection.skip"}}, map[string]func(*cards.Page) tea.Cmd{
			"mcp.connection.wake": wake,
			"mcp.connection.skip": skip,
		})
	}
	var fields []cards.Field
	for _, spec := range target.GetRequired() {
		kind := cards.FieldText
		if spec.GetSecret() {
			kind = cards.FieldSecret
		}
		fields = append(fields, cards.Field{ID: spec.GetName(), Label: cmp.Or(strings.TrimSpace(spec.GetPrompt()), spec.GetName()), Kind: kind,
			Value: spec.GetDefault(), Required: spec.GetRequired(), Help: spec.GetName()})
	}
	verb := cmp.Or(verbs[target.GetAction()], "Connect")
	return cards.NewForm(c.kit.Env, cards.FormSpec{
		Title:  verb + " " + name + "?",
		Intro:  append(c.lines(op), notes()...),
		Fields: fields,
		Submit: verb + " " + name,
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			env := map[string]string{}
			for _, spec := range target.GetRequired() {
				if value := strings.TrimSpace(values[spec.GetName()]); value != "" {
					env[spec.GetName()] = value
				}
			}
			return answer("approved", env, false)
		},
		Cancel: func(*cards.Form) tea.Cmd { return answer("skipped", nil, false) },
	}), subject
}
