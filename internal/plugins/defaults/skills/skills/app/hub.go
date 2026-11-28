package app

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Skills a page of the skill hub shows
const hubPage = 50

// Registries the skill hub searches, by the name the service takes, every one of them first
var registries = []tui.Choice{
	{ID: "all", Title: "Every registry", Detail: "Search them all, official skills first"},
	{ID: "official", Title: "Official", Detail: "Skills icebath makes"},
	{ID: "skills-sh", Title: "skills.sh"}, {ID: "well-known", Title: "Well-known sites"}, {ID: "github", Title: "GitHub", Detail: "Your sources too"},
	{ID: "clawhub", Title: "ClawHub"}, {ID: "lobehub", Title: "LobeHub"}, {ID: "browse-sh", Title: "browse.sh"}, {ID: "nvidia", Title: "NVIDIA"},
	{ID: "openai", Title: "OpenAI"}, {ID: "anthropic", Title: "Anthropic"}, {ID: "huggingface", Title: "Hugging Face"},
	{ID: "voltagent", Title: "VoltAgent"}, {ID: "gstack", Title: "gstack"}, {ID: "minimax", Title: "MiniMax"},
}

// Title of a registry, its name when the page does not know it
func registry(id string) string {
	for _, r := range registries {
		if r.ID == id {
			return r.Title
		}
	}
	return id
}

// One page of the skill hub: what a browse or a search found, and the registries that were too slow
type found struct {
	skills []*skillsv1.HubSkill
	pages  int32
	late   []string
}

// Row of the skill hub: a skill, something the page does, or a step to the next or the page before
type hubRow struct {
	skill *skillsv1.HubSkill
	act   *act
	step  int
}

// Opens the skill hub: searching it, the registry it searches, installing a skill by name and updating your skills, then every skill the
// registries offer a page at a time, or those a search found; enter installs one, or reads one installed already
func (s *screen) findSkills() tea.Cmd {
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
	// Once a skill is installed the registries are read again for what they now mark as installed, and your skills for what the page and
	// the pages under it say of them
	installed := func() tea.Cmd { return tea.Batch(reload(), s.reload()) }
	t := s.kit.Env.Theme
	acts := func() []*act {
		out := []*act{
			{title: "Search", value: s.query, about: "Searches the registries by name, description and tags.", verb: "search", opens: true,
				run: func(*cards.List) tea.Cmd { return s.search(reload) }},
			{title: "Registry", value: registry(cmp.Or(s.source, "all")), about: "Which registry of the skill hub the page lists and searches.",
				verb: "change", run: func(*cards.List) tea.Cmd { return s.pickRegistry(reload) }},
			{title: "Install by name", about: "A skill you know the name, the identifier or the address of.", verb: "install", opens: true,
				run: func(*cards.List) tea.Cmd { return s.installByName(installed) }},
		}
		// Updates are of the skills installed from the hub, so the row shows once there is one
		if slices.ContainsFunc(s.state.GetSkills(), func(skill *skillsv1.Skill) bool { return skill.GetSource() == "hub" }) {
			out = append(out, &act{title: "Updates", about: "Whether your skills from the hub have newer versions, and getting them.", verb: "open",
				opens: true, run: func(*cards.List) tea.Cmd { return s.updates() }})
		}
		return out
	}
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Find skills",
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":hub",
		Context: context,
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search this page",
		Intro: func() []string {
			lines := []string{"Skills you can install from the registries of the skill hub."}
			again := s.kit.Env.Keys.Display("hub.refresh") + " tries again."
			switch {
			case !loaded:
			case failed != nil:
				lines = append(lines, render.Warm("The registries could not be read. "+tui.Reason(failed), t))
			case len(page.skills) == 0 && s.query != "":
				lines = append(lines, render.Warm("Nothing in the registries matches “"+s.query+"”.", t))
			case len(page.skills) == 0:
				lines = append(lines, render.Warm("The registries listed nothing. Check your network, then "+again, t))
			case len(page.late) > 0:
				lines = append(lines, render.Warm("These registries were too slow and were left out: "+strings.Join(page.late, ", ")+". "+again, t))
			}
			return lines
		},
		Status: func() string {
			if s.query != "" {
				return "Found for “" + s.query + "”"
			}
			if page.pages > 1 {
				return fmt.Sprintf("Page %d of %d", s.page, page.pages)
			}
			return ""
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			rows = rows[:0]
			for _, a := range acts() {
				rows = append(rows, hubRow{act: a})
			}
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
				out[i] = s.hubCells(r, page.pages)
			}
			return out
		},
		Group: func(i int) string {
			if rows[i].act != nil {
				return ""
			}
			return "Skills"
		},
		Words: func(i int) string {
			if skill := rows[i].skill; skill != nil {
				return strings.Join(skill.GetTags(), " ") + " " + skill.GetIdentifier() + " " + skill.GetDescription()
			}
			return ""
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(rows) {
				return nil
			}
			return s.hubDetail(rows[i])
		},
		Free: map[string]bool{"hub.add": true, "skills.search": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			others := []wm.Hint{{Action: "skills.search", Label: "search"}, {Action: "hub.add", Label: "install by name"}}
			if i < 0 || i >= len(rows) {
				return others
			}
			switch r := rows[i]; {
			case r.act != nil:
				return append([]wm.Hint{{Action: "hub.use", Label: r.act.verb}}, others...)
			case r.skill == nil:
				return append([]wm.Hint{{Action: "hub.use", Label: "show"}}, others...)
			case r.skill.GetInstalled():
				return append([]wm.Hint{{Action: "hub.use", Label: "read"}}, others...)
			}
			return append([]wm.Hint{{Action: "hub.use", Label: "install"}, {Action: "hub.edit", Label: "read"}}, others...)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				switch r := rows[i]; {
				case r.act != nil:
					return r.act.run(l)
				case r.step != 0:
					s.page += r.step
					return reload()
				case r.skill.GetInstalled():
					return s.inspectHub(cmp.Or(r.skill.GetIdentifier(), r.skill.GetName()), installed)
				default:
					return s.install(cmp.Or(r.skill.GetIdentifier(), r.skill.GetName()), "", "", installed)
				}
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if skill := rows[i].skill; skill != nil {
					return s.inspectHub(cmp.Or(skill.GetIdentifier(), skill.GetName()), installed)
				}
				return nil
			},
			"hub.add":       func(*cards.List, int) tea.Cmd { return s.installByName(installed) },
			"skills.search": func(*cards.List, int) tea.Cmd { return s.search(reload) },
			"hub.refresh":   func(*cards.List, int) tea.Cmd { return reload() },
		},
	})
	list.ToggleDetail()
	s.views.Add(list)
	return tea.Batch(s.hub.Push(list), reload())
}

// Cells of a row of the skill hub: what the page does with its value, a skill with where it comes from, how far it is trusted and whether it
// is installed, or a step to another page
func (s *screen) hubCells(r hubRow, pages int32) []string {
	t := s.kit.Env.Theme
	switch {
	case r.act != nil && r.act.opens:
		return []string{r.act.title, render.Dim(t.Glyphs.Forward, t), r.act.value}
	case r.act != nil:
		return []string{r.act.title, "", r.act.value}
	case r.step > 0:
		return []string{"Next page", "", render.Dim(fmt.Sprintf("Page %d of %d", s.page+1, pages), t)}
	case r.step < 0:
		return []string{"Page before", "", render.Dim(fmt.Sprintf("Page %d of %d", s.page-1, pages), t)}
	}
	// Where it comes from by the registry's name, and how far it is trusted when that says more
	said := registry(r.skill.GetLabel())
	if trust := trustWords[r.skill.GetTrust()]; trust != "" && !strings.EqualFold(trust, said) {
		said += " · " + trust
	}
	if r.skill.GetInstalled() {
		return []string{r.skill.GetName(), "", render.Good("Installed", t) + render.Dim(" · "+said, t)}
	}
	return []string{r.skill.GetName(), "", render.Dim(said, t)}
}

// Lines under the skill hub about the row in hand: what a skill does and what its registry says of it
func (s *screen) hubDetail(r hubRow) []string {
	t := s.kit.Env.Theme
	switch {
	case r.act != nil:
		return []string{r.act.about}
	case r.skill == nil:
		return nil
	}
	it := r.skill
	lines := []string{described(it.GetDescription()), render.Dim("Identifier: "+it.GetIdentifier(), t)}
	if tags := it.GetTags(); len(tags) > 0 {
		lines = append(lines, render.Dim("Tags: "+strings.Join(tags, ", "), t))
	}
	for _, detail := range it.GetDetails() {
		lines = append(lines, render.Dim(detail, t))
	}
	return lines
}

// Searches the registries for words typed, in the registry the page shows; nothing typed browses every skill again
func (s *screen) search(reload func() tea.Cmd) tea.Cmd {
	field := cards.Field{ID: "query", Label: "Words", Value: s.query, Select: true,
		Help: "Words of a name, a description or a tag, such as pdf.\nLeave empty to browse every skill again."}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{Title: "Search", Fields: []cards.Field{field}, Submit: "Search",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			s.query, s.page = strings.TrimSpace(values["query"]), 1
			return tea.Batch(s.hub.Close(form), reload())
		}})
	return s.hub.Push(form)
}

// Picks the registry the page lists and searches from the registries of the skill hub
func (s *screen) pickRegistry(reload func() tea.Cmd) tea.Cmd {
	return s.kit.PickFrom(s.hub, "Registry", []string{"Which registry of the skill hub to list and search."}, "choose", registries,
		cmp.Or(s.source, "all"), func(choice tui.Choice) tea.Cmd {
			s.source, s.page = choice.ID, 1
			if choice.ID == "all" {
				s.source = ""
			}
			return reload()
		})
}

// Asks for a skill by its name, identifier or address, then installs it
func (s *screen) installByName(reload func() tea.Cmd) tea.Cmd {
	field := cards.Field{Label: "Skill", Help: "A name, an identifier such as openai/skills/skill-creator, or the address of a SKILL.md."}
	return s.kit.Ask(s.hub, "Install by name", field, "Install", func(id string) tea.Cmd { return s.install(id, "", "", reload) })
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
			op.add(render.Heading(skill.GetName(), t), oneLine(skill.GetDescription()), from, render.Dim("Identifier: "+skill.GetIdentifier(), t))
			if tags := skill.GetTags(); len(tags) > 0 {
				op.add(render.Dim("Tags: "+strings.Join(tags, ", "), t))
			}
			op.add(skill.GetDetails()...)
			if preview := final.GetPreview(); preview != "" {
				op.add("", render.Heading("The start of its SKILL.md", t))
				op.add(strings.Split(preview, "\n")...)
			}
			if !skill.GetInstalled() {
				op.act, op.label = func() tea.Cmd { return s.install(skill.GetIdentifier(), "", "", after) }, "install"
			}
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
			return s.inspectHub(name, s.reload)
		}
	})
}

// Fetches, quarantines and scans a skill on a page, then waits there for the person's yes; after runs once it is installed
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

// Name a skill at an address may be given: lower case letters, digits, - and _, from a letter
var skillName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// What a staged install leaves the person: a name to give, a skill to install, its result, or the install to force
func (s *screen) staged(op *operation, identifier, name, category string, outcome skillsv1.Outcome, pending *skillsv1.Pending, after func() tea.Cmd) tea.Cmd {
	t := s.kit.Env.Theme
	switch outcome {
	case skillsv1.Outcome_OUTCOME_NEEDS_NAME:
		field := cards.Field{Label: "Skill name", Check: func(typed string) string {
			if !skillName.MatchString(strings.TrimSpace(typed)) {
				return "Use lower case letters, digits, - and _, starting with a letter."
			}
			return ""
		}, Help: "The skill at " + pending.GetUrl() + " has no name of its own. Give it one, such as pdf-tools."}
		return s.kit.Ask(s.hub, "Name the skill", field, "Install", func(name string) tea.Cmd {
			s.stage(op, identifier, name, category, false, after)
			return nil
		})
	case skillsv1.Outcome_OUTCOME_STAGED:
		// The staged copy is dropped as the page leaves the screen; shown again, its page says so and offers to fetch and scan it again
		// in place of an install that is no longer there
		s.waiting[pending.GetId()] = func() {
			op.add("", render.Warm("The scanned copy was set aside when this page left the screen, so nothing waits to be installed.", t))
			op.act, op.label = func() tea.Cmd {
				op.add("")
				s.stage(op, identifier, name, category, false, after)
				return nil
			}, "scan again"
		}
		where := pending.GetCategory()
		if where != "" {
			where += "/"
		}
		files := fmt.Sprintf("Its files go in %s/%s%s/.", pending.GetSkillsDir(), where, pending.GetName())
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
		return s.hub.Done("Read the scan, then " + s.kit.Env.Keys.Display("skills.go") + " installs " + pending.GetName() + ".")
	case skillsv1.Outcome_OUTCOME_INSTALLED:
		return tea.Batch(s.hub.Done("Installed "+identifier+"."), after())
	case skillsv1.Outcome_OUTCOME_FAILED:
		op.act, op.label = func() tea.Cmd {
			op.page.Confirm(cards.Question{Text: "Install " + identifier + " anyway? The scan stopped it, or it could not be fetched cleanly.",
				Verb: "Install"}, func() tea.Cmd { return s.again(op, identifier, name, category, after) })
			return nil
		}, "install anyway"
		return s.hub.Fail(identifier + " was not installed. The page says why.")
	}
	op.act, op.label = func() tea.Cmd {
		op.page.Confirm(cards.Question{Text: "Install " + identifier + " again? A fresh copy replaces the one you have.", Verb: "Install"},
			func() tea.Cmd { return s.again(op, identifier, name, category, after) })
		return nil
	}, "reinstall"
	return s.hub.Done("Nothing to install. " + identifier + " is installed already.")
}

// Runs an install again on the same page, over the installed copy and past what the scan found
func (s *screen) again(op *operation, identifier, name, category string, after func() tea.Cmd) tea.Cmd {
	op.add("")
	s.stage(op, identifier, name, category, true, after)
	return nil
}

// Installs a staged skill, picking its folder among your skills first when it is a skill at an address
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
				return s.hub.Fail(pending.GetName() + " was not installed. The page says why.")
			}
		})
		return nil
	}
	if !pending.GetAskCategory() {
		return do("")
	}
	return s.pickCategory(pending, do)
}

// Picks the folder among your skills a skill goes in: one you have, the top of the skills folder, or a new one typed
func (s *screen) pickCategory(pending *skillsv1.Pending, done func(category string) tea.Cmd) tea.Cmd {
	const fresh = "\x00new"
	choices := []tui.Choice{{ID: "", Title: "No folder", Detail: "At the top of your skills folder", Verb: "Install"}}
	for _, category := range pending.GetCategories() {
		choices = append(choices, tui.Choice{ID: category, Title: category, Verb: "Install"})
	}
	choices = append(choices, tui.Choice{ID: fresh, Title: "A new folder", Detail: "One you name"})
	intro := []string{"The folder among your skills that " + pending.GetName() + " goes in."}
	return s.kit.PickFrom(s.hub, "Folder", intro, "choose", choices, pending.GetCategory(), func(choice tui.Choice) tea.Cmd {
		if choice.ID != fresh {
			return done(choice.ID)
		}
		field := cards.Field{Label: "Folder", Check: func(typed string) string {
			if !categoryName.MatchString(strings.TrimSpace(typed)) {
				return "Use lower case letters, digits, -, _ and /, starting with a letter."
			}
			return ""
		}, Help: "The name of the new folder, such as research."}
		return s.kit.Ask(s.hub, "A new folder", field, "Install", func(name string) tea.Cmd { return done(name) })
	})
}

// Name of a folder among the skills
var categoryName = regexp.MustCompile(`^[a-z][a-z0-9_/-]*$`)

// Drops the installs left waiting when the page closes
func (s *screen) discardWaiting() tea.Cmd {
	ids := make([]string, 0, len(s.waiting))
	for id, dropped := range s.waiting {
		ids = append(ids, id)
		dropped()
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
