package app

import (
	"errors"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"

	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of a page following an operation of the hub
const review = "skills.review"

// Lines of a line of the hub, styled by its tone
func (s *screen) styled(l *skillsv1.Line) []string {
	t := s.kit.Env.Theme
	style := map[skillsv1.Tone]func(string) string{
		skillsv1.Tone_TONE_DIM:     func(text string) string { return render.Dim(text, t) },
		skillsv1.Tone_TONE_GOOD:    func(text string) string { return render.Good(text, t) },
		skillsv1.Tone_TONE_WARN:    func(text string) string { return render.Warm(text, t) },
		skillsv1.Tone_TONE_BAD:     func(text string) string { return render.Bad(text, t) },
		skillsv1.Tone_TONE_HEADING: func(text string) string { return render.Heading(text, t) },
		skillsv1.Tone_TONE_ACCENT:  func(text string) string { return render.Cool(text, t) },
	}[l.GetTone()]
	var out []string
	for _, line := range strings.Split(l.GetText(), "\n") {
		if style != nil && line != "" {
			line = style(line)
		}
		out = append(out, line)
	}
	return out
}

// A page following one operation of the hub: its lines as they come, and once it ends what it leaves the person to do, if anything
type operation struct {
	page  *cards.Page
	lines []string
	// What the page's action does and is called, nil while it offers none
	act   func() tea.Cmd
	label string
}

func (s *screen) operation(title string) *operation {
	op := &operation{}
	op.page = cards.NewPage(s.kit.Env, cards.PageSpec{
		Title:   title,
		Key:     Screen + ":run",
		Follow:  true,
		Context: review,
		Lines: func(int) []string {
			if len(op.lines) == 0 {
				return []string{render.Dim("Working…", s.kit.Env.Theme)}
			}
			return op.lines
		},
		Hints: func(*cards.Page) []wm.Hint {
			if op.act == nil {
				return nil
			}
			return []wm.Hint{{Action: "skills.go", Label: op.label}}
		},
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"skills.go": func(*cards.Page) tea.Cmd {
				act := op.act
				if act == nil {
					return nil
				}
				op.act = nil
				return act()
			},
		},
	})
	return op
}

func (op *operation) add(lines ...string) {
	op.lines = append(op.lines, lines...)
	op.page.End()
}

// Runs work off the UI loop, each line it says added to the page as it comes and what it ends with applied after them
func (s *screen) work(op *operation, busy string, work func(say func(*skillsv1.Line)) func() tea.Cmd) {
	s.hub.Busy(busy)
	go func() {
		finish := work(func(l *skillsv1.Line) {
			lines := s.styled(l)
			s.kit.Apply(func() tea.Cmd {
				op.add(lines...)
				return nil
			})
		})
		s.kit.Apply(func() tea.Cmd {
			s.hub.Quiet()
			return finish()
		})
	}()
}

// Shows an operation's page and runs its work
func (s *screen) run(title, busy string, work func(say func(*skillsv1.Line)) func() tea.Cmd) tea.Cmd {
	op := s.operation(title)
	s.work(op, busy, work)
	return s.hub.Push(op.page)
}

// What a stream that stops before its last message is told as
var errEarly = errors.New("The server stopped before it finished. Try again.")

// Reads a stream of lines to its last message, handing each line on
func receive[R any](recv func() (*R, error), parts func(*R) (*skillsv1.Line, bool), say func(*skillsv1.Line)) (*R, error) {
	for {
		response, err := recv()
		if errors.Is(err, io.EOF) {
			return nil, errEarly
		}
		if err != nil {
			return nil, err
		}
		line, done := parts(response)
		if line != nil {
			say(line)
		}
		if done {
			return response, nil
		}
	}
}

// Ends an operation that failed to run at all
func (s *screen) failed(err error) func() tea.Cmd {
	return func() tea.Cmd { return s.hub.Fail(tui.Reason(err)) }
}

// Runs a stream of lines on a page: an update, a scan or a publish
func (s *screen) lines(title, done string, ok func(bool) tea.Cmd, open func() (func() (bool, *skillsv1.Line, bool, error), func() error, error)) tea.Cmd {
	return s.run(title, title+"…", func(say func(*skillsv1.Line)) func() tea.Cmd {
		next, closer, err := open()
		if err != nil {
			return s.failed(err)
		}
		defer closer()
		for {
			finished, line, good, err := next()
			if err != nil {
				return s.failed(err)
			}
			if line != nil {
				say(line)
			}
			if finished {
				return func() tea.Cmd {
					note := s.hub.Done(done)
					if !good {
						note = s.hub.Fail("Not everything went through. The page says why.")
					}
					return tea.Batch(note, ok(good))
				}
			}
		}
	})
}

// Reader of a stream of hub lines with an ok flag
func linesOf[R interface {
	GetLine() *skillsv1.Line
	GetDone() bool
	GetOk() bool
}](recv func() (R, error)) func() (bool, *skillsv1.Line, bool, error) {
	return func() (bool, *skillsv1.Line, bool, error) {
		response, err := recv()
		if errors.Is(err, io.EOF) {
			return false, nil, false, errEarly
		}
		if err != nil {
			return false, nil, false, err
		}
		return response.GetDone(), response.GetLine(), response.GetOk(), nil
	}
}

// Updates the skills from the hub, or the one named, over the changes the person made when forced
func (s *screen) update(name string, force bool, after func() tea.Cmd) tea.Cmd {
	title, done := "Updating every skill", "Updated your skills from the hub."
	if name != "" {
		title, done = "Updating "+name, "Updated "+name+"."
	}
	return s.lines(title, done, func(bool) tea.Cmd {
		s.updated++
		return after()
	}, func() (func() (bool, *skillsv1.Line, bool, error), func() error, error) {
		stream, err := s.service.Update(s.kit.Env.Ctx, &skillsv1.UpdateRequest{Scope: s.scope(), Name: name, Force: force})
		if err != nil {
			return nil, nil, err
		}
		return linesOf(stream.Receive), stream.Close, nil
	})
}

// Scans the skills from the hub again
func (s *screen) audit() tea.Cmd {
	return s.lines("Scanning your skills", "Scanned your skills from the hub.", func(bool) tea.Cmd { return nil },
		func() (func() (bool, *skillsv1.Line, bool, error), func() error, error) {
			stream, err := s.service.Audit(s.kit.Env.Ctx, &skillsv1.AuditRequest{Scope: s.scope(), Deep: true})
			if err != nil {
				return nil, nil, err
			}
			return linesOf(stream.Receive), stream.Close, nil
		})
}

// A page of lines
func (s *screen) textPage(title string, lines []string) tea.Cmd {
	return s.hub.Push(cards.NewPage(s.kit.Env, cards.PageSpec{Title: title, Key: Screen + ":output", Lines: func(int) []string { return lines }}))
}

// Trusts a folder with its own skills, or stops trusting it, the note saying what happened; once it was done and the skills are read
// again, after puts what was made in hand
func (s *screen) trust(path string, untrust bool, after func()) tea.Cmd {
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*skillsv1.TrustResponse, error) {
		return s.service.Trust(s.kit.Env.Ctx, &skillsv1.TrustRequest{Scope: s.scope(), Path: path, Untrust: untrust})
	}, func(response *skillsv1.TrustResponse) tea.Cmd {
		if !response.GetOk() {
			return s.hub.Fail(response.GetSaid())
		}
		s.after = after
		return tea.Batch(s.hub.Done(response.GetSaid()), s.reload())
	})
}
