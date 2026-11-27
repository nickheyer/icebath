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

// Registries the skill hub searches, by the name the service takes, every one of them first
var registries = []struct{ id, title string }{
	{"all", "Every registry"}, {"official", "Official"}, {"skills-sh", "skills.sh"}, {"well-known", "Well-known sites"}, {"github", "GitHub"},
	{"clawhub", "ClawHub"}, {"lobehub", "LobeHub"}, {"browse-sh", "browse.sh"}, {"nvidia", "NVIDIA"}, {"openai", "OpenAI"}, {"anthropic", "Anthropic"},
	{"huggingface", "Hugging Face"}, {"voltagent", "VoltAgent"}, {"gstack", "gstack"}, {"minimax", "MiniMax"},
}

// Title of a registry, its name when the screen does not know it
func registry(id string) string {
	for _, r := range registries {
		if r.id == id {
			return r.title
		}
	}
	return id
}

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

// Fetches, quarantines and scans a skill on a page, then waits there for the user's yes; after runs once it is installed
func (s *screen) install(identifier, name, category string, after func() tea.Cmd) tea.Cmd {
	op := s.operation("Install " + identifier)
	s.stage(op, identifier, name, category, false, after)
	return s.hub.Push(op.page)
}

// Fetches and scans a skill on the page of the operation, over an installed copy and past what the scan found when forced
func (s *screen) stage(op *operation, identifier, name, category string, force bool, after func() tea.Cmd) {
	s.work(op, "Fetching "+identifier+"…", func(say func(*skillsv1.Line)) func() tea.Cmd {
		stream, err := s.service.Stage(s.kit.Env.Ctx, &skillsv1.StageRequest{Scope: s.scope(), Identifier: identifier, Name: name, Category: category,
			Force: force})
		if err != nil {
			return s.failed(err)
		}
		defer stream.Close()
		final, err := receive(stream.Receive, func(r *skillsv1.StageResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, say)
		if err != nil {
			return s.failed(err)
		}
		return func() tea.Cmd {
			return s.staged(op, identifier, name, category, final.GetOutcome(), final.GetPending(), after)
		}
	})
}

// What a staged install leaves the user: a name to give, a skill to install, its result, or the install to force
func (s *screen) staged(op *operation, identifier, name, category string, outcome skillsv1.Outcome, pending *skillsv1.Pending, after func() tea.Cmd) tea.Cmd {
	t := s.kit.Env.Theme
	switch outcome {
	case skillsv1.Outcome_OUTCOME_NEEDS_NAME:
		field := cards.Field{Label: "Name", Help: "The skill at " + pending.GetUrl() + " has no name of its own.\nUse lower case letters, digits, - and _, starting with a letter."}
		return s.kit.Ask(s.hub, "Name the skill", field, "Install", func(name string) tea.Cmd {
			s.stage(op, identifier, name, category, false, after)
			return nil
		})
	case skillsv1.Outcome_OUTCOME_STAGED:
		s.waiting[pending.GetId()] = true
		where := pending.GetCategory()
		if where != "" {
			where += "/"
		}
		files := fmt.Sprintf("Its files go in %s/%s%s/", pending.GetSkillsDir(), where, pending.GetName())
		op.add("")
		if pending.GetOfficial() {
			op.add(render.Heading("An official skill", t), render.Cool("icebath makes this skill and ships it, off until you install it.", t),
				"Installing copies it to your skills folder, where the agent can use it.", files)
		} else {
			op.add(render.Heading("Before you install", t), render.Warm("This skill comes from someone other than icebath.", t),
				"A skill can tell the agent what to do, and can hold commands and scripts. The scan above looks for danger, but read its files before you use it.",
				files)
		}
		op.act, op.label = func() tea.Cmd { return s.commit(op, pending, after) }, "install"
		return s.hub.Done("Read the scan above. " + s.kit.Env.Keys.Display("skills.go") + " installs " + pending.GetName() + ".")
	case skillsv1.Outcome_OUTCOME_INSTALLED:
		return tea.Batch(s.hub.Done("Installed "+identifier+"."), after())
	case skillsv1.Outcome_OUTCOME_FAILED:
		op.act, op.label = func() tea.Cmd {
			return s.force(op, identifier, name, category, "Install "+identifier+" anyway?",
				"Install it even though the scan stopped it, or it couldn't be fetched cleanly", after)
		}, "install anyway"
		return s.hub.Fail(identifier + " was not installed. The output above says why.")
	}
	op.act, op.label = func() tea.Cmd {
		return s.force(op, identifier, name, category, "Install "+identifier+" again?", "Replace the copy you have with a fresh one", after)
	}, "reinstall"
	return s.hub.Done("Nothing to install. " + identifier + " is installed already.")
}

// Asks before an install that goes over an installed copy and past what the scan found, then runs it on the same page
func (s *screen) force(op *operation, identifier, name, category, question, about string, after func() tea.Cmd) tea.Cmd {
	choices := []tui.Choice{{ID: "yes", Title: "Install it", Detail: about}, {ID: "no", Title: "Leave it", Detail: "Nothing changes"}}
	return s.kit.Pick(s.hub, question, "choose", choices, "", func(choice tui.Choice) tea.Cmd {
		if choice.ID == "yes" {
			op.add("")
			s.stage(op, identifier, name, category, true, after)
		}
		return nil
	})
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
					return tea.Batch(s.hub.Done("Installed "+pending.GetName()+"."), after())
				}
				return s.hub.Fail(pending.GetName() + " was not installed. The output above says why.")
			}
		})
		return nil
	}
	if !pending.GetAskCategory() {
		return do("")
	}
	help := "Leave empty to put it at the top of " + pending.GetSkillsDir() + "."
	if existing := pending.GetCategories(); len(existing) > 0 {
		help = "Folders you have: " + strings.Join(existing, ", ") + ".\nLeave empty to put it at the top of your skills folder."
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title: "Category",
		Intro: []string{"The folder among your skills that " + pending.GetName() + " goes in."},
		Fields: []cards.Field{{ID: "category", Label: "Category", Value: pending.GetCategory(), Help: help, Check: func(value string) string {
			if value = strings.TrimSpace(value); value != "" && !categoryName.MatchString(value) {
				return "Use lower case letters, digits, -, _ and /, starting with a letter."
			}
			return ""
		}}},
		Submit: "Install",
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

// One page of the skill hub: what a browse or a search found, and the registries that were too slow
type found struct {
	skills []*skillsv1.HubSkill
	pages  int32
	late   []string
}

// Row of the skill hub: a skill, or a step to the next or the page before
type hubRow struct {
	skill *skillsv1.HubSkill
	step  int
}

// Skill hub: every skill the registries offer a page at a time, or those a search found; enter installs one
func (s *screen) hubPart() cards.ListSpec {
	var (
		page   found
		loaded bool
		failed error
		list   *cards.List
		rows   []hubRow
	)
	s.page = max(s.page, 1)
	reload := func() tea.Cmd {
		if s.query != "" {
			s.hub.Busy("Searching the skill registries…")
		} else {
			s.hub.Busy("Reading the skill registries…")
		}
		query, source, at := s.query, s.source, s.page
		return tui.Async(func() func() tea.Cmd {
			got, err := s.browse(query, source, at)
			return func() tea.Cmd {
				s.hub.Quiet()
				if err == nil {
					page = got
				}
				failed, loaded = err, true
				if list != nil {
					list.Refresh()
				}
				return nil
			}
		})
	}
	t := s.kit.Env.Theme
	return cards.ListSpec{
		Title:   "Skill hub",
		Context: cards.HubContext,
		Shared:  []string{context},
		Pick:    true,
		Bar:     true,
		Search:  "Search this page",
		Intro: func() []string {
			lines := []string{"Skills you can install, from the registries of the skill hub. " + s.kit.Env.Keys.Display("skills.search") + " searches them all."}
			if s.source != "" && s.source != "all" {
				lines = append(lines, "Only from "+registry(s.source)+".")
			}
			if len(page.late) > 0 {
				lines = append(lines, "These registries were too slow and were left out: "+strings.Join(page.late, ", ")+". "+
					s.kit.Env.Keys.Display("hub.refresh")+" tries again.")
			}
			return lines
		},
		Status: func() string {
			if s.query != "" {
				return "Results for “" + s.query + "”"
			}
			if page.pages > 1 {
				return fmt.Sprintf("Page %d of %d", s.page, page.pages)
			}
			return ""
		},
		Columns: []render.Column{{}, {}, {}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows = rows[:0]
			for _, skill := range page.skills {
				rows = append(rows, hubRow{skill: skill})
			}
			if s.query == "" && int32(s.page) < page.pages {
				rows = append(rows, hubRow{step: 1})
			}
			if s.query == "" && s.page > 1 {
				rows = append(rows, hubRow{step: -1})
			}
			out := make([][]string, len(rows))
			for i, r := range rows {
				switch {
				case r.step > 0:
					out[i] = []string{"Next page", "", "", "", render.Dim(fmt.Sprintf("Page %d of %d", s.page+1, page.pages), t)}
				case r.step < 0:
					out[i] = []string{"Page before", "", "", "", render.Dim(fmt.Sprintf("Page %d of %d", s.page-1, page.pages), t)}
				default:
					state := ""
					if r.skill.GetInstalled() {
						state = render.Good("Installed", t)
					}
					out[i] = []string{r.skill.GetName(), render.Dim(r.skill.GetLabel(), t), cmp.Or(trustWords[r.skill.GetTrust()], r.skill.GetTrust()), state,
						render.Dim(oneLine(r.skill.GetDescription()), t)}
				}
			}
			return out
		},
		Words: func(i int) string {
			if skill := rows[i].skill; skill != nil {
				return strings.Join(skill.GetTags(), " ") + " " + skill.GetIdentifier() + " " + skill.GetDescription()
			}
			return ""
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(rows) || rows[i].skill == nil {
				return nil
			}
			it := rows[i].skill
			var lines []string
			if !list.Shows(4) {
				lines = append(lines, oneLine(it.GetDescription()))
			}
			from := "From " + it.GetLabel() + "."
			if trust := trustWords[it.GetTrust()]; trust != "" {
				from += " " + trust + "."
			}
			lines = append(lines, from, render.Dim("Identifier: "+it.GetIdentifier(), t))
			if tags := it.GetTags(); len(tags) > 0 {
				lines = append(lines, render.Dim("Tags: "+strings.Join(tags, ", "), t))
			}
			for _, detail := range it.GetDetails() {
				lines = append(lines, render.Dim(detail, t))
			}
			if it.GetInstalled() {
				lines = append(lines, render.Good("Installed in this profile.", t))
			}
			return lines
		},
		Empty: func() string {
			if s.query != "" {
				return tui.Waiting(loaded, failed, "Nothing in the registries matches “"+s.query+"”. "+s.kit.Env.Keys.Display("skills.search")+" searches again.")
			}
			return tui.Waiting(loaded, failed, "The registries listed nothing. Check your network, then "+s.kit.Env.Keys.Display("hub.refresh")+" tries again.")
		},
		Free: map[string]bool{"hub.add": true, "skills.search": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			others := []wm.Hint{{Action: "hub.add", Label: "install by name"}, {Action: "skills.search", Label: "search"}}
			if i < 0 || i >= len(rows) {
				return others
			}
			if rows[i].skill == nil {
				return append([]wm.Hint{{Action: "hub.use", Label: "show"}}, others...)
			}
			if rows[i].skill.GetInstalled() {
				return append([]wm.Hint{{Action: "hub.use", Label: "read"}}, others...)
			}
			return append([]wm.Hint{{Action: "hub.use", Label: "install"}, {Action: "hub.edit", Label: "read"}}, others...)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch r := rows[i]; {
				case r.step != 0:
					s.page += r.step
					return reload()
				case r.skill.GetInstalled():
					return s.inspectHub(cmp.Or(r.skill.GetIdentifier(), r.skill.GetName()), reload)
				default:
					return s.install(cmp.Or(r.skill.GetIdentifier(), r.skill.GetName()), "", "", reload)
				}
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if skill := rows[i].skill; skill != nil {
					return s.inspectHub(cmp.Or(skill.GetIdentifier(), skill.GetName()), reload)
				}
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				return s.kit.Ask(s.hub, "Install by name", cards.Field{Label: "Skill",
					Help: "A name, an identifier such as openai/skills/skill-creator, or the address of a SKILL.md."},
					"Install", func(id string) tea.Cmd { return s.install(id, "", "", reload) })
			},
			"skills.search": func(*cards.List, int) tea.Cmd { return s.search(reload) },
			"hub.refresh":   func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(l *cards.List) tea.Cmd {
			first := list == nil
			list = l
			if first {
				return reload()
			}
			return nil
		},
	}
}

// Form that searches the registries for words, in every registry or one of them
func (s *screen) search(reload func() tea.Cmd) tea.Cmd {
	where := cards.Field{ID: "source", Label: "Where", Kind: cards.FieldChoice, Value: cmp.Or(s.source, "all")}
	for _, r := range registries {
		where.Choices = append(where.Choices, cards.Choice{Value: r.id, Label: r.title})
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title: "Search",
		Intro: []string{"Search the registries of the skill hub by name, description and tags."},
		Fields: []cards.Field{{ID: "query", Label: "Words", Value: s.query, Help: "Leave empty to browse every skill again."},
			where},
		Submit: "Search",
		Pick: func(f *cards.Form, field cards.Field) tea.Cmd {
			choices := make([]tui.Choice, len(field.Choices))
			for i, choice := range field.Choices {
				choices[i] = tui.Choice{ID: choice.Value, Title: choice.Label}
			}
			return s.kit.Pick(s.hub, "Where to search", "choose", choices, field.Value, func(choice tui.Choice) tea.Cmd {
				return f.Commit(field.ID, choice.ID)
			})
		},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			s.query, s.source, s.page = strings.TrimSpace(values["query"]), values["source"], 1
			if s.source == "all" {
				s.source = ""
			}
			return tea.Batch(s.hub.Close(form), reload())
		},
	})
	return s.hub.Push(form)
}

// One page of browsing, or the matches of a search, in every registry or the one named
func (s *screen) browse(query, source string, page int) (found, error) {
	ctx := s.kit.Env.Ctx
	if query != "" {
		response, err := s.service.Search(ctx, &skillsv1.SearchRequest{Scope: s.scope(), Query: query, Source: source, Limit: hubPage})
		return found{skills: response.GetSkills()}, err
	}
	stream, err := s.service.Browse(ctx, &skillsv1.BrowseRequest{Scope: s.scope(), Page: int32(page), Size: hubPage, Screen: true, Source: source})
	if err != nil {
		return found{}, err
	}
	defer stream.Close()
	var out found
	for {
		response, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if p := response.GetPage(); p != nil {
			out = found{skills: p.GetSkills(), pages: p.GetPages()}
			for _, late := range p.GetTimedOut() {
				out.late = append(out.late, registry(late))
			}
		}
	}
}

// Text of several lines and runs of spaces as one line
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// A skill of the hub read without installing it, enter installing it
func (s *screen) inspectHub(identifier string, after func() tea.Cmd) tea.Cmd {
	op := s.operation(identifier)
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
				return s.hub.Fail("No registry has " + identifier + ". Check the name, or search for it.")
			}
			from := "From " + cmp.Or(skill.GetLabel(), skill.GetSource()) + "."
			if trust := trustWords[skill.GetTrust()]; trust != "" {
				from += " " + trust + "."
			}
			op.add(render.Heading(skill.GetName(), t), skill.GetDescription(), from, render.Dim("Identifier: "+skill.GetIdentifier(), t))
			if tags := skill.GetTags(); len(tags) > 0 {
				op.add(render.Dim("Tags: "+strings.Join(tags, ", "), t))
			}
			op.add(skill.GetDetails()...)
			if preview := final.GetPreview(); preview != "" {
				op.add("", render.Heading("The start of its SKILL.md", t))
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
				return s.readPage(name, response)
			}
			if connect.CodeOf(err) != connect.CodeNotFound {
				return s.hub.Fail(tui.Reason(err))
			}
			return s.inspectHub(name, func() tea.Cmd { return nil })
		}
	})
}

// Skill sources: GitHub repositories the skill hub searches besides its own registries
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
		s.hub.Busy("Saving…")
		return tui.Call(s.hub, func() (*skillsv1.TapResponse, error) {
			return s.service.Tap(s.kit.Env.Ctx, &skillsv1.TapRequest{Scope: s.scope(), Repo: repo, Remove: remove})
		}, func(response *skillsv1.TapResponse) tea.Cmd {
			switch {
			case remove && response.GetChanged():
				return tea.Batch(s.hub.Done("Removed "+repo+"."), reload())
			case remove:
				return s.hub.Fail(repo + " is not one of your sources.")
			case response.GetChanged():
				return tea.Batch(s.hub.Done("Added "+repo+". The skill hub searches it now."), reload())
			}
			return s.hub.Done(repo + " is already one of your sources.")
		})
	}
	return cards.ListSpec{
		Title: "Sources",
		Intro: func() []string {
			return []string{"GitHub repositories the skill hub searches, besides its own registries."}
		},
		Columns: []render.Column{{}, {Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(taps))
			for i, t := range taps {
				out[i] = []string{t.GetRepo(), render.Dim("Skills in "+cmp.Or(t.GetPath(), "skills/"), s.kit.Env.Theme)}
			}
			return out
		},
		Empty: func() string {
			return tui.Waiting(loaded, failed, "No sources of your own yet. "+s.kit.Env.Keys.Display("hub.add")+" adds a GitHub repository of skills.")
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			add := wm.Hint{Action: "hub.add", Label: "add"}
			if i < 0 {
				return []wm.Hint{add}
			}
			return []wm.Hint{{Action: "hub.remove", Label: "remove"}, add}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd {
				field := cards.Field{Label: "Repository", Help: "A GitHub repository of skills, written owner/name, such as openai/skills."}
				return s.kit.Ask(s.hub, "Add a source", field, "Add", func(repo string) tea.Cmd { return tap(repo, false) })
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				repo := taps[i].GetRepo()
				l.Confirm("Remove "+repo+"? The skill hub stops searching it.", func() tea.Cmd { return tap(repo, true) })
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(l *cards.List) tea.Cmd {
			list = l
			return reload()
		},
	}
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
						note = s.hub.Fail("Not everything went through. The output above says why.")
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

// Updates the skills from the hub, or the one named, over the changes the user made when forced
func (s *screen) update(name string, force bool, after func() tea.Cmd) tea.Cmd {
	title, done := "Updating every skill", "Updated your skills from the hub."
	if name != "" {
		title, done = "Updating "+name, "Updated "+name+"."
	}
	return s.lines(title, done, func(bool) tea.Cmd { return after() }, func() (func() (bool, *skillsv1.Line, bool, error), func() error, error) {
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

// Shares a skill of the user's own on a registry: as a pull request to the GitHub repository asked for, or on ClawHub
func (s *screen) publish(skill *skillsv1.Skill) tea.Cmd {
	dir := filepath.Dir(skill.GetPath())
	to := "github"
	fields := func(values map[string]string) []cards.Field {
		out := []cards.Field{{ID: "to", Label: "Where", Kind: cards.FieldChoice, Value: to, Choices: []cards.Choice{
			{Value: "github", Label: "GitHub", Meaning: "A pull request to a repository of skills"},
			{Value: "clawhub", Label: "ClawHub", Meaning: "The ClawHub registry of skills"},
		}}}
		if to == "github" {
			out = append(out, cards.Field{ID: "repo", Label: "Repository", Value: values["repo"], Required: true,
				Help: "Written owner/name, such as openai/skills."})
		}
		return out
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  "Publish " + skill.GetName(),
		Intro:  []string{"Share this skill with others.", "Folder: " + dir},
		Fields: fields(nil),
		Submit: "Publish",
		Changed: func(f *cards.Form, field cards.Field) tea.Cmd {
			if field.ID == "to" && field.Value != to {
				to = field.Value
				f.SetFields(fields(f.Values()))
			}
			return nil
		},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			repo, where := strings.TrimSpace(values["repo"]), values["to"]
			return tea.Batch(s.hub.Close(form), s.lines("Publishing "+skill.GetName(), "Published "+skill.GetName()+".", func(bool) tea.Cmd { return nil },
				func() (func() (bool, *skillsv1.Line, bool, error), func() error, error) {
					stream, err := s.service.Publish(s.kit.Env.Ctx, &skillsv1.PublishRequest{Scope: s.scope(), Path: dir, To: where, Repo: repo})
					if err != nil {
						return nil, nil, err
					}
					return linesOf(stream.Receive), stream.Close, nil
				}))
		},
	})
	return s.hub.Push(form)
}

// Words for how a skill from the hub stands against its source
var checkWords = map[string]string{"up_to_date": "Up to date", "update_available": "Update available", "unavailable": "Can't be checked",
	"orphaned": "Gone from its source", "invalid_install": "Install is broken"}

// The skills from the hub checked against their sources, on a page
func (s *screen) check() tea.Cmd {
	s.hub.Busy("Checking for updates…")
	return tui.Call(s.hub, func() (*skillsv1.CheckResponse, error) {
		return s.service.Check(s.kit.Env.Ctx, &skillsv1.CheckRequest{Scope: s.scope()})
	}, func(response *skillsv1.CheckResponse) tea.Cmd {
		s.hub.Quiet()
		t := s.kit.Env.Theme
		checked := response.GetSkills()
		if len(checked) == 0 {
			return s.hub.Done("You have no skills from the hub to check.")
		}
		updates := 0
		width := 0
		for _, row := range checked {
			width = max(width, len([]rune(row.GetName())))
		}
		var lines []string
		for _, row := range checked {
			status := cmp.Or(checkWords[row.GetStatus()], row.GetStatus())
			switch row.GetStatus() {
			case "update_available":
				updates++
				status = render.Warm(status, t)
			case "up_to_date":
				status = render.Good(status, t)
			default:
				status = render.Bad(status, t)
			}
			name := row.GetName() + strings.Repeat(" ", width-len([]rune(row.GetName())))
			lines = append(lines, name+"  "+status+render.Dim("  from "+row.GetSource(), t))
		}
		summary := "Every skill from the hub is up to date."
		if updates > 0 {
			summary = fmt.Sprintf("%d of %s have an update. Update every skill gets them.", updates, render.Count(len(checked), "skill"))
		}
		return s.textPage("Updates", append([]string{summary, ""}, lines...))
	})
}

// A page of lines
func (s *screen) textPage(title string, lines []string) tea.Cmd {
	return s.hub.Push(cards.NewPage(s.kit.Env, cards.PageSpec{Title: title, Key: Screen + ":output", Lines: func(int) []string { return lines }}))
}

// Saves the skills from the hub and the sources to a file of this machine
func (s *screen) export() tea.Cmd {
	field := cards.Field{Label: "File", Value: "skills-snapshot.json",
		Help: "Where to save your skills from the hub and your sources, to install them elsewhere.\nA path that doesn't start with / starts in the session's folder."}
	return s.kit.Ask(s.hub, "Save the list", field, "Save", func(path string) tea.Cmd {
		return tui.Call(s.hub, func() (*skillsv1.ExportResponse, error) {
			response, err := s.service.Export(s.kit.Env.Ctx, &skillsv1.ExportRequest{Scope: s.scope()})
			if err == nil {
				err = os.WriteFile(s.local(path), []byte(response.GetSnapshot()), 0o644)
			}
			return response, err
		}, func(response *skillsv1.ExportResponse) tea.Cmd {
			return s.hub.Done(fmt.Sprintf("Saved %s and %s to %s.", render.Count(int(response.GetSkills()), "skill"), render.Count(int(response.GetTaps()), "source"),
				s.local(path)))
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

// Adds the sources of a saved list and installs its skills, without asking
func (s *screen) importList(after func() tea.Cmd) tea.Cmd {
	field := cards.Field{Label: "File", Help: "A list made by Save the list, or by icebath skills snapshot export."}
	return s.kit.Ask(s.hub, "Install from a list", field, "Install", func(path string) tea.Cmd {
		path = s.local(path)
		return s.run("Installing from a list", "Installing from "+path+"…", func(say func(*skillsv1.Line)) func() tea.Cmd {
			ok, err := s.importSnapshot(path, say)
			if err != nil {
				return s.failed(err)
			}
			return func() tea.Cmd {
				if !ok {
					return tea.Batch(s.hub.Fail("Not every skill on the list was installed. The output above says why."), after())
				}
				return tea.Batch(s.hub.Done("Installed every skill on the list."), after())
			}
		})
	})
}

func (s *screen) importSnapshot(path string, say func(*skillsv1.Line)) (bool, error) {
	ctx := s.kit.Env.Ctx
	note := func(tone skillsv1.Tone, text string) {
		say(&skillsv1.Line{Text: text, Tone: tone})
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		note(skillsv1.Tone_TONE_BAD, "There is no file at "+path+".")
		return false, nil
	}
	if err != nil {
		return false, err
	}
	snap, err := hub.ReadSnapshot(data)
	if err != nil {
		note(skillsv1.Tone_TONE_BAD, path+" is not a saved list of skills.")
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
		note(skillsv1.Tone_TONE_DIM, "Added "+render.Count(len(snap.Taps), "source")+".")
	}
	if len(snap.Skills) == 0 {
		note(skillsv1.Tone_TONE_DIM, "The list names no skills.")
		return true, nil
	}
	note(skillsv1.Tone_TONE_HEADING, "Installing "+render.Count(len(snap.Skills), "skill")+" from the list.")
	var failed []string
	for _, skill := range snap.Skills {
		if skill.Identifier == "" {
			note(skillsv1.Tone_TONE_WARN, "Left out "+cmp.Or(skill.Name, "a skill")+", which has no identifier.")
			continue
		}
		label := cmp.Or(skill.Name, skill.Identifier)
		note(skillsv1.Tone_TONE_HEADING, label)
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
		note(skillsv1.Tone_TONE_BAD, "Not installed: "+strings.Join(failed, ", ")+". The lines above say why.")
		return false, nil
	}
	note(skillsv1.Tone_TONE_GOOD, "Every skill on the list is installed.")
	return true, nil
}

// What can be done to every installed skill
func (s *screen) every() tea.Cmd {
	opt, optTitle := "opt-out", "Stop copying built-in skills"
	if s.state.GetOptedOut() {
		opt, optTitle = "opt-in", "Copy built-in skills again"
	}
	choices := []tui.Choice{
		{ID: "update", Title: "Update every skill", Detail: "Get the newest version of each skill you installed from the hub"},
		{ID: "update-force", Title: "Update over your changes", Detail: "Update every skill, also those you changed, replacing your changes"},
		{ID: "check", Title: "Check for updates", Detail: "See which skills from the hub have a newer version"},
		{ID: "audit", Title: "Scan again", Detail: "Look for danger in the skills from the hub again"},
		{ID: "modified", Title: "Skills you changed", Detail: "Built-in skills you edited, which updates leave alone"},
		{ID: opt, Title: optTitle, Detail: "Whether icebath copies the skills it ships into this profile"},
		{ID: "export", Title: "Save the list", Detail: "Write your skills from the hub to a file, to install them elsewhere"},
		{ID: "import", Title: "Install from a list", Detail: "Install every skill a saved list names"},
	}
	return s.kit.Pick(s.hub, "Manage every skill", "run", choices, "", func(choice tui.Choice) tea.Cmd {
		switch choice.ID {
		case "update", "update-force":
			return s.update("", choice.ID == "update-force", s.reload)
		case "check":
			return s.check()
		case "audit":
			return s.audit()
		case "modified":
			return s.modified()
		case "opt-out", "opt-in":
			s.hub.Busy("Saving…")
			return tui.Call(s.hub, func() (*skillsv1.OptOutResponse, error) {
				return s.service.OptOut(s.kit.Env.Ctx, &skillsv1.OptOutRequest{Scope: s.scope(), Out: choice.ID == "opt-out", Sync: choice.ID == "opt-in"})
			}, func(response *skillsv1.OptOutResponse) tea.Cmd {
				return tea.Batch(s.hub.Done(response.GetMessage()), s.reload())
			})
		case "export":
			return s.export()
		}
		return s.importList(s.reload)
	})
}

// Built-in skills the user changed, each opened to see how it differs or reset
func (s *screen) modified() tea.Cmd {
	s.hub.Busy("Looking for skills you changed…")
	return tui.Call(s.hub, func() (*skillsv1.ModifiedResponse, error) {
		return s.service.Modified(s.kit.Env.Ctx, &skillsv1.ModifiedRequest{Scope: s.scope()})
	}, func(response *skillsv1.ModifiedResponse) tea.Cmd {
		s.hub.Quiet()
		names := response.GetNames()
		if len(names) == 0 {
			return s.hub.Done("You haven't changed any built-in skill, so they all take updates.")
		}
		list := cards.NewList(s.kit.Env, cards.ListSpec{
			Title:   "Skills you changed",
			Glyph:   s.kit.Env.Theme.Glyphs.Ext,
			Key:     Screen + ":modified",
			Context: cards.HubContext,
			Primary: "hub.use",
			Intro: func() []string {
				return []string{"Built-in skills you edited. Updates leave them alone until you reset them."}
			},
			Columns: []render.Column{{Flex: 1}},
			Rows: func() [][]string {
				rows := make([][]string, len(names))
				for i, name := range names {
					rows[i] = []string{name}
				}
				return rows
			},
			Hints: func(_ *cards.List, i int) []wm.Hint {
				if i < 0 {
					return nil
				}
				return []wm.Hint{{Action: "hub.use", Label: "see changes"}, {Action: "hub.edit", Label: "reset"}}
			},
			Actions: map[string]func(*cards.List, int) tea.Cmd{
				"hub.use": func(_ *cards.List, i int) tea.Cmd { return s.diff(names[i]) },
				"hub.edit": func(_ *cards.List, i int) tea.Cmd {
					return s.reset(&skillsv1.Skill{Name: names[i], Source: "builtin", Modified: true})
				},
			},
		})
		return s.hub.Push(list)
	})
}

// How the profile's copy of a built-in skill differs from the one icebath ships, on a page
func (s *screen) diff(name string) tea.Cmd {
	return tui.Call(s.hub, func() (*skillsv1.DiffResponse, error) {
		return s.service.Diff(s.kit.Env.Ctx, &skillsv1.DiffRequest{Scope: s.scope(), Name: name})
	}, func(response *skillsv1.DiffResponse) tea.Cmd {
		t := s.kit.Env.Theme
		files := response.GetFiles()
		if len(files) == 0 {
			return s.hub.Done(name + " matches the copy icebath ships.")
		}
		lines := []string{name + " differs from the copy icebath ships in " + render.Count(len(files), "file") + ".", ""}
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
				lines = append(lines, render.Good("Only in your copy: ", t)+file.GetPath())
			case "removed":
				lines = append(lines, render.Bad("Only in the shipped copy: ", t)+file.GetPath())
			default:
				lines = append(lines, render.Warm(file.GetPath()+" differs, and is not text.", t))
			}
		}
		return s.textPage("Changes to "+name, lines)
	})
}

// Uninstalls a skill the hub installed, after a yes
func (s *screen) uninstall(l *cards.List, skill *skillsv1.Skill) tea.Cmd {
	if skill.GetSource() != "hub" {
		return s.hub.Fail(skill.GetName() + " didn't come from the skill hub, so it can't be uninstalled. Turn it off instead.")
	}
	l.Confirm("Uninstall "+skill.GetName()+"? Its files are deleted.", func() tea.Cmd {
		s.hub.Busy("Uninstalling " + skill.GetName() + "…")
		return tui.Call(s.hub, func() (*skillsv1.UninstallResponse, error) {
			return s.service.Uninstall(s.kit.Env.Ctx, &skillsv1.UninstallRequest{Scope: s.scope(), Name: skill.GetName()})
		}, func(response *skillsv1.UninstallResponse) tea.Cmd {
			if !response.GetOk() {
				return s.hub.Fail(response.GetMessage())
			}
			return tea.Batch(s.hub.PopTo(s.root), s.hub.Done(response.GetMessage()), s.reload())
		})
	})
	return nil
}

// Trusts a folder with its own skills, or stops trusting it, the note saying what happened
func (s *screen) trust(path string, untrust bool, after func() tea.Cmd) tea.Cmd {
	s.hub.Busy("Saving…")
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
