package cards

import (
	"bytes"
	"encoding/json"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Columns the dialog opens at and the fewest and most rows it takes
const (
	approvalWidth   = 72
	approvalMinRows = 6
	approvalMaxRows = 24
)

// Floating dialog that shows the head of a session's approval queue with what is needed to decide it
type Approval struct {
	env     *Env
	session string
	pending []*harness.Pending
	scroll  int
	w, h    int
	rows    int
	buttons []button
	// Reports whether a run belongs to the session the dialog serves
	Owns func(run string) bool
}

// Clickable verdict on the bottom row
type button struct {
	x, w    int
	verdict policy.Verdict
}

// Builds the dialog for the approvals of one session
func NewApproval(env *Env, sessionID string, owns func(run string) bool) *Approval {
	a := &Approval{env: env, session: sessionID, Owns: owns}
	a.refresh()
	return a
}

func (a *Approval) refresh() {
	a.pending = a.pending[:0]
	if a.env.Approver == nil {
		return
	}
	for _, p := range a.env.Approver.Pending() {
		if a.Owns == nil || a.Owns(p.Run) {
			a.pending = append(a.pending, p)
		}
	}
	if a.scroll < 0 {
		a.scroll = 0
	}
}

func (a *Approval) Title() string  { return "APPROVAL" }
func (a *Approval) Glyph() string  { return a.env.Theme.Glyphs.Approval }
func (a *Approval) Key() string    { return "approval:" + a.session }
func (a *Approval) Init() tea.Cmd  { return nil }
func (a *Approval) Floating() bool { return true }
func (a *Approval) Closable() bool { return len(a.pending) == 0 }

func (a *Approval) Contexts() []string { return []string{keys.Approval} }

// Size that fits the request at the head of the queue, within the row limits
func (a *Approval) MinSize() (int, int) {
	if len(a.pending) == 0 {
		return approvalWidth, approvalMinRows
	}
	rows := len(a.body(approvalWidth)) + 2
	return approvalWidth, min(max(rows, approvalMinRows), approvalMaxRows)
}

// Approvals waiting, oldest first
func (a *Approval) Pending() []*harness.Pending { return a.pending }

func (a *Approval) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.ApprovalMsg, harness.ApprovalDoneMsg:
		a.refresh()
		if len(a.pending) == 0 {
			return a, wm.RequestClose(a)
		}
		return a, nil
	case wm.ActionMsg:
		if len(a.pending) == 0 {
			return a, nil
		}
		head := a.pending[0]
		for _, c := range verdicts {
			if c.action == msg.ID {
				return a, a.answer(head.ID, c.verdict)
			}
		}
		page := max(a.h-3, 1)
		switch msg.ID {
		case "approval.down":
			a.scroll++
		case "approval.up":
			a.scroll = max(0, a.scroll-1)
		case "approval.page.down":
			a.scroll += page
		case "approval.page.up":
			a.scroll = max(0, a.scroll-page)
		case "approval.top":
			a.scroll = 0
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft || len(a.pending) == 0 {
			return a, nil
		}
		if msg.Y == a.h-1 {
			for _, b := range a.buttons {
				if msg.X >= b.x && msg.X < b.x+b.w {
					return a, a.answer(a.pending[0].ID, b.verdict)
				}
			}
		}
		if a.overflows() && msg.X == a.w-1 && msg.Y < a.h-2 {
			switch hit, line := a.gutter().hit(msg.Y); hit {
			case hitPageUp:
				a.scroll = max(0, a.scroll-max(a.h-3, 1))
			case hitPageDown:
				a.scroll += max(a.h-3, 1)
			case hitJump:
				a.scroll = line
			}
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

// Place of the shown request in the queue once more than one waits
func (a *Approval) Status() string {
	if len(a.pending) < 2 {
		return ""
	}
	return "1 of " + itoa(len(a.pending))
}

// Lines describing one pending request at width, the tool and its arguments first and the policy facts after
func Describe(p *harness.Pending, width int, t theme.Theme) []string {
	req := p.Request
	head := lipgloss.NewStyle().Foreground(t.Pulse).Bold(true).Render(render.ToolGlyph(req.Call.Name, t) + " " + req.Call.Name)
	for _, tag := range tags(req.Hints, t) {
		head += "  " + tag
	}
	lines := []string{spread(head, render.Dim(p.Asked.Local().Format("15:04:05"), t), width, t), ""}
	lines = append(lines, argLines(req.Call.Args, width, t)...)
	var facts [][2]string
	if req.Agent != "" && req.Agent != p.Run {
		facts = append(facts, [2]string{"by", req.Agent})
	}
	if req.Reason != "" && req.Reason != "default" {
		facts = append(facts, [2]string{"reason", req.Reason})
	}
	if req.Key != "" {
		facts = append(facts, [2]string{"rule", render.Cool(req.Key, t)})
	}
	if req.Tool != nil && req.Tool.Description != "" {
		facts = append(facts, [2]string{"tool", render.Truncate(req.Tool.Description, max(width-8, 10), t)})
	}
	if len(facts) > 0 {
		lines = append(append(lines, ""), table(facts, width, t)...)
	}
	if subjects := subjectLines(req.Subjects, width, t); len(subjects) > 0 {
		lines = append(append(lines, ""), subjects...)
	}
	return lines
}

// Short colored words for the annotations that bear on the decision
func tags(h tool.Annotations, t theme.Theme) []string {
	var out []string
	if h.ReadOnly {
		out = append(out, render.Good("read only", t))
	}
	if h.Destructive {
		out = append(out, render.Bad("destructive", t))
	}
	if h.OpenWorld {
		out = append(out, render.Warm("open world", t))
	}
	return out
}

// Left and right text on one line of width, the right dropped when both do not fit
func spread(left, right string, width int, t theme.Theme) string {
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	if lw+rw+2 > width {
		return render.Truncate(left, width, t)
	}
	return left + strings.Repeat(" ", width-lw-rw) + right
}

// Name and raw value of one member of a JSON object
type member struct {
	key string
	raw json.RawMessage
}

// Members of a JSON object in the order written, false when the text is not an object
func objectFields(raw json.RawMessage) ([]member, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var out []member
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		out = append(out, member{k.(string), v})
	}
	return out, true
}

// Text of an argument value, strings bare and anything else as compact JSON
func argText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return string(raw)
}

// Argument lines, one row per member of an object and the indented text for anything else
func argLines(raw json.RawMessage, width int, t theme.Theme) []string {
	fields, ok := objectFields(raw)
	if !ok || len(fields) == 0 {
		var out []string
		for _, l := range strings.Split(prettyArgs(raw), "\n") {
			out = append(out, render.Truncate(lipgloss.NewStyle().Foreground(t.Text).Render(l), width, t))
		}
		return out
	}
	rows := make([][2]string, 0, len(fields))
	for _, f := range fields {
		rows = append(rows, [2]string{f.key, argText(f.raw)})
	}
	return table(rows, width, t)
}

// Arguments indented for reading, the raw text when they are not valid JSON
func prettyArgs(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// Rows of dim keys in one column with their values wrapped beside them, wrapped lines indented under the value
func table(rows [][2]string, width int, t theme.Theme) []string {
	kw := 0
	for _, r := range rows {
		kw = max(kw, ansi.StringWidth(r[0]))
	}
	vw := max(width-kw-2, 8)
	var out []string
	for _, r := range rows {
		for i, v := range wrapLines(r[1], vw) {
			if i == 0 {
				out = append(out, render.Dim(render.Fit(r[0], kw, t), t)+"  "+v)
				continue
			}
			out = append(out, strings.Repeat(" ", kw+2)+v)
		}
	}
	return out
}

// Lines of a value wrapped at width with every line break it already had kept
func wrapLines(s string, width int) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		out = append(out, strings.Split(render.Wrap(l, width), "\n")...)
	}
	return out
}

// Subject rows when the request runs several things or one that hides commands, the kind labeling the first
func subjectLines(subjects []policy.Subject, width int, t theme.Theme) []string {
	unsafe := false
	for _, s := range subjects {
		unsafe = unsafe || s.Unsafe
	}
	if len(subjects) < 2 && !unsafe {
		return nil
	}
	rows := make([][2]string, 0, len(subjects))
	for i, s := range subjects {
		mark := render.Good(t.Glyphs.Bullet, t)
		note := ""
		if s.Unsafe {
			mark = render.Bad("!", t)
			note = render.Bad("  unsafe", t)
		}
		if s.Exact {
			note += render.Dim("  exact", t)
		}
		key := ""
		if i == 0 {
			key = s.Kind
		}
		rows = append(rows, [2]string{key, mark + " " + s.Text + note})
	}
	return table(rows, width, t)
}

// Lines above the buttons at width, the head request then the rest of the queue
func (a *Approval) body(w int) []string {
	t := a.env.Theme
	lines := Describe(a.pending[0], w, t)
	if len(a.pending) > 1 {
		lines = append(lines, "", render.Dim("then", t))
		for _, p := range a.pending[1:] {
			row := render.ToolGlyph(p.Request.Call.Name, t) + " " + p.Request.Call.Name + "  " + CallSummary(p.Request.Call, "", t)
			lines = append(lines, render.Truncate(render.Dim("  "+row, t), w, t))
		}
	}
	return lines
}

// Verdict a button answers with, the action whose key it shows and the color it glows
type verdictChoice struct {
	action  string
	verdict policy.Verdict
	color   func(t theme.Theme) color.Color
}

var verdicts = []verdictChoice{
	{"approval.allow", policy.Yes, func(t theme.Theme) color.Color { return t.Alive }},
	{"approval.deny", policy.No, func(t theme.Theme) color.Color { return t.Alarm }},
	{"approval.always", policy.Always, func(t theme.Theme) color.Color { return t.Ice }},
	{"approval.never", policy.Never, func(t theme.Theme) color.Color { return t.Pulse }},
}

// Verdict buttons on one row lettered with their keys, remembering where each one sits for clicks
func (a *Approval) footer(w int) string {
	t := a.env.Theme
	a.buttons = a.buttons[:0]
	var sb strings.Builder
	x := 0
	for i, c := range verdicts {
		fg := c.color(t)
		act, _ := a.env.Keys.Action(c.action)
		label := lipgloss.NewStyle().Foreground(fg).Render(" " + act.Label + " ")
		if k := a.env.key(c.action); k != "" {
			label = lipgloss.NewStyle().Foreground(t.Bg).Background(fg).Bold(true).Render(" "+k+" ") + label
		}
		lw := ansi.StringWidth(label)
		if x+lw > w {
			break
		}
		a.buttons = append(a.buttons, button{x: x, w: lw, verdict: c.verdict})
		sb.WriteString(label)
		x += lw
		if i < len(verdicts)-1 {
			sb.WriteString(" ")
			x++
		}
	}
	return sb.String()
}

// Reports whether the request runs past the rows above the buttons
func (a *Approval) overflows() bool { return a.rows > max(a.h-2, 1) }

// Scrollbar of the request capped with the keys that page it
func (a *Approval) gutter() gutter {
	body := max(a.h-2, 1)
	return gutter{h: body, total: a.rows, offset: a.scroll, visible: body, up: a.env.Keys.First("approval.page.up"), down: a.env.Keys.First("approval.page.down")}
}

func (a *Approval) View(w, h int) string {
	a.w, a.h = w, h
	t := a.env.Theme
	if len(a.pending) == 0 {
		return emptyView(w, h, "no approvals", t)
	}
	body := max(h-2, 1)
	lines := a.body(w)
	a.rows = len(lines)
	if a.overflows() && w >= minGutterWidth {
		lines = a.body(w - 2)
		a.rows = len(lines)
	}
	a.scroll = min(a.scroll, max(0, len(lines)-body))
	shown := lines[a.scroll:min(len(lines), a.scroll+body)]
	for len(shown) < body {
		shown = append(shown, "")
	}
	text := strings.Join(shown, "\n")
	if a.overflows() && w >= minGutterWidth {
		text = a.gutter().beside(text, w, t)
	}
	return text + "\n\n" + a.footer(w)
}

// Scroll keys, offered only while the request runs past the dialog
func (a *Approval) Hints() []wm.Hint {
	if a.overflows() {
		return []wm.Hint{{Action: "approval.down", Pair: "approval.up", Label: "scroll"}}
	}
	return nil
}
