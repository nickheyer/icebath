package app

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/worktrees/launch"
	worktreesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1/worktreesv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type screens struct {
	trees *launch.Trees
}

func Register(registry *app.Registry, trees *launch.Trees) {
	registry.Register(screens{trees: trees})
}

// What each subcommand of /worktree runs
var handlers = map[string]string{"": "status", "status": "status", "show": "status", "prune": "prune", "gc": "prune", "clean": "prune",
	"list": "list", "ls": "list", "new": "new", "add": "new", "create": "new"}

const usage = "Usage: /worktree [new [name] | list | prune [--dry-run]]"

func (s screens) Screens(conn *connect.Client) []tui.Screen {
	service := worktreesv1connect.NewWorktreeServiceClient(conn)
	stores := sessionsv1connect.NewSessionServiceClient(conn)
	return []tui.Screen{{
		ID:    "worktree",
		Title: "Worktrees",
		Short: "show, list, create, or prune isolated git worktrees",
		Slash: []string{"worktree"},
		Usage: "[new [name]|list|prune [--dry-run]]",
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			w := &worktree{kit: kit, service: service, sessions: stores, trees: s.trees}
			if shown, ok := kit.Focused().(tui.Sessioned); ok {
				w.session = shown.Session()
			}
			return w.run(argument)
		},
	}}
}

// One /worktree of the focused chat's session
type worktree struct {
	kit      *tui.Kit
	service  worktreesv1connect.WorktreeServiceClient
	sessions sessionsv1connect.SessionServiceClient
	trees    *launch.Trees
	session  string
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

func (w *worktree) run(argument string) tea.Cmd {
	sub, rest := split(argument)
	handler, known := handlers[sub]
	if !known {
		return w.page("Worktrees", []string{"Unknown /worktree subcommand: " + sub, usage})
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := w.kit.Env.Ctx
		cwd := w.cwd()
		switch handler {
		case "status":
			status, err := w.service.Status(ctx, &worktreesv1.StatusRequest{Directory: cwd})
			return w.show("Worktrees", err, func() []string { return w.status(status) })
		case "list":
			listed, err := w.service.List(ctx, &worktreesv1.ListRequest{Directory: cwd})
			return w.show("Worktrees", err, func() []string {
				switch {
				case listed.GetRepo() == "":
					return []string{"Not inside a git repository."}
				case len(listed.GetLines()) == 0:
					return []string{"Could not list worktrees."}
				}
				return listed.GetLines()
			})
		case "new":
			return w.create(cwd, rest)
		}
		return w.prune(cwd, dry(rest))
	})
}

// Folder the session works in, else the window's
func (w *worktree) cwd() string {
	if w.session != "" {
		if got, err := w.sessions.Get(w.kit.Env.Ctx, &sessionsv1.GetRequest{Id: w.session}); err == nil && got.GetSession().GetDirectory() != "" {
			return got.GetSession().GetDirectory()
		}
	}
	return w.kit.Env.WorkDir()
}

// Lines of the status: the tree the window works in or none, and what to do next inside a repository
func (w *worktree) status(status *worktreesv1.StatusResponse) []string {
	var lines []string
	if active := w.trees.Active(); active != nil {
		lines = append(lines, "Active worktree: "+active.GetPath(), "Branch: "+active.GetBranch())
	} else {
		lines = append(lines, "No active worktree for this session.")
	}
	if status.GetRepo() == "" {
		return append(lines, "(not inside a git repository)")
	}
	return append(lines, "/worktree new [name] — create one and move this session into it", "/worktree prune      — reclaim stale trees and merged branches")
}

// Pages the lines once the call answers, or says why it failed
func (w *worktree) show(title string, err error, lines func() []string) func() tea.Cmd {
	return func() tea.Cmd {
		if err != nil {
			return wm.Notify("❌ "+tui.Reason(err), wm.Error)
		}
		return w.page(title, lines())
	}
}

func (w *worktree) page(title string, lines []string) tea.Cmd {
	page := cards.NewPage(w.kit.Env, cards.PageSpec{
		Title: title,
		Key:   "worktree:" + w.session,
		Lines: func(width int) []string {
			var out []string
			for _, line := range lines {
				out = append(out, strings.Split(render.Wrap(line, max(width, 1)), "\n")...)
			}
			return out
		},
	})
	return w.kit.WM.ShowOverlay(w.kit.Hub(page.Key(), page))
}

// Makes a tree of the session's repository and moves the session and the window into it; a tree the session could not enter is
// settled again
func (w *worktree) create(cwd, name string) func() tea.Cmd {
	ctx := w.kit.Env.Ctx
	created, err := w.service.Create(ctx, &worktreesv1.CreateRequest{Directory: cwd, Name: name, Pid: int64(os.Getpid())})
	if err != nil {
		return w.show("Worktrees", err, nil)
	}
	if created.GetRepo() == "" {
		return func() tea.Cmd {
			return w.page("Worktrees", []string{"❌ /worktree new requires being inside a git repository."})
		}
	}
	lines := created.GetLines()
	tree := created.GetTree()
	if tree == nil {
		return func() tea.Cmd { return w.page("Worktrees", lines) }
	}
	entered, err := w.service.Enter(ctx, &worktreesv1.EnterRequest{SessionId: w.session, Tree: tree})
	if err != nil {
		lines = append(lines, "⚠ Created worktree but could not enter it: "+tui.Reason(err))
		if settled, err := w.service.Settle(ctx, &worktreesv1.SettleRequest{Tree: tree, Pid: int64(os.Getpid())}); err == nil {
			lines = append(lines, settled.GetLines()...)
		} else {
			lines = append(lines, "❌ "+tui.Reason(err))
		}
		return func() tea.Cmd { return w.page("Worktrees", lines) }
	}
	lines = append(lines, entered.GetLines()...)
	return func() tea.Cmd {
		w.trees.Made(tree)
		path := tree.GetPath()
		w.kit.Env.WorkDir = func() string { return path }
		w.kit.Env.ProjectDir, w.kit.Env.Root = path, path
		if err := os.Chdir(path); err != nil {
			lines = append(lines, fmt.Sprintf("⚠ Could not enter %s: %v", path, err))
		}
		return w.page("Worktrees", lines)
	}
}

// Reclaims the trees and merged branches of the session's repository, never the tree the window works in, or says what it would
func (w *worktree) prune(cwd string, dryRun bool) func() tea.Cmd {
	request := &worktreesv1.PruneRequest{Directory: cwd, DryRun: dryRun}
	if active := w.trees.Active(); active != nil {
		request.Keep = active.GetPath()
	}
	pruned, err := w.service.Prune(w.kit.Env.Ctx, request)
	if failure, ok := err.(*connect.Error); ok && failure.Code() == connect.CodeFailedPrecondition {
		return func() tea.Cmd { return w.page("Worktrees", []string{"Not inside a git repository."}) }
	}
	return w.show("Worktrees", err, func() []string {
		lines := append([]string{}, pruned.GetActions()...)
		switch {
		case len(lines) > 0 && dryRun:
			lines = append(lines, fmt.Sprintf("%d action(s) planned.", len(lines)))
		case len(lines) > 0:
			lines = append(lines, fmt.Sprintf("%d action(s) done.", len(lines)))
		default:
			lines = append(lines, "Nothing to reclaim — remaining trees/branches carry real work.")
		}
		if preserved := pruned.GetPreserved(); len(preserved) > 0 {
			lines = append(lines, fmt.Sprintf("Preserved %d tree(s) with real work:", len(preserved)))
			for _, kept := range preserved {
				lines = append(lines, "  "+kept.GetName()+": "+kept.GetReason())
			}
		}
		return lines
	})
}
