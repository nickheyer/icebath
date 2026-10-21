package cards

import (
	"bytes"
	"encoding/json"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Modal that shows the approval at the head of the queue with everything a person needs to decide it
type Approval struct {
	env     *Env
	pending []*harness.Pending
	scroll  int
	w, h    int
}

var (
	yesHint    = key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "allow"))
	noHint     = key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "deny"))
	alwaysHint = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "always"))
	neverHint  = key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "never"))
	scrollHint = key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "scroll"))
)

func NewApproval(env *Env) *Approval {
	a := &Approval{env: env}
	a.refresh()
	return a
}

func (a *Approval) refresh() {
	if a.env.Approver != nil {
		a.pending = a.env.Approver.Pending()
	}
	if a.scroll < 0 {
		a.scroll = 0
	}
}

func (a *Approval) Title() string {
	if len(a.pending) == 0 {
		return "APPROVAL"
	}
	return "APPROVAL " + a.pending[0].Request.Call.Name
}

func (a *Approval) Glyph() string       { return "⚑" }
func (a *Approval) Key() string         { return "approval" }
func (a *Approval) Init() tea.Cmd       { return nil }
func (a *Approval) Capturing() bool     { return len(a.pending) > 0 }
func (a *Approval) MinSize() (int, int) { return 40, 12 }
func (a *Approval) Hints() []key.Binding {
	return []key.Binding{yesHint, noHint, alwaysHint, neverHint, scrollHint}
}

// Approvals waiting, oldest first
func (a *Approval) Pending() []*harness.Pending { return a.pending }

func (a *Approval) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.ApprovalMsg, harness.ApprovalDoneMsg:
		a.refresh()
		if len(a.pending) == 0 {
			return a, deck.RequestDismiss(a)
		}
		return a, nil
	case tea.KeyPressMsg:
		if len(a.pending) == 0 {
			return a, nil
		}
		head := a.pending[0]
		switch msg.String() {
		case "y", "Y":
			return a, a.answer(head.ID, policy.Yes)
		case "n", "N":
			return a, a.answer(head.ID, policy.No)
		case "a", "A":
			return a, a.answer(head.ID, policy.Always)
		case "v", "V":
			return a, a.answer(head.ID, policy.Never)
		case "j", "down":
			a.scroll++
		case "k", "up":
			a.scroll = max(0, a.scroll-1)
		case "g":
			a.scroll = 0
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			a.scroll++
		case tea.MouseWheelUp:
			a.scroll = max(0, a.scroll-1)
		}
	}
	return a, nil
}

func (a *Approval) answer(id int, v policy.Verdict) tea.Cmd {
	a.scroll = 0
	return func() tea.Msg { return AnswerMsg{ID: id, Verdict: v} }
}

func (a *Approval) Status() string {
	switch len(a.pending) {
	case 0:
		return ""
	case 1:
		return "1 waiting"
	}
	return itoa(len(a.pending)) + " waiting"
}

// Lines describing one request at width
func Describe(req policy.Request, width int, env *Env) []string {
	t := env.Theme
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	glyph := render.ToolGlyph(req.Call.Name)
	head := lipgloss.NewStyle().Foreground(t.Pulse).Bold(true).Render(glyph+" "+req.Call.Name) + " "
	if req.Agent != "" {
		head += render.Dim("by "+req.Agent, t)
	}
	add(head)
	if req.Reason != "" {
		add(render.KV("reason", req.Reason, t))
	}
	if req.Review != nil {
		add(render.KV("judge", string(req.Review.Decision)+", "+req.Review.Reason, t))
	}
	add("")
	for _, l := range strings.Split(prettyArgs(req.Call.Args), "\n") {
		add(render.Truncate(lipgloss.NewStyle().Foreground(t.Text).Render(l), width))
	}
	if len(req.Subjects) > 0 {
		add("")
		add(render.Heading("subjects", t))
		for _, s := range req.Subjects {
			mark := render.Good("•", t)
			note := ""
			if s.Unsafe {
				mark = render.Bad("!", t)
				note = render.Bad(" unsafe, hides commands behind substitution", t)
			}
			if s.Exact {
				note += render.Dim(" exact", t)
			}
			add(render.Truncate(mark+" "+render.Dim(s.Kind, t)+" "+s.Text+note, width))
		}
	}
	if req.Key != "" {
		add("")
		add(render.KV("always would remember", render.Cool(req.Key, t), t))
	}
	if req.Tool != nil {
		add("")
		add(render.Heading("tool", t))
		for _, l := range strings.Split(render.Wrap(req.Tool.Description, width), "\n") {
			add(render.Dim(l, t))
		}
		var hints []string
		if req.Hints.ReadOnly {
			hints = append(hints, "read only")
		}
		if req.Hints.Destructive {
			hints = append(hints, "destructive")
		}
		if req.Hints.Idempotent {
			hints = append(hints, "idempotent")
		}
		if req.Hints.OpenWorld {
			hints = append(hints, "open world")
		}
		if len(hints) > 0 {
			add(render.KV("hints", strings.Join(hints, ", "), t))
		}
	}
	return lines
}

// Arguments indented for reading, the raw text when they are not valid JSON
func prettyArgs(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func (a *Approval) View(w, h int) string {
	a.w, a.h = w, h
	t := a.env.Theme
	if len(a.pending) == 0 {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, render.Dim("nothing waiting", t))
	}
	head := a.pending[0]
	lines := []string{render.Dim("approval 1 of "+itoa(len(a.pending))+" · "+head.Asked.Format("15:04:05"), t), ""}
	lines = append(lines, Describe(head.Request, w, a.env)...)
	if len(a.pending) > 1 {
		lines = append(lines, "", render.Heading("queued", t))
		for _, p := range a.pending[1:] {
			lines = append(lines, render.Truncate(render.Dim("  "+render.ToolGlyph(p.Request.Call.Name)+" "+p.Request.Call.Name+" "+render.Summary(string(p.Request.Call.Args), w-8), t), w))
		}
	}
	footer := lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Render("y") + render.Dim(" allow  ", t) + lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Render("n") + render.Dim(" deny  ", t) + lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Render("a") + render.Dim(" always  ", t) + lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Render("v") + render.Dim(" never", t)
	body := h - 2
	if body < 1 {
		body = 1
	}
	a.scroll = min(a.scroll, max(0, len(lines)-body))
	shown := lines[a.scroll:min(len(lines), a.scroll+body)]
	for len(shown) < body {
		shown = append(shown, "")
	}
	return strings.Join(append(append(shown, ""), footer), "\n")
}
