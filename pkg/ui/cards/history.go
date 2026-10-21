package cards

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// One point on a session worth flying to, with the transcript as it stood then
type Moment struct {
	// Entries the session held at the moment
	Entries int
	Kind    string
	Title   string
	Glyph   string
	Time    time.Time
	Seq     int
}

// Moments of a session, one per user turn, assistant turn, approval, compaction and fork point
func Moments(sess *session.Session) []Moment {
	entries := sess.Entries()
	records := sess.Records()
	var out []Moment
	if sess.Parent() != "" {
		out = append(out, Moment{Entries: 0, Kind: "fork", Title: "forked from " + render.Truncate(sess.Parent(), 16), Glyph: "⑂", Time: sess.Created(), Seq: -1})
	}
	ri := 0
	for i, e := range entries {
		for ri < len(records) && records[ri].Seq < e.Seq {
			if m, ok := momentOf(records[ri], i); ok {
				out = append(out, m)
			}
			ri++
		}
		switch e.Role {
		case protocol.RoleUser:
			text, _ := partsText(e.Content)
			title := render.Summary(text, 40)
			if strings.HasPrefix(text, "Summary of the earlier conversation:") {
				title = "compaction summary"
			}
			out = append(out, Moment{Entries: i + 1, Kind: "user", Title: title, Glyph: "▌", Time: e.Time, Seq: e.Seq})
		case protocol.RoleAssistant:
			calls := 0
			text := ""
			for _, p := range e.Content {
				switch v := p.(type) {
				case protocol.ToolCall:
					calls++
				case protocol.Text:
					if text == "" {
						text = v.Text
					}
				}
			}
			title := render.Summary(text, 40)
			if title == "" && calls > 0 {
				title = fmt.Sprintf("%d tool calls", calls)
			}
			if title == "" {
				title = "assistant"
			}
			name := e.Agent
			if name == "" {
				name = "agent"
			}
			out = append(out, Moment{Entries: i + 1, Kind: "assistant", Title: name + ": " + title, Glyph: "≋", Time: e.Time, Seq: e.Seq})
		}
	}
	for ; ri < len(records); ri++ {
		if m, ok := momentOf(records[ri], len(entries)); ok {
			out = append(out, m)
		}
	}
	return out
}

func momentOf(r session.Record, entries int) (Moment, bool) {
	switch r.Kind {
	case "approval":
		return Moment{Entries: entries, Kind: "approval", Title: "approval " + render.Truncate(string(r.Data), 30), Glyph: "⚑", Time: r.Time, Seq: r.Seq}, true
	case "compact":
		return Moment{Entries: entries, Kind: "compact", Title: "compaction", Glyph: "≡", Time: r.Time, Seq: r.Seq}, true
	case "mission":
		return Moment{Entries: entries, Kind: "mission", Title: "mission " + render.Truncate(string(r.Data), 30), Glyph: "◎", Time: r.Time, Seq: r.Seq}, true
	}
	return Moment{}, false
}

// Card holding the frozen transcript of a moment
type MomentCard struct {
	env    *Env
	moment Moment
	index  int
	tr     *Transcript
	marked bool
}

var (
	hFork    = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "fork here"))
	hCompare = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "compare"))
)

// Builds the card of moment i of the session, rendering the transcript prefix it covers
func NewMomentCard(env *Env, sess *session.Session, m Moment, i int) *MomentCard {
	tr := NewTranscript(env)
	tr.follow = false
	entries := sess.Entries()
	if m.Entries < len(entries) {
		entries = entries[:m.Entries]
	}
	var records []session.Record
	for _, r := range sess.Records() {
		if r.Seq <= m.Seq {
			records = append(records, r)
		}
	}
	tr.Load(entries, records)
	tr.cursor = -1
	return &MomentCard{env: env, moment: m, index: i, tr: tr}
}

func (c *MomentCard) Title() string {
	return fmt.Sprintf("%d %s", c.index+1, render.Truncate(c.moment.Title, 28))
}

func (c *MomentCard) Glyph() string        { return c.moment.Glyph }
func (c *MomentCard) Key() string          { return fmt.Sprintf("moment:%d", c.index) }
func (c *MomentCard) Init() tea.Cmd        { return nil }
func (c *MomentCard) Selectable() bool     { return true }
func (c *MomentCard) Moment() Moment       { return c.moment }
func (c *MomentCard) Hints() []key.Binding { return []key.Binding{hFork, hCompare, tScroll} }
func (c *MomentCard) Claims() []string {
	return []string{"j", "k", "g", "G", "c", "enter", "o", "[", "]"}
}

// Reports whether the card was marked for comparison
func (c *MomentCard) Marked() bool { return c.marked }

func (c *MomentCard) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			n := c.moment.Entries
			return c, func() tea.Msg { return ForkAtMsg{Entries: n} }
		case "c":
			c.marked = !c.marked
			return c, nil
		}
		_, cmd := c.tr.Update(msg)
		return c, cmd
	case tea.MouseWheelMsg, tea.MouseClickMsg:
		_, cmd := c.tr.Update(msg)
		return c, cmd
	}
	return c, nil
}

func (c *MomentCard) View(w, h int) string {
	t := c.env.Theme
	if h < 2 {
		return c.tr.View(w, h)
	}
	head := render.Dim(c.moment.Time.Local().Format("15:04:05")+" · "+c.moment.Kind+" · "+fmt.Sprintf("%d entries", c.moment.Entries), t)
	if c.marked {
		head += " " + render.Hot("marked", t)
	}
	return render.Truncate(head, w) + "\n" + c.tr.View(w, h-1)
}

func (c *MomentCard) Status() string {
	if c.marked {
		return "compare"
	}
	return ""
}
