package cards

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Page a caller describes: lines read on every view, keys of its own and whether it follows its last line
type PageSpec struct {
	Title, Key string
	// Lines at the width given, styled by the caller
	Lines func(w int) []string
	// Keymap context of the page's own actions and those actions by id
	Context string
	Actions map[string]func(p *Page) tea.Cmd
	// Legend of the page's own actions, ahead of the scroll keys
	Hints func(p *Page) []wm.Hint
	// Keeps the last line in view while the page sits at its end, for output that grows
	Follow bool
	// Runs when the page first shows
	Init func(p *Page) tea.Cmd
}

// Card of text lines scrolled with the list keys, such as help, a sign in in progress or the output of a command
type Page struct {
	env  *Env
	spec PageSpec
	top  int
	// Whether the page sits at its last line, kept there while it grows
	pinned bool
	// Whether the page is the help of a hub
	help bool
	n, h int
}

// Window manager interfaces the page answers
var _ interface {
	wm.Card
	wm.Keyed
	wm.Contexter
	wm.Hinter
	wm.Statuser
	wm.Selectable
} = (*Page)(nil)

// Builds a page from the spec, at its end when it follows its last line
func NewPage(env *Env, spec PageSpec) *Page {
	return &Page{env: env, spec: spec, pinned: spec.Follow}
}

func (p *Page) Title() string    { return p.spec.Title }
func (p *Page) Key() string      { return p.spec.Key }
func (p *Page) Selectable() bool { return true }

func (p *Page) Init() tea.Cmd {
	if p.spec.Init != nil {
		return p.spec.Init(p)
	}
	return nil
}

func (p *Page) Contexts() []string {
	if p.spec.Context != "" {
		return []string{p.spec.Context, keys.List}
	}
	return []string{keys.List}
}

func (p *Page) Hints() []wm.Hint {
	var out []wm.Hint
	if p.spec.Hints != nil {
		out = p.spec.Hints(p)
	}
	if p.n > p.h {
		out = append(out, wm.Hint{Action: "list.down", Pair: "list.up", Label: lang.T(txt.KitHintScroll)})
	}
	return out
}

// Count of lines out of view above and below
func (p *Page) Status() string {
	above, below := p.top, p.n-p.top-p.h
	if p.h <= 0 || (above <= 0 && below <= 0) {
		return ""
	}
	return wm.HiddenText(above, max(below, 0))
}

// Moves the view to its end
func (p *Page) End() { p.pinned = true }

func (p *Page) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.ActionMsg:
		if fn, ok := p.spec.Actions[msg.ID]; ok {
			return p, fn(p)
		}
		p.scroll(msg.ID)
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelUp {
			p.scroll("list.up")
		} else {
			p.scroll("list.down")
		}
	}
	return p, nil
}

// Moves the view for a list movement action
func (p *Page) scroll(id string) {
	last := max(p.n-p.h, 0)
	switch id {
	case "list.down":
		p.top++
	case "list.up":
		p.top--
	case "list.page.down":
		p.top += max(p.h-1, 1)
	case "list.page.up":
		p.top -= max(p.h-1, 1)
	case "list.top":
		p.top = 0
	case "list.bottom":
		p.top = last
	default:
		return
	}
	p.top = min(max(p.top, 0), last)
	p.pinned = p.spec.Follow && p.top == last
}

func (p *Page) View(w, h int) string {
	var lines []string
	if p.spec.Lines != nil {
		for _, line := range p.spec.Lines(max(w-2, 8)) {
			if line == "" {
				lines = append(lines, "")
				continue
			}
			lines = append(lines, wrapWords(line, max(w-2, 8))...)
		}
	}
	p.n, p.h = len(lines), h
	last := max(p.n-h, 0)
	if p.pinned {
		p.top = last
	}
	p.top = min(max(p.top, 0), last)
	shown := lines[p.top:min(p.top+h, len(lines))]
	out := make([]string, 0, h)
	for _, line := range shown {
		out = append(out, render.Fit(" "+line, w, p.env.Theme))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return strings.Join(slices.Clip(out), "\n")
}
