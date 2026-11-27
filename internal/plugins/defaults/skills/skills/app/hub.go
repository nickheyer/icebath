package app

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	hub "github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/skills"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of a page following an operation of the hub
const review = "skills.review"

// Skills a page of the skill hub shows
const hubPage = 50

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

// A page following one operation of the hub: its lines as they come, and once it ends what it leaves the user to do, if anything
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
		Lines: func(w int) []string {
			var out []string
			for _, line := range op.lines {
				out = append(out, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
			}
			if len(out) == 0 {
				out = append(out, render.Dim("Working…", s.kit.Env.Theme))
			}
			return out
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

// Reads a stream of lines to its last message, handing each line on
func receive[R any](recv func() (*R, error), parts func(*R) (*skillsv1.Line, bool), say func(*skillsv1.Line)) (*R, error) {
	for {
		response, err := recv()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("The server ended the operation early.")
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

// Fetches, quarantines and scans a skill on a page, then waits there for the user's yes; after runs once it is installed
func (s *screen) install(identifier, name, category string, after func() tea.Cmd) tea.Cmd {
	op := s.operation("Install " + identifier)
	s.stage(op, identifier, name, category, after)
	return s.hub.Push(op.page)
}

func (s *screen) stage(op *operation, identifier, name, category string, after func() tea.Cmd) {
	s.work(op, "Fetching "+identifier+"…", func(say func(*skillsv1.Line)) func() tea.Cmd {
		stream, err := s.service.Stage(s.kit.Env.Ctx, &skillsv1.StageRequest{Scope: s.scope(), Identifier: identifier, Name: name, Category: category})
		if err != nil {
			return s.failed(err)
		}
		defer stream.Close()
		final, err := receive(stream.Receive, func(r *skillsv1.StageResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, say)
		if err != nil {
			return s.failed(err)
		}
		return func() tea.Cmd { return s.staged(op, identifier, final.GetOutcome(), final.GetPending(), after) }
	})
}

// What a staged install leaves the user: a name to give, a skill to install, or its result
func (s *screen) staged(op *operation, identifier string, outcome skillsv1.Outcome, pending *skillsv1.Pending, after func() tea.Cmd) tea.Cmd {
	t := s.kit.Env.Theme
	switch outcome {
	case skillsv1.Outcome_OUTCOME_NEEDS_NAME:
		field := cards.Field{Label: "Skill name", Help: "The SKILL.md at " + pending.GetUrl() + " names no skill: lowercase letters, digits, hyphens and underscores, from a letter"}
		return s.kit.Ask(s.hub, "Name the skill", field, "install", func(name string) tea.Cmd {
			s.stage(op, identifier, name, "", after)
			return nil
		})
	case skillsv1.Outcome_OUTCOME_STAGED:
		s.waiting[pending.GetId()] = true
		where := pending.GetCategory()
		if where != "" {
			where += "/"
		}
		files := fmt.Sprintf("Files will be at: %s/%s%s/", pending.GetSkillsDir(), where, pending.GetName())
		op.add("")
		if pending.GetOfficial() {
			op.add(render.Heading("Official Skill", t), render.Cool("This is an official optional skill maintained by icebath.", t),
				"It ships with icebath but is not activated by default.", "Installing will copy it to your skills directory where the agent can use it.", files)
		} else {
			op.add(render.Heading("Disclaimer", t), render.Warm("You are installing a third-party skill at your own risk.", t),
				"External skills can contain instructions that influence agent behavior, shell commands, and scripts. Even after automated "+
					"scanning, you should review the installed files before use.", files)
		}
		op.act, op.label = func() tea.Cmd { return s.commit(op, pending, after) }, "install"
		return s.hub.Done("Read the scan, then " + s.kit.Env.Keys.Display("skills.go") + " installs " + pending.GetName())
	case skillsv1.Outcome_OUTCOME_INSTALLED:
		return tea.Batch(s.hub.Done("Installed "+identifier), after())
	case skillsv1.Outcome_OUTCOME_FAILED:
		return s.hub.Fail(identifier + " was not installed; see its output")
	}
	return s.hub.Done("Nothing to install: " + identifier)
}

var categoryName = regexp.MustCompile(`^[a-z][a-z0-9_/-]*$`)

// Installs a staged skill, asking its category first when it is a skill at a URL
func (s *screen) commit(op *operation, pending *skillsv1.Pending, after func() tea.Cmd) tea.Cmd {
	do := func(category string) tea.Cmd {
		delete(s.waiting, pending.GetId())
		s.work(op, "Installing "+pending.GetName()+"…", func(say func(*skillsv1.Line)) func() tea.Cmd {
			stream, err := s.service.Commit(s.kit.Env.Ctx, &skillsv1.CommitRequest{Scope: s.scope(), Id: pending.GetId(), Category: category})
			if err != nil {
				return s.failed(err)
			}
			defer stream.Close()
			final, err := receive(stream.Receive, func(r *skillsv1.CommitResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, say)
			if err != nil {
				return s.failed(err)
			}
			return func() tea.Cmd {
				if final.GetOutcome() == skillsv1.Outcome_OUTCOME_INSTALLED {
					return tea.Batch(s.hub.Done("Installed "+pending.GetName()), after())
				}
				return s.hub.Fail(pending.GetName() + " was not installed; see its output")
			}
		})
		return nil
	}
	if !pending.GetAskCategory() {
		return do("")
	}
	help := "Empty installs it flat in " + pending.GetSkillsDir()
	if existing := pending.GetCategories(); len(existing) > 0 {
		help = "Existing: " + strings.Join(existing, ", ") + "; empty installs it flat"
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title: "Category of " + pending.GetName(),
		Fields: []cards.Field{{ID: "category", Label: "Category", Help: help, Check: func(value string) string {
			if value = strings.TrimSpace(value); value != "" && !categoryName.MatchString(value) {
				return "Lowercase letters, digits, hyphens, underscores and slashes, from a letter"
			}
			return ""
		}}},
		Submit: "install",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(s.hub.Close(form), do(strings.TrimSpace(values["category"])))
		},
	})
	return s.hub.Push(form)
}

// Drops the installs left waiting when the screen closes
func (s *screen) discardWaiting() tea.Cmd {
	ids := make([]string, 0, len(s.waiting))
	for id := range s.waiting {
		ids = append(ids, id)
	}
	clear(s.waiting)
	if len(ids) == 0 {
		return nil
	}
	ctx := s.kit.Env.Ctx
	go func() {
		for _, id := range ids {
			s.service.Discard(ctx, &skillsv1.DiscardRequest{Id: id})
		}
	}()
	return nil
}

// Skill hub: every skill the registries offer a page at a time, or those a search found; enter installs one
func (s *screen) hubPart(query string, page int) cards.ListSpec {
	var (
		items  []*skillsv1.HubSkill
		pages  int32
		loaded bool
		failed error
		list   *cards.List
	)
	page = max(page, 1)
	reload := func() tea.Cmd {
		if query != "" {
			s.hub.Busy("Searching the skill registries…")
		} else {
			s.hub.Busy("Reading the skill registries…")
		}
		return tui.Async(func() func() tea.Cmd {
			var found []*skillsv1.HubSkill
			var total int32
			err := s.browse(query, page, &found, &total)
			return func() tea.Cmd {
				s.hub.Quiet()
				if err == nil {
					items, pages = found, total
				}
				failed, loaded = err, true
				if list != nil {
					list.Refresh()
				}
				return nil
			}
		})
	}
	at := func(i int) *skillsv1.HubSkill {
		if i < 0 || i >= len(items) {
			return nil
		}
		return items[i]
	}
	t := s.kit.Env.Theme
	return cards.ListSpec{
		Search:  "Filter this page",
		Columns: []render.Column{{}, {Title: "Skill", Max: 30}, {Title: "Source", Max: 16}, {Title: "Trust"}, {Title: "What it does", Flex: 1}},
		Yield:   []int{3, 2},
		Rows: func() [][]string {
			out := make([][]string, len(items))
			for i, it := range items {
				mark := ""
				if it.GetInstalled() {
					mark = render.Good(t.Glyphs.Check, t)
				}
				out[i] = []string{mark, it.GetName(), it.GetLabel(), it.GetTrust(), strings.Join(strings.Fields(it.GetDescription()), " ")}
			}
			return out
		},
		Detail: func(i int) []string {
			it := at(i)
			if it == nil {
				return nil
			}
			lines := []string{it.GetName(), it.GetDescription(), "From: " + it.GetLabel() + render.Dim("  trust "+it.GetTrust(), t), it.GetIdentifier()}
			if tags := it.GetTags(); len(tags) > 0 {
				lines = append(lines, render.Dim("Tags: "+strings.Join(tags, ", "), t))
			}
			for _, detail := range it.GetDetails() {
				lines = append(lines, render.Dim(detail, t))
			}
			if it.GetInstalled() {
				lines = append(lines, render.Good("Installed", t))
			}
			return lines
		},
		Status: func() string {
			if query != "" {
				return fmt.Sprintf("search: %s · %d found", query, len(items))
			}
			if pages > 1 {
				return fmt.Sprintf("page %d of %d", page, pages)
			}
			return ""
		},
		Empty: func() string {
			if query != "" {
				return tui.Waiting(loaded, failed, "Nothing in the registries matches "+query)
			}
			return tui.Waiting(loaded, failed, "The skill registries listed nothing; the network may be off.")
		},
		Free: map[string]bool{"hub.add": true, "skills.update": true, "skills.search": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			var hints []wm.Hint
			if at(i) != nil {
				hints = append(hints, wm.Hint{Action: "hub.use", Label: "install"}, wm.Hint{Action: "hub.edit", Label: "read"})
			}
			hints = append(hints, wm.Hint{Action: "hub.add", Label: "install by name"}, wm.Hint{Action: "skills.search", Label: "search"})
			if query == "" {
				hints = append(hints, wm.Hint{Action: "skills.update", Label: "next page"})
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if it := at(i); it != nil {
					return s.install(cmp.Or(it.GetIdentifier(), it.GetName()), "", "", reload)
				}
				return nil
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if it := at(i); it != nil {
					return s.inspectHub(cmp.Or(it.GetIdentifier(), it.GetName()), reload)
				}
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				return s.kit.Ask(s.hub, "Install by name", cards.Field{Label: "Skill", Help: "A name, an identifier such as openai/skills/skill-creator, or the URL of a SKILL.md"},
					"install", func(id string) tea.Cmd { return s.install(id, "", "", reload) })
			},
			"skills.search": func(*cards.List, int) tea.Cmd {
				var form *cards.Form
				form = cards.NewForm(s.kit.Env, cards.FormSpec{
					Title: "Search the skill registries",
					Fields: []cards.Field{{ID: "query", Label: "Search", Value: query,
						Help: "Words the skill's name, description or tags hold; empty browses every registry again"}},
					Submit: "search",
					Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
						query, page = strings.TrimSpace(values["query"]), 1
						return tea.Batch(s.hub.Close(form), reload())
					},
				})
				return s.hub.Push(form)
			},
			"skills.update": func(*cards.List, int) tea.Cmd {
				if query != "" {
					return nil
				}
				if pages > 0 {
					page = int(int32(page)%pages) + 1
				}
				return reload()
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(l *cards.List) tea.Cmd {
			list = l
			if loaded {
				return nil
			}
			return reload()
		},
	}
}

// One page of browsing, or the matches of a search
func (s *screen) browse(query string, page int, into *[]*skillsv1.HubSkill, pages *int32) error {
	ctx := s.kit.Env.Ctx
	if query != "" {
		response, err := s.service.Search(ctx, &skillsv1.SearchRequest{Scope: s.scope(), Query: query, Limit: hubPage})
		*into = response.GetSkills()
		return err
	}
	stream, err := s.service.Browse(ctx, &skillsv1.BrowseRequest{Scope: s.scope(), Page: int32(page), Size: hubPage, Screen: true})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		response, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if p := response.GetPage(); p != nil {
			*into, *pages = p.GetSkills(), p.GetPages()
		}
	}
}

// A skill of the hub read without installing it, enter installing it
func (s *screen) inspectHub(identifier string, after func() tea.Cmd) tea.Cmd {
	op := s.operation("Skill " + identifier)
	t := s.kit.Env.Theme
	s.work(op, "Reading "+identifier+"…", func(say func(*skillsv1.Line)) func() tea.Cmd {
		stream, err := s.service.Inspect(s.kit.Env.Ctx, &skillsv1.InspectRequest{Scope: s.scope(), Identifier: identifier})
		if err != nil {
			return s.failed(err)
		}
		defer stream.Close()
		final, err := receive(stream.Receive, func(r *skillsv1.InspectResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, say)
		if err != nil {
			return s.failed(err)
		}
		return func() tea.Cmd {
			skill := final.GetSkill()
			if skill == nil {
				return s.hub.Fail("Could not find " + identifier + " in any source")
			}
			op.add(render.Heading(skill.GetName(), t), skill.GetDescription(), "Source: "+skill.GetSource()+render.Dim("  trust "+skill.GetTrust(), t),
				"Identifier: "+skill.GetIdentifier())
			if tags := skill.GetTags(); len(tags) > 0 {
				op.add("Tags: " + strings.Join(tags, ", "))
			}
			op.add(skill.GetDetails()...)
			if preview := final.GetPreview(); preview != "" {
				op.add("", render.Heading("SKILL.md Preview", t))
				op.add(strings.Split(preview, "\n")...)
			}
			op.act, op.label = func() tea.Cmd { return s.install(skill.GetIdentifier(), "", "", after) }, "install"
			return nil
		}
	})
	return s.hub.Push(op.page)
}

// What /skills inspect shows: the installed skill of the name, else the hub's
func (s *screen) inspect(name string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := s.service.Show(s.kit.Env.Ctx, &skillsv1.ShowRequest{Scope: s.scope(), Name: name})
		return func() tea.Cmd {
			if err == nil {
				return s.viewPage(name, response)
			}
			if connect.CodeOf(err) != connect.CodeNotFound {
				return s.hub.Fail(tui.Reason(err))
			}
			return s.inspectHub(name, func() tea.Cmd { return nil })
		}
	})
}

// Skill sources: GitHub repositories the skill hub searches besides its default registries
func (s *screen) sources() cards.ListSpec {
	var (
		taps   []*skillsv1.SkillTap
		loaded bool
		failed error
		list   *cards.List
	)
	reload := func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := s.service.Taps(s.kit.Env.Ctx, &skillsv1.TapsRequest{Scope: s.scope()})
			return func() tea.Cmd {
				if err == nil {
					taps = response.GetTaps()
				}
				failed, loaded = err, true
				if list != nil {
					list.Refresh()
				}
				return nil
			}
		})
	}
	tap := func(repo string, remove bool) tea.Cmd {
		return tui.Call(s.hub, func() (*skillsv1.TapResponse, error) {
			return s.service.Tap(s.kit.Env.Ctx, &skillsv1.TapRequest{Scope: s.scope(), Repo: repo, Remove: remove})
		}, func(response *skillsv1.TapResponse) tea.Cmd {
			switch {
			case remove && response.GetChanged():
				return tea.Batch(s.hub.Done("Removed tap: "+repo), reload())
			case remove:
				return s.hub.Fail("Tap not found: " + repo)
			case response.GetChanged():
				return tea.Batch(s.hub.Done("Added tap: "+repo), reload())
			}
			return s.hub.Done("Tap already exists: " + repo)
		})
	}
	return cards.ListSpec{
		Columns: []render.Column{{Title: "Source", Flex: 1}, {Title: "Where", Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(taps))
			for i, t := range taps {
				out[i] = []string{t.GetRepo(), cmp.Or(t.GetPath(), "skills/")}
			}
			return out
		},
		Detail: func(int) []string {
			return []string{"The skill hub searches this GitHub repository for skills beside its default registries"}
		},
		Empty: func() string {
			return tui.Waiting(loaded, failed, "No extra sources. "+s.kit.Env.Keys.Display("hub.add")+" adds a GitHub repository of skills.")
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			hints := []wm.Hint{{Action: "hub.add", Label: "add"}}
			if i >= 0 {
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "remove"})
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd {
				return s.kit.Ask(s.hub, "Add a skill source", cards.Field{Label: "GitHub repository", Help: "owner/repo"}, "add", func(repo string) tea.Cmd {
					return tap(repo, false)
				})
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				repo := taps[i].GetRepo()
				l.Confirm("Stop searching "+repo+"?", func() tea.Cmd { return tap(repo, true) })
				return nil
			},
			"hub.use":     func(*cards.List, int) tea.Cmd { return nil },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(l *cards.List) tea.Cmd {
			list = l
			return reload()
		},
	}
}

// Runs a stream of lines on a page: an update, an audit or a publish
func (s *screen) lines(title string, ok func(bool) tea.Cmd, open func() (func() (bool, *skillsv1.Line, bool, error), func() error, error)) tea.Cmd {
	return s.run(title, "Running "+title+"…", func(say func(*skillsv1.Line)) func() tea.Cmd {
		next, done, err := open()
		if err != nil {
			return s.failed(err)
		}
		defer done()
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
					note := s.hub.Done(title + " finished.")
					if !good {
						note = s.hub.Fail(title + " did not finish cleanly; see its output.")
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
			return false, nil, false, errors.New("The server ended the operation early.")
		}
		if err != nil {
			return false, nil, false, err
		}
		return response.GetDone(), response.GetLine(), response.GetOk(), nil
	}
}

// Updates the hub skills, or the one named
func (s *screen) update(name string, after func() tea.Cmd) tea.Cmd {
	title := "Update every skill"
	if name != "" {
		title = "update " + name
	}
	return s.lines(title, func(bool) tea.Cmd { return after() }, func() (func() (bool, *skillsv1.Line, bool, error), func() error, error) {
		stream, err := s.service.Update(s.kit.Env.Ctx, &skillsv1.UpdateRequest{Scope: s.scope(), Name: name})
		if err != nil {
			return nil, nil, err
		}
		return linesOf(stream.Receive), stream.Close, nil
	})
}

// Scans the hub skills again
func (s *screen) audit() tea.Cmd {
	return s.lines("Audit", func(bool) tea.Cmd { return nil }, func() (func() (bool, *skillsv1.Line, bool, error), func() error, error) {
		stream, err := s.service.Audit(s.kit.Env.Ctx, &skillsv1.AuditRequest{Scope: s.scope(), Deep: true})
		if err != nil {
			return nil, nil, err
		}
		return linesOf(stream.Receive), stream.Close, nil
	})
}

// Shares a local skill on GitHub as a pull request to the repository asked for
func (s *screen) publish(skill *skillsv1.Skill) tea.Cmd {
	dir := filepath.Dir(skill.GetPath())
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  "Publish " + skill.GetName(),
		Intro:  []string{"share this skill on GitHub as a pull request to a skills repository", "path: " + dir},
		Fields: []cards.Field{{ID: "repo", Label: "GitHub repository", Help: "owner/name, such as openai/skills", Required: true}},
		Submit: "publish",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			repo := strings.TrimSpace(values["repo"])
			return tea.Batch(s.hub.Close(form), s.lines("Publish "+skill.GetName(), func(bool) tea.Cmd { return nil },
				func() (func() (bool, *skillsv1.Line, bool, error), func() error, error) {
					stream, err := s.service.Publish(s.kit.Env.Ctx, &skillsv1.PublishRequest{Scope: s.scope(), Path: dir, To: "github", Repo: repo})
					if err != nil {
						return nil, nil, err
					}
					return linesOf(stream.Receive), stream.Close, nil
				}))
		},
	})
	return s.hub.Push(form)
}

// The hub skills checked against their sources, on a page
func (s *screen) check() tea.Cmd {
	return tui.Call(s.hub, func() (*skillsv1.CheckResponse, error) {
		return s.service.Check(s.kit.Env.Ctx, &skillsv1.CheckRequest{Scope: s.scope()})
	}, func(response *skillsv1.CheckResponse) tea.Cmd {
		t := s.kit.Env.Theme
		var lines []string
		updates := 0
		for _, row := range response.GetSkills() {
			status := row.GetStatus()
			switch status {
			case "update_available":
				updates++
				status = render.Warm(status, t)
			case "up_to_date":
				status = render.Good(status, t)
			default:
				status = render.Dim(status, t)
			}
			lines = append(lines, fmt.Sprintf("%s  %s  %s", row.GetName(), render.Dim(row.GetSource(), t), status))
		}
		if len(lines) == 0 {
			lines = []string{render.Dim("No hub-installed skills to check.", t)}
		} else {
			lines = append(lines, "", render.Dim(fmt.Sprintf("%d update(s) available across %d checked skill(s)", updates, len(response.GetSkills())), t))
		}
		return s.textPage("Skill Updates", lines)
	})
}

// A page of lines
func (s *screen) textPage(title string, lines []string) tea.Cmd {
	page := cards.NewPage(s.kit.Env, cards.PageSpec{Title: title, Key: Screen + ":output", Lines: func(w int) []string {
		var out []string
		for _, line := range lines {
			out = append(out, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
		}
		return out
	}})
	return s.hub.Push(page)
}

// Saves the hub's skills and sources to a file of this machine
func (s *screen) export() tea.Cmd {
	return s.kit.Ask(s.hub, "Save the list", cards.Field{Label: "File", Value: "skills-snapshot.json", Help: "The installed hub skills and sources, to install elsewhere"},
		"save", func(path string) tea.Cmd {
			return tui.Call(s.hub, func() (*skillsv1.ExportResponse, error) {
				response, err := s.service.Export(s.kit.Env.Ctx, &skillsv1.ExportRequest{Scope: s.scope()})
				if err == nil {
					err = os.WriteFile(s.local(path), []byte(response.GetSnapshot()), 0o644)
				}
				return response, err
			}, func(response *skillsv1.ExportResponse) tea.Cmd {
				return s.hub.Done(fmt.Sprintf("Snapshot exported: %s (%d skill(s), %d tap(s))", s.local(path), response.GetSkills(), response.GetTaps()))
			})
		})
}

// A path of this machine, a relative one taken from the folder of the session in view
func (s *screen) local(path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.kit.Env.WorkDir(), path)
	}
	return filepath.Clean(path)
}

// Adds the sources of a snapshot file and installs its skills, without asking
func (s *screen) importList(after func() tea.Cmd) tea.Cmd {
	return s.kit.Ask(s.hub, "Install from a list", cards.Field{Label: "File", Help: "A snapshot saved by Save the list or icebath skills snapshot export"}, "install",
		func(path string) tea.Cmd {
			path = s.local(path)
			return s.run("Install from a list", "Installing from "+path+"…", func(say func(*skillsv1.Line)) func() tea.Cmd {
				ok, err := s.importSnapshot(path, say)
				if err != nil {
					return s.failed(err)
				}
				return func() tea.Cmd {
					if !ok {
						return tea.Batch(s.hub.Fail("Install from a list did not finish cleanly; see its output."), after())
					}
					return tea.Batch(s.hub.Done("Install from a list finished."), after())
				}
			})
		})
}

func (s *screen) importSnapshot(path string, say func(*skillsv1.Line)) (bool, error) {
	ctx := s.kit.Env.Ctx
	note := func(tone skillsv1.Tone, format string, args ...any) {
		say(&skillsv1.Line{Text: fmt.Sprintf(format, args...), Tone: tone})
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		note(skillsv1.Tone_TONE_BAD, "Error: File not found: %s", path)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	snap, err := hub.ReadSnapshot(data)
	if err != nil {
		note(skillsv1.Tone_TONE_BAD, "Error: Invalid JSON in %s", path)
		return false, nil
	}
	for _, tap := range snap.Taps {
		if tap.Repo != "" {
			if _, err := s.service.Tap(ctx, &skillsv1.TapRequest{Scope: s.scope(), Repo: tap.Repo}); err != nil {
				return false, err
			}
		}
	}
	if len(snap.Taps) > 0 {
		note(skillsv1.Tone_TONE_DIM, "Restored %d tap(s)", len(snap.Taps))
	}
	if len(snap.Skills) == 0 {
		note(skillsv1.Tone_TONE_DIM, "No skills in snapshot to install.")
		return true, nil
	}
	note(skillsv1.Tone_TONE_HEADING, "Importing %d skill(s) from snapshot...", len(snap.Skills))
	var failed []string
	for _, skill := range snap.Skills {
		if skill.Identifier == "" {
			note(skillsv1.Tone_TONE_WARN, "Skipping entry with no identifier: %s", cmp.Or(skill.Name, "?"))
			continue
		}
		label := cmp.Or(skill.Name, skill.Identifier)
		note(skillsv1.Tone_TONE_HEADING, "--- %s ---", label)
		stream, err := s.service.Stage(ctx, &skillsv1.StageRequest{Scope: s.scope(), Identifier: skill.Identifier, Category: skill.Category, Yes: true})
		if err != nil {
			return false, err
		}
		final, err := receive(stream.Receive, func(r *skillsv1.StageResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, say)
		stream.Close()
		if err != nil {
			return false, err
		}
		if final.GetOutcome() == skillsv1.Outcome_OUTCOME_FAILED {
			failed = append(failed, label)
		}
	}
	if len(failed) > 0 {
		note(skillsv1.Tone_TONE_BAD, "Snapshot import incomplete. Not installed: %s (see the messages above).", strings.Join(failed, ", "))
		return false, nil
	}
	note(skillsv1.Tone_TONE_GOOD, "Snapshot import complete.")
	return true, nil
}

// Commands over every installed skill
func (s *screen) every(optedOut bool, reload func() tea.Cmd) tea.Cmd {
	opt, optTitle := "opt-out", "Stop copying bundled skills"
	if optedOut {
		opt, optTitle = "opt-in", "Copy bundled skills again"
	}
	choices := []tui.Choice{
		{ID: "update", Title: "Update every skill", Detail: "move every hub skill to its newest version"},
		{ID: "check", Title: "Check for updates", Detail: "see which hub skills have a newer version"},
		{ID: "audit", Title: "Audit", Detail: "scan the installed hub skills again"},
		{ID: "modified", Title: "Skills you changed", Detail: "bundled skills you edited, which updates keep"},
		{ID: opt, Title: optTitle, Detail: "whether icebath copies its bundled skills into this profile"},
		{ID: "export", Title: "Save the list", Detail: "write the installed skills to a file to install elsewhere"},
		{ID: "import", Title: "Install from a list", Detail: "install every skill a saved list names"},
	}
	return s.kit.Pick(s.hub, "every skill", "run", choices, "", func(choice tui.Choice) tea.Cmd {
		switch choice.ID {
		case "update":
			return s.update("", reload)
		case "check":
			return s.check()
		case "audit":
			return s.audit()
		case "modified":
			return s.modified()
		case "opt-out", "opt-in":
			return tui.Call(s.hub, func() (*skillsv1.OptOutResponse, error) {
				return s.service.OptOut(s.kit.Env.Ctx, &skillsv1.OptOutRequest{Scope: s.scope(), Out: choice.ID == "opt-out", Sync: choice.ID == "opt-in"})
			}, func(response *skillsv1.OptOutResponse) tea.Cmd {
				return tea.Batch(s.hub.Done(response.GetMessage()), reload())
			})
		case "export":
			return s.export()
		}
		return s.importList(reload)
	})
}

// Bundled skills the user changed, on a page
func (s *screen) modified() tea.Cmd {
	return tui.Call(s.hub, func() (*skillsv1.ModifiedResponse, error) {
		return s.service.Modified(s.kit.Env.Ctx, &skillsv1.ModifiedRequest{Scope: s.scope()})
	}, func(response *skillsv1.ModifiedResponse) tea.Cmd {
		t := s.kit.Env.Theme
		names := response.GetNames()
		if len(names) == 0 {
			return s.hub.Done("No user-modified bundled skills — everything tracks upstream.")
		}
		lines := []string{fmt.Sprintf("%d user-modified bundled skill(s) (kept as-is by syncs):", len(names))}
		for _, name := range names {
			lines = append(lines, render.Warm("  ~ ", t)+name)
		}
		lines = append(lines, "", render.Dim("Changes shows how a skill differs; Follow updates again drops your changes", t))
		return s.textPage("Skills you changed", lines)
	})
}

// How the profile's copy of a bundled skill differs from the one icebath ships, on a page
func (s *screen) diff(name string) tea.Cmd {
	return tui.Call(s.hub, func() (*skillsv1.DiffResponse, error) {
		return s.service.Diff(s.kit.Env.Ctx, &skillsv1.DiffRequest{Scope: s.scope(), Name: name})
	}, func(response *skillsv1.DiffResponse) tea.Cmd {
		t := s.kit.Env.Theme
		files := response.GetFiles()
		if len(files) == 0 {
			return s.hub.Done(fmt.Sprintf("'%s' matches the stock version.", name))
		}
		lines := []string{fmt.Sprintf("'%s' differs from the stock version in %d file(s).", name, len(files)), ""}
		for _, file := range files {
			switch file.GetStatus() {
			case "modified":
				for _, line := range strings.Split(strings.TrimRight(file.GetDiff(), "\n"), "\n") {
					switch {
					case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
						line = render.Good(line, t)
					case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
						line = render.Bad(line, t)
					case strings.HasPrefix(line, "@@"):
						line = render.Cool(line, t)
					}
					lines = append(lines, line)
				}
			case "added":
				lines = append(lines, render.Good("+ only in your copy: ", t)+file.GetPath())
			case "removed":
				lines = append(lines, render.Bad("- only in stock: ", t)+file.GetPath())
			default:
				lines = append(lines, render.Warm("~ "+file.GetPath()+": ", t)+"binary file differs")
			}
		}
		return s.textPage("Changes to "+name, lines)
	})
}

// Uninstalls a skill the hub installed
func (s *screen) uninstall(l *cards.List, skill *skillsv1.Skill, reload func() tea.Cmd) tea.Cmd {
	if skill.GetSource() != "hub" {
		return s.hub.Fail(skill.GetName() + " did not come from the hub; turn it off instead.")
	}
	l.Confirm("Uninstall the skill "+skill.GetName()+"?", func() tea.Cmd {
		return tui.Call(s.hub, func() (*skillsv1.UninstallResponse, error) {
			return s.service.Uninstall(s.kit.Env.Ctx, &skillsv1.UninstallRequest{Scope: s.scope(), Name: skill.GetName()})
		}, func(response *skillsv1.UninstallResponse) tea.Cmd {
			if !response.GetOk() {
				return s.hub.Fail(response.GetMessage())
			}
			return tea.Batch(s.hub.Done(response.GetMessage()), reload())
		})
	})
	return nil
}

// What can be done with one installed skill, by where it comes from
func (s *screen) more(l *cards.List, skill *skillsv1.Skill, toggle func() tea.Cmd, reload func() tea.Cmd) tea.Cmd {
	choices := []tui.Choice{
		{ID: "toggle", Title: "Turn on or off", Detail: "let the agent use the skill or not"},
		{ID: "read", Title: "Read it", Detail: "what the skill says and where it lives"},
	}
	switch skill.GetSource() {
	case "local", "create_dir":
		choices = append(choices, tui.Choice{ID: "publish", Title: "Publish", Detail: "share this skill on GitHub as a pull request to a skills repository"})
	case "hub":
		choices = append(choices, tui.Choice{ID: "update", Title: "Update", Detail: "move it to its newest version"},
			tui.Choice{ID: "uninstall", Title: "Uninstall", Detail: "remove the skill installed from the hub"})
		if skill.GetTrust() == "builtin" {
			choices = append(choices, tui.Choice{ID: "restore", Title: "Restore it", Detail: "put the official copy back"})
		}
	case "builtin":
		choices = append(choices, tui.Choice{ID: "diff", Title: "Changes", Detail: "how your copy differs from the one icebath ships"},
			tui.Choice{ID: "reset", Title: "Follow updates again", Detail: "drop your changes so updates apply again"})
	}
	return s.kit.Pick(s.hub, skill.GetName(), "run", choices, "", func(choice tui.Choice) tea.Cmd {
		switch choice.ID {
		case "toggle":
			return toggle()
		case "read":
			return s.view(skill.GetName())
		case "publish":
			return s.publish(skill)
		case "update":
			return s.update(skill.GetName(), reload)
		case "uninstall":
			return s.uninstall(l, skill, reload)
		case "restore":
			return tui.Call(s.hub, func() (*skillsv1.RepairResponse, error) {
				return s.service.Repair(s.kit.Env.Ctx, &skillsv1.RepairRequest{Scope: s.scope(), Name: skill.GetName(), Restore: true})
			}, func(response *skillsv1.RepairResponse) tea.Cmd {
				note := "Official optional skill repair complete."
				if backed := response.GetBackedUp(); len(backed) > 0 {
					note += " Backed up: " + strings.Join(backed, ", ") + " in " + response.GetBackupDir()
				}
				return tea.Batch(s.hub.Done(note), reload())
			})
		case "diff":
			return s.diff(skill.GetName())
		}
		return s.reset(skill, reload)
	})
}

// Trusts a folder with its own skills, or stops trusting it, on a page of what happened
func (s *screen) trust(path string, untrust bool, after func() tea.Cmd) tea.Cmd {
	return tui.Call(s.hub, func() (*skillsv1.TrustResponse, error) {
		return s.service.Trust(s.kit.Env.Ctx, &skillsv1.TrustRequest{Scope: s.scope(), Path: path, Untrust: untrust})
	}, func(response *skillsv1.TrustResponse) tea.Cmd {
		var texts []string
		for _, line := range response.GetLines() {
			texts = append(texts, line.GetText())
		}
		note := strings.Join(texts, " ")
		if !response.GetOk() {
			return s.hub.Fail(note)
		}
		return tea.Batch(s.hub.Done(note), after())
	})
}

// The verb of /skills and the rest of its line
func verb(argument string) (string, string) {
	word, rest, _ := strings.Cut(strings.TrimSpace(argument), " ")
	return word, strings.TrimSpace(rest)
}

// Skill, name and category of the line of /skills install
func installArgs(rest string) (string, string, string) {
	fields := strings.Fields(rest)
	var identifier, name, category string
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "--name", "--category":
			if i+1 < len(fields) {
				if fields[i] == "--name" {
					name = fields[i+1]
				} else {
					category = fields[i+1]
				}
				i++
			}
		default:
			if identifier == "" {
				identifier = fields[i]
			}
		}
	}
	return identifier, name, category
}

// Page number of /skills browse, false when it is no positive number
func pageNumber(text string) (int, bool) {
	if text == "" {
		return 1, true
	}
	n, err := strconv.Atoi(text)
	return n, err == nil && n >= 1
}
