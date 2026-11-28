package app

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	hub "github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/skills"
	"github.com/nickheyer/icebath/pkg/fsx"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Opens the skills you changed: the built-in ones you edited and those you made, each opening its page, then saving your skills from the
// hub to a list, installing from one, and whether icebath copies its own skills into this profile
func (s *screen) changed() tea.Cmd {
	t := s.kit.Env.Theme
	var shown []*skillsv1.Skill
	var acts []*act
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Skills you changed",
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":changed",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{"Built-in skills you edited, and skills you made yourself."}
			if len(s.yours()) == 0 {
				lines = append(lines, "No skill changed yet.")
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown, acts = s.yours(), s.keepActs()
			rows := make([][]string, 0, len(shown)+len(acts))
			for _, skill := range shown {
				rows = append(rows, []string{skill.GetName(), "", skillState(skill.GetShown(), t)})
			}
			for _, a := range acts {
				mark := ""
				if a.opens {
					mark = render.Dim(t.Glyphs.Forward, t)
				}
				rows = append(rows, []string{a.title, mark, ""})
			}
			return rows
		},
		Group: func(i int) string {
			switch {
			case i >= len(shown):
				return "Copies"
			case shown[i].GetSource() == "builtin":
				return "Edited"
			}
			return "Made by you"
		},
		Detail: func(i int) []string {
			switch {
			case i < 0:
				return nil
			case i < len(shown):
				return []string{described(shown[i].GetDescription())}
			}
			return []string{acts[i-len(shown)].about}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return nil
			case i < len(shown):
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: acts[i-len(shown)].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if i < len(shown) {
					return s.skillPage(shown[i].GetName())
				}
				return acts[i-len(shown)].run(l)
			},
		},
	})
	list.ToggleDetail()
	s.views.Add(list)
	return s.hub.Push(list)
}

// What the page of the skills you changed does besides opening them: saving and installing a list, and copying the built-in skills or not
func (s *screen) keepActs() []*act {
	copying := &act{title: "Stop copying built-in skills", about: "icebath no longer copies its own skills into this profile, nor updates them there.",
		verb: "stop", run: func(l *cards.List) tea.Cmd {
			l.Confirm(cards.Question{Text: "Stop copying icebath's own skills into this profile? The ones copied stay, and no update reaches them.",
				Verb: "Stop copying"}, func() tea.Cmd { return s.optOut(true) })
			return nil
		}}
	if s.state.GetOptedOut() {
		copying = &act{title: "Copy built-in skills again", about: "icebath copies its own skills into this profile again and keeps them up to date.",
			verb: "copy", run: func(*cards.List) tea.Cmd { return s.optOut(false) }}
	}
	return []*act{
		{title: "Save the list", about: "Writes your skills from the hub and your sources to a file, to install them elsewhere.", verb: "save", opens: true,
			run: func(*cards.List) tea.Cmd { return s.export() }},
		{title: "Install from a list", about: "Installs every skill a saved list names, and adds its sources.", verb: "install", opens: true,
			run: func(*cards.List) tea.Cmd { return s.importList() }},
		copying,
	}
}

// Stops copying the built-in skills into the profile, or copies them again
func (s *screen) optOut(out bool) tea.Cmd {
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*skillsv1.OptOutResponse, error) {
		return s.service.OptOut(s.kit.Env.Ctx, &skillsv1.OptOutRequest{Scope: s.scope(), Out: out, Sync: !out})
	}, func(response *skillsv1.OptOutResponse) tea.Cmd {
		return tea.Batch(s.hub.Done(response.GetMessage()), s.reload())
	})
}

// How the profile's copy of a built-in skill differs from the one icebath comes with, on a page
func (s *screen) diff(name string) tea.Cmd {
	return tui.Call(s.hub, func() (*skillsv1.DiffResponse, error) {
		return s.service.Diff(s.kit.Env.Ctx, &skillsv1.DiffRequest{Scope: s.scope(), Name: name})
	}, func(response *skillsv1.DiffResponse) tea.Cmd {
		t := s.kit.Env.Theme
		files := response.GetFiles()
		if len(files) == 0 {
			return s.hub.Done(name + " matches the copy icebath comes with.")
		}
		lines := []string{"Your copy differs in " + render.Count(len(files), "file") + ".", ""}
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
				lines = append(lines, render.Bad("Only in the copy icebath comes with: ", t)+file.GetPath())
			default:
				lines = append(lines, render.Warm(file.GetPath()+" differs, and is not text.", t))
			}
		}
		return s.textPage("Changes to "+name, lines)
	})
}

// What is wrong with a path typed for a file, empty when it is a whole path
func whole(typed string) string {
	typed = strings.TrimSpace(typed)
	if !strings.HasPrefix(typed, "/") && !strings.HasPrefix(typed, "~") {
		return "Write the whole path of the file, starting with / or ~, such as ~/skills-list.json."
	}
	return ""
}

// Saves the skills from the hub and the sources to a file of this computer
func (s *screen) export() tea.Cmd {
	field := cards.Field{Label: "File", Path: cards.PathFile, Check: whole,
		Help: "Where to save your skills from the hub and your sources, such as ~/skills-list.json. Tab completes the path."}
	return s.kit.Ask(s.hub, "Save the list", field, "Save", func(path string) tea.Cmd {
		path = filepath.Clean(fsx.ExpandHome(path))
		return tui.Call(s.hub, func() (*skillsv1.ExportResponse, error) {
			response, err := s.service.Export(s.kit.Env.Ctx, &skillsv1.ExportRequest{Scope: s.scope()})
			if err == nil {
				err = os.WriteFile(path, []byte(response.GetSnapshot()), 0o644)
			}
			return response, err
		}, func(response *skillsv1.ExportResponse) tea.Cmd {
			return s.hub.Done(fmt.Sprintf("Saved %s and %s to %s.", render.Count(int(response.GetSkills()), "skill"), render.Count(int(response.GetTaps()), "source"),
				path))
		})
	})
}

// Adds the sources of a saved list and installs its skills, its lines on a page
func (s *screen) importList() tea.Cmd {
	field := cards.Field{Label: "File", Path: cards.PathFile, Check: whole,
		Help: "A list saved by Save the list or by icebath skills snapshot export, such as ~/skills-list.json."}
	return s.kit.Ask(s.hub, "Install from a list", field, "Install", func(path string) tea.Cmd {
		path = filepath.Clean(fsx.ExpandHome(path))
		return s.run("Installing from a list", "Installing from "+path+"…", func(say func(*skillsv1.Line)) func() tea.Cmd {
			ok, err := s.importSnapshot(path, say)
			if err != nil {
				return s.failed(err)
			}
			return func() tea.Cmd {
				if !ok {
					return tea.Batch(s.hub.Fail("Not every skill on the list was installed. The page says why."), s.reload())
				}
				return tea.Batch(s.hub.Done("Installed every skill on the list."), s.reload())
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

// Shares a skill of your own in steps: where, on GitHub as a pull request to a repository of skills or on ClawHub; for GitHub, the
// repository; then the publish, its lines on a page
func (s *screen) publish(skill *skillsv1.Skill) tea.Cmd {
	if skill == nil {
		return nil
	}
	dir := filepath.Dir(skill.GetPath())
	f := s.kit.Flow(s.hub, "Publish "+skill.GetName())
	run := func(to, repo string) tea.Cmd {
		return tea.Batch(f.End(), s.lines("Publishing "+skill.GetName(), "Published "+skill.GetName()+".", func(bool) tea.Cmd { return nil },
			func() (func() (bool, *skillsv1.Line, bool, error), func() error, error) {
				stream, err := s.service.Publish(s.kit.Env.Ctx, &skillsv1.PublishRequest{Scope: s.scope(), Path: dir, To: to, Repo: repo})
				if err != nil {
					return nil, nil, err
				}
				return linesOf(stream.Receive), stream.Close, nil
			}))
	}
	places := []tui.Choice{
		{ID: "github", Title: "GitHub", Detail: "A pull request to a repository of skills"},
		{ID: "clawhub", Title: "ClawHub", Detail: "The ClawHub registry of skills", Verb: "Publish"},
	}
	return f.Pick("Where to share "+skill.GetName()+".", places, "", func(choice tui.Choice) tea.Cmd {
		if choice.ID != "github" {
			return run(choice.ID, "")
		}
		repo := cards.Field{ID: "repo", Label: "Repository", Check: func(typed string) string {
			if owner, name, ok := strings.Cut(strings.TrimSpace(typed), "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
				return "Write the repository as owner/name, such as openai/skills."
			}
			return ""
		}, Help: "The GitHub repository of skills the pull request goes to, written owner/name, such as openai/skills."}
		return f.Ask("", []cards.Field{repo}, "Publish", func(values map[string]string) tea.Cmd {
			return run("github", strings.TrimSpace(values["repo"]))
		})
	})
}
