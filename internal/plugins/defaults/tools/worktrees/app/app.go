package app

import (
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/worktrees/launch"
	worktreesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1/worktreesv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the list of worktrees
const worktreeContext = "worktree"

var worktreeKeys = []keys.Action{
	{ID: "worktree.restore", Context: worktreeContext, Label: "restore", Doc: "Make the highlighted archived worktree again", Default: []string{"enter"}},
	{ID: "worktree.preview", Context: worktreeContext, Label: "preview prune", Doc: "Show what a prune would delete, without deleting it", Default: []string{"d"}},
	{ID: "worktree.prune", Context: worktreeContext, Label: "prune", Doc: "Delete the worktrees and merged branches that hold no work", Default: []string{"p"}},
}

type screens struct {
	trees *launch.Trees
}

func Register(registry *app.Registry, trees *launch.Trees) {
	registry.Register(screens{trees: trees})
}

// What each subcommand of /worktree runs
var handlers = map[string]string{"": "status", "status": "status", "show": "status", "prune": "prune", "gc": "prune", "clean": "prune",
	"list": "list", "ls": "list", "new": "new", "add": "new", "create": "new"}

func (s screens) Screens(conn *connect.Client) []tui.Screen {
	service := worktreesv1connect.NewWorktreeServiceClient(conn)
	stores := sessionsv1connect.NewSessionServiceClient(conn)
	return []tui.Screen{{
		ID:      "worktree",
		Group:   tui.GroupAbilities,
		Order:   110,
		Title:   "Worktrees",
		Short:   "Create, list and clean up git worktrees",
		Slash:   []string{"worktree"},
		Usage:   "[new [name]|list|prune [--dry-run]]",
		Actions: worktreeKeys,
		Where:   map[string]string{worktreeContext: "worktrees"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			w := &worktree{kit: kit, service: service, sessions: stores, trees: s.trees}
			if shown, ok := kit.Focused().(tui.Sessioned); ok {
				w.session = shown.Session()
			}
			return w.run(argument)
		},
	}}
}

// The worktrees of the focused chat's repository in a list
type worktree struct {
	kit      *tui.Kit
	service  worktreesv1connect.WorktreeServiceClient
	sessions sessionsv1connect.SessionServiceClient
	trees    *launch.Trees
	session  string
	hub      *cards.Hub
	list     *cards.List
	// Folder the session works in, else the window's, read once
	cwd   string
	audit *worktreesv1.AuditResponse
	// Whether the folder is in a git repository, known once it was checked
	repo   bool
	loaded bool
	err    error
	rows   []entry
}

// One row of the list: a worktree under .worktrees, one registered elsewhere, or an archive of one
type entry struct {
	tree     *worktreesv1.TreeRecord
	external *worktreesv1.ExternalTree
	archive  *worktreesv1.Archive
}

// Subcommand and the rest of the argument
func split(argument string) (string, string) {
	argument = strings.TrimSpace(argument)
	at := strings.IndexFunc(argument, unicode.IsSpace)
	if at < 0 {
		return strings.ToLower(argument), ""
	}
	return strings.ToLower(argument[:at]), strings.TrimSpace(argument[at:])
}

// Whether a prune is a dry run
func dry(rest string) bool {
	rest = strings.ToLower(rest)
	if strings.Contains(rest, "--dry-run") {
		return true
	}
	for _, word := range strings.Fields(rest) {
		if word == "-n" {
			return true
		}
	}
	return false
}

// Opens the list, then runs what the subcommand asks over it
func (w *worktree) run(argument string) tea.Cmd {
	sub, rest := split(argument)
	handler, known := handlers[sub]
	open := w.open()
	return w.load(open, func() tea.Cmd {
		switch {
		case !known:
			return w.hub.Fail("There is no /worktree " + sub + ". It takes new and a name, list, or prune and --dry-run.")
		case !w.repo:
			// The list says the folder is in no repository
			return nil
		case handler == "list":
			return w.listed()
		case handler == "new" && rest != "":
			return w.create(rest)
		case handler == "new":
			return w.ask()
		case handler == "prune" && dry(rest):
			return w.prune(true)
		case handler == "prune":
			w.list.Confirm(prunes, func() tea.Cmd { return w.prune(false) })
		}
		return nil
	})
}

func (w *worktree) open() tea.Cmd {
	t := w.kit.Env.Theme
	w.list = cards.NewList(w.kit.Env, cards.ListSpec{
		Title:   "Worktrees",
		Glyph:   t.Glyphs.Jobs,
		Key:     "worktree:" + w.session,
		Context: worktreeContext,
		Shared:  []string{cards.HubContext},
		Primary: "worktree.restore",
		Intro:   w.intro,
		Columns: []render.Column{{}, {}, {}},
		Rows: func() [][]string {
			rows := make([][]string, len(w.rows))
			for i, e := range w.rows {
				rows[i] = w.cells(e)
			}
			return rows
		},
		Group: func(i int) string {
			switch e := w.rows[i]; {
			case e.tree != nil:
				return "Worktrees"
			case e.external != nil:
				return "Elsewhere"
			}
			return "Archived"
		},
		Detail: func(i int) []string { return w.detail(w.rows[i]) },
		Empty:  w.empty,
		Free:   map[string]bool{"hub.add": true, "worktree.preview": true, "worktree.prune": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if !w.repo {
				return nil
			}
			var out []wm.Hint
			if i >= 0 && i < len(w.rows) && w.rows[i].archive != nil {
				out = append(out, wm.Hint{Action: "worktree.restore"})
			}
			out = append(out, wm.Hint{Action: "hub.add"})
			if len(w.audit.GetTrees()) > 0 || len(w.audit.GetBranches()) > 0 {
				out = append(out, wm.Hint{Action: "worktree.preview"}, wm.Hint{Action: "worktree.prune"})
			}
			return out
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"worktree.restore": func(_ *cards.List, i int) tea.Cmd {
				if archive := w.rows[i].archive; archive != nil {
					return w.restore(archive)
				}
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				if !w.repo {
					return nil
				}
				return w.ask()
			},
			"worktree.preview": func(*cards.List, int) tea.Cmd {
				if !w.repo {
					return nil
				}
				return w.prune(true)
			},
			"worktree.prune": func(l *cards.List, _ int) tea.Cmd {
				if !w.repo {
					return nil
				}
				l.Confirm(prunes, func() tea.Cmd { return w.prune(false) })
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return w.load(nil, nil) },
		},
	})
	w.list.ToggleDetail()
	w.hub = w.kit.Hub(w.list.Key(), w.list)
	return w.kit.WM.ShowOverlay(w.hub)
}

// Lines above the rows: the repository, and the worktree the session works in
func (w *worktree) intro() []string {
	if !w.repo {
		return nil
	}
	lines := []string{"Worktrees of " + w.audit.GetRepo() + "."}
	if active := w.trees.Active(); active != nil {
		said := "This session works in " + active.GetPath()
		if branch := active.GetBranch(); branch != "" {
			said += ", on the branch " + branch
		}
		lines = append(lines, said+".")
	} else {
		lines = append(lines, "This session works in the repository itself, in no worktree.")
	}
	return lines
}

// What the list says while it has no rows
func (w *worktree) empty() string {
	switch {
	case w.err != nil:
		return "Could not read the worktrees. " + strs.Full(tui.Reason(w.err)) + " " + w.kit.Env.Keys.Display("hub.refresh") + " tries again."
	case !w.loaded:
		return "Reading the worktrees…"
	case !w.repo:
		return "This folder is not inside a git repository, so it has no worktrees."
	}
	return "No worktrees yet. " + w.kit.Env.Keys.Display("hub.add") + " adds one and moves this session into it."
}

// Whether the worktree is the one the session works in
func (w *worktree) here(path string) bool {
	active := w.trees.Active()
	return active != nil && filepath.Clean(active.GetPath()) == filepath.Clean(path)
}

// Cells of a row: its name, its branch and how it stands
func (w *worktree) cells(e entry) []string {
	t := w.kit.Env.Theme
	switch {
	case e.tree != nil:
		state := render.Good("Holds work", t)
		switch {
		case w.here(e.tree.GetPath()):
			state = render.Cool("This session works here", t)
		case strings.HasPrefix(e.tree.GetVerdict(), "reap"):
			state = render.Dim("Can be pruned", t)
		}
		return []string{e.tree.GetName(), render.Dim(branchOf(e.tree.GetBranch()), t), state}
	case e.external != nil:
		state := ""
		switch {
		case w.here(e.external.GetPath()):
			state = render.Cool("This session works here", t)
		case e.external.GetMissing():
			state = render.Warm("Folder is gone", t)
		case e.external.GetLocked():
			state = render.Dim("Locked", t)
		}
		return []string{e.external.GetPath(), render.Dim(branchOf(e.external.GetBranch()), t), state}
	}
	return []string{e.archive.GetName(), render.Dim(branchOf(e.archive.GetBranch()), t), render.Dim("Archived "+ago(e.archive.GetCreated().AsTime()), t)}
}

// Branch as a row names it, or what stands for none
func branchOf(branch string) string {
	if branch == "" {
		return "No branch"
	}
	return branch
}

// Length of time in words, rounded down to its largest whole unit
func span(d time.Duration) string {
	for _, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute} {
		if d >= unit {
			return config.Span(d.Truncate(unit))
		}
	}
	return config.Span(max(d, 0).Truncate(time.Second))
}

func ago(at time.Time) string { return span(time.Since(at)) + " ago" }

// Lines under the list about a row: where it is, how old and large, and why it holds work or can go
func (w *worktree) detail(e entry) []string {
	t := w.kit.Env.Theme
	switch {
	case e.tree != nil:
		lines := []string{e.tree.GetPath()}
		facts := []string{span(time.Duration(e.tree.GetAgeDays()*float64(24*time.Hour))) + " old"}
		if e.tree.SizeMb != nil {
			facts = append(facts, config.Size(float64(e.tree.GetSizeMb())*1024*1024))
		}
		if n := len(e.tree.GetUntracked()); n > 0 {
			facts = append(facts, render.Count(n, "untracked file"))
		}
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
		if reason := e.tree.GetReason(); reason != "" {
			lines = append(lines, strs.Full(reason))
		}
		return lines
	case e.external != nil && e.external.GetMissing():
		return []string{e.external.GetPath(), render.Warm("Its folder is gone, and git still lists it. A prune makes git forget it.", t)}
	case e.external != nil:
		return []string{e.external.GetPath(), "Registered outside .worktrees, so a prune never touches it."}
	}
	lines := []string{"An archive of the worktree " + e.archive.GetName() + ". Restoring makes the worktree again."}
	if reason := e.archive.GetReason(); reason != "" {
		lines = append(lines, strs.Full("Archived because "+reason))
	}
	if path := e.archive.GetPath(); path != "" {
		lines = append(lines, render.Dim("Was in "+path, t))
	}
	if n := len(e.archive.GetSkipped()); n > 0 {
		lines = append(lines, render.Warm(render.Count(n, "large untracked file")+" did not fit in the archive and will be missing.", t))
	}
	return lines
}

// Checks the folder, then reads its worktrees; first runs before the read when given, then after it once the worktrees are in
func (w *worktree) load(first tea.Cmd, then func() tea.Cmd) tea.Cmd {
	return tea.Batch(first, tui.Async(func() func() tea.Cmd {
		ctx := w.kit.Env.Ctx
		cwd := w.cwd
		if cwd == "" {
			cwd = w.folder()
		}
		status, err := w.service.Status(ctx, &worktreesv1.StatusRequest{Directory: cwd})
		var audit *worktreesv1.AuditResponse
		if err == nil && status.GetRepo() != "" {
			audit, err = w.service.Audit(ctx, &worktreesv1.AuditRequest{Directory: cwd})
		}
		return func() tea.Cmd {
			w.cwd, w.err, w.loaded = cwd, err, true
			if err == nil {
				w.repo, w.audit = status.GetRepo() != "", audit
				w.rows = w.rows[:0]
				for _, tree := range audit.GetTrees() {
					w.rows = append(w.rows, entry{tree: tree})
				}
				for _, external := range audit.GetExternal() {
					w.rows = append(w.rows, entry{external: external})
				}
				for _, archive := range audit.GetArchives() {
					w.rows = append(w.rows, entry{archive: archive})
				}
			}
			w.list.Refresh()
			if then != nil && err == nil {
				return then()
			}
			return nil
		}
	}))
}

// Folder the session works in, else the window's
func (w *worktree) folder() string {
	if w.session != "" {
		if got, err := w.sessions.Get(w.kit.Env.Ctx, &sessionsv1.GetRequest{Id: w.session}); err == nil && got.GetSession().GetDirectory() != "" {
			return got.GetSession().GetDirectory()
		}
	}
	return w.kit.Env.WorkDir()
}

// Question asked before a prune
var prunes = cards.Question{Text: "Delete the worktrees and merged branches that hold no work? Worktrees that hold work stay.", Verb: "Prune",
	Deletes: true}

// Pushes a page of lines over the list
func (w *worktree) page(title string, lines []string) tea.Cmd {
	return w.hub.Push(cards.NewPage(w.kit.Env, cards.PageSpec{Title: title, Key: "worktree:" + w.session + ":" + title, Lines: wrapped(lines)}))
}

// Lines of a page wrapped to its width
func wrapped(lines []string) func(width int) []string {
	return func(width int) []string {
		var out []string
		for _, line := range lines {
			out = append(out, strings.Split(render.Wrap(line, max(width, 1)), "\n")...)
		}
		return out
	}
}

// Pushes the page of what a prune would do over the list, the prune itself on its key there
func (w *worktree) preview(lines []string) tea.Cmd {
	title := "What a prune would do"
	return w.hub.Push(cards.NewPage(w.kit.Env, cards.PageSpec{
		Title:   title,
		Key:     "worktree:" + w.session + ":" + title,
		Lines:   wrapped(lines),
		Context: worktreeContext,
		Hints:   func(*cards.Page) []wm.Hint { return []wm.Hint{{Action: "worktree.prune"}} },
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"worktree.prune": func(p *cards.Page) tea.Cmd {
				p.Confirm(prunes, func() tea.Cmd { return tea.Batch(w.hub.Close(p), w.prune(false)) })
				return nil
			},
		},
	}))
}

// Pages the worktrees as git lists them
func (w *worktree) listed() tea.Cmd {
	if !w.repo {
		return nil
	}
	return tui.Call(w.hub, func() (*worktreesv1.ListResponse, error) {
		return w.service.List(w.kit.Env.Ctx, &worktreesv1.ListRequest{Directory: w.cwd})
	}, func(listed *worktreesv1.ListResponse) tea.Cmd {
		if len(listed.GetLines()) == 0 {
			return w.hub.Say("git listed no worktrees.", cards.NoteInfo)
		}
		return w.page("As git lists them", listed.GetLines())
	})
}

// Asks the name of a worktree to add, a made up one when left empty
func (w *worktree) ask() tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(w.kit.Env, cards.FormSpec{
		Title: "Add a worktree",
		Intro: []string{"Make a worktree of " + w.audit.GetRepo() + " on a branch of its own, and move this session into it."},
		Fields: []cards.Field{{ID: "name", Label: "Worktree name",
			Help: "The name of the worktree and of its branch, such as fix-login.\nLeft empty, icebath makes one up."}},
		Submit: "Add",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(w.hub.Close(form), w.create(strings.TrimSpace(values["name"])))
		},
	})
	return w.hub.Push(form)
}

// Makes a worktree of the session's repository and moves the session and the window into it; a worktree the session could not enter is
// settled again. What happened is paged
func (w *worktree) create(name string) tea.Cmd {
	w.hub.Busy("Making the worktree…")
	return tui.Async(func() func() tea.Cmd {
		ctx := w.kit.Env.Ctx
		created, err := w.service.Create(ctx, &worktreesv1.CreateRequest{Directory: w.cwd, Name: name, Pid: int64(os.Getpid())})
		if err != nil {
			return func() tea.Cmd { return w.hub.Fail("Could not make the worktree. " + strs.Full(tui.Reason(err))) }
		}
		if created.GetRepo() == "" {
			return func() tea.Cmd { return w.hub.Fail("A worktree can only be made inside a git repository.") }
		}
		lines := created.GetLines()
		tree := created.GetTree()
		if tree == nil {
			return func() tea.Cmd {
				w.hub.Quiet()
				return w.page("Add a worktree", lines)
			}
		}
		entered, err := w.service.Enter(ctx, &worktreesv1.EnterRequest{SessionId: w.session, Tree: tree})
		if err != nil {
			lines = append(lines, "", "The worktree was made, but this session could not move into it. "+strs.Full(tui.Reason(err)))
			// A worktree the tidying up left there is put in hand on the list, where it can be pruned
			kept := true
			if settled, err := w.service.Settle(ctx, &worktreesv1.SettleRequest{Tree: tree, Pid: int64(os.Getpid())}); err == nil {
				lines = append(lines, settled.GetLines()...)
				kept = !settled.GetGone()
			} else {
				lines = append(lines, "Could not tidy the worktree up. "+strs.Full(tui.Reason(err)))
			}
			return func() tea.Cmd {
				w.hub.Quiet()
				return tea.Batch(w.page("Add a worktree", lines), w.load(nil, func() tea.Cmd {
					for i, row := range w.rows {
						if kept && row.tree != nil && row.tree.GetPath() == tree.GetPath() {
							w.list.Select(i)
						}
					}
					return nil
				}))
			}
		}
		lines = append(lines, entered.GetLines()...)
		return func() tea.Cmd {
			w.trees.Made(tree)
			path := tree.GetPath()
			w.kit.Env.WorkDir = func() string { return path }
			w.kit.Env.ProjectDir, w.kit.Env.Root = path, path
			if err := os.Chdir(path); err != nil {
				lines = append(lines, "", "This window could not move into "+path+". "+strs.Full(err.Error()))
			}
			return tea.Batch(w.page("Add a worktree", lines), w.hub.Done("This session now works in the worktree "+filepath.Base(path)+"."), w.load(nil, nil))
		}
	})
}

// Makes an archived worktree again, then reads the worktrees again
func (w *worktree) restore(archive *worktreesv1.Archive) tea.Cmd {
	w.hub.Busy("Restoring " + archive.GetName() + "…")
	return tui.Call(w.hub, func() (*worktreesv1.RestoreResponse, error) {
		return w.service.Restore(w.kit.Env.Ctx, &worktreesv1.RestoreRequest{Directory: w.cwd, Name: archive.GetName()})
	}, func(restored *worktreesv1.RestoreResponse) tea.Cmd {
		if failure := restored.GetError(); failure != "" {
			return tea.Batch(w.hub.Fail("Could not restore "+archive.GetName()+". "+strs.Full(failure)), w.load(nil, nil))
		}
		said := "Restored " + archive.GetName() + " to " + restored.GetPath()
		if branch := restored.GetBranch(); branch != "" && !restored.GetDetached() {
			said += ", on the branch " + branch
		}
		said += "."
		if n := len(restored.GetSkipped()); n > 0 {
			said += " " + render.Count(n, "large untracked file") + " could not come back."
		}
		return tea.Batch(w.hub.Done(said), w.load(nil, nil))
	})
}

// Deletes the worktrees and merged branches of the session's repository that hold no work, never the worktree the window works in, or
// says what it would. What happened is paged
func (w *worktree) prune(dryRun bool) tea.Cmd {
	request := &worktreesv1.PruneRequest{Directory: w.cwd, DryRun: dryRun}
	if active := w.trees.Active(); active != nil {
		request.Keep = active.GetPath()
	}
	if dryRun {
		w.hub.Busy("Working out what a prune would delete…")
	} else {
		w.hub.Busy("Pruning…")
	}
	return tui.Async(func() func() tea.Cmd {
		pruned, err := w.service.Prune(w.kit.Env.Ctx, request)
		return func() tea.Cmd {
			if failure, ok := err.(*connect.Error); ok && failure.Code() == connect.CodeFailedPrecondition {
				return w.hub.Fail("This folder is not inside a git repository, so it has no worktrees.")
			}
			if err != nil {
				return w.hub.Fail("Could not prune. " + strs.Full(tui.Reason(err)))
			}
			w.hub.Quiet()
			lines := append([]string{}, pruned.GetActions()...)
			n := len(lines)
			switch {
			case n > 0 && dryRun:
				lines = append(lines, "", render.Count(n, "change")+" would be made.")
			case n > 0:
				lines = append(lines, "", render.Count(n, "change")+" made.")
			default:
				lines = append(lines, "Nothing to delete: every worktree and branch left holds work.")
			}
			if preserved := pruned.GetPreserved(); len(preserved) > 0 {
				lines = append(lines, "", "Kept "+render.Count(len(preserved), "worktree")+" that hold work:")
				for _, kept := range preserved {
					lines = append(lines, "  "+kept.GetName()+": "+kept.GetReason())
				}
			}
			if dryRun && n > 0 {
				return tea.Batch(w.preview(lines), w.load(nil, nil))
			}
			title := "Prune"
			if dryRun {
				title = "What a prune would do"
			}
			return tea.Batch(w.page(title, lines), w.load(nil, nil))
		}
	})
}
