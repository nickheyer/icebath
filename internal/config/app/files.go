package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Page of the files the settings and keys are kept in, read as it opens: where they are and whether they exist, each opened in the editor,
// the settings file checked after an edit and the keys counted again, and their paths copied
func (s *settings) filesPage() (*cards.Hub, tea.Cmd) {
	page := s.on(nil)
	view, reread := page.files()
	page.hub = s.kit.Hub(Settings+filesArea, view)
	return page.hub, reread
}

// List of what can be done with the files, and the reading of where they are that fills it
func (s *settings) files() (*cards.List, tea.Cmd) {
	t := s.kit.Env.Theme
	var files *configv1.FilesResponse
	var acts []action
	var loaded bool
	var failed error
	where := func(path string, exists bool) string {
		if exists {
			return path
		}
		return path + " (not made yet)"
	}
	detail := func() []string {
		var lines []string
		if own := files.GetOwn(); own != "" {
			lines = append(lines, "Settings file: "+where(own, files.GetOwnExists()))
		}
		// The dated copies icebath keeps of the settings file, which a broken one is put back from
		if copies := files.GetBackups(); copies != "" && files.GetOwnExists() {
			lines = append(lines, "Copies of it: "+copies)
		}
		lines = append(lines, "Keys file: "+where(files.GetEnv(), files.GetEnvExists()))
		switch {
		case files.GetManagedSystem() != "":
			lines = append(lines, render.Dim(files.GetManagedSystem()+" manages this install, so its files are not edited here.", t))
		case files.GetOwn() == "":
			lines = append(lines, render.Dim("This profile keeps its settings in a store that has no file, so there is nothing to edit by hand.", t))
		default:
			lines = append(lines, render.Dim("icebath reads a change you make by hand the next time it needs the setting.", t))
		}
		return lines
	}
	var view *cards.List
	var reread func() tea.Cmd
	reread = func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := s.service.Files(s.kit.Env.Ctx, &configv1.FilesRequest{Profile: s.named})
			return func() tea.Cmd {
				loaded, failed = true, err
				if err == nil {
					files = response
					acts = s.fileActions(files)
				}
				view.Refresh()
				if err != nil {
					return s.fail(tui.Reason(err))
				}
				return nil
			}
		})
	}
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Settings files",
		Glyph:   t.Glyphs.Settings,
		Key:     "settings:files",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"Open config.yaml and your keys file in an editor"} },
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(acts))
			for i, act := range acts {
				rows[i] = []string{act.title, render.Dim(act.about, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if view.Shows(1) {
				return detail()
			}
			return append([]string{acts[i].about, ""}, detail()...)
		},
		Empty: func() string { return tui.Waiting(loaded, failed, "Nothing to do with the files of this profile.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: acts[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return acts[i].run() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reread() },
		},
		Free: map[string]bool{"hub.refresh": true},
	})
	view.ToggleDetail()
	return view, reread()
}

// What the files page offers for the files as they are: editing the settings file and the keys file, there yet or not, unless a package
// manager keeps them or the profile keeps no file, checking the settings file once it is there, and copying both paths
func (s *settings) fileActions(files *configv1.FilesResponse) []action {
	var out []action
	own := files.GetOwn()
	if files.GetManagedSystem() == "" && own != "" {
		out = append(out, action{title: "Edit the settings file", about: "Open config.yaml in your editor, then check it", verb: "edit",
			run: func() tea.Cmd { return s.editSettings(files) }})
		// Offered before the file is there too: the first key a person keeps by hand makes it
		if s.local(files, files.GetEnv()) {
			out = append(out, action{title: "Edit the keys file", about: "Open .env, where your API keys are kept, in your editor", verb: "edit",
				run: func() tea.Cmd { return s.editKeys(files.GetEnv()) }})
		}
	}
	if own != "" && files.GetOwnExists() {
		out = append(out, action{title: "Check the settings file", about: "Look for mistakes and for keys that are missing", verb: "check", run: s.check})
	}
	if own != "" {
		out = append(out, action{title: "Copy the path of the settings file", about: "Put where config.yaml is on the clipboard", verb: "copy",
			run: func() tea.Cmd { return s.copy(own, "settings file") }})
	}
	return append(out, action{title: "Copy the path of the keys file", about: "Put where .env is on the clipboard", verb: "copy",
		run: func() tea.Cmd { return s.copy(files.GetEnv(), "keys file") }})
}

// Whether the file is on this machine, where the editor runs, rather than on the machine of a server the app reaches
func (s *settings) local(files *configv1.FilesResponse, path string) bool {
	host, _ := os.Hostname()
	info, err := os.Stat(filepath.Dir(path))
	return host == files.GetHost() && err == nil && info.IsDir()
}

func (s *settings) copy(path, what string) tea.Cmd {
	return tea.Batch(clip.Copy(path), s.hub.Done("Copied the path of the "+what+"."))
}

// Runs the editor on the file with the terminal handed over to it, then runs then once it is back; unopened runs instead when the editor
// never opened the file, such as to remove a copy made for it
func (s *settings) edit(path string, then func() tea.Cmd, unopened func()) tea.Cmd {
	never := func(why string) tea.Cmd {
		if unopened != nil {
			unopened()
		}
		return s.hub.Fail(why)
	}
	editor := config.Editor()
	if editor == "" {
		return never("No editor was found. Set EDITOR, or open the file yourself: " + path)
	}
	// An editor that ends with a status of its own still ran; the answer comes back as the message the callback returns
	return tea.ExecProcess(config.EditorCommand(s.kit.Env.Ctx, editor, path), func(err error) tea.Msg {
		return tui.Async(func() func() tea.Cmd {
			return func() tea.Cmd {
				if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
					return never("The editor did not start: " + err.Error())
				}
				return then()
			}
		})()
	})
}

// Opens the profile's config.yaml in the editor, made first from the example when it is not there, then checks it: in place on this
// machine, else a copy saved back through the server
func (s *settings) editSettings(files *configv1.FilesResponse) tea.Cmd {
	path := files.GetOwn()
	open := func(made bool) tea.Cmd {
		note := func() tea.Cmd { return nil }
		if made {
			note = func() tea.Cmd { return s.hub.Done("Made " + path + " from the example.") }
		}
		if s.local(files, path) {
			return tea.Batch(note(), s.edit(path, s.check, nil))
		}
		return tui.Call(s.hub, func() (*configv1.ReadResponse, error) {
			return s.service.Read(s.kit.Env.Ctx, &configv1.ReadRequest{Path: path, Profile: s.named})
		}, func(read *configv1.ReadResponse) tea.Cmd {
			temp, err := os.MkdirTemp("", "icebath-config-")
			if err != nil {
				return s.hub.Fail(err.Error())
			}
			copied := filepath.Join(temp, filepath.Base(path))
			if err := os.WriteFile(copied, []byte(read.GetText()), 0o600); err != nil {
				os.RemoveAll(temp)
				return s.hub.Fail(err.Error())
			}
			return tea.Batch(note(), s.edit(copied, func() tea.Cmd {
				edited, err := os.ReadFile(copied)
				os.RemoveAll(temp)
				if err != nil {
					return s.hub.Fail(err.Error())
				}
				if string(edited) == read.GetText() {
					return s.check()
				}
				return tui.Call(s.hub, func() (*configv1.WriteResponse, error) {
					return s.service.Write(s.kit.Env.Ctx, &configv1.WriteRequest{Path: path, Text: string(edited), Profile: s.named})
				}, func(*configv1.WriteResponse) tea.Cmd { return s.check() })
				// The copy holds what the settings file holds, so it never outlives an editor that did not open it
			}, func() { os.RemoveAll(temp) }))
		})
	}
	if files.GetOwnExists() {
		return open(false)
	}
	return tui.Call(s.hub, func() (*configv1.SeedResponse, error) {
		return s.service.Seed(s.kit.Env.Ctx, &configv1.SeedRequest{Path: path, Profile: s.named})
	}, func(seeded *configv1.SeedResponse) tea.Cmd { return open(seeded.GetSeeded()) })
}

// Opens the profile's .env in the editor, then says how many of its variables changed, which icebath reads at their next use
func (s *settings) editKeys(path string) tea.Cmd {
	before, err := config.ReadDotenv(path)
	if err != nil {
		return s.hub.Fail(err.Error())
	}
	// A keys file that is not there yet is made empty and readable by its owner alone, so the editor does not make one others can read
	if file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		file.Close()
	} else {
		return s.hub.Fail("Could not make the keys file. " + strs.Capital(strings.TrimSuffix(err.Error(), ".")) + ".")
	}
	return s.edit(path, func() tea.Cmd {
		after, err := config.ReadDotenv(path)
		if err != nil {
			return s.hub.Fail(err.Error())
		}
		updated := 0
		for name, value := range after {
			if old, ok := before[name]; !ok || old != value {
				updated++
			}
		}
		for name := range before {
			if _, ok := after[name]; !ok {
				updated++
			}
		}
		if updated == 0 {
			return s.hub.Done("Read the keys file again. Nothing in it changed.")
		}
		return s.hub.Done("Read the keys file again. " + render.Count(updated, "key") + " changed.")
	}, nil)
}

// Checks the settings file and the keys the settings read, the report shown over the hub in the order Settings files gives it: what is wrong
// in the file, or that nothing is, then the keys that are needed and missing, those that are set, and last one line counting the others
// that are not set, followed by their names
func (s *settings) check() tea.Cmd {
	return tui.Call(s.hub, func() (*configv1.CheckResponse, error) {
		return s.service.Check(s.kit.Env.Ctx, &configv1.CheckRequest{Profile: s.named})
	}, func(report *configv1.CheckResponse) tea.Cmd {
		t := s.kit.Env.Theme
		var lines []string
		part := func(heading string, body []string) {
			if len(body) == 0 {
				return
			}
			lines = append(append(lines, "", render.Heading(heading, t)), body...)
		}
		if problems := report.GetDiagnostics(); len(problems) > 0 {
			lines = append(lines, render.Heading("Problems in config.yaml", t))
			for _, problem := range problems {
				lines = append(lines, render.Warm("  "+problem, t))
			}
		} else {
			lines = append(lines, render.Good("No problems found in config.yaml.", t))
		}
		var missing, set, unset []string
		named := func(variable *configv1.Variable) string {
			line := "  " + variable.GetName()
			if about := variable.GetAbout(); about != "" {
				line += render.Dim("  "+about, t)
			}
			return line
		}
		for _, variable := range report.GetRequired() {
			if variable.GetSet() {
				set = append(set, named(variable))
			} else {
				missing = append(missing, render.Warm(named(variable), t))
			}
		}
		for _, variable := range report.GetOptional() {
			if variable.GetSet() {
				set = append(set, named(variable))
			} else {
				unset = append(unset, variable.GetName())
			}
		}
		part("Keys that are needed and missing", missing)
		part("Keys that are set", set)
		if len(unset) > 0 {
			verb := " are not set."
			if len(unset) == 1 {
				verb = " is not set."
			}
			lines = append(lines, "", render.Count(len(unset), "other key")+verb, render.Dim(strings.Join(unset, ", "), t))
		}
		return s.hub.Push(cards.NewPage(s.kit.Env, cards.PageSpec{Title: "Check the settings file", Key: "settings:check",
			Lines: func(w int) []string {
				var out []string
				for _, line := range lines {
					out = append(out, strings.Split(render.Wrap(line, max(w, 8)), "\n")...)
				}
				return out
			}}))
	})
}
