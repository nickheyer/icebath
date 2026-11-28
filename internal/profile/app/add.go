package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/pkg/fsx"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Ways to add a profile, and how a new one starts
const (
	wayMake    = "make"
	wayImport  = "import"
	wayInstall = "install"

	startNothing  = "nothing"
	startSettings = "settings"
	startAll      = "all"
)

// What the flow of adding a profile has been told so far
type adding struct {
	way, name, start, from string
}

// Adds a profile in steps: how, then its name, then for a new one what it starts from, which profile it copies when there are several, and
// after a copy whether its keys and sign-ins come too; for a file the file, for a distribution where it comes from and what it brings. The
// profile is made by the last step alone, with a shell command of its own, and is in hand on the list once it is
func (p *profiles) add() tea.Cmd {
	flow := p.kit.Flow(p.hub, "Add a profile")
	a := &adding{}
	ways := []tui.Choice{
		{ID: wayMake, Title: "Make one", Detail: "A new set of settings and sessions of its own"},
		{ID: wayImport, Title: "Import a file", Detail: "A profile someone exported to a file"},
		{ID: wayInstall, Title: "Install a distribution", Detail: "A ready-made profile from a git address or a folder"},
	}
	return flow.Pick("How to add it.", ways, "", func(choice tui.Choice) tea.Cmd {
		a.way = choice.ID
		return p.askName(flow, a)
	})
}

// Asks the name of the new profile: needed for a new one, else the name the file or the distribution gives when left empty
func (p *profiles) askName(flow *tui.Flow, a *adding) tea.Cmd {
	field := cards.Field{ID: "name", Label: "Profile name", Required: a.way == wayMake,
		Help: "Lower case letters, digits and dashes, such as work or research.", Check: p.checkName(a.way != wayMake)}
	if a.way != wayMake {
		field.Help += "\nLeave it empty for the name the " + map[string]string{wayImport: "file", wayInstall: "distribution"}[a.way] + " gives."
	}
	return flow.Ask("", []cards.Field{field}, "Next", func(values map[string]string) tea.Cmd {
		a.name = strings.TrimSpace(values["name"])
		switch a.way {
		case wayImport:
			return p.askFile(flow, a)
		case wayInstall:
			return p.askSource(flow, a)
		}
		return p.askStart(flow, a)
	})
}

// Why a name typed for a new profile can't be it, empty when it can: one a profile has already, or one no profile can have
func (p *profiles) checkName(optional bool) func(string) string {
	return func(typed string) string {
		typed = strings.TrimSpace(typed)
		if typed == "" && optional {
			return ""
		}
		canon, err := profile.Canon(typed)
		switch {
		case err != nil:
			return tui.Reason(err)
		case p.find(canon) != nil:
			return "There is a profile named " + canon + " already."
		}
		return ""
	}
}

// Asks what a new profile starts from, then the profile it copies when there is a choice of them
func (p *profiles) askStart(flow *tui.Flow, a *adding) tea.Cmd {
	starts := []tui.Choice{
		{ID: startNothing, Title: "Nothing", Detail: "Default settings, no keys, and the skills that come with icebath", Verb: "Add"},
		{ID: startSettings, Title: "A copy of the settings", Detail: "The settings and skills of a profile you have"},
		{ID: startAll, Title: "A full copy", Detail: "Everything of a profile you have except its history"},
	}
	return flow.Pick("What "+a.name+" starts from.", starts, startNothing, func(choice tui.Choice) tea.Cmd {
		a.start = choice.ID
		if a.start == startNothing {
			return p.create(flow, a, false)
		}
		if len(p.rows) < 2 {
			a.from = p.active().GetName()
			return p.askKeys(flow, a)
		}
		var sources []tui.Choice
		for _, row := range p.rows {
			choice := tui.Choice{ID: row.GetName(), Title: label(row)}
			if row.GetActive() {
				choice.Detail = "The profile this window runs on"
				sources = slices.Insert(sources, 0, choice)
				continue
			}
			sources = append(sources, choice)
		}
		return flow.Pick("The profile "+a.name+" is a copy of.", sources, p.active().GetName(), func(choice tui.Choice) tea.Cmd {
			a.from = choice.ID
			return p.askKeys(flow, a)
		})
	})
}

// Asks whether a copy takes the keys and sign-ins of the profile it copies, then makes it
func (p *profiles) askKeys(flow *tui.Flow, a *adding) tea.Cmd {
	keys := []tui.Choice{
		{ID: "copy", Title: "Copy them", Detail: "The API keys and sign-ins of " + a.from + ", copied once"},
		{ID: "none", Title: "None", Detail: "No keys or sign-ins. Add them in the new profile"},
	}
	return flow.PickVerb("Keys and sign-ins of "+a.name+".", keys, "copy", "Add", func(choice tui.Choice) tea.Cmd {
		return p.create(flow, a, choice.ID == "none")
	})
}

// Makes the profile the flow was told of, with a shell command of its own, ending the flow with it in hand; a refusal is said under the step
// in view, which stays
func (p *profiles) create(flow *tui.Flow, a *adding, bare bool) tea.Cmd {
	request := &profilev1.CreateRequest{Name: a.name, NoCredentials: bare || a.start == startNothing}
	if a.start != startNothing {
		request.Clone, request.From, request.All = true, a.from, a.start == startAll
	}
	p.hub.Busy("Adding " + a.name + "…")
	return tui.Call(p.hub, func() (*profilev1.CreateResponse, error) {
		return p.service.Create(p.kit.Env.Ctx, request)
	}, func(made *profilev1.CreateResponse) tea.Cmd {
		name := made.GetProfile().GetName()
		_, failed := profile.Command(name)
		return p.added(flow, name, "Added "+name+".", append(made.GetWarnings(), failed)...)
	})
}

// Ends the flow with the profile added in hand on the list, saying so and what failed on the way
func (p *profiles) added(flow *tui.Flow, name, told string, failed ...string) tea.Cmd {
	p.hub.Quiet()
	said := p.hub.Done(told)
	if problems := joined(failed...); problems != "" {
		said = p.hub.Fail(joined(told, problems))
	}
	return tea.Batch(flow.End(), p.load(func() tea.Cmd {
		p.point(name)
		return nil
	}), said)
}

// Asks for the file to import, then imports it
func (p *profiles) askFile(flow *tui.Flow, a *adding) tea.Cmd {
	field := cards.Field{ID: "file", Label: "File", Required: true, Path: cards.PathFile,
		Help: "The .tar.gz file someone exported, such as ~/work.tar.gz. Tab completes it."}
	return flow.Ask("", []cards.Field{field}, "Import", func(values map[string]string) tea.Cmd {
		path := fsx.ExpandHome(strings.TrimSpace(values["file"]))
		p.hub.Busy("Importing " + filepath.Base(path) + "…")
		return tui.Call(p.hub, func() (*profilev1.ImportResponse, error) {
			data, err := os.ReadFile(path)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errors.New("There is no file at " + path + ".")
			}
			if err != nil {
				return nil, err
			}
			return p.service.Import(p.kit.Env.Ctx, &profilev1.ImportRequest{Archive: data, Name: a.name})
		}, func(imported *profilev1.ImportResponse) tea.Cmd {
			_, failed := profile.Command(imported.GetName())
			return p.added(flow, imported.GetName(), "Imported "+imported.GetName()+".", failed)
		})
	})
}

// Asks where the distribution comes from, then shows what installing it would do, as the last step
func (p *profiles) askSource(flow *tui.Flow, a *adding) tea.Cmd {
	field := cards.Field{ID: "source", Label: "Distribution", Required: true, Path: cards.PathFolder,
		Help: "A git address, such as github.com/user/repo, or a folder that holds distribution.yaml."}
	return flow.Ask("", []cards.Field{field}, "Next", func(values map[string]string) tea.Cmd {
		source := strings.TrimSpace(values["source"])
		if expanded := fsx.ExpandHome(source); fsx.IsDir(expanded) {
			if abs, err := filepath.Abs(expanded); err == nil {
				source = abs
			}
		}
		p.hub.Busy("Reading the distribution…")
		return tui.Call(p.hub, func() (*profilev1.PlanResponse, error) {
			return p.service.Plan(p.kit.Env.Ctx, &profilev1.PlanRequest{Source: source, Name: a.name})
		}, func(planned *profilev1.PlanResponse) tea.Cmd {
			p.hub.Quiet()
			return flow.Show(p.planPage(flow, planned.GetPlan(), source, a.name))
		})
	})
}

// Lines of what an install would do: the distribution, where it goes, what it replaces, the keys it uses and the jobs it brings
func planLines(plan *profilev1.Plan, t theme.Theme) []string {
	lines := manifestLines(plan.GetManifest(), t)
	lines = append(lines, "", "From: "+plan.GetProvenance(), "Goes to: "+profile.Tilde(plan.GetTarget()))
	if plan.GetExisting() {
		if plan.GetDistribution() {
			lines = append(lines, "", "A profile of that name is there already. Only the files this distribution owns are replaced.")
		} else {
			lines = append(lines, "", render.Warm("A profile of that name is there already, and it is not a distribution. Its SOUL.md, its mcp.json, and any "+
				"skill or scheduled job of the same name are replaced. Your memories, sessions, sign-ins and keys file stay.", t))
		}
		// An install writes the distribution's settings over the profile's; only an update leaves them
		if !plan.GetPreservesConfig() {
			lines = append(lines, render.Warm("Its settings in config.yaml are replaced with the distribution's when it brings them.", t))
		}
	}
	if plan.GetHasCron() {
		lines = append(lines, "", render.Warm("It brings scheduled jobs in cron/jobs.json, and this build of icebath does not run them.", t))
	}
	return lines
}

// Last step of installing a distribution: what it would do, installed on enter, a profile of that name replaced after a question; the flow
// ends with the profile in hand and what to do next, when anything is, in the note
func (p *profiles) planPage(flow *tui.Flow, plan *profilev1.Plan, source, name string) *cards.Page {
	t := p.kit.Env.Theme
	m := plan.GetManifest()
	lines := planLines(plan, t)
	verb := "install"
	if plan.GetExisting() {
		verb = "replace"
	}
	install := func() tea.Cmd {
		p.hub.Busy("Installing " + m.GetName() + "…")
		return tui.Call(p.hub, func() (*profilev1.InstallResponse, error) {
			return p.service.Install(p.kit.Env.Ctx, &profilev1.InstallRequest{Source: source, Name: name, Force: plan.GetExisting()})
		}, func(installed *profilev1.InstallResponse) tea.Cmd {
			done := installed.GetPlan()
			made := done.GetManifest().GetName()
			told := "Installed " + made + version(done.GetManifest().GetVersion()) + "."
			if len(done.GetManifest().GetEnv()) > 0 {
				told += " It uses keys, so fill in .env.EXAMPLE in " + profile.Tilde(done.GetTarget()) + " and save it as .env."
			}
			if done.GetHasCron() {
				told += " It brought scheduled jobs in cron/jobs.json, and this build of icebath does not run them."
			}
			_, failed := profile.Command(filepath.Base(done.GetTarget()))
			return p.added(flow, filepath.Base(done.GetTarget()), told, failed)
		})
	}
	var page *cards.Page
	page = cards.NewPage(p.kit.Env, cards.PageSpec{
		Title: "Add a profile",
		Key:   "install:" + m.GetName(),
		Lines: func(w int) []string {
			var out []string
			for _, line := range lines {
				out = append(out, strings.Split(render.Wrap(line, max(w, 8)), "\n")...)
			}
			return out
		},
		Context: cards.HubContext,
		Hints:   func(*cards.Page) []wm.Hint { return []wm.Hint{{Action: "hub.use", Label: verb}} },
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"hub.use": func(*cards.Page) tea.Cmd {
				if !plan.GetExisting() {
					return install()
				}
				page.Confirm(cards.Question{Text: "Replace " + filepath.Base(plan.GetTarget()) + " with " + m.GetName() + "? The files the distribution " +
					"brings are written over, and your sessions, memories and keys stay.", Verb: "Replace"}, install)
				return nil
			},
		},
	})
	return page
}
