package cards

import (
	"encoding/json"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Window of one session, its transcript above the prompt that steers it, closable while nothing runs on it
type Session struct {
	env        *Env
	live       *harness.Live
	Transcript *Transcript
	Composer   *Composer
	focused    bool
	w, h       int
	split      int
	// Reports whether the app holds the session beyond its runs, such as while compacting it
	Locked func() bool
}

// Builds the session card over an open session, keeping the prompt draft under dir
func NewSession(env *Env, live *harness.Live, dir string) *Session {
	s := &Session{env: env, live: live}
	s.Transcript = NewTranscript(env)
	s.Transcript.Load(live.Session.Entries(), live.Session.Records())
	base := func() string {
		if env.Shell != nil {
			return env.Shell.Dir()
		}
		return env.ProjectDir
	}
	s.Composer = NewComposer(env.Theme, env.Keys, dir, base)
	s.Composer.Bind(live.Session.ID())
	return s
}

// View choices of the card kept between launches
type sessionState struct {
	Hidden bool `json:"thinking_hidden,omitempty"`
}

// Whether thinking blocks are hidden, for the next launch
func (s *Session) State() any { return sessionState{Hidden: !s.Transcript.Thinking()} }

// Shows thinking blocks as the saved state did
func (s *Session) SetState(raw json.RawMessage) error {
	var st sessionState
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	s.Transcript.ShowThinking(!st.Hidden)
	return nil
}

// Open session the card shows
func (s *Session) Live() *harness.Live { return s.live }

// Glides the transcript between frames
func (s *Session) Animate(dt float64) bool { return s.Transcript.Animate(dt) }

// Session the card shows
func (s *Session) Session() *session.Session { return s.live.Session }

// Derived or chosen title of the session, NEW before either
func (s *Session) Title() string {
	if t := s.live.Session.Title(); t != "" {
		return render.Truncate(t, 40, s.env.Theme)
	}
	return "NEW"
}

func (s *Session) Glyph() string       { return s.env.Theme.Glyphs.Session }
func (s *Session) Key() string         { return "session:" + s.live.Session.ID() }
func (s *Session) Selectable() bool    { return true }
func (s *Session) MinSize() (int, int) { return 30, 8 }

// Restarts the transcript repaint loop the card may have missed a tick of while closed
func (s *Session) Init() tea.Cmd { return s.Transcript.Init() }

// Reports whether the card may close, which it may while no run goes and the app lets go of the session
func (s *Session) Closable() bool { return !s.busy() && (s.Locked == nil || !s.Locked()) }

// Rebuilds the transcript from the session after compaction or a fork
func (s *Session) Reload() {
	s.Transcript.Load(s.live.Session.Entries(), s.live.Session.Records())
}

// Points the card at another open session, the prompt carrying its draft over
func (s *Session) Rebind(live *harness.Live) {
	s.live = live
	s.Composer.Bind(live.Session.ID())
	s.Reload()
}

// Repaints the prompt with a theme
func (s *Session) SetTheme(t theme.Theme) { s.Composer.SetTheme(t) }

// Keymap contexts in force, the search bar or the completion popup ahead of the prompt
func (s *Session) Contexts() []string {
	switch {
	case s.Transcript.Searching():
		return []string{keys.Search}
	case s.Composer.Completing():
		return []string{keys.Complete, keys.Session}
	}
	return []string{keys.Session}
}

// Reports whether the run belongs to this session
func (s *Session) owns(run string) bool {
	if s.env.Control == nil {
		return true
	}
	for run != "" {
		r, ok := s.env.Control.Get(run)
		if !ok {
			return false
		}
		if r.Session == s.live.Session {
			return true
		}
		run = r.Parent
	}
	return false
}

// Reports whether a root run of this session is going
func (s *Session) busy() bool {
	if s.env.Control == nil {
		return false
	}
	for _, r := range s.env.Control.Active() {
		if r.Parent == "" && r.Session == s.live.Session {
			return true
		}
	}
	return false
}

func (s *Session) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.EventMsg:
		if !s.owns(msg.Run) {
			return s, nil
		}
		_, cmd := s.Transcript.Update(msg)
		return s, cmd
	case harness.ProgressMsg:
		if !s.owns(msg.Run) {
			return s, nil
		}
		_, cmd := s.Transcript.Update(msg)
		return s, cmd
	case wm.FocusMsg:
		s.focused = msg.Focused
		_, cmd := s.Transcript.Update(msg)
		return s, tea.Batch(cmd, s.Composer.SetFocus(msg.Focused))
	case wm.ClosedMsg:
		id := s.live.Session.ID()
		return s, func() tea.Msg { return SessionClosedMsg{Session: id} }
	case wm.ActionMsg:
		return s, s.act(msg.ID)
	case keys.ChangedMsg:
		s.Composer.Rebind()
		return s, nil
	case tea.KeyPressMsg:
		if s.Transcript.Searching() {
			if msg.Text != "" {
				s.Transcript.SearchType(msg.Text)
			}
			return s, nil
		}
		return s, s.Composer.Update(msg)
	case tea.PasteMsg:
		if s.Transcript.Searching() {
			s.Transcript.SearchType(msg.Content)
			return s, nil
		}
		return s, s.Composer.Update(msg)
	case tea.MouseWheelMsg:
		_, cmd := s.Transcript.Update(msg)
		return s, cmd
	case tea.MouseClickMsg:
		switch {
		case msg.Y < s.split:
			_, cmd := s.Transcript.Update(msg)
			return s, cmd
		case msg.Y > s.split && msg.Button == tea.MouseLeft:
			cmd, _ := s.Composer.Click(msg.X, msg.Y-s.split-1)
			return s, cmd
		}
		return s, nil
	case tickMsg:
		_, cmd := s.Transcript.Update(msg)
		return s, cmd
	}
	_, cmd := s.Transcript.Update(msg)
	return s, cmd
}

// Carries out a keymap action on the search bar, the transcript or the prompt
func (s *Session) act(id string) tea.Cmd {
	tr := s.Transcript
	if strings.HasPrefix(id, "search.") {
		tr.SearchAction(id)
		return nil
	}
	switch id {
	case "session.search":
		tr.Search()
	case "session.scroll.up":
		tr.Page(-1)
	case "session.scroll.down":
		tr.Page(1)
	case "session.scroll.top":
		tr.Top()
	case "session.scroll.end":
		tr.Follow()
	case "session.expand":
		tr.ExpandAll(true)
	case "session.collapse":
		tr.ExpandAll(false)
	case "session.thinking":
		tr.ShowThinking(!tr.Thinking())
	case "mode.cycle":
		session := s.live.Session.ID()
		return func() tea.Msg { return CommandMsg{Session: session, Name: "mode"} }
	default:
		return s.Composer.Action(id)
	}
	return nil
}

func (s *Session) View(w, h int) string {
	s.w, s.h = w, h
	t := s.env.Theme
	s.Composer.SetBusy(s.busy())
	s.Composer.Fit(w)
	ch := min(s.Composer.Height(), max(h-3, 1))
	th := max(h-ch-1, 0)
	s.split = th
	var sb strings.Builder
	if th > 0 {
		sb.WriteString(s.Transcript.View(w, th))
		sb.WriteString("\n")
	}
	rule := lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Rule, w))
	if s.Transcript.SearchShown() {
		rule = s.Transcript.SearchView(w, "search")
	}
	sb.WriteString(rule)
	sb.WriteString("\n")
	sb.WriteString(s.Composer.View(w, ch))
	return sb.String()
}

// Cursor in the search bar while it takes typing, else in the prompt below the transcript and the rule
func (s *Session) Cursor() *tea.Cursor {
	if s.Transcript.Searching() {
		return tea.NewCursor(s.Transcript.SearchCursor(), s.split)
	}
	cur := s.Composer.Cursor()
	if cur == nil {
		return nil
	}
	cur.Y += s.split + 1
	return cur
}

// Transcript and prompt state, whether the session is ghost or bare, then the model it runs on
func (s *Session) Status() string {
	parts := []string{}
	if st := s.Transcript.Status(); st != "" {
		parts = append(parts, st)
	}
	if st := s.Composer.Status(); st != "" {
		parts = append(parts, st)
	}
	p := s.live.Endpoint.Profile
	if p.Ghost {
		parts = append(parts, "ghost")
	}
	if p.Bare {
		parts = append(parts, "bare")
	}
	if model := s.live.Endpoint.Model; model != "" {
		parts = append(parts, model)
	}
	return strings.Join(parts, " "+s.env.Theme.Glyphs.Sep+" ")
}

// Actions worth a reminder, sending and paging having their own button and scrollbar
func (s *Session) Hints() []wm.Hint {
	switch {
	case s.Transcript.Searching():
		return []wm.Hint{{Action: "search.accept"}, {Action: "search.next", Pair: "search.prev", Label: "matches"}, {Action: "search.close"}}
	case s.Composer.Completing():
		return []wm.Hint{{Action: "complete.accept"}, {Action: "complete.next", Pair: "complete.prev", Label: "choose"}, {Action: "complete.close"}}
	}
	return []wm.Hint{{Action: "session.search"}, {Action: "mode.cycle"}}
}
